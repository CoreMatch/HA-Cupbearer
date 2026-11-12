package bootstrap

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"HA-Cupbearer/internal/docker"
)

// 凭据预填（契约 ha-cupbearer.yaml v1.7.0，POST
// /api/v1/deploy/bootstrap/credential-hints）：接管/沿用既有实例时，为向导
// 找回"本工具生成且可追溯"的凭据。铁律：
//   - 只对三类可追溯来源收集候选（受管容器 env → 本实例 state/.env →
//     本机历史实例产物），顺序即优先级；
//   - 每个候选先对目标账号/实例验证，验证通过才回显明文；验证不过的
//     一律丢弃（只计数，绝不回显）；
//   - 外部口令（非本工具生成、不可追溯）一律不预填、不回显；
//   - 每次明文回显记审计日志（谁、哪个来源、何时取用）——审计记录只含
//     指纹，绝不含明文。
//
// 本层纯只读：不写状态、不改容器/数据库；唯一写面是审计日志文件。

// 来源类别枚举（契约 CredentialHint.source）。
const (
	SourceManagedContainerEnv = "managed_container_env" // 受管容器 env（基础设施容器与平台服务容器）
	SourceBootstrapState      = "bootstrap_state"       // 本实例状态文件
	SourceEnvFile             = "env_file"              // 本实例写出的 .env（仅本工具补齐的键）
	SourceHistorical          = "historical"            // 本机历史实例产物（受限扫描根）
	SourceNone                = "none"                  // 无可追溯来源
)

// 预填目标标识（契约 CredentialHint.target）。
const (
	TargetMySQLAdmin = "mysql_admin"
	TargetAppUser    = "app_user"
	TargetRedis      = "redis"
)

// ScanRootsEnvVar 显式声明历史扫描根（逗号分隔的目录列表）。除此之外，
// 历史发现只来自在跑 compose 容器的工作目录标签——受限集合，绝不遍历
// 文件系统。
const ScanRootsEnvVar = "HACB_BOOTSTRAP_SCAN_ROOTS"

const (
	// envFileMarker 是 MergeEnvFile 补齐缺失变量时写的来源注释。标记之后
	// 的键才是本工具写的；标记之前的键按外部既有值对待（绝不预填回显）。
	envFileMarker = "由 HA-Cupbearer 首次运行初始化补齐"

	maxScanDirs       = 32 // 历史扫描目录上限（受限集合，防意外扩散）
	maxCandidates     = 12 // 单目标候选上限（验证尝试的次数上限）
	maxHistoricalFile = 1 << 20
)

// CredentialHint 是单个目标的预填结论（契约 CredentialHint）。
type CredentialHint struct {
	Target            string `json:"target"` // mysql_admin | app_user | redis
	Account           string `json:"account"`
	Address           string `json:"address"`
	Found             bool   `json:"found"`
	Source            string `json:"source"` // 来源枚举；未找到为 none
	Value             string `json:"value,omitempty"`
	Provenance        string `json:"provenance,omitempty"`
	CandidatesChecked int    `json:"candidates_checked"`
	Detail            string `json:"detail"`
}

// CredentialHintsReport 是一次预填探测的完整结论（契约 CredentialHintsReport）。
type CredentialHintsReport struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Hints       []CredentialHint `json:"hints"`
}

// hintCandidate 是一个可追溯候选（未验证）。
type hintCandidate struct {
	source     string
	value      string
	provenance string
}

// HintDocker 是凭据预填需要的容器只读操作面（docker.CLI 实现；
// nil = 容器 env 这类来源不可用，只查文件类来源）。
type HintDocker interface {
	ContainerEnv(ctx context.Context, name string) (map[string]string, error)
	ListComposeContainers(ctx context.Context) ([]docker.ComposeContainerRef, error)
}

// HintCollector 收集并验证预填候选。验证函数可注入（测试用）；
// 生产由 Engine 装配真实连接验证。
type HintCollector struct {
	store   *Store
	dk      HintDocker
	envFile string // 本实例的 .env（②类来源），默认 deploy/.env

	// 验证覆写（nil = 真实连接验证）。测试注入，生产留空。
	verifyAdminOverride func(ctx context.Context, password string) bool
	verifyAppOverride   func(ctx context.Context, password string) bool
	verifyRedisOverride func(ctx context.Context, password string) bool
}

