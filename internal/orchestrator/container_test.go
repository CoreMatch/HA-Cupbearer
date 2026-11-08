package orchestrator

// 容器流水线测试：fake 驱动 + 真实健康端点。
// 验收口径（与裸金属的 /proc/<pid>/exe 实证等价）：
//   - 幂等重跑不得重建容器（容器 ID 不变）；
//   - 回滚必须给出切换前后的容器 ID 与镜像摘要对照。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/docker"
	"HA-Services-Deployer/internal/models"
)

// ---------- fake 驱动 ----------

type fakeDocker struct {
	mu            sync.Mutex
	infoErr       error
	buildErr      error
	createErr     error
	images        map[string]string            // ref -> imageID
	containers    map[string]*docker.ContainerState
	creates       []docker.CreateOptions
	removes       []string
	removedImages []string
	listManaged   []docker.ManagedContainer
	imageTags     map[string][]string // repo -> 标签（新→旧）
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		images:     map[string]string{},
		containers: map[string]*docker.ContainerState{},
		imageTags:  map[string][]string{},
	}
}

func (f *fakeDocker) Info(ctx context.Context) error { return f.infoErr }

func (f *fakeDocker) ImageExists(ctx context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.images[ref]
	return ok, nil
}

func (f *fakeDocker) ImageID(ctx context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.images[ref]
	if !ok {
		return "", docker.ErrNotFound
	}
	return id, nil
}

func (f *fakeDocker) addImage(tag string) {
	f.images[tag] = "sha256:img-" + tag
	repo, version, _ := strings.Cut(tag, ":")
	f.imageTags[repo] = append([]string{version}, f.imageTags[repo]...)
}

func (f *fakeDocker) Build(ctx context.Context, tag, contextDir, dockerfile string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buildErr != nil {
		return f.buildErr
	}
	f.addImage(tag)
	return nil
}

func (f *fakeDocker) LoadAndTag(ctx context.Context, tarball, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addImage(tag)
	return nil
}

func (f *fakeDocker) Create(ctx context.Context, opts docker.CreateOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	id := fmt.Sprintf("ctr%04d", len(f.creates)+1)
	f.creates = append(f.creates, opts)
	f.containers[opts.Name] = &docker.ContainerState{
		ID: id, Name: opts.Name, Image: opts.Image,
		ImageID: f.images[opts.Image], State: "created",
	}
	return id, nil
}

func (f *fakeDocker) Start(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.containers[name]; ok {
		c.State = "running"
		c.Running = true
	}
	return nil
}

func (f *fakeDocker) Remove(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, name)
	f.removes = append(f.removes, name)
	return nil
}

func (f *fakeDocker) Inspect(ctx context.Context, name string) (*docker.ContainerState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return nil, docker.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (f *fakeDocker) ListManaged(ctx context.Context) ([]docker.ManagedContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]docker.ManagedContainer(nil), f.listManaged...), nil
}

func (f *fakeDocker) ImageTags(ctx context.Context, repo string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.imageTags[repo]...), nil
}

func (f *fakeDocker) RemoveImage(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.images, ref)
	f.removedImages = append(f.removedImages, ref)
	return nil
}

// ---------- 测试环境 ----------

type containerEnv struct {
	orch     *Orchestrator
	fake     *fakeDocker
	cfg      *models.Config
	scanRoot string
	root     string
	srv      *httptest.Server
	healthy  bool
	mu       sync.Mutex
}

func (e *containerEnv) setHealthy(v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.healthy = v
}

func (e *containerEnv) isHealthy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.healthy
}

