package orchestrator

// 部署后冒烟验证测试：冒烟通过、状态码不符、响应体断言失败、超时、
// 需要密钥但凭据缺失，以及端到端"冒烟失败 = 部署失败 → 自动回滚"。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/manifest"
	"HA-Services-Deployer/internal/models"

	"github.com/goccy/go-yaml"
)

// smokeBackend 是冒烟请求的 HTTP 后端桩，监听真实端口（供清单声明）。
type smokeBackend struct {
	port int
	srv  *httptest.Server
}

func newSmokeBackend(t *testing.T, handler http.HandlerFunc) *smokeBackend {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// srv.URL 形如 http://127.0.0.1:PORT
	port, err := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])
	if err != nil {
		t.Fatal(err)
	}
	return &smokeBackend{port: port, srv: srv}
}

// ---------- 单元级：smokeOne / runSmoke ----------

// newSmokeOrch 构造只含最小配置的编排器（冒烟执行不依赖清单聚合）。
func newSmokeOrch(t *testing.T, creds *config.Credentials) *Orchestrator {
	t.Helper()
	cfg := &models.Config{}
	cfg.Orchestration.Enabled = true
	cfg.Orchestration.StateDir = t.TempDir()
	cfg.Orchestration.ProbeHost = "127.0.0.1"
	cfg.Orchestration.Tinyvisor.URL = "http://127.0.0.1:1" // 冒烟执行用不到
	return New(cfg, creds)
}

// smokeManifest 构造声明了单条冒烟检查的清单，端口取后端实际监听端口。
func smokeManifest(port int, chk manifest.SmokeCheck) *manifest.Manifest {
	return &manifest.Manifest{
		ManifestVersion: 1,
		Kind:            "service",
		Project:         "svc-s",
		ServiceVersion:  "1.0.0",
		Facts: manifest.Facts{
			Ports: []manifest.Port{{Name: "http", Port: port, Exposure: "loopback"}},
			Health: manifest.Health{HTTP: &manifest.HTTPProbe{
				PortName: "http", Path: "/healthz", ExpectStatus: 200,
				TimeoutSeconds: 2, IntervalSeconds: 1, Retries: 1,
			}},
			Smoke: []manifest.SmokeCheck{chk},
		},
	}
}

func TestSmokePass(t *testing.T) {
	srv := newSmokeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"success":true,"data":{"version":"1.2.3","items":[1,2]}}`)
	})
	o := newSmokeOrch(t, &config.Credentials{})
	m := smokeManifest(srv.port, manifest.SmokeCheck{
		Name: "functional", Method: "GET", Path: "/check", ExpectStatus: 200,
		Assert: manifest.SmokeAssert{Contains: "version", JSONField: "data.version", JSONNotEmpty: "data.items"},
	})
	if err := o.runSmoke(context.Background(), m, 0); err != nil {
		t.Fatalf("smoke should pass: %v", err)
	}
}

func TestSmokeStatusMismatch(t *testing.T) {
	srv := newSmokeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	o := newSmokeOrch(t, &config.Credentials{})
	m := smokeManifest(srv.port, manifest.SmokeCheck{Name: "must-200", Path: "/check", ExpectStatus: 200})

	err := o.runSmoke(context.Background(), m, 0)
	if err == nil {
		t.Fatal("expected status mismatch error")
	}
	if !strings.Contains(err.Error(), "expect status 200, got 500") {
		t.Errorf("error must state expected vs actual status: %v", err)
	}
	if !strings.Contains(err.Error(), `"must-200"`) {
		t.Errorf("error must name the failed check: %v", err)
	}
}

func TestSmokeDeterministicNegativeStatus(t *testing.T) {
	// 期望 404 的确定性负结果（如"查无此人"）也算冒烟通过。
	srv := newSmokeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"ProfileNotFoundException"}`)
	})
	o := newSmokeOrch(t, &config.Credentials{})
	m := smokeManifest(srv.port, manifest.SmokeCheck{
		Name: "profile-miss", Path: "/profile/0", ExpectStatus: 404,
		Assert: manifest.SmokeAssert{Contains: "ProfileNotFoundException"},
	})
	if err := o.runSmoke(context.Background(), m, 0); err != nil {
		t.Fatalf("deterministic 404 should pass: %v", err)
	}
}

