package orchestrator

// preflight 测试：覆盖各类失败的组合。
// 注意：本文件内的测试不并行运行（共享 diskSpaceCheck 注入点）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/manifest"
	"HA-Services-Deployer/internal/models"

	"github.com/goccy/go-yaml"
)

// ---------- Tinyvisor 测试桩 ----------

// tinyvisorStub 模拟 Tinyvisor 的关键鉴权与查询行为：
// /ping 免鉴权；/api/* 校验 X-Minivisor-Key，无效返回 401（与真实实现一致）。
type tinyvisorStub struct {
	key     string
	mu      sync.Mutex
	scripts map[string]string // name -> status
	server  *httptest.Server
}

func newTinyvisorStub(t *testing.T, key string) *tinyvisorStub {
	t.Helper()
	s := &tinyvisorStub{key: key, scripts: map[string]string{}}

	writeJSON := func(w http.ResponseWriter, code int, v interface{}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	authed := func(r *http.Request) bool { return r.Header.Get("X-Minivisor-Key") == s.key }

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"message": "pong"})
	})
	mux.HandleFunc("/api/scripts", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		s.mu.Lock()
		scripts := make([]map[string]interface{}, 0, len(s.scripts))
		for name, status := range s.scripts {
			scripts = append(scripts, map[string]interface{}{"id": name, "name": name, "status": status})
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]interface{}{"scripts": scripts})
	})
	mux.HandleFunc("/api/scripts/by-name/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/scripts/by-name/")
		// POST /api/scripts/by-name/<name>/restart —— 原子重启桩。
		if name, ok := strings.CutSuffix(rest, "/restart"); ok {
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
				return
			}
			s.setStatus(name, "running")
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"script": map[string]interface{}{"id": name, "name": name, "status": "running"},
			})
			return
		}
		name := rest
		switch r.Method {
		case http.MethodGet:
			s.mu.Lock()
			status, ok := s.scripts[name]
			s.mu.Unlock()
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"script": map[string]interface{}{"id": name, "name": name, "status": status},
			})
		case http.MethodPut:
			// 按名幂等登记桩：记录即视为成功。
			var payload struct {
				WorkDir string `json:"workDir"`
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			s.mu.Lock()
			_, created := s.scripts[name]
			if _, ok := s.scripts[name]; !ok {
				s.scripts[name] = "stopped"
			}
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"script":  map[string]interface{}{"id": name, "name": name, "status": "stopped", "workDir": payload.WorkDir, "content": payload.Content},
				"created": !created,
			})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		}
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *tinyvisorStub) setStatus(name, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts[name] = status
}

// ---------- 测试环境 ----------

// preflightEnv 是 preflight 测试环境：
// 一个 tinyvisor 管理的 svc-a（required 依赖 external 的 ext-dep）+ ext-dep。
type preflightEnv struct {
	orch     *Orchestrator
	cfg      *models.Config
	stub     *tinyvisorStub
	scanRoot string
	root     string // svc-a 安装根（初始不存在，验证"将被创建"路径）
	envFile  string
	svcPort  int
	depPort  int
	depLn    net.Listener // ext-dep 的 TCP 可达性桩
}