// NewHintCollector 构造收集器。envFile 为空取默认 deploy/.env。
func NewHintCollector(store *Store, dk HintDocker, envFile string) *HintCollector {
	if envFile == "" {
		envFile = DefaultEnvFile
	}
	return &HintCollector{store: store, dk: dk, envFile: envFile}
}

// Collect 计算三个目标的预填提示。mysqlMode/redisMode 是基础设施现状
// （container/external/absent），只用于无预填值时的情境化说明。
// remoteAddr 是调用方地址（审计用）。
func (c *HintCollector) Collect(ctx context.Context, in *Inputs, mysqlMode, redisMode, remoteAddr string) *CredentialHintsReport {
	rep := &CredentialHintsReport{GeneratedAt: time.Now(), Hints: make([]CredentialHint, 0, 3)}
	rep.Hints = append(rep.Hints, c.hintMySQLAdmin(ctx, in, mysqlMode, remoteAddr))
	rep.Hints = append(rep.Hints, c.hintAppUser(ctx, in, mysqlMode, remoteAddr))
	rep.Hints = append(rep.Hints, c.hintRedis(ctx, in, redisMode, remoteAddr))
	return rep
}

// --- 三个目标 ---

func (c *HintCollector) hintMySQLAdmin(ctx context.Context, in *Inputs, mode, remoteAddr string) CredentialHint {
	addr := net.JoinHostPort(in.Database.AdminHost, strconv.Itoa(in.Database.AdminPort))
	verify := c.verifyAdminOverride
	if verify == nil {
		verify = func(ctx context.Context, password string) bool {
			return mysqlLoginVerify(ctx, in.Database.AdminHost, in.Database.AdminPort, in.Database.AdminUser, password)
		}
	}
	candidates := c.containerInfraCandidates(ctx, MySQLContainerName, "MYSQL_ROOT_PASSWORD")
	candidates = append(candidates, c.stateCandidates(KeyMySQLRootPassword)...)
	candidates = append(candidates, c.envFileCandidates("MYSQL_ROOT_PASSWORD", "")...)
	candidates = append(candidates, c.historicalCandidates(ctx, "MYSQL_ROOT_PASSWORD", "")...)
	hint := c.resolve(ctx, TargetMySQLAdmin, in.Database.AdminUser, addr, candidates, verify, remoteAddr)
	hint.Detail = detailFor(hint, mode, "MySQL 管理口")
	return hint
}

func (c *HintCollector) hintAppUser(ctx context.Context, in *Inputs, mysqlMode, remoteAddr string) CredentialHint {
	addr := net.JoinHostPort(in.Database.AdminHost, strconv.Itoa(in.Database.AdminPort))
	verify := c.verifyAppOverride
	if verify == nil {
		verify = func(ctx context.Context, password string) bool {
			ok, err := CheckAppPassword(ctx, in, password)
			return err == nil && ok
		}
	}
	candidates := c.containerAppCandidates(ctx, in.Database.AppUser)
	candidates = append(candidates, c.stateCandidates(KeyAppDBPassword)...)
	candidates = append(candidates, c.envFileCandidates("DB_PASSWORD", in.Database.AppUser)...)
	candidates = append(candidates, c.historicalCandidates(ctx, "DB_PASSWORD", in.Database.AppUser)...)
	hint := c.resolve(ctx, TargetAppUser, in.Database.AppUser, addr, candidates, verify, remoteAddr)
	hint.Detail = detailFor(hint, mysqlMode, "应用账号")
	return hint
}

