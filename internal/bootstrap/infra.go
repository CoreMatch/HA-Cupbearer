package bootstrap

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"HA-Services-Deployer/internal/docker"

	_ "github.com/go-sql-driver/mysql"
)

// 基础设施（MySQL/Redis）是两条部署链路共用的：裸金属连宿主地址+发布端口，
// compose 连服务名+容器口。本文件负责探测、接管复用、必要时一键拉起容器，
// 并等待"真能用"（MySQL 能执行查询、Redis 能 PING）才返回。
//
// 幂等与复用纪律：已有可用实例一律接管复用，绝不重复起；只管理本层命名的
// 容器（ha-infra-*），不删任何既有容器或卷。

const (
	// DefaultInfraCompose 是基础设施编排文件（本仓所有，不碰主 compose 模板）。
	DefaultInfraCompose = "deploy/infra.yml"
	// InfraNetwork 是 mysql/redis 所在网络；主栈挂入后即可按服务名解析。
	InfraNetwork = "ha-infra"
	// 受管容器名（compose 项目名 ha-infra + 服务名）。
	MySQLContainerName = "ha-infra-mysql-1"
	RedisContainerName = "ha-infra-redis-1"
	// compose 网络内的服务名/容器口（容器链路连接形态）。
	MySQLComposeAddress = "mysql:3306"
	RedisComposeAddress = "redis:6379"

	mysqlContainerPort = 3306
	redisContainerPort = 6379

	infraReadyTimeout  = 180 * time.Second
	infraPollInterval  = 2 * time.Second
	probeTimeout       = 3 * time.Second
)

// InfraDocker 是基础设施步骤需要的容器运行时操作面（docker.CLI 实现）。
type InfraDocker interface {
	Info(ctx context.Context) error
	ComposeUpEnv(ctx context.Context, files []string, extraEnv []string) (string, error)
	Inspect(ctx context.Context, name string) (*docker.ContainerState, error)
	NetworkExists(ctx context.Context, name string) (bool, error)
}

// InfraInstance 是单个基础设施实例的事实（同一份事实推导两种地址形态）。
type InfraInstance struct {
	Kind             string `json:"kind"` // mysql | redis
	Mode             string `json:"mode"` // container | external | absent
	ContainerName    string `json:"container_name,omitempty"`
	ComposeService   string `json:"compose_service,omitempty"`
	Image            string `json:"image,omitempty"`
	HostPort         int    `json:"host_port,omitempty"`
	ContainerPort    int    `json:"container_port,omitempty"`
	BaremetalAddress string `json:"baremetal_address,omitempty"` // 宿主地址+发布端口
	ComposeAddress   string `json:"compose_address,omitempty"`   // 服务名+容器口
	DataVolume       string `json:"data_volume,omitempty"`
	Healthy          bool   `json:"healthy"`
	Detail           string `json:"detail,omitempty"`
}

// InfraFacts 是 MySQL+Redis 的现状与推导结果。
type InfraFacts struct {
	MySQL InfraInstance `json:"mysql"`
	Redis InfraInstance `json:"redis"`
}

// EnsureInfra 探测并按需拉起 MySQL/Redis，把派生凭据与连接事实写回 in，
// 返回双实例的事实。幂等：已有可用实例接管复用，不重复起。
func EnsureInfra(ctx context.Context, in *Inputs, store *Store, dk InfraDocker, composeFile string) (*InfraFacts, error) {
	if composeFile == "" {
		composeFile = DefaultInfraCompose
	}
	facts := &InfraFacts{}

	mysqlFacts, err := ensureMySQL(ctx, in, store, dk, composeFile)
	if err != nil {
		return nil, err
	}
	facts.MySQL = *mysqlFacts

	redisFacts, err := ensureRedis(ctx, in, store, dk, composeFile)
	if err != nil {
		return nil, err
	}
	facts.Redis = *redisFacts

	return facts, nil
}