type envTweak func(cfg *models.Config, creds *config.Credentials, stub *tinyvisorStub)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func writeManifestFile(t *testing.T, path string, m *manifest.Manifest) {
	t.Helper()
	data, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newPreflightEnv(t *testing.T, tweaks ...envTweak) *preflightEnv {
	t.Helper()
	base := t.TempDir()
	scanRoot := filepath.Join(base, "scan")
	extDir := filepath.Join(base, "ext")

	env := &preflightEnv{
		scanRoot: scanRoot,
		root:     filepath.Join(base, "install", "svc-a"),
		envFile:  filepath.Join(base, "svc-a.env"),
		svcPort:  freePort(t),
		depPort:  freePort(t),
	}
	// 两个声明端口必须不同（聚合校验会拦跨项目端口冲突）。
	for env.depPort == env.svcPort {
		env.depPort = freePort(t)
	}
	if err := os.MkdirAll(filepath.Join(base, "install"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.envFile, []byte("FOO=bar\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(base, "svc-a-linux-amd64")
	artifactContent := []byte("binary-v1")
	if err := os.WriteFile(artifactPath, artifactContent, 0o755); err != nil {
		t.Fatal(err)
	}
	// local_path 来源强制 sha256 校验：旁路校验和文件必须同时在。
	sum := sha256.Sum256(artifactContent)
	if err := os.WriteFile(artifactPath+".sha256", []byte(hex.EncodeToString(sum[:])+"  svc-a-linux-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	depLn, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(env.depPort))
	if err != nil {
		t.Fatal(err)
	}
	env.depLn = depLn
	t.Cleanup(func() { depLn.Close() })

	svc := &manifest.Manifest{
		ManifestVersion: 1,
		Kind:            "service",
		Project:         "svc-a",
		DisplayName:     "Service A",
		Group:           "core",
		ServiceVersion:  "1.0.0",
		Facts: manifest.Facts{
			Ports: []manifest.Port{{Name: "http", Port: env.svcPort, Exposure: "loopback"}},
			Health: manifest.Health{HTTP: &manifest.HTTPProbe{
				PortName: "http", Path: "/healthz", ExpectStatus: 200,
				TimeoutSeconds: 2, IntervalSeconds: 1, Retries: 1,
			}},
			DependsOn: []manifest.Dependency{{Project: "ext-dep", Kind: "required"}},
		},
		Baremetal: manifest.Baremetal{
			Process: manifest.ProcessSpec{
				Manager:   "tinyvisor",
				Tinyvisor: manifest.TinyvisorSpec{EnvFile: env.envFile, AutoStart: true, RestartPolicy: "unless-stopped"},
			},
			Install:  manifest.InstallSpec{Root: env.root, Layout: "releases", BinaryName: "svc-a", KeepReleases: 3},
			Artifact: manifest.ArtifactSpec{Source: "local_path", LocalPath: artifactPath},
		},
	}
	ext := &manifest.Manifest{
		ManifestVersion: 1,
		Kind:            "external",
		Project:         "ext-dep",
		Group:           "infra",
		Facts: manifest.Facts{
			Ports:    []manifest.Port{{Name: "tcp", Port: env.depPort, Exposure: "loopback"}},
			External: &manifest.ExternalFacts{Host: "127.0.0.1"},
		},
	}
	writeManifestFile(t, filepath.Join(scanRoot, "svc-a", "deploy-manifest.yaml"), svc)
	writeManifestFile(t, filepath.Join(extDir, "ext-dep.yaml"), ext)

	env.stub = newTinyvisorStub(t, "test-key")
	cfg := &models.Config{}
	cfg.Orchestration.Enabled = true
	cfg.Orchestration.ScanRoots = []string{scanRoot}
	cfg.Orchestration.ExternalManifests = extDir
	cfg.Orchestration.StateDir = filepath.Join(base, "state")
	cfg.Orchestration.ProbeHost = "127.0.0.1"
	cfg.Orchestration.Tinyvisor.URL = env.stub.server.URL
	cfg.Storage.ArtifactsRoot = filepath.Join(base, "artifacts")
	creds := &config.Credentials{TinyvisorAPIKey: "test-key"}

	for _, tw := range tweaks {
		tw(cfg, creds, env.stub)
	}

	env.cfg = cfg
	env.orch = New(cfg, creds)

	// 聚合校验必须先通过，否则后续断言无意义。
	if _, rep := env.orch.Load(); !rep.OK() {
		t.Fatalf("aggregate validation failed: %v", rep.Errors)
	}
	return env
}

func runPreflight(t *testing.T, env *preflightEnv, mode string) *PreflightReport {
	t.Helper()
	report, err := env.orch.Preflight(context.Background(), nil, "", mode)
	if err != nil {
		t.Fatalf("preflight error: %v", err)
	}
	return report
}

func findResults(report *PreflightReport, name string) []CheckResult {
	var out []CheckResult
	for _, r := range report.Results {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

func assertNoFindings(t *testing.T, report *PreflightReport) {
	t.Helper()
	if report.HasBlockers() {
		t.Fatalf("unexpected blockers: %+v", report.Blockers)
	}
	if len(report.Warnings) > 0 {
		t.Fatalf("unexpected warnings: %+v", report.Warnings)
	}
}

// ---------- 测试 ----------

func TestPreflightAllGreen(t *testing.T) {
	env := newPreflightEnv(t)
	report := runPreflight(t, env, "deploy")
	assertNoFindings(t, report)

	// 一次查全：每个检查面都要有结果。
	for _, name := range []string{"tinyvisor", "install-root", "version-dir", "current-link", "workdir", "env-file", "port", "disk-space", "dependency", "reachability"} {
		if len(findResults(report, name)) == 0 {
			t.Errorf("check %q produced no result; results=%+v", name, report.Results)
		}
	}
	// 安装根初始不存在：结论必须是"将被创建"而不是报错。
	rootRes := findResults(report, "install-root")
	if !rootRes[0].OK || !strings.Contains(rootRes[0].Detail, "will be created") {
		t.Errorf("install-root should be creatable, got %+v", rootRes[0])
	}
}

func TestPreflightPortOccupiedByOther(t *testing.T) {
	env := newPreflightEnv(t)
	occ, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(env.svcPort))
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()

	report := runPreflight(t, env, "deploy")
	if !report.HasBlockers() {
		t.Fatal("expected blocker for occupied port")
	}
	ports := findResults(report, "port")
	if len(ports) != 1 || ports[0].OK || ports[0].Level != LevelBlocker {
		t.Fatalf("port check should be a single blocker, got %+v", ports)
	}
	if !strings.Contains(ports[0].Detail, strconv.Itoa(env.svcPort)) {
		t.Errorf("blocker detail must carry the port number: %s", ports[0].Detail)
	}
}

func TestPreflightPortHeldBySelfIsSkipped(t *testing.T) {
	env := newPreflightEnv(t)
	env.stub.setStatus("svc-a", "running")
	occ, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(env.svcPort))
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()

	report := runPreflight(t, env, "deploy")
	assertNoFindings(t, report)
	ports := findResults(report, "port")
	if len(ports) != 1 || !ports[0].OK || !strings.Contains(ports[0].Detail, "itself") {
		t.Fatalf("port held by the project itself must be OK, got %+v", ports)
	}
}

func TestPreflightPortRemapByOverride(t *testing.T) {
	remap := freePort(t)
	env := newPreflightEnv(t, func(cfg *models.Config, creds *config.Credentials, stub *tinyvisorStub) {
		cfg.Orchestration.Overrides = map[string]models.ProjectOverride{
			"svc-a": {Ports: map[string]int{"http": remap}},
		}
	})
	// 声明端口被占：隔离环境里重映射后的端口才是检查对象。
	occ, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(env.svcPort))
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()

	report := runPreflight(t, env, "deploy")
	ports := findResults(report, "port")
	if len(ports) != 1 || !ports[0].OK {
		t.Fatalf("remapped free port must pass, got %+v (blockers=%+v)", ports, report.Blockers)
	}
	if !strings.Contains(ports[0].Detail, strconv.Itoa(remap)) {
		t.Errorf("detail must carry the remapped port: %s", ports[0].Detail)
	}

	// 重映射目标被占 → 阻断，且报的是重映射后的端口。
	occ2, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(remap))
	if err != nil {
		t.Fatal(err)
	}
	defer occ2.Close()
	report = runPreflight(t, env, "deploy")
	ports = findResults(report, "port")
	if len(ports) != 1 || ports[0].OK || ports[0].Level != LevelBlocker {
		t.Fatalf("occupied remapped port must block, got %+v", ports)
	}
	if !strings.Contains(ports[0].Detail, strconv.Itoa(remap)) {
		t.Errorf("blocker must name the remapped port: %s", ports[0].Detail)
	}
}

func TestPreflightEnvFileMissingIsWarning(t *testing.T) {
	env := newPreflightEnv(t)
	os.Remove(env.envFile)

	report := runPreflight(t, env, "deploy")
	if report.HasBlockers() {
		t.Fatalf("missing env file must not block: %+v", report.Blockers)
	}
	envs := findResults(report, "env-file")
	if len(envs) != 1 || envs[0].OK || envs[0].Level != LevelWarning {
		t.Fatalf("env-file should be a single warning, got %+v", envs)
	}
	if !strings.Contains(envs[0].Detail, env.envFile) {
		t.Errorf("warning detail must carry the path: %s", envs[0].Detail)
	}
}

func TestPreflightEnvFileUnreadableIsBlocker(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission checks are ineffective as root")
	}
	env := newPreflightEnv(t)
	if err := os.Chmod(env.envFile, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(env.envFile, 0o600) })

	report := runPreflight(t, env, "deploy")
	envs := findResults(report, "env-file")
	if len(envs) != 1 || envs[0].OK || envs[0].Level != LevelBlocker {
		t.Fatalf("unreadable env file must be a blocker, got %+v", envs)
	}
	if !strings.Contains(envs[0].Detail, env.envFile) {
		t.Errorf("blocker detail must carry the path: %s", envs[0].Detail)
	}
}

func TestPreflightRootNotWritableIsBlocker(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission checks are ineffective as root")
	}
	env := newPreflightEnv(t)
	parent := filepath.Dir(env.root)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) })

	report := runPreflight(t, env, "deploy")
	// 安装根、版本目录、workdir 同源失败，必须一次全部报出。
	for _, name := range []string{"install-root", "version-dir", "workdir"} {
		res := findResults(report, name)
		if len(res) != 1 || res[0].OK || res[0].Level != LevelBlocker {
			t.Errorf("%s must be a blocker, got %+v", name, res)
		} else if !strings.Contains(res[0].Detail, "not writable") {
			t.Errorf("%s detail must name the unwritable path: %s", name, res[0].Detail)
		}
	}
	if len(report.Blockers) < 3 {
		t.Errorf("all three write checks must be reported at once, blockers=%+v", report.Blockers)
	}
}

