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

// runtime 选择器与 container 段的校验。
func TestValidateRuntimeSelector(t *testing.T) {
	// 单清单装载：去掉对 mysql 的依赖，避免依赖缺失干扰本测试。
	base := strings.Replace(validManifest,
		"  depends_on:\n    - { project: mysql, kind: required }\n", "", 1)
	cases := []struct {
		name    string
		content string
		wantErr string // 为空表示应通过
	}{
		{"默认 baremetal", base, ""},
		{"显式 baremetal", base + "runtime: baremetal\n", ""},
		{"container 段齐备", validContainerManifest, ""},
		{"非法 runtime", base + "runtime: kubernetes\n", "runtime 必须为"},
		{"退役开关开启", base + "container: { enabled: true }\n", "container.enabled 已退役"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := validateSingleManifestYAML(t, c.content)
			if c.wantErr == "" {
				if !rep.OK() {
					t.Fatalf("expected pass, got: %v", rep.Errors)
				}
				return
			}
			found := false
			for _, e := range rep.Errors {
				if strings.Contains(e, c.wantErr) {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected error containing %q, got: %v", c.wantErr, rep.Errors)
			}
		})
	}
}

const validContainerManifest = `
manifest_version: 1
project: svc-a
display_name: Service A
group: core
service_version: "1.2.0"
runtime: container
facts:
  ports:
    - { name: http, port: 2778, exposure: public }
  health:
    http: { port_name: http, path: /health, expect_status: 200, timeout_seconds: 3, interval_seconds: 1, retries: 5 }
baremetal:
  # install.root 对容器项目同样必填：卷的宿主侧根目录。
  install: { root: /opt/svc-a }
container:
  image: { source: build, context: build/docker }
  volumes:
    - { container: /data, host_relative: data }
`

func TestValidateContainerSection(t *testing.T) {
	if rep := validateSingleManifestYAML(t, validContainerManifest); !rep.OK() {
		t.Fatalf("valid container manifest rejected: %v", rep.Errors)
	}

	cases := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"image source 非法", func(s string) string { return strings.Replace(s, "source: build", "source: ftp", 1) }, "image.source 非法"},
		{"build 缺 context", func(s string) string { return strings.Replace(s, "context: build/docker", "context: \"\"", 1) }, "context 必填"},
		{"registry 缺 repo", func(s string) string {
			return strings.Replace(s, "image: { source: build, context: build/docker }", "image: { source: registry }", 1)
		}, "repo 必填"},
		{"卷容器路径非绝对", func(s string) string { return strings.Replace(s, "container: /data", "container: data", 1) }, "绝对路径"},
		{"卷宿主相对路径越界", func(s string) string { return strings.Replace(s, "host_relative: data", "host_relative: ../escape", 1) }, "host_relative"},
		{"container_ports 未声明端口", func(s string) string {
			return s + "  container_ports: { ghost: 9999 }\n"
		}, "container_ports 端口名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := validateSingleManifestYAML(t, c.mutate(validContainerManifest))
			found := false
			for _, e := range rep.Errors {
				if strings.Contains(e, c.wantErr) {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected error containing %q, got: %v", c.wantErr, rep.Errors)
			}
		})
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