// ensureMySQL 处理 MySQL：接管已有 / 一键拉起，并把管理口事实写回 in。
func ensureMySQL(ctx context.Context, in *Inputs, store *Store, dk InfraDocker, composeFile string) (*InfraInstance, error) {
	hostPort := in.Infra.MySQLHostPort
	inst := &InfraInstance{
		Kind:             "mysql",
		ComposeService:   "mysql",
		ContainerName:    MySQLContainerName,
		HostPort:         hostPort,
		ContainerPort:    mysqlContainerPort,
		BaremetalAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)),
		ComposeAddress:   MySQLComposeAddress,
		DataVolume:       "ha-infra-mysql-data",
	}

	// 1. 我们受管的容器已存在 → 复用（确保在跑 + 等健康）。
	// 端口以状态记录的发布口为准：换端口意味着重建容器，而本层不删
	// 容器——请求了不同端口时沿用既有端口并如实说明。
	if st, err := dk.Inspect(ctx, MySQLContainerName); err == nil && st != nil {
		if saved := store.GetInfraPort("mysql"); saved != 0 && saved != hostPort {
			inst.Detail = fmt.Sprintf("受管容器已发布在宿主端口 %d，本次请求的 %d 被忽略（换端口需先移除现有容器，本层不删容器）", saved, hostPort)
			hostPort = saved
			inst.HostPort = saved
			inst.BaremetalAddress = net.JoinHostPort("127.0.0.1", strconv.Itoa(saved))
		}
		rootPW, _, err := store.EnsureInfraSecret(KeyMySQLRootPassword)
		if err != nil {
			return nil, err
		}
		redisPW, _, err := store.EnsureInfraSecret(KeyRedisPassword)
		if err != nil {
			return nil, err
		}
		if _, err := launchInfra(ctx, dk, composeFile, hostPort, redisHostPort(store, in), rootPW, redisPW); err != nil {
			return nil, fmt.Errorf("复用受管 MySQL 容器失败：%w", err)
		}
		if err := waitMySQLReady(ctx, "127.0.0.1", hostPort, "root", rootPW); err != nil {
			return nil, fmt.Errorf("受管 MySQL 容器未在时限内就绪：%w", err)
		}
		if err := store.SetInfraPort("mysql", hostPort); err != nil {
			return nil, err
		}
		inst.Mode = "container"
		inst.Healthy = true
		inst.Image = st.Image
		if inst.Detail == "" {
			inst.Detail = "复用既有受管容器（未新建）"
		}
		applyMySQLAdmin(in, hostPort, rootPW)
		return inst, nil
	}

	// 2. 宿主端口上已有可用的 MySQL（宿主装的或别人起的容器）→ 接管复用。
	if probeMySQL(ctx, "127.0.0.1", hostPort) {
		if strings.TrimSpace(in.Database.AdminPassword) == "" {
			return nil, fmt.Errorf("检测到 127.0.0.1:%d 已有可用的 MySQL（可能是宿主安装或既有容器），"+
				"接管复用需要它的管理口令：请在向导填写 database.admin_password；"+
				"若要改由本工具一键拉起容器，请换一个空闲的 infra.mysql_host_port", hostPort)
		}
		if err := waitMySQLReady(ctx, "127.0.0.1", hostPort, in.Database.AdminUser, in.Database.AdminPassword); err != nil {
			return nil, fmt.Errorf("已有 MySQL（127.0.0.1:%d）用提供的管理口令连不上或不可用：%w", hostPort, err)
		}
		inst.Mode = "external"
		inst.Healthy = true
		inst.ContainerName = ""
		inst.Detail = "接管复用既有 MySQL 实例（未新建容器）"
		// 管理口事实沿用调用方提供的值（host/port/user/password 均不覆盖）。
		return inst, nil
	}

	// 3. 端口被占但不是可用 MySQL → 冲突，人话提示。
	if !portFree(hostPort) {
		return nil, fmt.Errorf("端口 %d 已被其它进程占用，且上面不是可用的 MySQL："+
			"请换一个空闲的 infra.mysql_host_port，或先停掉占用该端口的进程", hostPort)
	}

	// 4. 没有可用实例 → 按需一键拉起容器。
	if !in.Infra.InfraLaunch() {
		return nil, fmt.Errorf("未找到可用的 MySQL（127.0.0.1:%d 无响应），且 launch=false 不允许拉起容器", hostPort)
	}
	if err := dk.Info(ctx); err != nil {
		return nil, fmt.Errorf("无法拉起 MySQL 容器：docker 不可达或无权限：%w", err)
	}
	rootPW, created, err := store.EnsureInfraSecret(KeyMySQLRootPassword)
	if err != nil {
		return nil, err
	}
	// compose 对整个文件插值：即使只起 MySQL，REDIS_PASSWORD 也必须在场，
	// 否则 :? 插值立即失败。两对基础设施凭据总是一起取或生成。
	redisPW, _, err := store.EnsureInfraSecret(KeyRedisPassword)
	if err != nil {
		return nil, err
	}
	if _, err := launchInfra(ctx, dk, composeFile, hostPort, redisHostPort(store, in), rootPW, redisPW); err != nil {
		return nil, fmt.Errorf("拉起 MySQL 容器失败：%w", err)
	}
	if err := waitMySQLReady(ctx, "127.0.0.1", hostPort, "root", rootPW); err != nil {
		return nil, fmt.Errorf("MySQL 容器启动后未在时限内就绪（首次初始化需建库，属正常耗时；仍失败请查容器日志）：%w", err)
	}
	if err := store.SetInfraPort("mysql", hostPort); err != nil {
		return nil, err
	}
	inst.Mode = "container"
	inst.Healthy = true
	inst.Detail = "一键拉起受管容器"
	if created {
		inst.Detail += "（root 口令为新生成值，仅以指纹记录）"
	}
	applyMySQLAdmin(in, hostPort, rootPW)
	return inst, nil
}

