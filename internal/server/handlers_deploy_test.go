package server

// 部署编排端点的 HTTP 层测试：preflight 端点接线、运行入口的
// preflight 拒绝映射（400 + 完整报告）、编排关闭时的 503。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/manifest"
	"HA-Services-Deployer/internal/models"
	"HA-Services-Deployer/internal/orchestrator"

	"github.com/gin-gonic/gin"
	"github.com/goccy/go-yaml"
)

// startTinyvisorStub 提供 preflight 所需的最小 Tinyvisor 桩（/ping 免鉴权，
// /api/* 校验 X-Minivisor-Key，无效 401——与真实实现一致）。
func startTinyvisorStub(t *testing.T, key string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, code int, v interface{}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	authed := func(r *http.Request) bool { return r.Header.Get("X-Minivisor-Key") == key }
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"message": "pong"})
	})
	mux.HandleFunc("/api/scripts", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"scripts": []interface{}{}})
	})
	mux.HandleFunc("/api/scripts/by-name/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newDeployTestEnv 构造启用了编排的 Router 测试环境：
// 单个 tinyvisor 管理的 svc-a，无依赖。返回 router 与其声明端口。
func newDeployTestEnv(t *testing.T, orch *orchestrator.Orchestrator, cfg *models.Config) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return NewRouter(cfg, orch, "test-deploy-token")
}

func buildOrchestrationEnv(t *testing.T) (*orchestrator.Orchestrator, *models.Config, int) {
	t.Helper()
	base := t.TempDir()
	scanRoot := filepath.Join(base, "scan")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	artifactPath := filepath.Join(base, "svc-a-linux-amd64")
	if err := os.WriteFile(artifactPath, []byte("binary-v1"), 0o755); err != nil {
		t.Fatal(err)
	}

	svc := &manifest.Manifest{
		ManifestVersion: 1,
		Kind:            "service",
		Project:         "svc-a",
		DisplayName:     "Service A",
		Group:           "core",
		ServiceVersion:  "1.0.0",
		Facts: manifest.Facts{
			Ports: []manifest.Port{{Name: "http", Port: port, Exposure: "loopback"}},
			Health: manifest.Health{HTTP: &manifest.HTTPProbe{
				PortName: "http", Path: "/healthz", ExpectStatus: 200,
				TimeoutSeconds: 2, IntervalSeconds: 1, Retries: 1,
			}},
		},
		Baremetal: manifest.Baremetal{
			Process:  manifest.ProcessSpec{Manager: "tinyvisor"},
			Install:  manifest.InstallSpec{Root: filepath.Join(base, "install", "svc-a"), Layout: "releases", BinaryName: "svc-a", KeepReleases: 3},
			Artifact: manifest.ArtifactSpec{Source: "local_path", LocalPath: artifactPath},
		},
	}
	data, err := yaml.Marshal(svc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(scanRoot, "svc-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scanRoot, "svc-a", "deploy-manifest.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	stub := startTinyvisorStub(t, "test-key")
	cfg := &models.Config{}
	cfg.Orchestration.Enabled = true
	cfg.Orchestration.ScanRoots = []string{scanRoot}
	cfg.Orchestration.StateDir = filepath.Join(base, "state")
	cfg.Orchestration.ProbeHost = "127.0.0.1"
	cfg.Orchestration.Tinyvisor.URL = stub.URL
	cfg.Storage.ArtifactsRoot = filepath.Join(base, "artifacts")
	creds := &config.Credentials{TinyvisorAPIKey: "test-key"}

	orch := orchestrator.New(cfg, creds)
	if _, rep := orch.Load(); !rep.OK() {
		t.Fatalf("aggregate validation failed: %v", rep.Errors)
	}
	return orch, cfg, port
}

func TestGetDeployPreflight(t *testing.T) {
	orch, cfg, port := buildOrchestrationEnv(t)
	router := newDeployTestEnv(t, orch, cfg)

	// 全绿：200 + 无阻断项。
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploy/preflight", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Mode     string `json:"mode"`
			Blockers []struct {
				Name   string `json:"name"`
				Detail string `json:"detail"`
			} `json:"blockers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Success || out.Data.Mode != "deploy" || len(out.Data.Blockers) != 0 {
		t.Fatalf("unexpected report: %s", w.Body.String())
	}

	// 端口被占：报告接口仍 200，但阻断项必须带上端口号。
	occ, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploy/preflight", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), strconv.Itoa(port)) {
		t.Errorf("blocker detail must carry the port number: %s", w.Body.String())
	}

	// 范围参数生效。
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploy/preflight?projects=svc-a", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"scope"`) {
		t.Fatalf("scoped preflight failed: %d %s", w.Code, w.Body.String())
	}

	// 未知项目 → 400。
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploy/preflight?projects=nope", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad scope status = %d", w.Code)
	}
}

func TestGetDeployPreflightDisabled(t *testing.T) {
	_, cfg, _ := buildOrchestrationEnv(t)
	gin.SetMode(gin.TestMode)
	router := NewRouter(cfg, nil, "")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploy/preflight", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", w.Code)
	}
}

// TestPostDeployRunPreflightBlocked：运行入口发现阻断项时 400 拒绝，
// 响应体携带完整分级报告（每条带具体路径/端口）。
func TestPostDeployRunPreflightBlocked(t *testing.T) {
	orch, cfg, port := buildOrchestrationEnv(t)
	router := newDeployTestEnv(t, orch, cfg)

	occ, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/deploy/run",
		bytes.NewReader([]byte(`{"projects":["svc-a"]}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Deploy-Token", "test-deploy-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "preflight blocked") {
		t.Errorf("message must say preflight blocked: %s", body)
	}
	var out struct {
		Data struct {
			Preflight struct {
				Blockers []struct {
					Name   string `json:"name"`
					Detail string `json:"detail"`
				} `json:"blockers"`
			} `json:"preflight"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data.Preflight.Blockers) == 0 {
		t.Fatal("refusal must carry the graded report")
	}
	found := false
	for _, b := range out.Data.Preflight.Blockers {
		if b.Name == "port" && strings.Contains(b.Detail, fmt.Sprintf("%d", port)) {
			found = true
		}
	}
	if !found {
		t.Errorf("report must contain the port blocker with its number: %+v", out.Data.Preflight.Blockers)
	}
}