func TestPreflightCurrentNotSymlinkIsBlocker(t *testing.T) {
	env := newPreflightEnv(t)
	if err := os.MkdirAll(filepath.Join(env.root, "current"), 0o755); err != nil {
		t.Fatal(err)
	}

	report := runPreflight(t, env, "deploy")
	links := findResults(report, "current-link")
	if len(links) != 1 || links[0].OK || links[0].Level != LevelBlocker {
		t.Fatalf("current as a real directory must be a blocker, got %+v", links)
	}
	if !strings.Contains(links[0].Detail, "not a symlink") {
		t.Errorf("detail must explain the shape problem: %s", links[0].Detail)
	}
}

func TestPreflightTinyvisorUnreachableIsBlocker(t *testing.T) {
	env := newPreflightEnv(t, func(cfg *models.Config, creds *config.Credentials, stub *tinyvisorStub) {
		cfg.Orchestration.Tinyvisor.URL = "http://127.0.0.1:1" // 立即拒绝
	})

	report := runPreflight(t, env, "deploy")
	tvs := findResults(report, "tinyvisor")
	if len(tvs) != 1 || tvs[0].OK || tvs[0].Level != LevelBlocker {
		t.Fatalf("unreachable tinyvisor must be a blocker, got %+v", tvs)
	}
	if !strings.Contains(tvs[0].Detail, "unreachable") || !strings.Contains(tvs[0].Detail, "http://127.0.0.1:1") {
		t.Errorf("detail must carry the URL: %s", tvs[0].Detail)
	}
}