// 非法占位符（非 ${secret:<name>}）现在由装载层凭据拦截直接拒绝。
func TestLoadFile_RejectsBadTokenInSmokeBody(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, "svc-a", smokeManifestYAML(`
    - name: s
      path: /x
      body: '{"v":"${env:HOME}"}'
`))
	if _, err := LoadFile(path); err == nil || !strings.Contains(err.Error(), "占位符") {
		t.Fatalf("bad token must be rejected at load time, got: %v", err)
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

func TestValidateAggregate_ContainerEnabledRetired(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "frozen", validManifest+"\ncontainer: { enabled: true }\n")
	manifests, _, errs := LoadAggregate([]string{dir}, "")
	rep := ValidateAggregate(manifests, errs)
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "container.enabled 已退役") {
		t.Errorf("missing retired-flag error: %v", rep.Errors)
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

// ---------- kind=plugin（插件型产物） ----------

// pluginManifestYAML 构造 plugin 段可替换的插件清单（依赖 svc-a 存在）。
func pluginManifestYAML(pluginBlock string) string {
	return `
manifest_version: 1
kind: plugin
project: plug-a
display_name: Plugin A
group: edge
service_version: "1.0.0"
facts:
  depends_on:
    - { project: svc-a, kind: required }
  migration: { mode: none }
` + pluginBlock
}

// pluginDepManifest 是插件测试用的被依赖方：与 validManifest 同源，
// 但去掉了对 mysql 的依赖（插件测试不关心它，避免聚合校验噪音）。
const pluginDepManifest = `
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
  process:
    manager: tinyvisor
  install: { root: /opt/svc-a, layout: releases, binary_name: svc-a, keep_releases: 3 }
  artifact:
    source: local_path
    local_path: /tmp/svc-a-linux-amd64
`

// validatePluginWithDep 装载插件清单 + 其依赖的 svc-a，返回聚合校验报告。
func validatePluginWithDep(t *testing.T, pluginYAML string) *Report {
	t.Helper()
	dir := t.TempDir()
	writeManifest(t, dir, "svc-a", pluginDepManifest)
	writeManifest(t, dir, "plug-a", pluginYAML)
	manifests, _, errs := LoadAggregate([]string{dir}, "")
	if len(errs) != 0 {
		t.Fatalf("load errors: %v", errs)
	}
	return ValidateAggregate(manifests, errs)
}

func TestValidatePlugin_Valid(t *testing.T) {
	rep := validatePluginWithDep(t, pluginManifestYAML(`plugin:
  runtime: velocity-proxy
  plugins_dir: plugins
  artifact: { source: local_path, local_path: target/plug-a.jar }
`))
	if !rep.OK() {
		t.Fatalf("valid plugin manifest rejected: %v", rep.Errors)
	}
}

func TestValidatePlugin_MissingSection(t *testing.T) {
	rep := validatePluginWithDep(t, pluginManifestYAML(""))
	if rep.OK() {
		t.Fatal("plugin manifest without plugin section must fail")
	}
	if !strings.Contains(strings.Join(rep.Errors, "\n"), "plugin 段") {
		t.Fatalf("error must name the missing plugin section: %v", rep.Errors)
	}
}

func TestValidatePlugin_MissingFieldsFailLoudly(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  string
	}{
		{"missing runtime", `plugin:
  plugins_dir: plugins
  artifact: { source: local_path, local_path: target/plug-a.jar }
`, "plugin.runtime 必填"},
		{"missing plugins_dir", `plugin:
  runtime: velocity-proxy
  artifact: { source: local_path, local_path: target/plug-a.jar }
`, "plugin.plugins_dir 必填"},
		{"missing artifact local_path", `plugin:
  runtime: velocity-proxy
  plugins_dir: plugins
  artifact: { source: local_path }
`, "plugin.artifact.local_path 必填"},
		{"bad artifact source", `plugin:
  runtime: velocity-proxy
  plugins_dir: plugins
  artifact: { source: scp, local_path: target/plug-a.jar }
`, "plugin.artifact.source 非法"},
		{"plugins_dir escapes", `plugin:
  runtime: velocity-proxy
  plugins_dir: ../etc
  artifact: { source: local_path, local_path: target/plug-a.jar }
`, "plugin.plugins_dir 非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := validatePluginWithDep(t, pluginManifestYAML(tc.block))
			if rep.OK() {
				t.Fatalf("case %s must fail", tc.name)
			}
			if !strings.Contains(strings.Join(rep.Errors, "\n"), tc.want) {
				t.Fatalf("missing %q in errors: %v", tc.want, rep.Errors)
			}
		})
	}
}

func TestValidatePlugin_RejectsProcessAndContainerRuntime(t *testing.T) {
	rep := validatePluginWithDep(t, pluginManifestYAML(`runtime: container
plugin:
  runtime: velocity-proxy
  plugins_dir: plugins
  artifact: { source: local_path, local_path: target/plug-a.jar }
baremetal:
  process: { manager: tinyvisor }
`))
	joined := strings.Join(rep.Errors, "\n")
	for _, want := range []string{"不应携带 baremetal.process.manager", "runtime 不能为 container"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in errors: %v", want, rep.Errors)
		}
	}
}

func TestValidatePlugin_ServiceWithPluginSectionRejected(t *testing.T) {
	rep := validateSingleManifestYAML(t, validManifest+`plugin:
  runtime: velocity-proxy
  plugins_dir: plugins
  artifact: { source: local_path, local_path: target/x.jar }
`)
	if rep.OK() {
		t.Fatal("service manifest with plugin section must fail")
	}
	if !strings.Contains(strings.Join(rep.Errors, "\n"), "仅 kind=plugin 清单可声明 plugin 段") {
		t.Fatalf("unexpected errors: %v", rep.Errors)
	}
}

func TestValidatePlugin_TargetProjectMustExist(t *testing.T) {
	rep := validatePluginWithDep(t, pluginManifestYAML(`plugin:
  runtime: velocity-proxy
  target_project: no-such-project
  plugins_dir: plugins
  artifact: { source: local_path, local_path: target/plug-a.jar }
`))
	if rep.OK() {
		t.Fatal("plugin.target_project pointing at nothing must fail")
	}
	if !strings.Contains(strings.Join(rep.Errors, "\n"), "plugin.target_project \"no-such-project\" 不存在") {
		t.Fatalf("unexpected errors: %v", rep.Errors)
	}
}

func TestValidatePlugin_TargetProjectValid(t *testing.T) {
	rep := validatePluginWithDep(t, pluginManifestYAML(`plugin:
  runtime: gameserver-runtime
  target_project: svc-a
  plugins_dir: plugins
  artifact: { source: local_path, local_path: target/plug-a.jar }
`))
	if !rep.OK() {
		t.Fatalf("plugin.target_project at existing project must pass: %v", rep.Errors)
	}
}
