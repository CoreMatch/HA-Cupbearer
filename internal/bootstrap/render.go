package bootstrap

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 配置供给的单一事实源：全部变量由 Inputs + 生成的机密值推导，
// 容器路径经 .env 注入（模板仍由容器 entrypoint 渲染——既有设计不变），
// 裸金属路径由本层直接渲染 deploy/config/*.tmpl（此前 Go 代码零引用，
// 自本层起真正被消费）。同一份字典喂两侧，"同一对凭据同时落到 HRPAuth
// service_clients 与消费方配置"由构造保证，不存在只写一半。

// TemplateServices 是模板文件 → 服务名映射（渲染输出按服务名索引）。
var TemplateServices = map[string]string{
	"hrpauth.yaml.tmpl":                      "hrpauth",
	"hrpauth-yggdrasil-api.yaml.tmpl":        "hrpauth-yggdrasil-api",
	"haskinlib.yaml.tmpl":                    "haskinlib",
	"haskinproxy.yaml.tmpl":                  "haskinproxy",
	"winnerproxy.yaml.tmpl":                  "winnerproxy",
	"mca-server.yaml.tmpl":                   "mca-server",
	"sdkhandler.yaml.tmpl":                   "ha-webui-sdkhandler",
	"ha-services-maintenance-view.yaml.tmpl": "ha-services-maintenance-view",
	"ha-cupbearer.yaml.tmpl":                 "ha-cupbearer",
}

var envVarRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// EnvForContainer 生成容器路径 .env 的全量变量（与 .env.example 的
// 变量集逐项对齐；内部发现地址不在 .env——compose 在各服务 environment
// 里直接注入，既有接线不变）。facts 为 nil 时按输入原值推导。
//
// DB/Redis 连接形态按所选部署路径各自成立：基础设施是受管容器时，容器
// 编排一侧用服务名+容器口（经 ha-infra 网络解析），裸金属一侧用
// 127.0.0.1+宿主发布端口；外部实例时沿用调用方给的地址。
func EnvForContainer(in *Inputs, secrets map[string]string, facts *InfraFacts) map[string]string {
	dbHost, dbPort := dbEndpoint(in, facts, true)
	redisHost, redisPort := redisEndpoint(in, facts, true)
	env := map[string]string{
		"VERSION": "latest",
		"TZ":      "Asia/Shanghai",

		"DB_HOST":     dbHost,
		"DB_PORT":     dbPort,
		"DB_USER":     in.Database.AppUser,
		"DB_PASSWORD": secrets[KeyAppDBPassword],
		"DB_NAME":     in.Database.Names.HRPAuth,

		"HRPAUTH_DB_NAME":   in.Database.Names.HRPAuth,
		"YGG_DB_NAME":       in.Database.Names.YGG,
		"HASKINLIB_DB_NAME": in.Database.Names.HASkinLib,
		"MCA_DB_NAME":       in.Database.Names.MCA,

		"REDIS_HOST":     redisHost,
		"REDIS_PORT":     redisPort,
		"REDIS_PASSWORD": in.Redis.Password,
		"REDIS_DB":       strconv.Itoa(in.Redis.DB),

		// 基础设施（受管容器）的凭据与端口：一键拉起时由本层生成并写进
		// 它该在的地方（.env），运维不必再抄一遍；外部实例时口令按输入。
		"MYSQL_ROOT_PASSWORD": secrets[KeyMySQLRootPassword],
		"MYSQL_HOST_PORT":     strconv.Itoa(in.Infra.MySQLHostPort),
		"REDIS_HOST_PORT":     strconv.Itoa(in.Infra.RedisHostPort),

		"HRPAUTH_PUBLIC_URL":         in.PublicURLs.HRPAuth,
		"HRPAUTH_FRONTEND_URL":       in.PublicURLs.Frontend,
		"YGG_PUBLIC_URL":             in.PublicURLs.YGG,
		"HASKINPROXY_PUBLIC_URL":     in.PublicURLs.HASkinProxy,
		"WINNERPROXY_PUBLIC_URL":     in.PublicURLs.WinnerProxy,
		"MAINTENANCEVIEW_PUBLIC_URL": in.PublicURLs.MaintenanceView,

		"MANAGE_TOKEN":                secrets[KeyManageToken],
		"HRPAUTH_PUBLIC_CLIENT_ID":    in.ClientIDs.Public,
		"HRPAUTH_SUPER_CLIENT_ID":     in.ClientIDs.Super,
		"HRPAUTH_SUPER_CLIENT_SECRET": secrets[KeySuperClientSecret],

		"WINNERPROXY_CLIENT_ID":     in.ClientIDs.WinnerProxy,
		"WINNERPROXY_CLIENT_SECRET": secrets[KeyWinnerProxySecret],
		"YGG_CLIENT_ID":             in.ClientIDs.YGG,
		"YGG_CLIENT_SECRET":         secrets[KeyYGGSecret],
		"HASKINPROXY_CLIENT_ID":     in.ClientIDs.HASkinProxy,
		"HASKINPROXY_CLIENT_SECRET": secrets[KeySkinProxySecret],
		"SDKHANDLER_CLIENT_ID":      in.ClientIDs.SDKHandler,
		"SDKHANDLER_CLIENT_SECRET":  secrets[KeySDKHandlerSecret],
		"HASKINLIB_CLIENT_ID":       in.ClientIDs.HASkinLib,
		"HASKINLIB_CLIENT_SECRET":   secrets[KeySkinLibSecret],

		"SDKHANDLER_INTERNAL_KEY": secrets[KeySDKHandlerInternalKey],
		"MCA_MAINTENANCE_TOKEN":   secrets[KeyMCAMaintenanceToken],
		"MCA_JOIN_TARGET_HOST":    "",
		"MCA_JOIN_TARGET_PORT":    "0",

		"SDKHANDLER_BIND":      "127.0.0.1",
		"CUPBEARER_BIND":       "127.0.0.1",
		"MAINTENANCEVIEW_BIND": "127.0.0.1",
		// 服务默认端口。注意：MV 向导（HA-Services-MaintenanceView
		// frontend/src/wizard/BootstrapStep.tsx 的 PUBLIC_URL_FIELDS）按"回环 +
		// 这些端口"预填六个对外地址的本机默认值；改动这里的端口须同步那里与其
		// knowledge/0008 的记载。
		"HRPAUTH_PORT":                "2778",
		"YGG_PORT":                    "2770",
		"HASKINLIB_PORT":              "2701",
		"HASKINPROXY_PORT":            "2702",
		"WINNERPROXY_PORT":            "2777",
		"MCA_SERVER_PORT":             "9178",
		"SDKHANDLER_PORT":             "2704",
		"SDKHANDLER_MAINTENANCE_PORT": "2705",
		"CUPBEARER_PORT":              "18080",
		"MAINTENANCEVIEW_PORT":        "18081",
		"WEBUI_PORT":                  "8080",
		"WEBUI_BIND":                  "0.0.0.0",

		"SDKHANDLER_TEMPLATE_DIR":     "",
		"SDKHANDLER_TEMPLATE_GIT_URL": "https://github.com/CoreMatch/HRPAuth-WebUI.git",

		"SMTP_HOST":       in.SMTP.Host,
		"SMTP_PORT":       strconv.Itoa(in.SMTP.Port),
		"SMTP_USERNAME":   in.SMTP.Username,
		"SMTP_PASSWORD":   in.SMTP.Password,
		"SMTP_FROM_EMAIL": in.SMTP.FromEmail,

		"SIGNATURE_KEY_DIR": "/app/secrets",
	}
	return env
}

