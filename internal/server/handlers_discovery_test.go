package server

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"HA-Cupbearer/internal/discovery"
	"HA-Cupbearer/internal/manifest"
)

func TestDiscoveryImportEndpoint(t *testing.T) {
	orch, cfg, _ := buildOrchestrationEnv(t)
	root := t.TempDir()
	directory := filepath.Join(root, "extra-api")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "extra-api"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer health.Close()
	port := healthPort(t, health)
	if err := os.WriteFile(filepath.Join(directory, "config.yaml"), []byte("server:\n  port: "+strconv.Itoa(port)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Orchestration.DeploymentRoots = []string{root}
	router := newDeployTestEnv(t, orch, cfg)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deploy/discovery", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"project":"extra-api"`) {
		t.Fatalf("discovery failed: %d %s", w.Code, w.Body.String())
	}

	payload, err := json.Marshal(discovery.ImportRequest{Root: root, Items: []discovery.ImportItem{{
		Directory: directory, Project: "extra-api", DisplayName: "Extra API", Manager: "manual",
		Port: port, HealthPath: "/healthz", HealthStatus: 200,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	post := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/deploy/discovery/import", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Deploy-Token", token)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	if response := post(""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated import status = %d", response.Code)
	}
	response := post("test-deploy-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("import failed: %d %s", response.Code, response.Body.String())
	}
	manifests, rep := orch.Load()
	if !rep.OK() {
		t.Fatalf("imported manifest invalid: %v", rep.Errors)
	}
	var imported *manifest.Manifest
	for _, m := range manifests {
		if m.Project == "extra-api" {
			imported = m
		}
	}
	if imported == nil || imported.Baremetal.Process.Manager != "manual" {
		t.Fatalf("imported read-only project missing: %+v", manifests)
	}
	if filepath.Dir(imported.SourcePath) != filepath.Join(cfg.Orchestration.StateDir, "imported-manifests") {
		t.Fatalf("manifest written outside state: %s", imported.SourcePath)
	}
}

func TestManifestBatchImportEndpoint(t *testing.T) {
	orch, cfg, _ := buildOrchestrationEnv(t)
	root := t.TempDir()
	writeDiscoveryTestFile(t, filepath.Join(root, "deploy-manifest.yaml"), "discovery:\n  depth: 3\n")
	directory := filepath.Join(root, "team", "services", "demo-api")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(directory, "deploy-manifest.yaml")
	source := `manifest_version: 1
kind: service
project: demo-api
display_name: Demo API
group: ops
service_version: unknown
runtime: baremetal
facts:
  ports:
    - {name: http, port: 18081, exposure: loopback}
  health:
    http: {port_name: http, path: /ready, expect_status: 204}
baremetal:
  process: {manager: aapanel}
  install: {root: unknown, binary_name: service, keep_releases: 5}
`
	writeDiscoveryTestFile(t, sourcePath, source)
	duplicateDir := filepath.Join(root, "team", "services", "duplicate-api")
	if err := os.MkdirAll(duplicateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	duplicateSource := strings.ReplaceAll(source, "display_name: Demo API", "display_name: Duplicate API")
	writeDiscoveryTestFile(t, filepath.Join(duplicateDir, "deploy-manifest.yaml"), duplicateSource)
	cfg.Orchestration.DeploymentRoots = []string{root}
	router := newDeployTestEnv(t, orch, cfg)

	payload := []byte(`{"root":` + strconv.Quote(root) + `,"import_manifests":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deploy/discovery/import", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Deploy-Token", "test-deploy-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusCreated {
		t.Fatalf("manifest batch import failed: %d %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data discovery.ImportResult `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Imported) != 1 || len(envelope.Data.Skipped) != 1 || !strings.Contains(envelope.Data.Skipped[0].Reason, "项目名重复") {
		t.Fatalf("duplicate manifest was not skipped: %s", response.Body.String())
	}
	manifests, rep := orch.Load()
	if !rep.OK() {
		t.Fatalf("imported manifest invalid: %v", rep.Errors)
	}
	var imported *manifest.Manifest
	for _, m := range manifests {
		if m.Project == "demo-api" {
			imported = m
		}
	}
	if imported == nil || imported.Baremetal.Process.Manager != "aapanel" || imported.Facts.Health.HTTP.Path != "/ready" || imported.Facts.Health.HTTP.ExpectStatus != 204 {
		t.Fatalf("manifest data not imported: %+v", manifests)
	}
	if filepath.Dir(imported.SourcePath) != filepath.Join(cfg.Orchestration.StateDir, "imported-manifests") {
		t.Fatalf("manifest written outside state: %s", imported.SourcePath)
	}
	unchanged, err := os.ReadFile(sourcePath)
	if err != nil || string(unchanged) != source {
		t.Fatal("source service directory was modified")
	}
}

func writeDiscoveryTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func healthPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	address, err := net.ResolveTCPAddr("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return address.Port
}