// redisHostPort 返回 Redis 的宿主发布端口（状态已记录时以记录为准，
// 与 MySQL 复用同端口纪律一致）。
func redisHostPort(store *Store, in *Inputs) int {
	if saved := store.GetInfraPort("redis"); saved != 0 {
		return saved
	}
	return in.Infra.RedisHostPort
}

// mysqlHostPort 同 redisHostPort，MySQL 侧。
func mysqlHostPort(store *Store, in *Inputs) int {
	if saved := store.GetInfraPort("mysql"); saved != 0 {
		return saved
	}
	return in.Infra.MySQLHostPort
}

// ensureRedis 处理 Redis：接管已有 / 一键拉起，并把连接事实写回 in。
func ensureRedis(ctx context.Context, in *Inputs, store *Store, dk InfraDocker, composeFile string) (*InfraInstance, error) {
	hostPort := in.Infra.RedisHostPort
	inst := &InfraInstance{
		Kind:             "redis",
		ComposeService:   "redis",
		ContainerName:    RedisContainerName,
		HostPort:         hostPort,
		ContainerPort:    redisContainerPort,
		BaremetalAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)),
		ComposeAddress:   RedisComposeAddress,
		DataVolume:       "ha-infra-redis-data",
	}

	// 1. 受管容器已存在 → 复用（端口纪律同 MySQL）。
	if st, err := dk.Inspect(ctx, RedisContainerName); err == nil && st != nil {
		if saved := store.GetInfraPort("redis"); saved != 0 && saved != hostPort {
			inst.Detail = fmt.Sprintf("受管容器已发布在宿主端口 %d，本次请求的 %d 被忽略（换端口需先移除现有容器，本层不删容器）", saved, hostPort)
			hostPort = saved
			inst.HostPort = saved
			inst.BaremetalAddress = net.JoinHostPort("127.0.0.1", strconv.Itoa(saved))
		}
		pw, _, err := store.EnsureInfraSecret(KeyRedisPassword)
		if err != nil {
			return nil, err
		}
		rootPW, _, err := store.EnsureInfraSecret(KeyMySQLRootPassword)
		if err != nil {
			return nil, err
		}
		if _, err := launchInfra(ctx, dk, composeFile, mysqlHostPort(store, in), hostPort, rootPW, pw); err != nil {
			return nil, fmt.Errorf("复用受管 Redis 容器失败：%w", err)
		}
		if err := waitRedisReady(ctx, "127.0.0.1", hostPort, pw); err != nil {
			return nil, fmt.Errorf("受管 Redis 容器未在时限内就绪：%w", err)
		}
		if err := store.SetInfraPort("redis", hostPort); err != nil {
			return nil, err
		}
		inst.Mode = "container"
		inst.Healthy = true
		inst.Image = st.Image
		if inst.Detail == "" {
			inst.Detail = "复用既有受管容器（未新建）"
		}
		applyRedis(in, hostPort, pw)
		return inst, nil
	}

	// 2. 宿主端口已有可用 Redis → 接管复用。
	if probeRedis(ctx, "127.0.0.1", hostPort) {
		pw := in.Redis.Password
		if err := waitRedisReady(ctx, "127.0.0.1", hostPort, pw); err != nil {
			return nil, fmt.Errorf("已有 Redis（127.0.0.1:%d）用提供的口令连不上或不可用（若其无口令请留空 redis.password）：%w", hostPort, err)
		}
		inst.Mode = "external"
		inst.Healthy = true
		inst.ContainerName = ""
		inst.Detail = "接管复用既有 Redis 实例（未新建容器）"
		return inst, nil
	}

	// 3. 端口冲突。
	if !portFree(hostPort) {
		return nil, fmt.Errorf("端口 %d 已被其它进程占用，且上面不是可用的 Redis："+
			"请换一个空闲的 infra.redis_host_port，或先停掉占用该端口的进程", hostPort)
	}

	// 4. 一键拉起。
	if !in.Infra.InfraLaunch() {
		return nil, fmt.Errorf("未找到可用的 Redis（127.0.0.1:%d 无响应），且 launch=false 不允许拉起容器", hostPort)
	}
	if err := dk.Info(ctx); err != nil {
		return nil, fmt.Errorf("无法拉起 Redis 容器：docker 不可达或无权限：%w", err)
	}
	pw, created, err := store.EnsureInfraSecret(KeyRedisPassword)
	if err != nil {
		return nil, err
	}
	// 同 MySQL：compose 全文件插值，两对凭据必须同时在场。
	rootPW, _, err := store.EnsureInfraSecret(KeyMySQLRootPassword)
	if err != nil {
		return nil, err
	}
	if _, err := launchInfra(ctx, dk, composeFile, mysqlHostPort(store, in), hostPort, rootPW, pw); err != nil {
		return nil, fmt.Errorf("拉起 Redis 容器失败：%w", err)
	}
	if err := waitRedisReady(ctx, "127.0.0.1", hostPort, pw); err != nil {
		return nil, fmt.Errorf("Redis 容器启动后未在时限内就绪：%w", err)
	}
	if err := store.SetInfraPort("redis", hostPort); err != nil {
		return nil, err
	}
	inst.Mode = "container"
	inst.Healthy = true
	inst.Detail = "一键拉起受管容器"
	if created {
		inst.Detail += "（口令为新生成值，仅以指纹记录）"
	}
	applyRedis(in, hostPort, pw)
	return inst, nil
}