// EnvForBaremetal 生成某个服务裸金属渲染用的变量字典。
// 内部发现地址默认 127.0.0.1+约定端口（可经 internal_urls 覆盖）。
// DB/Redis 用宿主地址+发布端口形态（裸金属进程在宿主机上直连）。
func EnvForBaremetal(in *Inputs, secrets map[string]string, service, secretsDirAbs string, facts *InfraFacts) map[string]string {
	internalHRPAuth := in.InternalURLs.HRPAuth
	if internalHRPAuth == "" {
		internalHRPAuth = "http://127.0.0.1:2778"
	}
	internalYGG := in.InternalURLs.YGG
	if internalYGG == "" {
		internalYGG = "http://127.0.0.1:2770"
	}
	internalCupbearer := in.InternalURLs.Cupbearer
	if internalCupbearer == "" {
		internalCupbearer = "http://127.0.0.1:18080"
	}

	dbName := in.Database.Names.HRPAuth
	switch service {
	case "hrpauth-yggdrasil-api":
		dbName = in.Database.Names.YGG
	case "haskinlib":
		dbName = in.Database.Names.HASkinLib
	case "mca-server":
		dbName = in.Database.Names.MCA
	}

	env := EnvForContainer(in, secrets, facts) // 复用全集，再按裸金属语境覆盖
	dbHost, dbPort := dbEndpoint(in, facts, false)
	redisHost, redisPort := redisEndpoint(in, facts, false)
	env["DB_HOST"] = dbHost
	env["DB_PORT"] = dbPort
	env["REDIS_HOST"] = redisHost
	env["REDIS_PORT"] = redisPort
	env["DB_NAME"] = dbName
	env["HRPAUTH_URL"] = internalHRPAuth
	env["HRPAUTH_YGG_URL"] = internalYGG
	env["YGG_INTERNAL_URL"] = internalYGG
	env["CUPBEARER_URL"] = internalCupbearer
	env["SIGNATURE_KEY_DIR"] = secretsDirAbs
	// 裸金属没有 compose 网络名；其余变量（机密/公开 URL/库名）同源同值。
	return env
}

// dbEndpoint 按目标链路推导 MySQL 连接地址（裸金属 = 宿主地址+发布端口，
// 容器 = 服务名+容器口）。
// container=true 返回容器网络形态（受管容器 = 服务名:容器口）；
// false 返回宿主形态（受管容器 = 127.0.0.1:发布口；外部实例 = 输入地址）。
func dbEndpoint(in *Inputs, facts *InfraFacts, container bool) (string, string) {
	if facts != nil && facts.MySQL.Mode == "container" {
		if container {
			return facts.MySQL.ComposeService, strconv.Itoa(facts.MySQL.ContainerPort)
		}
		return "127.0.0.1", strconv.Itoa(facts.MySQL.HostPort)
	}
	return in.Database.AdminHost, strconv.Itoa(in.Database.AdminPort)
}

// redisEndpoint 同 dbEndpoint，推导 Redis 连接地址。
func redisEndpoint(in *Inputs, facts *InfraFacts, container bool) (string, string) {
	if facts != nil && facts.Redis.Mode == "container" {
		if container {
			return facts.Redis.ComposeService, strconv.Itoa(facts.Redis.ContainerPort)
		}
		return "127.0.0.1", strconv.Itoa(facts.Redis.HostPort)
	}
	return in.Redis.Host, strconv.Itoa(in.Redis.Port)
}