func newContainerEnv(t *testing.T) *containerEnv {
	t.Helper()
	base := t.TempDir()
	scanRoot := filepath.Join(base, "scan")
	repoDir := filepath.Join(scanRoot, "svc-a")
	if err := os.MkdirAll(filepath.Join(repoDir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "build", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	env := &containerEnv{
		scanRoot: scanRoot,
		root:     filepath.Join(base, "install", "svc-a"),
		healthy:  true,
	}
	env.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if env.isHealthy() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(env.srv.Close)
	srvPort := env.srv.Listener.Addr().(*net.TCPAddr).Port

	manifestYAML := fmt.Sprintf(`
manifest_version: 1
project: svc-a
display_name: Service A
group: core
service_version: "1.0.0"
runtime: container
facts:
  ports:
    - { name: http, port: %d, exposure: loopback }
  health:
    http: { port_name: http, path: /healthz, expect_status: 200, timeout_seconds: 2, interval_seconds: 1, retries: 1 }
baremetal:
  install: { root: %s }
container:
  image: { source: build, context: build }
  volumes:
    - { container: /data, host_relative: data }
`, freePort(t), env.root)
	if err := os.WriteFile(filepath.Join(repoDir, "deploy-manifest.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &models.Config{}
	cfg.Orchestration.Enabled = true
	cfg.Orchestration.ScanRoots = []string{scanRoot}
	cfg.Orchestration.StateDir = filepath.Join(base, "state")
	cfg.Orchestration.ProbeHost = "127.0.0.1"
	cfg.Orchestration.Overrides = map[string]models.ProjectOverride{
		"svc-a": {HealthPort: srvPort},
	}
	env.cfg = cfg
	env.orch = New(cfg, &config.Credentials{})
	env.fake = newFakeDocker()
	env.orch.docker = env.fake
	// 预置一个运行中的旧实例（升级场景）：健康端点占着生效端口，
	// 端口预检按"被项目自己占用"放行，与真实部署一致。
	env.fake.containers["svc-a"] = &docker.ContainerState{
		Name: "svc-a", Image: "svc-a:0.0.1", ImageID: "sha256:img-seed",
		State: "running", Running: true, ID: "seed0001",
	}

	if _, rep := env.orch.Load(); !rep.OK() {
		t.Fatalf("aggregate validation failed: %v", rep.Errors)
	}
	return env
}

func (e *containerEnv) runDeploy(t *testing.T) *Run {
	t.Helper()
	snap, err := e.orch.StartRun(RunOptions{Projects: []string{"svc-a"}})
	if err != nil {
		var perr *ErrPreflightBlocked
		if errors.As(err, &perr) {
			t.Fatalf("run refused by preflight: %+v", perr.Report.Blockers)
		}
		t.Fatalf("run refused: %v", err)
	}
	return waitForRunFile(t, e.orch, snap.ID)
}

// ---------- 测试 ----------

// 部署成功 + 幂等重跑不重建容器（容器 ID 不变）。
func TestContainerDeployAndIdempotentNoRecreate(t *testing.T) {
	env := newContainerEnv(t)

	run := env.runDeploy(t)
	res := run.Results["svc-a"]
	if res.Outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s, detail = %s, steps = %+v", res.Outcome, res.Detail, res.Steps)
	}
	if len(env.fake.creates) != 1 {
		t.Fatalf("first deploy must create exactly one container, got %d", len(env.fake.creates))
	}
	firstID := env.fake.containers["svc-a"].ID
	// 证据链：运行记录里必须有容器 ID 与镜像摘要。
	if !strings.Contains(res.Detail, firstID) || !strings.Contains(res.Detail, "image id") {
		t.Errorf("deploy detail must carry container ID and image digest: %s", res.Detail)
	}
	// 卷宿主目录必须已创建。
	if _, err := os.Stat(filepath.Join(env.root, "data")); err != nil {
		t.Errorf("volume host dir must be created: %v", err)
	}

	// 幂等重跑：容器不得重建（ID 不变，create 次数不增）。
	run2 := env.runDeploy(t)
	res2 := run2.Results["svc-a"]
	if res2.Outcome != OutcomeUpToDate {
		t.Fatalf("re-run outcome = %s, want up-to-date (detail: %s)", res2.Outcome, res2.Detail)
	}
	if len(env.fake.creates) != 1 {
		t.Fatalf("idempotent re-run must not recreate the container, creates=%d", len(env.fake.creates))
	}
	if env.fake.containers["svc-a"].ID != firstID {
		t.Fatalf("container ID changed on idempotent re-run: %s -> %s", firstID, env.fake.containers["svc-a"].ID)
	}
	if !strings.Contains(res2.Detail, firstID) {
		t.Errorf("up-to-date detail must name the untouched container: %s", res2.Detail)
	}
}

// 健康判败 → 自动回滚到切换前的镜像标签；证据为前后容器 ID 与镜像摘要对照。
func TestContainerDeployFailureAutoRollback(t *testing.T) {
	env := newContainerEnv(t)

	run := env.runDeploy(t)
	if run.Results["svc-a"].Outcome != OutcomeDeployed {
		t.Fatalf("v1 deploy failed: %+v", run.Results["svc-a"])
	}
	v1Container := env.fake.containers["svc-a"].ID
	v1ImageID := env.fake.images["svc-a:1.0.0"]

	// 切到坏版本 2.0.0：健康端点转 503。
	env.cfg.Orchestration.Overrides["svc-a"] = models.ProjectOverride{
		HealthPort:     env.cfg.Orchestration.Overrides["svc-a"].HealthPort,
		ServiceVersion: "2.0.0",
	}
	env.setHealthy(false)

	run2 := env.runDeploy(t)
	res2 := run2.Results["svc-a"]
	if res2.Outcome != OutcomeFailed {
		t.Fatalf("bad version must fail, got %s (%s)", res2.Outcome, res2.Detail)
	}
	if !strings.Contains(res2.Detail, "auto-rolled back to svc-a:1.0.0") {
		t.Fatalf("must auto-roll back to previous image tag: %s", res2.Detail)
	}

	// 对照证据：切换前（inspect-before）、坏版本（create）、回滚后（auto-rollback）
	// 三段都要有容器 ID 与镜像信息。
	var inspectBefore, rollbackStep *StepRecord
	for i := range res2.Steps {
		switch res2.Steps[i].Name {
		case "inspect-before":
			inspectBefore = &res2.Steps[i]
		case "auto-rollback":
			rollbackStep = &res2.Steps[i]
		}
	}
	if inspectBefore == nil || !strings.Contains(inspectBefore.Detail, v1Container) || !strings.Contains(inspectBefore.Detail, shortID(v1ImageID)) {
		t.Fatalf("inspect-before must record pre-switch container ID + image digest: %+v", inspectBefore)
	}
	if rollbackStep == nil || !strings.Contains(rollbackStep.Detail, "svc-a:1.0.0") {
		t.Fatalf("auto-rollback step must record the restored tag: %+v", rollbackStep)
	}

	// 回滚后运行的必须是 v1 镜像，且容器是新实例（旧的已被替换）。
	cur := env.fake.containers["svc-a"]
	if cur.Image != "svc-a:1.0.0" {
		t.Fatalf("after rollback container runs %s, want svc-a:1.0.0", cur.Image)
	}
	if cur.ID == v1Container {
		t.Fatal("rollback must recreate the container (new ID expected)")
	}
	if len(env.fake.creates) != 3 { // v1 + 坏 v2 + 回滚
		t.Fatalf("expected 3 creates (v1, bad v2, rollback), got %d", len(env.fake.creates))
	}
}

// 孤儿容器（带托管标签但项目已不在清单）：默认只报不删。
func TestContainerOrphanReportOnly(t *testing.T) {
	env := newContainerEnv(t)
	env.fake.listManaged = []docker.ManagedContainer{
		{Name: "svc-a", Project: "svc-a"},
		{Name: "ghost-ctr", Project: "ghost-proj"},
	}

	run := env.runDeploy(t)
	if run.Results["svc-a"].Outcome != OutcomeDeployed {
		t.Fatalf("deploy failed: %+v", run.Results["svc-a"])
	}
	found := false
	for _, n := range run.Notes {
		if strings.Contains(n, "ghost-ctr") && strings.Contains(n, "report-only") {
			found = true
		}
	}
	if !found {
		t.Fatalf("orphan must be reported in run notes, got: %v", run.Notes)
	}
	for _, removed := range env.fake.removes {
		if removed == "ghost-ctr" {
			t.Fatal("report-only policy must not remove the orphan")
		}
	}
}

// 镜像 prune：保留最近 N 个标签与生效标签，删更旧的。
func TestContainerPruneKeepsRecentAndActive(t *testing.T) {
	env := newContainerEnv(t)
	// 预置 6 个旧版本标签；addImage 头插，按旧→新插入后序列为新→旧。
	for _, v := range []string{"0.4.0", "0.5.0", "0.6.0", "0.7.0", "0.8.0", "0.9.0"} {
		env.fake.addImage("svc-a:" + v)
	}

	run := env.runDeploy(t) // 构建并激活 1.0.0
	if run.Results["svc-a"].Outcome != OutcomeDeployed {
		t.Fatalf("deploy failed: %+v", run.Results["svc-a"])
	}
	// 7 个标签，保留 5 个（含生效的 1.0.0），删最旧的 2 个。
	want := map[string]bool{"svc-a:0.5.0": true, "svc-a:0.4.0": true}
	if len(env.fake.removedImages) != len(want) {
		t.Fatalf("removed = %v, want %v", env.fake.removedImages, want)
	}
	for _, ref := range env.fake.removedImages {
		if !want[ref] {
			t.Fatalf("unexpectedly removed %s", ref)
		}
	}
	if _, ok := env.fake.images["svc-a:1.0.0"]; !ok {
		t.Fatal("active image tag must never be pruned")
	}
}