// launchInfra 经 compose 幂等拉起基础设施（up -d 对已在跑的容器零动作）。
// 凭据经进程环境注入，不落任何文件。失败时把 compose 输出尾部带回
//（响亮失败：不允许只剩一句 "exit status 1"）。
func launchInfra(ctx context.Context, dk InfraDocker, composeFile string, mysqlPort, redisPort int, mysqlRootPW, redisPW string) (string, error) {
	env := []string{
		"MYSQL_HOST_PORT=" + strconv.Itoa(mysqlPort),
		"REDIS_HOST_PORT=" + strconv.Itoa(redisPort),
	}
	if mysqlRootPW != "" {
		env = append(env, "MYSQL_ROOT_PASSWORD="+mysqlRootPW)
	}
	if redisPW != "" {
		env = append(env, "REDIS_PASSWORD="+redisPW)
	}
	out, err := dk.ComposeUpEnv(ctx, []string{composeFile}, env)
	if err != nil {
		tail := strings.TrimSpace(out)
		if len(tail) > 1500 {
			tail = tail[len(tail)-1500:]
		}
		if tail != "" {
			return out, fmt.Errorf("%v\ncompose 输出：\n%s", err, tail)
		}
		return out, err
	}
	return out, nil
}

// applyMySQLAdmin 把"受管容器"的管理口事实写回 in（供建库建账号步骤）。
func applyMySQLAdmin(in *Inputs, hostPort int, rootPW string) {
	in.Database.AdminHost = "127.0.0.1"
	in.Database.AdminPort = hostPort
	in.Database.AdminUser = "root"
	in.Database.AdminPassword = rootPW
}