func (c *HintCollector) hintRedis(ctx context.Context, in *Inputs, mode, remoteAddr string) CredentialHint {
	addr := net.JoinHostPort(in.Redis.Host, strconv.Itoa(in.Redis.Port))
	verify := c.verifyRedisOverride
	if verify == nil {
		verify = func(ctx context.Context, password string) bool {
			return redisAuthPing(ctx, in.Redis.Host, in.Redis.Port, password) == nil
		}
	}
	// 目标 Redis 本就无口令（无鉴权 PING 通过）：无需预填，如实说明。
	if verify(ctx, "") {
		return CredentialHint{
			Target: TargetRedis, Address: addr, Source: SourceNone,
			Detail: "该 Redis 不要求口令（无鉴权 PING 通过）：口令留空即可。",
		}
	}
	candidates := c.containerInfraCandidates(ctx, RedisContainerName, "REDIS_PASSWORD")
	candidates = append(candidates, c.stateCandidates(KeyRedisPassword)...)
	candidates = append(candidates, c.envFileCandidates("REDIS_PASSWORD", "")...)
	candidates = append(candidates, c.historicalCandidates(ctx, "REDIS_PASSWORD", "")...)
	hint := c.resolve(ctx, TargetRedis, "", addr, candidates, verify, remoteAddr)
	hint.Detail = detailFor(hint, mode, "Redis")
	return hint
}

// resolve 按序验证候选，回显第一个验证通过者（并记审计）。
func (c *HintCollector) resolve(ctx context.Context, target, account, address string, candidates []hintCandidate, verify func(ctx context.Context, password string) bool, remoteAddr string) CredentialHint {
	hint := CredentialHint{Target: target, Account: account, Address: address, Source: SourceNone}
	checked := 0
	for _, cand := range candidates {
		if cand.value == "" {
			continue
		}
		if checked >= maxCandidates {
			break
		}
		checked++
		if verify(ctx, cand.value) {
			hint.Found = true
			hint.Source = cand.source
			hint.Value = cand.value
			hint.Provenance = cand.provenance
			c.audit(remoteAddr, target, account, cand)
			break
		}
	}
	hint.CandidatesChecked = checked
	return hint
}

// --- 候选收集：三类来源 ---

// containerInfraCandidates ①类：受管基础设施容器 env 中的凭据。
func (c *HintCollector) containerInfraCandidates(ctx context.Context, container, envKey string) []hintCandidate {
	if c.dk == nil {
		return nil
	}
	env, err := c.dk.ContainerEnv(ctx, container)
	if err != nil {
		return nil
	}
	if v := env[envKey]; v != "" {
		return []hintCandidate{{
			source:     SourceManagedContainerEnv,
			value:      v,
			provenance: fmt.Sprintf("受管容器 %s（env %s）", container, envKey),
		}}
	}
	return nil
}

// containerAppCandidates ①类：平台服务容器 env 中的应用账号凭据。
// 只认"容器自己声明的 DB_USER 与目标账号一致"的条目——容器是本生态
// compose 拉起的受管产物，其 env 由本工具的初始化供给，属可追溯来源。
func (c *HintCollector) containerAppCandidates(ctx context.Context, appUser string) []hintCandidate {
	if c.dk == nil {
		return nil
	}
	refs, err := c.dk.ListComposeContainers(ctx)
	if err != nil {
		return nil
	}
	var out []hintCandidate
	inspected := 0
	for _, ref := range refs {
		if ref.Name == MySQLContainerName || ref.Name == RedisContainerName {
			continue
		}
		if inspected >= maxCandidates {
			break
		}
		inspected++
		env, err := c.dk.ContainerEnv(ctx, ref.Name)
		if err != nil {
			continue
		}
		pw := env["DB_PASSWORD"]
		if pw == "" || env["DB_USER"] != appUser {
			continue
		}
		out = append(out, hintCandidate{
			source: SourceManagedContainerEnv,
			value:  pw,
			provenance: fmt.Sprintf("compose 容器 %s（项目 %s，env DB_PASSWORD；容器声明 DB_USER=%s）",
				ref.Name, ref.Project, appUser),
		})
	}
	return out
}

// stateCandidates ②类：本实例状态文件中的凭据。
func (c *HintCollector) stateCandidates(key string) []hintCandidate {
	if c.store == nil {
		return nil
	}
	st, err := c.store.LoadState()
	if err != nil {
		return nil
	}
	if v := st.Secrets[key]; v != "" {
		return []hintCandidate{{
			source:     SourceBootstrapState,
			value:      v,
			provenance: fmt.Sprintf("本实例状态文件 %s（键 %s）", c.store.statePath(), key),
		}}
	}
	return nil
}