// MergeEnvFile 把期望变量合并进 .env：
//   - 已存在的键一律保留原值（绝不覆盖）；与期望值不一致记为漂移告警；
//   - 缺失的键按稳定顺序追加在文件尾部（带来源注释）。
//
// 返回 (新增键, 保留键, 漂移键, 覆写键, 错误)。文件不存在时整体创建。
//
// overwrite 是"已有值不覆盖"铁律的唯一例外通道：仅限本次运行被显式验证
// 过的凭据键（databases 步 takeover/reuse/验证通过的处置）。这类键的既有
// 值若与验证值不同，可证明是陈旧值（服务用它连不上真实账号），就地更新
// 并如实报告；绝不做静默覆写。
func MergeEnvFile(path string, desired map[string]string, overwrite map[string]bool) (added, kept, drifted, overwritten []string, err error) {
	existing := map[string]string{}
	var rawLines []string

	if data, rerr := os.ReadFile(path); rerr == nil {
		rawLines = strings.Split(string(data), "\n")
		for _, line := range rawLines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			key, val, ok := parseEnvLine(trimmed)
			if ok {
				existing[key] = val
			}
		}
	} else if !os.IsNotExist(rerr) {
		return nil, nil, nil, nil, fmt.Errorf("read %s: %w", path, rerr)
	}

	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if cur, ok := existing[k]; ok {
			if overwrite[k] && cur != desired[k] {
				overwritten = append(overwritten, k)
				for i, line := range rawLines {
					lk, _, lok := parseEnvLine(strings.TrimSpace(line))
					if lok && lk == k {
						rawLines[i] = fmt.Sprintf("%s=%s", k, desired[k])
					}
				}
				continue
			}
			kept = append(kept, k)
			if cur != desired[k] {
				drifted = append(drifted, k)
			}
			continue
		}
		added = append(added, k)
		rawLines = append(rawLines, fmt.Sprintf("%s=%s", k, desired[k]))
	}

	if len(added) == 0 && len(overwritten) == 0 {
		return added, kept, drifted, overwritten, nil
	}
	content := strings.TrimRight(strings.Join(rawLines, "\n"), "\n")
	if len(added) > 0 {
		header := "\n# ---- 以下变量由 HA-Cupbearer 首次运行初始化补齐（幂等；已有值不覆盖） ----"
		content = content + header + "\n"
		for _, k := range added {
			content += fmt.Sprintf("%s=%s\n", k, desired[k])
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, nil, nil, nil, err
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("write %s: %w", path, err)
	}
	return added, kept, drifted, overwritten, nil
}

func parseEnvLine(line string) (key, value string, ok bool) {
	idx := strings.Index(line, "=")
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	value = strings.TrimSpace(line[idx+1:])
	// 去掉成对引号（.env 惯例）。
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
	}
	return key, value, true
}

// RenderTemplate 用变量字典渲染模板（envsubst 语义，只替换 ${VAR}）。
// 模板引用了字典里没有的变量 = 硬失败并点名（绝不渲染出空值造成静默坏配置）。
func RenderTemplate(templatePath string, vars map[string]string) ([]byte, error) {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("read template %s: %w", templatePath, err)
	}
	var missing []string
	out := envVarRefRe.ReplaceAllFunc(data, func(ref []byte) []byte {
		name := envVarRefRe.FindSubmatch(ref)[1]
		val, ok := vars[string(name)]
		if !ok {
			missing = append(missing, string(name))
			return ref
		}
		return []byte(val)
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("模板 %s 引用的变量缺失：%s（值必须由统一输入推导，不允许渲染空值）",
			filepath.Base(templatePath), strings.Join(uniqueStrings(missing), ", "))
	}
	return out, nil
}

// WriteIfAbsent 原子写入目标文件；目标已存在则绝不覆盖（返回 false）。
func WriteIfAbsent(path string, content []byte) (written bool, err error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	return true, nil
}

// ListTemplates 返回模板目录里登记的服务模板（按服务名排序）。
func ListTemplates(templateDir string) ([]string, error) {
	var out []string
	for file := range TemplateServices {
		if _, err := os.Stat(filepath.Join(templateDir, file)); err == nil {
			out = append(out, TemplateServices[file])
		}
	}
	sort.Strings(out)
	return out, nil
}

// ConventionalConfigFile 是服务的配置文件名约定（清单 facts.config 缺席时
// 的回落值）：WinnerProxy 用 .yml 后缀（其 main.go configFileName 事实），
// 其余服务用 config.yaml。
func ConventionalConfigFile(service string) string {
	if service == "winnerproxy" {
		return "config.yml"
	}
	return "config.yaml"
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ReadEnvFileValues 只读解析 .env 现有键值（计划/状态报告用）。
func ReadEnvFileValues(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		trimmed := strings.TrimSpace(scanner.Text())
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if key, val, ok := parseEnvLine(trimmed); ok {
			out[key] = val
		}
	}
	return out
}