func TestPreflightTinyvisorBadKeyIsBlocker(t *testing.T) {
	env := newPreflightEnv(t, func(cfg *models.Config, creds *config.Credentials, stub *tinyvisorStub) {
		creds.TinyvisorAPIKey = "wrong-key"
	})

	report := runPreflight(t, env, "deploy")
	tvs := findResults(report, "tinyvisor")
	if len(tvs) != 1 || tvs[0].OK || tvs[0].Level != LevelBlocker {
		t.Fatalf("invalid apiKey must be a blocker, got %+v", tvs)
	}
	if !strings.Contains(tvs[0].Detail, "apiKey is invalid") {
		t.Errorf("detail must say the key is invalid: %s", tvs[0].Detail)
	}
}

func TestPreflightRequiredDependencyDownIsBlocker(t *testing.T) {
	env := newPreflightEnv(t)
	env.depLn.Close() // ext-dep 不可达

	report := runPreflight(t, env, "deploy")
	deps := findResults(report, "dependency")
	if len(deps) != 1 || deps[0].OK || deps[0].Level != LevelBlocker {
		t.Fatalf("unreachable required dependency must be a blocker, got %+v", deps)
	}
	if !strings.Contains(deps[0].Detail, "ext-dep") {
		t.Errorf("detail must name the dependency: %s", deps[0].Detail)
	}
	// ext-dep 自身在范围内，其可达性检查也必须独立报出。
	reach := findResults(report, "reachability")
	if len(reach) != 1 || reach[0].OK || reach[0].Level != LevelBlocker {
		t.Fatalf("external project self-reachability must be a blocker, got %+v", reach)
	}
}