// applyRedis 把受管 Redis 的连接事实写回 in。
func applyRedis(in *Inputs, hostPort int, pw string) {
	in.Redis.Host = "127.0.0.1"
	in.Redis.Port = hostPort
	in.Redis.Password = pw
}

// --- 探测与等待 ---

// portFree 以绑定测试判定宿主端口是否空闲。
func portFree(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// probeMySQL 判定地址上是否有 MySQL 在监听（读握手包协议版本号）。
func probeMySQL(ctx context.Context, host string, port int) bool {
	conn, err := dialTimeout(ctx, host, port)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(probeTimeout))
	buf := make([]byte, 128)
	n, err := conn.Read(buf)
	if err != nil || n < 5 {
		return false
	}
	// MySQL 握手包：第 5 字节为协议版本（0x0a = 3.20+）。
	return buf[4] == 0x0a
}

// probeRedis 判定地址上是否有 Redis 在监听（发 PING，读任何 RESP 应答）。
func probeRedis(ctx context.Context, host string, port int) bool {
	conn, err := dialTimeout(ctx, host, port)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(probeTimeout))
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return false
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return false
	}
	// +PONG（无鉴权）或 -NOAUTH（有鉴权）都说明是 Redis。
	return strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")
}

func dialTimeout(ctx context.Context, host string, port int) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
}

// waitMySQLReady 轮询直到能以管理账号执行一条查询（真能用才返回）。
func waitMySQLReady(ctx context.Context, host string, port int, user, password string) error {
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=5s&readTimeout=5s", user, password,
		net.JoinHostPort(host, strconv.Itoa(port)))
	deadline := time.Now().Add(infraReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		db, err := sql.Open("mysql", dsn)
		if err == nil {
			var one int
			if qerr := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); qerr == nil {
				db.Close()
				return nil
			} else {
				lastErr = qerr
			}
			db.Close()
		} else {
			lastErr = err
		}
		time.Sleep(infraPollInterval)
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("等待 MySQL 就绪超时")
}

// waitRedisReady 轮询直到 AUTH+PING 通过（无口令则直接 PING）。
func waitRedisReady(ctx context.Context, host string, port int, password string) error {
	deadline := time.Now().Add(infraReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := redisAuthPing(ctx, host, port, password); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(infraPollInterval)
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("等待 Redis 就绪超时")
}

func redisAuthPing(ctx context.Context, host string, port int, password string) error {
	conn, err := dialTimeout(ctx, host, port)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(probeTimeout))
	r := bufio.NewReader(conn)

	if password != "" {
		auth := fmt.Sprintf("*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(password), password)
		if _, err := conn.Write([]byte(auth)); err != nil {
			return err
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		if !strings.HasPrefix(line, "+OK") {
			return fmt.Errorf("AUTH 被拒：%s", strings.TrimSpace(line))
		}
	}
	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return err
	}
	if !strings.HasPrefix(line, "+PONG") {
		return fmt.Errorf("PING 未回 PONG：%s", strings.TrimSpace(line))
	}
	return nil
}
