package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, dir, repo, content string) string {
	t.Helper()
	path := filepath.Join(dir, repo, FileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const validManifest = `
manifest_version: 1
project: svc-a
display_name: Service A
group: core
service_version: "1.2.0"
facts:
  ports:
    - { name: http, port: 2778, exposure: public }
  health:
    http: { port_name: http, path: /health, expect_status: 200, timeout_seconds: 3, interval_seconds: 1, retries: 5 }
  depends_on:
    - { project: mysql, kind: required }
  migration: { mode: auto_on_startup }
  data_dirs: [data]
baremetal:
  process:
    manager: tinyvisor
    tinyvisor: { restart_policy: unless-stopped, auto_start: true }
  install: { root: /opt/svc-a, layout: releases, binary_name: svc-a, keep_releases: 3 }
  artifact:
    source: local_path
    local_path: /tmp/svc-a-linux-amd64
`

const externalManifest = `
manifest_version: 1
kind: external
project: mysql
group: infra
service_version: "8.0"
facts:
  ports:
    - { name: mysql, port: 3306, exposure: loopback }
  external: { host: 127.0.0.1 }
`

func TestValidateAggregate_Valid(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "svc-a", validManifest)
	writeManifest(t, dir, "mysql", externalManifest)

	manifests, errs := LoadAggregate([]string{dir}, "")
	if len(errs) != 0 {
		t.Fatalf("load errors: %v", errs)
	}
	if len(manifests) != 2 {
		t.Fatalf("want 2 manifests, got %d", len(manifests))
	}

	rep := ValidateAggregate(manifests, errs)
	if !rep.OK() {
		t.Fatalf("expected valid, got errors: %v", rep.Errors)
	}

	for _, m := range manifests {
		if m.Project != "svc-a" {
			continue
		}
		if got := m.EffectiveTag(); got != "v1.2.0" {
			t.Errorf("EffectiveTag = %q, want v1.2.0", got)
		}
		if got := m.EffectiveAsset(); got != "svc-a-linux-amd64" {
			t.Errorf("EffectiveAsset = %q (硬约定推导)", got)
		}
		if got := m.EffectiveChecksumAsset(); got != "svc-a-linux-amd64.sha256" {
			t.Errorf("EffectiveChecksumAsset = %q", got)
		}
	}
}

func TestValidateAggregate_MissingHealthAndDependency(t *testing.T) {
	dir := t.TempDir()
	noHealth := strings.Replace(validManifest,
		"  health:\n    http: { port_name: http, path: /health, expect_status: 200, timeout_seconds: 3, interval_seconds: 1, retries: 5 }\n",
		"", 1)
	writeManifest(t, dir, "no-health", noHealth)
	// svc-b 依赖 mysql，但 mysql 清单不在扫描范围内。
	writeManifest(t, dir, "svc-b", strings.Replace(validManifest, "project: svc-a", "project: svc-b", 1))

	manifests, errs := LoadAggregate([]string{dir}, "")
	rep := ValidateAggregate(manifests, errs)
	if rep.OK() {
		t.Fatalf("expected errors, got none")
	}
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "facts.health.http 必填") {
		t.Errorf("missing health error, got: %v", rep.Errors)
	}
	if !strings.Contains(joined, `依赖 "mysql" 不存在`) {
		t.Errorf("missing dependency error, got: %v", rep.Errors)
	}
}

func TestValidateAggregate_DuplicateAndPortConflict(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "one", validManifest)
	writeManifest(t, dir, "two", validManifest) // 完全同名
	// 不同名但端口冲突
	other := strings.Replace(validManifest, "project: svc-a", "project: svc-c", 1)
	writeManifest(t, dir, "three", other)

	manifests, errs := LoadAggregate([]string{dir}, "")
	rep := ValidateAggregate(manifests, errs)
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "duplicate project") {
		t.Errorf("missing duplicate error: %v", rep.Errors)
	}
	if !strings.Contains(joined, "port conflict") {
		t.Errorf("missing port conflict error: %v", rep.Errors)
	}
}

func serviceManifest(name string, port int, deps ...string) string {
	var depLines string
	for _, d := range deps {
		depLines += fmt.Sprintf("    - { project: %s, kind: required }\n", d)
	}
	depsBlock := ""
	if depLines != "" {
		depsBlock = "  depends_on:\n" + depLines
	}
	return fmt.Sprintf(`
manifest_version: 1
project: %s
display_name: %s
group: core
service_version: "1.0.0"
facts:
  ports:
    - { name: http, port: %d, exposure: public }
  health:
    http: { port_name: http, path: /health }
%sbaremetal:
  process: { manager: aapanel }
`, name, name, port, depsBlock)
}

func TestValidateAggregate_Cycle(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "a", serviceManifest("x-a", 9001, "x-b"))
	writeManifest(t, dir, "b", serviceManifest("x-b", 9002, "x-a"))

	manifests, errs := LoadAggregate([]string{dir}, "")
	rep := ValidateAggregate(manifests, errs)
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "dependency cycle") {
		t.Errorf("missing cycle error: %v", rep.Errors)
	}
}

func TestValidateAggregate_ContainerFrozen(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "frozen", validManifest+"\ncontainer: { enabled: true }\n")
	manifests, errs := LoadAggregate([]string{dir}, "")
	rep := ValidateAggregate(manifests, errs)
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "container 段当前冻结") {
		t.Errorf("missing frozen-container error: %v", rep.Errors)
	}
}

func TestValidateAggregate_BadEnums(t *testing.T) {
	dir := t.TempDir()
	bad := strings.Replace(validManifest, "manager: tinyvisor", "manager: systemd", 1)
	bad = strings.Replace(bad, "exposure: public", "exposure: anywhere", 1)
	bad = strings.Replace(bad, "group: core", "group: kitchen", 1)
	writeManifest(t, dir, "bad", bad)

	manifests, errs := LoadAggregate([]string{dir}, "")
	rep := ValidateAggregate(manifests, errs)
	joined := strings.Join(rep.Errors, "\n")
	for _, want := range []string{"group 非法", "manager 非法", "exposure 非法"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in errors: %v", want, rep.Errors)
		}
	}
}
