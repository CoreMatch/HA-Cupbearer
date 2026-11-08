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

// smokeManifestYAML 构造带 facts.smoke 段的清单（smokeBlock 为列表项缩进文本）。
func smokeManifestYAML(smokeBlock string) string {
	return `
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
  smoke:
` + smokeBlock + `
baremetal:
  process: { manager: tinyvisor }
  install: { root: /opt/svc-a, layout: releases, binary_name: svc-a, keep_releases: 3 }
  artifact: { source: local_path, local_path: /tmp/svc-a-linux-amd64 }
`
}

func validateSingleManifestYAML(t *testing.T, content string) *Report {
	t.Helper()
	dir := t.TempDir()
	writeManifest(t, dir, "svc-a", content)
	manifests, _, errs := LoadAggregate([]string{dir}, "")
	if len(errs) != 0 {
		t.Fatalf("load errors: %v", errs)
	}
	return ValidateAggregate(manifests, errs)
}

func TestValidateSharedData(t *testing.T) {
	base := `
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
baremetal:
  process: { manager: tinyvisor }
  install: { root: /opt/svc-a, layout: releases, binary_name: svc-a, keep_releases: 3, shared_data: %s }
  artifact: { source: local_path, local_path: /tmp/svc-a-linux-amd64 }
`
	cases := []struct {
		value   string
		wantErr string // 为空表示应通过
	}{
		{`"shared"`, ""},
		{`"data/shared"`, ""},
		{`"/abs/shared"`, "必须为相对部署根"},
		{`"../escape"`, "非法"},
		{`"a/../b"`, "非法"},
	}
	for _, c := range cases {
		rep := validateSingleManifestYAML(t, fmt.Sprintf(base, c.value))
		if c.wantErr == "" {
			if !rep.OK() {
				t.Errorf("shared_data=%s should pass, got: %v", c.value, rep.Errors)
			}
			continue
		}
		found := false
		for _, e := range rep.Errors {
			if strings.Contains(e, "shared_data") && strings.Contains(e, c.wantErr) {
				found = true
			}
		}
		if !found {
			t.Errorf("shared_data=%s should be rejected with %q, got: %v", c.value, c.wantErr, rep.Errors)
		}
	}
}

func TestValidateSmoke_Valid(t *testing.T) {
	rep := validateSingleManifestYAML(t, smokeManifestYAML(`
    - name: token-issue
      method: POST
      path: /oauth/token
      content_type: application/json
      body: '{"client_id":"${secret:cid}","client_secret":"${secret:csecret}"}'
      expect_status: 200
      assert: { json_field: access_token, json_not_empty: access_token }
    - name: profile-miss
      path: /profile/00000000-0000-0000-0000-000000000000
      expect_status: 404
      assert: { contains: NotFound }
      timeout_seconds: 3
    - name: with-header
      path: /internal
      headers: { X-Internal-Key: "${secret:internal-key}" }
`))
	if !rep.OK() {
		t.Fatalf("expected valid smoke section, got: %v", rep.Errors)
	}
}

func TestValidateSmoke_Rejections(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  string
	}{
		{"bad method", `
    - { name: s, method: PATCH, path: /x }
`, "method 非法"},
		{"path without slash", `
    - { name: s, path: x }
`, "path 必须以 / 开头"},
		{"undeclared port_name", `
    - { name: s, path: /x, port_name: admin }
`, "port_name"},
		{"expect_status out of range", `
    - { name: s, path: /x, expect_status: 999 }
`, "expect_status 越界"},
		{"partial secret in header", `
    - name: s
      path: /x
      headers: { X-Key: "prefix-${secret:k}" }
`, "不是完整的"},
		{"bad token in body", `
    - name: s
      path: /x
      body: '{"v":"${env:HOME}"}'
`, "非法占位符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := validateSingleManifestYAML(t, smokeManifestYAML(tc.block))
			if rep.OK() {
				t.Fatal("expected validation error")
			}
			found := false
			for _, e := range rep.Errors {
				if strings.Contains(e, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("errors %v must contain %q", rep.Errors, tc.want)
			}
		})
	}
}

func TestValidateAggregate_Valid(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "svc-a", validManifest)
	writeManifest(t, dir, "mysql", externalManifest)

	manifests, _, errs := LoadAggregate([]string{dir}, "")
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

	manifests, _, errs := LoadAggregate([]string{dir}, "")
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

func TestValidateAggregate_PortConflict(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "one", validManifest)
	// 不同名但端口冲突
	other := strings.Replace(validManifest, "project: svc-a", "project: svc-c", 1)
	writeManifest(t, dir, "three", other)

	manifests, _, errs := LoadAggregate([]string{dir}, "")
	rep := ValidateAggregate(manifests, errs)
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "port conflict") {
		t.Errorf("missing port conflict error: %v", rep.Errors)
	}
}

// 重名不再在聚合校验层报错：装载层已按确定性规则取舍
// （见 load_test.go），校验层只见得到去重后的清单。
func TestValidateAggregate_DeduplicatedInputPasses(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "one", validManifest)
	writeManifest(t, dir, "two", validManifest) // 完全同名
	writeManifest(t, dir, "mysql", externalManifest)

	manifests, dups, errs := LoadAggregate([]string{dir}, "")
	if len(dups) != 1 {
		t.Fatalf("want 1 duplicate group, got %d", len(dups))
	}
	rep := ValidateAggregate(manifests, errs)
	if !rep.OK() {
		t.Fatalf("deduplicated input must validate, got: %v", rep.Errors)
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

	manifests, _, errs := LoadAggregate([]string{dir}, "")
	rep := ValidateAggregate(manifests, errs)
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "dependency cycle") {
		t.Errorf("missing cycle error: %v", rep.Errors)
	}
}

func TestValidateAggregate_ContainerFrozen(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "frozen", validManifest+"\ncontainer: { enabled: true }\n")
	manifests, _, errs := LoadAggregate([]string{dir}, "")
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

	manifests, _, errs := LoadAggregate([]string{dir}, "")
	rep := ValidateAggregate(manifests, errs)
	joined := strings.Join(rep.Errors, "\n")
	for _, want := range []string{"group 非法", "manager 非法", "exposure 非法"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in errors: %v", want, rep.Errors)
		}
	}
}
