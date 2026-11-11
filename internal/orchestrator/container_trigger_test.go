package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/models"
)

// 一键触发（容器路径默认交互）的行为验证。触发只负责：按清单已发布
// 镜像引用生成 override、调用 compose、如实回报；逐容器管理不在其内。

// triggerEnv 是触发测试的最小环境：若干清单 + 平台 compose 模板 + fake docker。
type triggerEnv struct {
	orch     *Orchestrator
	fake     *fakeDocker
	base     string
	template string
}

func newTriggerEnv(t *testing.T, manifests map[string]string, templateServices []string) *triggerEnv {
	t.Helper()
	base := t.TempDir()
	scanRoot := filepath.Join(base, "scan")
	for repo, body := range manifests {
		dir := filepath.Join(scanRoot, repo)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "deploy-manifest.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	template := filepath.Join(base, "docker-compose.yml")
	// 模板只需在场（触发前按路径检查存在性）；服务名集合以 compose
	// 自身为权威，测试里经 fake 的 ComposeServices 给出。
	if err := os.WriteFile(template, []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &models.Config{}
	cfg.Orchestration.Enabled = true
	cfg.Orchestration.ScanRoots = []string{scanRoot}
	cfg.Orchestration.StateDir = filepath.Join(base, "state")
	cfg.Orchestration.Container.ComposeFile = template

	orch := New(cfg, &config.Credentials{})
	fake := newFakeDocker()
	fake.composeServices = templateServices
	orch.docker = fake
	return &triggerEnv{orch: orch, fake: fake, base: base, template: template}
}

func registryManifest(project, version string, port int, ref string) string {
	return `
manifest_version: 1
project: ` + project + `
display_name: ` + project + `
group: core
service_version: "` + version + `"
facts:
  ports:
    - { name: http, port: ` + fmt.Sprintf("%d", port) + `, exposure: loopback }
  health:
    http: { port_name: http, path: /health, expect_status: 200, timeout_seconds: 2, interval_seconds: 1, retries: 1 }
baremetal:
  process: { manager: manual }
  install: { root: /opt/` + project + ` }
container:
  enabled: false
  image:
    ` + ref + `
`
}

func TestTriggerContainerStack_PinsPublishedRefsAndInvokesCompose(t *testing.T) {
	env := newTriggerEnv(t, map[string]string{
		// digest 优先：声明了 digest 的按 repo@sha256 钉。
		"svc-a": registryManifest("svc-a", "1.0.0", 2781,
			"source: registry\n    repo: limeight/ha-svc-a\n    tag: \"1.0.0-abc1234\"\n    digest: \"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\""),
		// 仅 tag：退回 repo:tag。
		"svc-b": registryManifest("svc-b", "2.0.0", 2782,
			"source: registry\n    repo: limeight/ha-svc-b\n    tag: \"2.0.0-bbb2222\""),
		// build 来源不是已发布引用：不进触发范围。
		"svc-c": `
manifest_version: 1
project: svc-c
display_name: svc-c
group: core
service_version: "3.0.0"
facts:
  ports:
    - { name: http, port: 2783, exposure: loopback }
  health:
    http: { port_name: http, path: /health, expect_status: 200, timeout_seconds: 2, interval_seconds: 1, retries: 1 }
baremetal:
  process: { manager: manual }
  install: { root: /opt/svc-c }
container:
  enabled: false
  image: { source: build, context: build }
`,
		// 已发布引用但模板无同名服务：记备注、不触发。
		"svc-e": registryManifest("svc-e", "5.0.0", 2785,
			"source: registry\n    repo: limeight/ha-svc-e\n    tag: \"5.0.0-eee5555\""),
	}, []string{"svc-a", "svc-b", "svc-d"})

	env.fake.composePsOut = "NAME   STATUS\nsvc-a  running\nsvc-b  running"

	snap, err := env.orch.TriggerContainerStack()
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if snap.Type != "container-trigger" {
		t.Fatalf("run type = %q, want container-trigger", snap.Type)
	}
	run := waitForRunFile(t, env.orch, snap.ID)
	if run.State != "done" {
		t.Fatalf("run state = %q, want done; results=%+v", run.State, run.Results)
	}

	// digest 优先钉选；仅 tag 的退回 tag。
	if res := run.Results["svc-a"]; res == nil || res.Outcome != OutcomeTriggered ||
		!strings.Contains(res.Detail, "limeight/ha-svc-a@sha256:aaaa") {
		t.Fatalf("svc-a result = %+v, want triggered with digest-pinned ref", res)
	}
	if res := run.Results["svc-b"]; res == nil || !strings.Contains(res.Detail, "limeight/ha-svc-b:2.0.0-bbb2222") {
		t.Fatalf("svc-b result = %+v, want triggered with tag ref", res)
	}
	// build 来源不进触发范围。
	if _, ok := run.Results["svc-c"]; ok {
		t.Fatal("svc-c (build source) must not be part of the trigger")
	}
	// compose-stack 汇总条目与步骤。
	stack := run.Results["compose-stack"]
	if stack == nil || stack.Outcome != OutcomeTriggered {
		t.Fatalf("compose-stack result = %+v, want triggered", stack)
	}
	var stepNames []string
	for _, s := range stack.Steps {
		if !s.OK {
			t.Fatalf("step %s failed: %s", s.Name, s.Detail)
		}
		stepNames = append(stepNames, s.Name)
	}
	if strings.Join(stepNames, ",") != "write-override,compose-up,compose-ps" {
		t.Fatalf("steps = %v", stepNames)
	}

	// compose 调用 = [模板, override]，override 内容钉住两个已发布引用。
	if len(env.fake.composeUpCalls) != 1 {
		t.Fatalf("compose up calls = %d, want 1", len(env.fake.composeUpCalls))
	}
	files := env.fake.composeUpCalls[0]
	if len(files) != 2 || files[0] != env.template {
		t.Fatalf("compose files = %v, want [%s, override]", files, env.template)
	}
	data, err := os.ReadFile(files[1])
	if err != nil {
		t.Fatal(err)
	}
	override := string(data)
	for _, want := range []string{
		"svc-a:\n    image: limeight/ha-svc-a@sha256:aaaa",
		"svc-b:\n    image: limeight/ha-svc-b:2.0.0-bbb2222",
	} {
		if !strings.Contains(override, want) {
			t.Fatalf("override missing %q:\n%s", want, override)
		}
	}
	if strings.Contains(override, "svc-c") || strings.Contains(override, "svc-e") {
		t.Fatalf("override must not pin svc-c/svc-e:\n%s", override)
	}

	// 备注：模板无同名服务（svc-e）与模板服务无已发布引用（svc-d）都要可见。
	notes := strings.Join(run.Notes, "\n")
	if !strings.Contains(notes, "svc-e") || !strings.Contains(notes, "svc-d") {
		t.Fatalf("notes must surface svc-e (no template service) and svc-d (no published ref):\n%s", notes)
	}
}

func TestTriggerContainerStack_NoPublishedRefsRefused(t *testing.T) {
	env := newTriggerEnv(t, map[string]string{
		"svc-c": `
manifest_version: 1
project: svc-c
display_name: svc-c
group: core
service_version: "3.0.0"
facts:
  ports:
    - { name: http, port: 2783, exposure: loopback }
  health:
    http: { port_name: http, path: /health, expect_status: 200, timeout_seconds: 2, interval_seconds: 1, retries: 1 }
baremetal:
  process: { manager: manual }
  install: { root: /opt/svc-c }
container:
  enabled: false
  image: { source: build, context: build }
`,
	}, []string{"svc-c"})
	if _, err := env.orch.TriggerContainerStack(); err == nil {
		t.Fatal("trigger must be refused when no published registry reference exists")
	}
}

func TestTriggerContainerStack_ComposeFailureReportedLoud(t *testing.T) {
	env := newTriggerEnv(t, map[string]string{
		"svc-a": registryManifest("svc-a", "1.0.0", 2781,
			"source: registry\n    repo: limeight/ha-svc-a\n    tag: \"1.0.0-abc1234\""),
	}, []string{"svc-a"})
	env.fake.composeUpErr = os.ErrPermission
	env.fake.composeUpOut = "permission denied on docker.sock"

	snap, err := env.orch.TriggerContainerStack()
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	run := waitForRunFile(t, env.orch, snap.ID)
	if run.State != "failed" {
		t.Fatalf("run state = %q, want failed", run.State)
	}
	stack := run.Results["compose-stack"]
	if stack == nil || stack.Outcome != OutcomeFailed {
		t.Fatalf("compose-stack = %+v, want failed", stack)
	}
}

func TestTriggerContainerStack_MissingTemplateRefused(t *testing.T) {
	env := newTriggerEnv(t, map[string]string{
		"svc-a": registryManifest("svc-a", "1.0.0", 2781,
			"source: registry\n    repo: limeight/ha-svc-a\n    tag: \"1.0.0-abc1234\""),
	}, []string{"svc-a"})
	env.orch.cfg.Orchestration.Container.ComposeFile = filepath.Join(env.base, "no-such-compose.yml")
	if _, err := env.orch.TriggerContainerStack(); err == nil || !strings.Contains(err.Error(), "compose") {
		t.Fatalf("trigger must fail loud on missing template, got %v", err)
	}
}

func TestTriggerContainerStack_DockerUnreachableRefused(t *testing.T) {
	env := newTriggerEnv(t, map[string]string{
		"svc-a": registryManifest("svc-a", "1.0.0", 2781,
			"source: registry\n    repo: limeight/ha-svc-a\n    tag: \"1.0.0-abc1234\""),
	}, []string{"svc-a"})
	env.fake.infoErr = os.ErrNotExist
	if _, err := env.orch.TriggerContainerStack(); err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("trigger must fail loud when docker daemon unreachable, got %v", err)
	}
}