func TestPreflightOptionalDependencyDownIsWarning(t *testing.T) {
	env := newPreflightEnv(t)
	env.depLn.Close()

	// 把依赖改写为 optional（Load 每次从磁盘重读）。
	svcPath := filepath.Join(env.scanRoot, "svc-a", "deploy-manifest.yaml")
	data, err := os.ReadFile(svcPath)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.Facts.DependsOn[0].Kind = "optional"
	writeManifestFile(t, svcPath, &m)

	report := runPreflight(t, env, "deploy")
	deps := findResults(report, "dependency")
	if len(deps) != 1 || deps[0].OK || deps[0].Level != LevelWarning {
		t.Fatalf("unreachable optional dependency must be a warning, got %+v", deps)
	}
	if report.HasBlockers() {
		// ext-dep 自身可达性仍是阻断；依赖边本身必须是警告。
		for _, b := range report.Blockers {
			if b.Name == "dependency" {
				t.Fatalf("optional dependency must not block: %+v", b)
			}
		}
	}
}

func TestPreflightDiskInsufficientIsBlocker(t *testing.T) {
	env := newPreflightEnv(t)
	orig := diskSpaceCheck
	diskSpaceCheck = func(ctx context.Context, m *manifest.Manifest, destDir, token string) (bool, error) {
		return true, fmt.Errorf("insufficient disk space at %s: need 2.0 GiB, available 1.0 GiB, short by 1.0 GiB", destDir)
	}
	defer func() { diskSpaceCheck = orig }()

	report := runPreflight(t, env, "deploy")
	disks := findResults(report, "disk-space")
	if len(disks) != 1 || disks[0].OK || disks[0].Level != LevelBlocker {
		t.Fatalf("insufficient disk must be a blocker, got %+v", disks)
	}
	if !strings.Contains(disks[0].Detail, "short by") {
		t.Errorf("detail must carry the shortfall: %s", disks[0].Detail)
	}
}

func TestPreflightDiskUnknownSizeIsWarning(t *testing.T) {
	env := newPreflightEnv(t)
	orig := diskSpaceCheck
	diskSpaceCheck = func(ctx context.Context, m *manifest.Manifest, destDir, token string) (bool, error) {
		return false, nil
	}
	defer func() { diskSpaceCheck = orig }()

	report := runPreflight(t, env, "deploy")
	if report.HasBlockers() {
		t.Fatalf("unknown size must not block: %+v", report.Blockers)
	}
	disks := findResults(report, "disk-space")
	if len(disks) != 1 || disks[0].OK || disks[0].Level != LevelWarning {
		t.Fatalf("unknown artifact size must be a warning, got %+v", disks)
	}
}