// envFileCandidates ②类：本实例写出的 .env——只认本工具补齐的键
// （来源标记之后），标记之前的键按外部既有值对待。appUser 非空时要求
// 同文件 DB_USER 一致（或未声明）。
func (c *HintCollector) envFileCandidates(envKey, appUser string) []hintCandidate {
	vals := toolAppendedEnvValues(c.envFile)
	if vals == nil {
		return nil
	}
	v := vals[envKey]
	if v == "" {
		return nil
	}
	if appUser != "" {
		if u := vals["DB_USER"]; u != "" && u != appUser {
			return nil
		}
	}
	return []hintCandidate{{
		source:     SourceEnvFile,
		value:      v,
		provenance: fmt.Sprintf("本实例写出的 .env %s（%s）", c.envFile, envKey),
	}}
}

// historicalCandidates ③类：本机历史实例的产物。扫描根为受限集合：
// 显式声明的扫描根 + 在跑 compose 容器的工作目录（及其上级实例根）。
// 只认带本工具来源标记的 .env 与本工具形态的 state 副本；找到即报
// "来自哪个目录/哪次初始化"。
func (c *HintCollector) historicalCandidates(ctx context.Context, envKey, appUser string) []hintCandidate {
	var out []hintCandidate
	ownEnv, _ := filepath.Abs(c.envFile)
	ownState := ""
	if c.store != nil {
		ownState, _ = filepath.Abs(c.store.statePath())
	}
	for _, dir := range c.scanDirs(ctx) {
		// 历史 .env
		envPath := filepath.Join(dir, ".env")
		if abs, _ := filepath.Abs(envPath); abs != ownEnv {
			vals := toolAppendedEnvValues(envPath)
			if v := vals[envKey]; v != "" {
				userOK := true
				if appUser != "" {
					if u := vals["DB_USER"]; u != "" && u != appUser {
						userOK = false
					}
				}
				if userOK {
					out = append(out, hintCandidate{
						source:     SourceHistorical,
						value:      v,
						provenance: fmt.Sprintf("历史实例 .env %s（%s，带本工具来源标记）", envPath, envKey),
					})
				}
			}
		}
		// 历史 state 副本：实例根形态 <dir>/data/bootstrap/state.json，
		// 与散落的 state.json 副本。
		for _, sp := range []string{
			filepath.Join(dir, "data", "bootstrap", "state.json"),
			filepath.Join(dir, "state.json"),
		} {
			if abs, _ := filepath.Abs(sp); sp == "" || abs == ownState {
				continue
			}
			if v, when, ok := readHistoricalState(sp, envKey); ok {
				prov := fmt.Sprintf("历史实例状态 %s（键 %s", sp, envKey)
				if !when.IsZero() {
					prov += "；初始化于 " + when.Format("2006-01-02 15:04:05")
				}
				prov += "）"
				out = append(out, hintCandidate{source: SourceHistorical, value: v, provenance: prov})
			}
		}
	}
	return out
}

// scanDirs 返回历史扫描的受限目录集合（去重、只取存在者、有上限）。
func (c *HintCollector) scanDirs(ctx context.Context) []string {
	seen := map[string]bool{}
	var dirs []string
	add := func(d string) {
		d = strings.TrimSpace(d)
		if d == "" || len(dirs) >= maxScanDirs {
			return
		}
		abs, err := filepath.Abs(d)
		if err != nil || seen[abs] {
			return
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return
		}
		seen[abs] = true
		dirs = append(dirs, abs)
	}
	for _, root := range strings.Split(os.Getenv(ScanRootsEnvVar), ",") {
		add(root)
	}
	if c.dk != nil {
		if refs, err := c.dk.ListComposeContainers(ctx); err == nil {
			for _, ref := range refs {
				add(ref.WorkingDir)
				add(filepath.Dir(ref.WorkingDir))
			}
		}
	}
	return dirs
}

// --- 辅助 ---

