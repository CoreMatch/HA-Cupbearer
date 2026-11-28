package discovery

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"HA-Cupbearer/internal/manifest"
)

func TestScanDiscoversAndProbesDirectChild(t *testing.T) {
	root := t.TempDir()
	serviceDir := filepath.Join(root, "demo-api")
	if err := os.Mkdir(serviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serviceDir, "demo-api"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	port := serverPort(t, server.URL)
	config := "server:\n  port: " + strconv.Itoa(port) + "\n  password: never-return-this\n"
	if err := os.WriteFile(filepath.Join(serviceDir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	report := Scan(context.Background(), []string{root}, filepath.Join(root, "state"), Snapshot{}, nil)
	candidate := onlyCandidate(t, report)
	if candidate.Project != "demo-api" || !candidate.Importable || candidate.Port != port || candidate.HealthPath != "/healthz" {
		t.Fatalf("unexpected candidate: %+v", candidate)
	}
	if strings.Contains(strings.Join(candidate.Evidence, " "), "never-return-this") {
		t.Fatal("discovery leaked config contents")
	}
}

func TestScanMarksContainersAndMissingHealthAsBlocked(t *testing.T) {
	root := t.TempDir()
	containerDir := filepath.Join(root, "web")
	if err := os.Mkdir(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "web"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{Containers: []Container{{Name: "web-1", Project: "web", WorkingDir: containerDir}}}
	report := Scan(context.Background(), []string{root}, "", snapshot, nil)
	candidate := onlyCandidate(t, report)
	if candidate.Runtime != "container" || candidate.Importable || len(candidate.Blockers) == 0 {
		t.Fatalf("container candidate must stay read-only: %+v", candidate)
	}

	if err := os.RemoveAll(containerDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "web"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	report = Scan(context.Background(), []string{root}, "", Snapshot{}, nil)
	candidate = onlyCandidate(t, report)
	if candidate.Importable || !strings.Contains(strings.Join(candidate.Blockers, " "), "健康端点") {
		t.Fatalf("missing health must block automatic import: %+v", candidate)
	}
}

func TestValidateDirectoryRejectsNestedAndManifestDirectories(t *testing.T) {
	root := t.TempDir()
	direct := filepath.Join(root, "service")
	nested := filepath.Join(direct, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDirectory(root, nested); err == nil {
		t.Fatal("nested directory accepted")
	}
	if err := os.WriteFile(filepath.Join(direct, "deploy-manifest.yaml"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDirectory(root, direct); err == nil {
		t.Fatal("directory with existing manifest accepted")
	}
}

func TestImportValidationRejectsUnconfiguredRootAndManagedManager(t *testing.T) {
	configured := t.TempDir()
	other := t.TempDir()
	if _, err := ValidateRoot([]string{configured}, other); err == nil {
		t.Fatal("unconfigured root accepted")
	}
	item := ImportItem{Directory: "/srv/demo", Project: "demo", DisplayName: "Demo", Manager: "tinyvisor", Port: 8080, HealthPath: "/healthz", HealthStatus: 200}
	if _, err := BuildImportedManifest(item, item.Directory, ""); err == nil {
		t.Fatal("phase 1 accepted a managed tinyvisor import")
	}
}

func TestWriteBatchDoesNotOverwriteAndRollsBack(t *testing.T) {
	dir := t.TempDir()
	item := ImportItem{Directory: "/srv/demo", Project: "demo", DisplayName: "Demo", Manager: "manual", Port: 8080, HealthPath: "/healthz", HealthStatus: 200}
	m, err := BuildImportedManifest(item, item.Directory, "")
	if err != nil {
		t.Fatal(err)
	}
	other := *m
	other.Project = "other"
	other.DisplayName = "Other"
	if err := os.WriteFile(filepath.Join(dir, "other.yaml"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteBatch(dir, []*manifest.Manifest{m, &other}); err == nil {
		t.Fatal("overwrite accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "demo.yaml")); !os.IsNotExist(err) {
		t.Fatal("created manifest was not rolled back")
	}
	data, err := os.ReadFile(filepath.Join(dir, "other.yaml"))
	if err != nil || string(data) != "keep" {
		t.Fatal("existing manifest changed")
	}
}

func TestScanImportsManifestAtDeclaredDepth(t *testing.T) {
	root := t.TempDir()
	writeDiscoveryFile(t, filepath.Join(root, "deploy-manifest.yaml"), "discovery:\n  depth: 3\n")
	directory := filepath.Join(root, "team", "services", "demo-api")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDiscoveryFile(t, filepath.Join(directory, "deploy-manifest.yaml"), readOnlyServiceManifest("demo-api", "Demo API", "manual", directory))

	report := Scan(context.Background(), []string{root}, "", Snapshot{}, nil)
	if report.Roots[0].DiscoveryDepth != 3 {
		t.Fatalf("discovery depth not applied: %+v", report.Roots[0])
	}
	candidate := onlyCandidate(t, report)
	if !candidate.ExistingManifest || !candidate.Importable || candidate.Project != "demo-api" || candidate.DisplayName != "Demo API" {
		t.Fatalf("manifest candidate not importable: %+v", candidate)
	}
	if candidate.Port != 18081 || candidate.HealthPath != "/ready" || candidate.HealthStatus != 204 || candidate.ImportManager != "manual" {
		t.Fatalf("manifest health config not used: %+v", candidate)
	}
	validated, err := ValidateImportDirectory(report.Roots[0].Root, directory, true)
	if err != nil || validated != directory {
		t.Fatalf("nested manifest import rejected: %q %v", validated, err)
	}
}

func TestScanManifestDepthDefaultsToDirectChildren(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "team", "demo-api")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDiscoveryFile(t, filepath.Join(directory, "deploy-manifest.yaml"), readOnlyServiceManifest("demo-api", "Demo API", "manual", directory))
	report := Scan(context.Background(), []string{root}, "", Snapshot{}, nil)
	if report.Roots[0].DiscoveryDepth != 1 || len(report.Roots[0].Candidates) != 0 {
		t.Fatalf("default depth scanned nested manifests: %+v", report.Roots[0])
	}
}

func TestScanBlocksTinyvisorManifestImport(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "managed-api")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDiscoveryFile(t, filepath.Join(directory, "deploy-manifest.yaml"), readOnlyServiceManifest("managed-api", "Managed API", "tinyvisor", directory))
	candidate := onlyCandidate(t, Scan(context.Background(), []string{root}, "", Snapshot{}, nil))
	if candidate.Importable || !strings.Contains(strings.Join(candidate.Blockers, " "), "scan_roots") {
		t.Fatalf("managed manifest must not be imported read-only: %+v", candidate)
	}
}

func writeDiscoveryFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readOnlyServiceManifest(project, displayName, manager, root string) string {
	return fmt.Sprintf(`manifest_version: 1
kind: service
project: %s
display_name: %s
group: ops
service_version: unknown
runtime: baremetal
facts:
  ports:
    - {name: admin, port: 19090, exposure: loopback}
    - {name: http, port: 18081, exposure: loopback}
  health:
    http:
      port_name: http
      path: /ready
      expect_status: 204
      timeout_seconds: 3
      interval_seconds: 30
      retries: 3
baremetal:
  process:
    manager: %s
  install:
    root: %q
    binary_name: service
    keep_releases: 5
  artifact:
    source: local_path
    local_path: build/service
`, project, displayName, manager, root)
}

func onlyCandidate(t *testing.T, report DiscoveryReport) Candidate {
	t.Helper()
	if len(report.Roots) != 1 || len(report.Roots[0].Candidates) != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}
	return report.Roots[0].Candidates[0]
}

func serverPort(t *testing.T, rawURL string) int {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}