func TestPreflightReportsAllFailuresAtOnce(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission checks are ineffective as root")
	}
	env := newPreflightEnv(t)
	// 同时制造三类互不相关的失败：端口被占 + env 不可读 + 依赖不可达。
	occ, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(env.svcPort))
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()
	if err := os.Chmod(env.envFile, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(env.envFile, 0o600) })
	env.depLn.Close()

	report := runPreflight(t, env, "deploy")
	names := map[string]bool{}
	for _, b := range report.Blockers {
		names[b.Name] = true
	}
	for _, want := range []string{"port", "env-file", "dependency", "reachability"} {
		if !names[want] {
			t.Errorf("blocker %q missing from one-pass report; blockers=%+v", want, report.Blockers)
		}
	}
}

func TestStartRunRefusedOnPreflightBlockers(t *testing.T) {
	env := newPreflightEnv(t)
	occ, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(env.svcPort))
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()

	for _, dryRun := range []bool{false, true} {
		_, err := env.orch.StartRun(RunOptions{DryRun: dryRun})
		var perr *ErrPreflightBlocked
		if !errors.As(err, &perr) {
			t.Fatalf("dry_run=%v: expected ErrPreflightBlocked, got %v", dryRun, err)
		}
		if len(perr.Report.Blockers) == 0 {
			t.Fatalf("dry_run=%v: error must carry the graded report", dryRun)
		}
		if cur := env.orch.CurrentRun(); cur != nil {
			t.Fatalf("dry_run=%v: refused run must not start, current=%+v", dryRun, cur)
		}
	}
}

// waitForRunFile 轮询运行记录文件直至完成（避免与执行协程并发读同一内存结构）。
func waitForRunFile(t *testing.T, o *Orchestrator, id string) *Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if run, err := o.store.LoadRun(id); err == nil && run.FinishedAt != nil && run.State != "running" {
			return run
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish in time", id)
	return nil
}

func TestDryRunRecordsPreflightReport(t *testing.T) {
	env := newPreflightEnv(t)

	snap, err := env.orch.StartRun(RunOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run refused: %v", err)
	}
	run := waitForRunFile(t, env.orch, snap.ID)

	if run.Preflight == nil {
		t.Fatal("run record must carry the preflight report")
	}
	if run.Preflight.HasBlockers() {
		t.Fatalf("passing preflight stored with blockers: %+v", run.Preflight.Blockers)
	}
	if run.State != "done" {
		t.Fatalf("dry run state = %s", run.State)
	}
	res, ok := run.Results["svc-a"]
	if !ok || res.Outcome != OutcomeDryRun {
		t.Fatalf("svc-a outcome = %+v", res)
	}
	// dry_run 必须把计划步骤打出来。
	if len(res.Steps) == 0 {
		t.Error("dry run must list planned steps")
	}
}

func TestPreflightRollbackModeSkipsArtifactChecks(t *testing.T) {
	env := newPreflightEnv(t)
	orig := diskSpaceCheck
	diskSpaceCheck = func(ctx context.Context, m *manifest.Manifest, destDir, token string) (bool, error) {
		return true, errors.New("disk check must not run in rollback mode")
	}
	defer func() { diskSpaceCheck = orig }()

	report := runPreflight(t, env, "rollback")
	assertNoFindings(t, report)
	for _, name := range []string{"disk-space", "version-dir"} {
		if res := findResults(report, name); len(res) != 0 {
			t.Errorf("rollback mode must skip %s, got %+v", name, res)
		}
	}
	// 其余检查面仍然在。
	for _, name := range []string{"install-root", "current-link", "env-file", "port", "tinyvisor"} {
		if len(findResults(report, name)) == 0 {
			t.Errorf("rollback mode must still check %s", name)
		}
	}
}