// toolAppendedEnvValues 解析 .env 中"本工具补齐"的键值：来源标记
// （MergeEnvFile 的注释行）之后的键才算；标记之前的键是外部既有值，
// 绝不进入预填。文件无标记返回 nil（整个文件按外部对待）。
func toolAppendedEnvValues(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxHistoricalFile {
		return nil
	}
	out := map[string]string{}
	marked := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !marked {
			if strings.Contains(trimmed, envFileMarker) {
				marked = true
			}
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if k, v, ok := parseEnvLine(trimmed); ok && v != "" {
			out[k] = v
		}
	}
	if !marked {
		return nil
	}
	return out
}

// readHistoricalState 读取一个历史 state 副本并取指定键。只认本工具
// 形态的状态（version≥1、secrets 含至少一个已知键），避免误读任意 JSON。
func readHistoricalState(path, key string) (string, time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxHistoricalFile {
		return "", time.Time{}, false
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return "", time.Time{}, false
	}
	if st.Version < 1 || len(st.Secrets) == 0 {
		return "", time.Time{}, false
	}
	known := false
	for _, k := range append(append([]string{}, secretKeys...), KeyMySQLRootPassword, KeyRedisPassword) {
		if _, ok := st.Secrets[k]; ok {
			known = true
			break
		}
	}
	if !known {
		return "", time.Time{}, false
	}
	if v := st.Secrets[key]; v != "" {
		return v, st.CreatedAt, true
	}
	return "", time.Time{}, false
}

// mysqlLoginVerify 以给定口令尝试登录 MySQL（SELECT 1 通过 = 验证通过）。
func mysqlLoginVerify(ctx context.Context, host string, port int, user, password string) bool {
	dsn := fmt.Sprintf("%s:%s@tcp(%s)/?timeout=8s", user, password,
		net.JoinHostPort(host, strconv.Itoa(port)))
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	var one int
	return db.QueryRowContext(ctx, "SELECT 1").Scan(&one) == nil
}

// detailFor 生成情境化说明：找到时报来源与验证结论；没找到时按实例
// 形态给两种文案（受管但不可读 / 外部实例），不留纯空格子。
func detailFor(hint CredentialHint, mode, subject string) string {
	if hint.Found {
		return fmt.Sprintf("已找到可追溯凭据（%s），并对 %s 验证通过。", hint.Provenance, hint.Address)
	}
	switch mode {
	case "container":
		return fmt.Sprintf("本机检测到受管实例，但%s凭据不可读或全部验证不通过——请手动提供。", subject)
	case "external":
		return fmt.Sprintf("这是外部实例（非本工具拉起）：本工具不掌握可追溯凭据——请手动提供%s口令。", subject)
	case "absent":
		return "未检测到实例：一键拉起时会自动生成凭据，无需填写。"
	default:
		return fmt.Sprintf("未找到可追溯的%s凭据；如需沿用既有实例请手动提供。", subject)
	}
}

// audit 记录一次明文回显（谁、哪个来源、何时取用）。审计记录只含
// 指纹，绝不含明文；同时打一条服务日志（同样只含指纹）。
func (c *HintCollector) audit(remoteAddr, target, account string, cand hintCandidate) {
	fp := Fingerprint(cand.value)
	log.Printf("bootstrap credential-hint: plaintext echoed target=%s account=%s source=%s provenance=%s fingerprint=%s remote=%s",
		target, account, cand.source, cand.provenance, fp, remoteAddr)
	if c.store == nil {
		return
	}
	rec := struct {
		Time        string `json:"time"`
		RemoteAddr  string `json:"remote_addr"`
		Action      string `json:"action"`
		Target      string `json:"target"`
		Account     string `json:"account"`
		Source      string `json:"source"`
		Provenance  string `json:"provenance"`
		Fingerprint string `json:"fingerprint"`
	}{
		Time:        time.Now().Format(time.RFC3339),
		RemoteAddr:  remoteAddr,
		Action:      "credential-plaintext-echoed",
		Target:      target,
		Account:     account,
		Source:      cand.source,
		Provenance:  cand.provenance,
		Fingerprint: fp,
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(c.store.dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(c.store.dir, "credential-audit.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(string(line) + "\n")
}