func TestSmokeBodyAssertionFailures(t *testing.T) {
	body := `{"data":{"version":"1.0","items":[]}}`
	cases := []struct {
		name   string
		assert manifest.SmokeAssert
		want   string
	}{
		{"contains", manifest.SmokeAssert{Contains: "no-such-token"}, `does not contain "no-such-token"`},
		{"json-field-missing", manifest.SmokeAssert{JSONField: "data.absent"}, `json field "data.absent" not present`},
		{"json-not-empty-array", manifest.SmokeAssert{JSONNotEmpty: "data.items"}, `json field "data.items" is empty`},
		{"json-not-empty-missing", manifest.SmokeAssert{JSONNotEmpty: "nope"}, `json field "nope" not present`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSmokeBackend(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
			o := newSmokeOrch(t, &config.Credentials{})
			m := smokeManifest(srv.port, manifest.SmokeCheck{Name: "assert", Path: "/check", Assert: tc.assert})
			err := o.runSmoke(context.Background(), m, 0)
			if err == nil {
				t.Fatal("expected assertion failure")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestSmokeBodyNotJSON(t *testing.T) {
	srv := newSmokeBackend(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "plain text") })
	o := newSmokeOrch(t, &config.Credentials{})
	m := smokeManifest(srv.port, manifest.SmokeCheck{Name: "json", Path: "/check", Assert: manifest.SmokeAssert{JSONField: "data"}})
	err := o.runSmoke(context.Background(), m, 0)
	if err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("non-JSON body must fail json assertions: %v", err)
	}
}

// SPA 静态回退场景：未知路径回 200 + text/html。状态码是伪造的，
// content-type 断言必须识破——这比没有冒烟更坏，因为绿勾是假的。
func TestSmokeContentTypeCatchesSPAFallback(t *testing.T) {
	srv := newSmokeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body>SPA fallback</body></html>`)
	})
	o := newSmokeOrch(t, &config.Credentials{})
	m := smokeManifest(srv.port, manifest.SmokeCheck{
		Name: "session-status", Path: "/api/session", ExpectStatus: 200,
		Assert: manifest.SmokeAssert{ExpectContentType: "application/json", JSONField: "data.auth_required"},
	})
	err := o.runSmoke(context.Background(), m, 0)
	if err == nil {
		t.Fatal("SPA fallback (text/html) must not pass a smoke expecting application/json")
	}
	if !strings.Contains(err.Error(), "content-type") || !strings.Contains(err.Error(), "text/html") {
		t.Errorf("error must name the content-type mismatch: %v", err)
	}
}

// 真实 JSON 端点：带 charset 参数、大小写差异都应放行。
func TestSmokeContentTypePassesRealEndpoint(t *testing.T) {
	srv := newSmokeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprint(w, `{"success":true,"message":"session status","data":{"auth_required":false,"authenticated":false}}`)
	})
	o := newSmokeOrch(t, &config.Credentials{})
	m := smokeManifest(srv.port, manifest.SmokeCheck{
		Name: "session-status", Path: "/api/session", ExpectStatus: 200,
		Assert: manifest.SmokeAssert{ExpectContentType: "Application/JSON", JSONField: "data.auth_required"},
	})
	if err := o.runSmoke(context.Background(), m, 0); err != nil {
		t.Fatalf("real JSON endpoint must pass: %v", err)
	}
}

func TestContentTypeMatches(t *testing.T) {
	cases := []struct {
		got, want string
		ok        bool
	}{
		{"application/json", "application/json", true},
		{"application/json; charset=utf-8", "application/json", true},
		{"Application/JSON", "application/json", true},
		{"text/html; charset=utf-8", "application/json", false},
		{"", "application/json", false},
	}
	for _, tc := range cases {
		if contentTypeMatches(tc.got, tc.want) != tc.ok {
			t.Errorf("contentTypeMatches(%q, %q) = %v, want %v", tc.got, tc.want, !tc.ok, tc.ok)
		}
	}
}

func TestSmokeTimeout(t *testing.T) {
	srv := newSmokeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	})
	o := newSmokeOrch(t, &config.Credentials{})
	m := smokeManifest(srv.port, manifest.SmokeCheck{Name: "slow", Path: "/check", TimeoutSeconds: 1})

	start := time.Now()
	err := o.runSmoke(context.Background(), m, 0)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timeout after 1s") {
		t.Errorf("error must state the timeout: %v", err)
	}
	if time.Since(start) > 1900*time.Millisecond {
		t.Errorf("timeout not enforced promptly: %v", time.Since(start))
	}
}

func TestSmokeSecretMissing(t *testing.T) {
	srv := newSmokeBackend(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	o := newSmokeOrch(t, &config.Credentials{}) // 无 smoke_secrets
	m := smokeManifest(srv.port, manifest.SmokeCheck{
		Name: "needs-secret", Path: "/check",
		Headers: map[string]string{"X-Internal-Key": "${secret:internal-key}"},
	})
	err := o.runSmoke(context.Background(), m, 0)
	if err == nil {
		t.Fatal("expected missing-secret error")
	}
	if !strings.Contains(err.Error(), "internal-key") || !strings.Contains(err.Error(), "credentials file") {
		t.Errorf("error must name the secret and the credentials file: %v", err)
	}
}

func TestSmokeSecretsResolvedFromCredentials(t *testing.T) {
	var gotHeader, gotBody string
	srv := newSmokeBackend(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Internal-Key")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	})
	creds := &config.Credentials{SmokeSecrets: map[string]string{
		"internal-key": "v1-secret",
		"body-secret":  "s3cr3t",
	}}
	o := newSmokeOrch(t, creds)
	m := smokeManifest(srv.port, manifest.SmokeCheck{
		Name: "with-secrets", Method: "POST", Path: "/check",
		Headers: map[string]string{"X-Internal-Key": "${secret:internal-key}"},
		Body:    `{"password":"${secret:body-secret}"}`,
	})
	if err := o.runSmoke(context.Background(), m, 0); err != nil {
		t.Fatalf("smoke with resolved secrets should pass: %v", err)
	}
	if gotHeader != "v1-secret" {
		t.Errorf("header secret not resolved: %q", gotHeader)
	}
	if !strings.Contains(gotBody, `"password":"s3cr3t"`) {
		t.Errorf("body secret not resolved inline: %q", gotBody)
	}
	if strings.Contains(gotBody, "${secret:") {
		t.Errorf("unresolved reference leaked into request body: %q", gotBody)
	}
}

// ---------- preflight：smoke 密钥解析检查 ----------

func TestPreflightSmokeSecretMissingIsBlocker(t *testing.T) {
	env := newPreflightEnv(t)
	addSmokeToSvcManifest(t, env, manifest.SmokeCheck{
		Name: "needs-secret", Path: "/check",
		Headers: map[string]string{"X-Internal-Key": "${secret:smoke-key}"},
	})

	report := runPreflight(t, env, "deploy")
	secrets := findResults(report, "smoke-secrets")
	if len(secrets) != 1 || secrets[0].OK || secrets[0].Level != LevelBlocker {
		t.Fatalf("missing smoke secret must be a blocker, got %+v", secrets)
	}
	if !strings.Contains(secrets[0].Detail, "smoke-key") || !strings.Contains(secrets[0].Detail, "credentials file") {
		t.Errorf("detail must name the secret and where to configure it: %s", secrets[0].Detail)
	}
}

func TestPreflightSmokeSecretConfiguredPasses(t *testing.T) {
	env := newPreflightEnv(t, func(cfg *models.Config, creds *config.Credentials, stub *tinyvisorStub) {
		creds.SmokeSecrets = map[string]string{"smoke-key": "v1"}
	})
	addSmokeToSvcManifest(t, env, manifest.SmokeCheck{
		Name: "needs-secret", Path: "/check",
		Headers: map[string]string{"X-Internal-Key": "${secret:smoke-key}"},
	})

	report := runPreflight(t, env, "deploy")
	assertNoFindings(t, report)
	if res := findResults(report, "smoke-secrets"); len(res) != 1 || !res[0].OK {
		t.Fatalf("configured smoke secret must pass, got %+v", res)
	}
}

// addSmokeToSvcManifest 给环境中的 svc-a 清单追加一条冒烟声明（Load 每次重读磁盘）。
func addSmokeToSvcManifest(t *testing.T, env *preflightEnv, chk manifest.SmokeCheck) {
	t.Helper()
	path := filepath.Join(env.scanRoot, "svc-a", "deploy-manifest.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest.Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.Facts.Smoke = append(m.Facts.Smoke, chk)
	writeManifestFile(t, path, &m)
}

// ---------- 端到端：部署流水线中的冒烟 ----------

// startSvcServer 在 svc-a 声明的端口上起真实 HTTP 服务：
// /healthz 恒 200；smokeOK 决定 /check 的成败。
func startSvcServer(t *testing.T, env *preflightEnv, smokeOK bool) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/check", func(w http.ResponseWriter, r *http.Request) {
		if smokeOK {
			fmt.Fprint(w, `{"data":{"items":[1]}}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", env.svcPort))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

func functionalSmokeCheck() manifest.SmokeCheck {
	return manifest.SmokeCheck{Name: "functional", Path: "/check", ExpectStatus: 200,
		Assert: manifest.SmokeAssert{JSONNotEmpty: "data.items"}}
}

func TestDeploySmokePassEndToEnd(t *testing.T) {
	env := newPreflightEnv(t)
	addSmokeToSvcManifest(t, env, functionalSmokeCheck())
	startSvcServer(t, env, true)
	// 端口由项目自己的（模拟）在跑实例持有，Tinyvisor 状态 running。
	env.stub.setStatus("svc-a", "running")

	snap, err := env.orch.StartRun(RunOptions{Projects: []string{"svc-a"}})
	if err != nil {
		t.Fatalf("run refused: %v", err)
	}
	run := waitForRunFile(t, env.orch, snap.ID)

	res := run.Results["svc-a"]
	if res.Outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s, detail=%s, steps=%+v", res.Outcome, res.Detail, res.Steps)
	}
	var smokeStep *StepRecord
	for i := range res.Steps {
		if res.Steps[i].Name == "smoke" {
			smokeStep = &res.Steps[i]
		}
	}
	if smokeStep == nil || !smokeStep.OK {
		t.Fatalf("deployed run must record a passing smoke step, steps=%+v", res.Steps)
	}
}

func TestDeploySmokeFailTriggersAutoRollback(t *testing.T) {
	env := newPreflightEnv(t)
	addSmokeToSvcManifest(t, env, functionalSmokeCheck())
	startSvcServer(t, env, false)
	env.stub.setStatus("svc-a", "running")

	// 预置上一版本：releases/0.9.0 + current -> 0.9.0。
	if err := os.MkdirAll(filepath.Join(env.root, "releases", "0.9.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := switchSymlink(env.root, "0.9.0"); err != nil {
		t.Fatal(err)
	}

	snap, err := env.orch.StartRun(RunOptions{Projects: []string{"svc-a"}})
	if err != nil {
		t.Fatalf("run refused: %v", err)
	}
	run := waitForRunFile(t, env.orch, snap.ID)

	res := run.Results["svc-a"]
	if res.Outcome != OutcomeFailed {
		t.Fatalf("smoke failure must fail the deploy, outcome=%s detail=%s", res.Outcome, res.Detail)
	}
	if !strings.HasPrefix(res.Detail, "smoke verification failed after health") {
		t.Errorf("detail = %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "auto-rolled back to 0.9.0") {
		t.Errorf("detail must record the auto-rollback: %q", res.Detail)
	}
	// 冒烟失败必须走与健康失败相同的自动回滚路径。
	if got := readActiveVersion(env.root); got != "0.9.0" {
		t.Errorf("current must be rolled back to 0.9.0, got %q", got)
	}
	var sawSmokeFail, sawRollback bool
	for _, s := range res.Steps {
		if s.Name == "smoke" && !s.OK && strings.Contains(s.Detail, "expect status 200, got 500") {
			sawSmokeFail = true
		}
		if s.Name == "auto-rollback" && s.OK {
			sawRollback = true
		}
	}
	if !sawSmokeFail {
		t.Errorf("smoke step must record the status mismatch, steps=%+v", res.Steps)
	}
	if !sawRollback {
		t.Errorf("auto-rollback step missing, steps=%+v", res.Steps)
	}
}

func TestDeploySmokeSecretMissingBlockedAtPreflight(t *testing.T) {
	env := newPreflightEnv(t) // 凭据里没有 smoke_secrets
	addSmokeToSvcManifest(t, env, manifest.SmokeCheck{
		Name: "needs-secret", Path: "/check",
		Headers: map[string]string{"X-Internal-Key": "${secret:absent-key}"},
	})

	_, err := env.orch.StartRun(RunOptions{Projects: []string{"svc-a"}})
	var perr *ErrPreflightBlocked
	if !errors.As(err, &perr) {
		t.Fatalf("expected ErrPreflightBlocked, got %v", err)
	}
	var found bool
	for _, b := range perr.Report.Blockers {
		if b.Name == "smoke-secrets" && strings.Contains(b.Detail, "absent-key") {
			found = true
		}
	}
	if !found {
		t.Fatalf("blockers must include missing smoke secret, got %+v", perr.Report.Blockers)
	}
	if cur := env.orch.CurrentRun(); cur != nil {
		t.Fatal("refused run must not start")
	}
}
