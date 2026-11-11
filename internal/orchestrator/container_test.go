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

	"HA-Cupbearer/internal/config"
	"HA-Cupbearer/internal/docker"
	"HA-Cupbearer/internal/models"
)

// ---------- fake 驱动 ----------

type fakeDocker struct {
	mu            sync.Mutex
	infoErr       error
	buildErr      error
	createErr     error
	pullErr       error
	images        map[string]string // ref -> imageID
	knownDigests  map[string]bool   // 仍存在的镜像摘要（可脱离标签独立存在）
	containers    map[string]*docker.ContainerState
	creates       []docker.CreateOptions
	removes       []string
	removedImages []string
	listManaged   []docker.ManagedContainer
	imageTags     map[string][]string // repo -> 标签（新→旧）
	pulledRefs    []string            // Pull 调用记录（按序）
	repoDigests   map[string][]string // ref -> RepoDigests（可预置以模拟漂移）
	networks      map[string]bool     // 具名网络存在性（NetworkExists）

	composeUpErr   error
	composeUpOut   string
	composePsOut   string
	composeUpCalls [][]string // 每次 ComposeUp 的文件序列

	composeServices    []string // 模板声明的服务名（ComposeServices 返回）
	composeServicesErr error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		images:       map[string]string{},
		knownDigests: map[string]bool{},
		containers:   map[string]*docker.ContainerState{},
		imageTags:    map[string][]string{},
		repoDigests:  map[string][]string{},
	}
}

func (f *fakeDocker) Info(ctx context.Context) error { return f.infoErr }

func (f *fakeDocker) ImageExists(ctx context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(ref, "sha256:") {
		return f.knownDigests[ref], nil
	}
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
	id := "sha256:img-" + tag
	f.images[tag] = id
	f.knownDigests[id] = true
	repo, version, _ := strings.Cut(tag, ":")
	f.imageTags[repo] = append([]string{version}, f.imageTags[repo]...)
}

// retag 把标签移到另一份内容上（模拟"同版本重打标签"——标签撒谎）。
func (f *fakeDocker) retag(tag, imageID string) {
	f.images[tag] = imageID
	f.knownDigests[imageID] = true
}

// forgetImage 模拟镜像被删除：摘要不再存在（连带摘除指向它的标签）。
func (f *fakeDocker) forgetImage(imageID string) {
	delete(f.knownDigests, imageID)
	for t, id := range f.images {
		if id == imageID {
			delete(f.images, t)
		}
	}
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

func (f *fakeDocker) Pull(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullErr != nil {
		return f.pullErr
	}
	f.pulledRefs = append(f.pulledRefs, ref)
	id := "sha256:img-" + ref
	f.images[ref] = id
	f.knownDigests[id] = true
	// 未预置时默认"拉到的就是声明的"：RepoDigests 含引用本身。
	if _, ok := f.repoDigests[ref]; !ok {
		f.repoDigests[ref] = []string{ref}
	}
	return nil
}

func (f *fakeDocker) RepoDigests(ctx context.Context, ref string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.repoDigests[ref]...), nil
}

func (f *fakeDocker) Tag(ctx context.Context, src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.images[src]
	if !ok {
		return docker.ErrNotFound
	}
	f.images[dst] = id
	repo, version, _ := strings.Cut(dst, ":")
	f.imageTags[repo] = append([]string{version}, f.imageTags[repo]...)
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
	imageID := f.images[opts.Image]
	if imageID == "" && strings.HasPrefix(opts.Image, "sha256:") {
		imageID = opts.Image // 按摘要定位创建（钉死回滚）
	}
	f.containers[opts.Name] = &docker.ContainerState{
		ID: id, Name: opts.Name, Image: opts.Image,
		ImageID: imageID, State: "created",
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
	if id, ok := f.images[ref]; ok {
		delete(f.images, ref)
		stillReferenced := false
		for _, other := range f.images {
			if other == id {
				stillReferenced = true
				break
			}
		}
		if !stillReferenced {
			delete(f.knownDigests, id)
		}
	}
	f.removedImages = append(f.removedImages, ref)
	return nil
}

func (f *fakeDocker) ComposeUp(ctx context.Context, files []string) (string, error) {
	f.mu.Lock()
	f.composeUpCalls = append(f.composeUpCalls, append([]string(nil), files...))
	f.mu.Unlock()
	return f.composeUpOut, f.composeUpErr
}

func (f *fakeDocker) ComposePs(ctx context.Context, files []string) (string, error) {
	return f.composePsOut, nil
}

func (f *fakeDocker) NetworkExists(ctx context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.networks[name], nil
}

func (f *fakeDocker) ComposeServices(ctx context.Context, files []string) ([]string, error) {
	return append([]string(nil), f.composeServices...), f.composeServicesErr
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
	return newContainerEnvWithImage(t, `container:
  image: { source: build, context: build }
  volumes:
    - { container: /data, host_relative: data }
`)
}

// newContainerEnvWithImage 与 newContainerEnv 相同，但 container 段可替换
//（registry 来源等场景用）。
func newContainerEnvWithImage(t *testing.T, containerSection string) *containerEnv {
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
%s
`, freePort(t), env.root, containerSection)
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

// 健康判败 → 自动回滚按切换前的镜像摘要钉死；证据为前后容器 ID 与镜像摘要对照。
func TestContainerDeployFailureAutoRollback(t *testing.T) {
	env := newContainerEnv(t)

	run := env.runDeploy(t)
	if run.Results["svc-a"].Outcome != OutcomeDeployed {
		t.Fatalf("v1 deploy failed: %+v", run.Results["svc-a"])
	}
	v1Container := env.fake.containers["svc-a"].ID
	v1ImageID := env.fake.images["svc-a:1.0.0"]
	if run.Results["svc-a"].ActiveImageID != v1ImageID {
		t.Fatalf("run record must carry the active image digest, got %q", run.Results["svc-a"].ActiveImageID)
	}

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
	if !strings.Contains(res2.Detail, "auto-rolled back to version 1.0.0") || !strings.Contains(res2.Detail, shortID(v1ImageID)) {
		t.Fatalf("must auto-roll back pinned by the previous digest: %s", res2.Detail)
	}
	if res2.ActiveImageID != v1ImageID {
		t.Fatalf("run record after auto-rollback must carry the restored digest, got %q", res2.ActiveImageID)
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
	if rollbackStep == nil || !strings.Contains(rollbackStep.Detail, shortID(v1ImageID)) {
		t.Fatalf("auto-rollback step must record the pinned digest: %+v", rollbackStep)
	}

	// 回滚必须按摘要定位创建（引用就是摘要本身），内容落回 v1 镜像，
	// 且容器是新实例（旧的已被替换）。
	cur := env.fake.containers["svc-a"]
	if cur.Image != v1ImageID {
		t.Fatalf("rollback must create by digest, got image ref %q want %q", cur.Image, v1ImageID)
	}
	if cur.ImageID != v1ImageID {
		t.Fatalf("after rollback container must run the v1 content, got %s want %s", cur.ImageID, v1ImageID)
	}
	if cur.ID == v1Container {
		t.Fatal("rollback must recreate the container (new ID expected)")
	}
	if len(env.fake.creates) != 3 { // v1 + 坏 v2 + 回滚
		t.Fatalf("expected 3 creates (v1, bad v2, rollback), got %d", len(env.fake.creates))
	}
	if got := env.fake.creates[2].Image; got != v1ImageID {
		t.Fatalf("rollback create must be addressed by digest, got %q", got)
	}
}

// 同版本重打标签指向不同内容（标签撒谎）：自动回滚不受影响，
// 仍落回切换前的那份内容——因为定位依据是摘要，不是标签。
func TestContainerAutoRollbackUnaffectedByRetag(t *testing.T) {
	env := newContainerEnv(t)

	run := env.runDeploy(t)
	if run.Results["svc-a"].Outcome != OutcomeDeployed {
		t.Fatalf("v1 deploy failed: %+v", run.Results["svc-a"])
	}
	goodID := env.fake.images["svc-a:1.0.0"]

	// 重打标签：1.0.0 现在指向另一份内容（模拟同版本重打）。
	env.fake.retag("svc-a:1.0.0", "sha256:img-evil-content")

	// 部署坏版本 2.0.0：健康判败 → 自动回滚。
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

	// 回滚落点必须是切换前的摘要内容；而标签此刻指向别处——
	// 两者不同即证明回滚没有走标签。
	cur := env.fake.containers["svc-a"]
	if cur.ImageID != goodID {
		t.Fatalf("rollback must land on the pre-switch content %s, got %s", goodID, cur.ImageID)
	}
	if env.fake.images["svc-a:1.0.0"] == goodID {
		t.Fatal("test setup broken: the tag must point elsewhere after retag")
	}
	if cur.Image == "svc-a:1.0.0" {
		t.Fatal("rollback must not create from the (lying) tag")
	}
	if cur.Image != goodID {
		t.Fatalf("rollback create must be addressed by digest, got %q", cur.Image)
	}
}

// 切换前镜像的摘要已不在本机：自动回滚明确拒绝，不退化到任何标签。
func TestContainerAutoRollbackRefusesWhenDigestGone(t *testing.T) {
	env := newContainerEnv(t)

	run := env.runDeploy(t)
	if run.Results["svc-a"].Outcome != OutcomeDeployed {
		t.Fatalf("v1 deploy failed: %+v", run.Results["svc-a"])
	}
	goodID := env.fake.images["svc-a:1.0.0"]

	// 镜像被删（摘要不存在），但标签移向另一份内容——按标签回滚会"成功"落到错处。
	env.fake.forgetImage(goodID)
	env.fake.retag("svc-a:1.0.0", "sha256:img-other-content")

	env.cfg.Orchestration.Overrides["svc-a"] = models.ProjectOverride{
		HealthPort:     env.cfg.Orchestration.Overrides["svc-a"].HealthPort,
		ServiceVersion: "2.0.0",
	}
	env.setHealthy(false)

	createsBefore := len(env.fake.creates)
	run2 := env.runDeploy(t)
	res2 := run2.Results["svc-a"]
	if res2.Outcome != OutcomeFailed {
		t.Fatalf("bad version must fail, got %s", res2.Outcome)
	}
	if !strings.Contains(res2.Detail, "目标镜像已不存在") {
		t.Fatalf("must explicitly report the missing target image: %s", res2.Detail)
	}
	if len(env.fake.creates) != createsBefore+1 { // 只有坏版本那一次 create，没有回滚 create
		t.Fatalf("no rollback create may happen when the digest is gone, creates=%d", len(env.fake.creates))
	}
}

// 一键回滚：目标版本的摘要来自运行记录；镜像已不存在 → 明确拒绝且不建容器。
func TestContainerOneClickRollbackRefusesWhenDigestGone(t *testing.T) {
	env := newContainerEnv(t)

	run := env.runDeploy(t) // v1.0.0
	if run.Results["svc-a"].Outcome != OutcomeDeployed {
		t.Fatalf("v1 deploy failed: %+v", run.Results["svc-a"])
	}
	goodID := env.fake.images["svc-a:1.0.0"]

	// 升到 2.0.0，使 1.0.0 成为可回滚目标。
	env.cfg.Orchestration.Overrides["svc-a"] = models.ProjectOverride{
		HealthPort:     env.cfg.Orchestration.Overrides["svc-a"].HealthPort,
		ServiceVersion: "2.0.0",
	}
	if run2 := env.runDeploy(t); run2.Results["svc-a"].Outcome != OutcomeDeployed {
		t.Fatalf("v2 deploy failed: %+v", run2.Results["svc-a"])
	}

	// 1.0.0 的镜像被删（摘要不存在，标签移向别处）。
	env.fake.forgetImage(goodID)
	env.fake.retag("svc-a:1.0.0", "sha256:img-other-content")

	createsBefore := len(env.fake.creates)
	_, err := env.orch.Rollback("svc-a", "1.0.0", false)
	if err == nil || !strings.Contains(err.Error(), "目标镜像已不存在") {
		t.Fatalf("rollback must be refused with an explicit missing-image error, got: %v", err)
	}
	if len(env.fake.creates) != createsBefore {
		t.Fatal("refused rollback must not create any container")
	}
}

// 一键回滚：运行记录中没有目标版本的摘要 → 拒绝，不退化为按标签定位。
func TestContainerOneClickRollbackRefusesWithoutRecord(t *testing.T) {
	env := newContainerEnv(t)
	env.fake.addImage("svc-a:0.9.0") // 有标签、有镜像，但从未部署过（无运行记录）

	if run := env.runDeploy(t); run.Results["svc-a"].Outcome != OutcomeDeployed {
		t.Fatalf("deploy failed: %+v", run.Results["svc-a"])
	}

	_, err := env.orch.Rollback("svc-a", "0.9.0", false)
	if err == nil || !strings.Contains(err.Error(), "运行记录中没有") {
		t.Fatalf("rollback to an unrecorded version must be refused, got: %v", err)
	}
}

// ---------- cascade 连带回滚（双容器项目）----------

type cascadeEnv struct {
	orch  *Orchestrator
	fake  *fakeDocker
	cfg   *models.Config
	srvA  *httptest.Server
	srvB  *httptest.Server
	rootA string
	rootB string
}

func newCascadeEnv(t *testing.T) *cascadeEnv {
	t.Helper()
	base := t.TempDir()
	scanRoot := filepath.Join(base, "scan")

	for _, name := range []string{"svc-a", "svc-b"} {
		repoDir := filepath.Join(scanRoot, name)
		if err := os.MkdirAll(filepath.Join(repoDir, "build"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, "build", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	env := &cascadeEnv{
		rootA: filepath.Join(base, "install", "svc-a"),
		rootB: filepath.Join(base, "install", "svc-b"),
	}
	newHealthyServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}
	env.srvA = newHealthyServer()
	env.srvB = newHealthyServer()
	t.Cleanup(env.srvA.Close)
	t.Cleanup(env.srvB.Close)
	portA := env.srvA.Listener.Addr().(*net.TCPAddr).Port
	portB := env.srvB.Listener.Addr().(*net.TCPAddr).Port

	manifestA := fmt.Sprintf(`
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
`, freePort(t), env.rootA)
	manifestB := fmt.Sprintf(`
manifest_version: 1
project: svc-b
display_name: Service B
group: core
service_version: "1.0.0"
runtime: container
facts:
  ports:
    - { name: http, port: %d, exposure: loopback }
  health:
    http: { port_name: http, path: /healthz, expect_status: 200, timeout_seconds: 2, interval_seconds: 1, retries: 1 }
  depends_on:
    - { project: svc-a, kind: required }
baremetal:
  install: { root: %s }
container:
  image: { source: build, context: build }
`, freePort(t), env.rootB)
	if err := os.WriteFile(filepath.Join(scanRoot, "svc-a", "deploy-manifest.yaml"), []byte(manifestA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scanRoot, "svc-b", "deploy-manifest.yaml"), []byte(manifestB), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &models.Config{}
	cfg.Orchestration.Enabled = true
	cfg.Orchestration.ScanRoots = []string{scanRoot}
	cfg.Orchestration.StateDir = filepath.Join(base, "state")
	cfg.Orchestration.ProbeHost = "127.0.0.1"
	cfg.Orchestration.Overrides = map[string]models.ProjectOverride{
		"svc-a": {HealthPort: portA},
		"svc-b": {HealthPort: portB},
	}
	env.cfg = cfg
	env.orch = New(cfg, &config.Credentials{})
	env.fake = newFakeDocker()
	env.orch.docker = env.fake
	// 预置运行中的旧实例：健康端点被项目自己占用，端口预检放行。
	env.fake.containers["svc-a"] = &docker.ContainerState{
		Name: "svc-a", Image: "svc-a:0.0.1", ImageID: "sha256:img-seed-a",
		State: "running", Running: true, ID: "seedA001",
	}
	env.fake.containers["svc-b"] = &docker.ContainerState{
		Name: "svc-b", Image: "svc-b:0.0.1", ImageID: "sha256:img-seed-b",
		State: "running", Running: true, ID: "seedB001",
	}

	if _, rep := env.orch.Load(); !rep.OK() {
		t.Fatalf("aggregate validation failed: %v", rep.Errors)
	}
	return env
}

func (e *cascadeEnv) runDeploy(t *testing.T) *Run {
	t.Helper()
	snap, err := e.orch.StartRun(RunOptions{})
	if err != nil {
		var perr *ErrPreflightBlocked
		if errors.As(err, &perr) {
			t.Fatalf("run refused by preflight: %+v", perr.Report.Blockers)
		}
		t.Fatalf("run refused: %v", err)
	}
	return waitForRunFile(t, e.orch, snap.ID)
}

// cascade 连带回滚真实现：上游回滚到记录摘要，依赖方按自己当前运行摘要
// 重建——绝不允许"接受 cascade 参数却只回滚一个"。
func TestContainerCascadeRollbackRecreatesDependents(t *testing.T) {
	env := newCascadeEnv(t)

	run := env.runDeploy(t)
	if run.Results["svc-a"].Outcome != OutcomeDeployed || run.Results["svc-b"].Outcome != OutcomeDeployed {
		t.Fatalf("initial deploy failed: %+v / %+v", run.Results["svc-a"], run.Results["svc-b"])
	}
	imgA1 := env.fake.images["svc-a:1.0.0"]
	imgB := env.fake.images["svc-b:1.0.0"]

	// 升级上游 svc-a 到 2.0.0；svc-b 幂等不动。
	env.cfg.Orchestration.Overrides["svc-a"] = models.ProjectOverride{
		HealthPort:     env.cfg.Orchestration.Overrides["svc-a"].HealthPort,
		ServiceVersion: "2.0.0",
	}
	run2 := env.runDeploy(t)
	if run2.Results["svc-a"].Outcome != OutcomeDeployed {
		t.Fatalf("upgrade failed: %+v", run2.Results["svc-a"])
	}
	if run2.Results["svc-b"].Outcome != OutcomeUpToDate {
		t.Fatalf("svc-b must stay untouched on upstream upgrade, got %s", run2.Results["svc-b"].Outcome)
	}
	ctrA2 := env.fake.containers["svc-a"].ID
	ctrB := env.fake.containers["svc-b"].ID

	// 一键回滚 svc-a（不指定版本 → 最近可回滚目标 1.0.0），cascade。
	snap, err := env.orch.Rollback("svc-a", "", true)
	if err != nil {
		t.Fatalf("rollback refused: %v", err)
	}
	rb := waitForRunFile(t, env.orch, snap.ID)

	resA := rb.Results["svc-a"]
	if resA.Outcome != OutcomeRolledBack || resA.ActiveVersion != "1.0.0" || resA.ActiveImageID != imgA1 {
		t.Fatalf("svc-a rollback result wrong: %+v", resA)
	}
	resB := rb.Results["svc-b"]
	if resB == nil {
		t.Fatal("cascade must include dependent svc-b in the rollback run")
	}
	if resB.Outcome != OutcomeDeployed || resB.ActiveImageID != imgB {
		t.Fatalf("svc-b cascade result wrong: %+v", resB)
	}

	// 定位依据：两个容器都必须按摘要创建（引用即摘要），内容各归其位。
	curA := env.fake.containers["svc-a"]
	if curA.Image != imgA1 || curA.ImageID != imgA1 {
		t.Fatalf("svc-a must be recreated on its pinned digest: ref=%s id=%s want %s", curA.Image, curA.ImageID, imgA1)
	}
	curB := env.fake.containers["svc-b"]
	if curB.Image != imgB || curB.ImageID != imgB {
		t.Fatalf("svc-b must be recreated on its own current digest: ref=%s id=%s want %s", curB.Image, curB.ImageID, imgB)
	}
	if curA.ID == ctrA2 {
		t.Fatal("svc-a container must be recreated (new ID expected)")
	}
	if curB.ID == ctrB {
		t.Fatal("svc-b container must be recreated by cascade (new ID expected)")
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

// ---------- registry 来源（真实拉取 + digest 钉扎） ----------

// registryImageSection 构造钉 digest 的 registry 镜像段。
func registryImageSection(digest string) string {
	return fmt.Sprintf(`container:
  image:
    source: registry
    repo: limeight/ha-svc-a
    tag: "1.0.0-deadbee"
    digest: %q
`, digest)
}

// 声明了 digest：拉取引用必须是 repo@sha256:...（digest 优先，不按 tag 拉），
// 部署成功且运行记录钉住内容摘要。
func TestContainerDeployRegistryPullByDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	env := newContainerEnvWithImage(t, registryImageSection(digest))

	run := env.runDeploy(t)
	res := run.Results["svc-a"]
	if res.Outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s, detail = %s, steps = %+v", res.Outcome, res.Detail, res.Steps)
	}
	wantRef := "limeight/ha-svc-a@" + digest
	if len(env.fake.pulledRefs) != 1 || env.fake.pulledRefs[0] != wantRef {
		t.Fatalf("pull ref = %v, want exactly [%s] (digest-first, never tag)", env.fake.pulledRefs, wantRef)
	}
	if res.ActiveImageID == "" {
		t.Fatal("run record must pin the image content digest")
	}
	// 编排标签照常落：<project>:<version> 指向拉到的内容。
	if id, ok := env.fake.images["svc-a:1.0.0"]; !ok || id != env.fake.images[wantRef] {
		t.Fatalf("orchestration tag must point at the pulled content: %v", env.fake.images)
	}
}

// digest 漂移：拉到的内容摘要与清单声明不一致 → 响亮失败，绝不建容器。
func TestContainerDeployRegistryDigestMismatchRefuses(t *testing.T) {
	declared := "sha256:" + strings.Repeat("ab", 32)
	env := newContainerEnvWithImage(t, registryImageSection(declared))
	// 预置"registry 侧内容已被换"：拉取引用的 RepoDigests 指向别的摘要。
	pullRef := "limeight/ha-svc-a@" + declared
	env.fake.repoDigests[pullRef] = []string{"limeight/ha-svc-a@sha256:" + strings.Repeat("cd", 32)}

	run := env.runDeploy(t)
	res := run.Results["svc-a"]
	if res.Outcome != OutcomeFailed {
		t.Fatalf("digest mismatch must fail the deploy, got %s", res.Outcome)
	}
	if !strings.Contains(res.Detail, "does not match") {
		t.Fatalf("detail must say the digest does not match: %s", res.Detail)
	}
	if len(env.fake.creates) != 0 {
		t.Fatalf("no container may be created on digest mismatch, got %d", len(env.fake.creates))
	}
}

// 只声明 tag（schema 允许、不推荐）：按 repo:tag 拉，无 digest 对照。
func TestContainerDeployRegistryTagOnlyFallback(t *testing.T) {
	env := newContainerEnvWithImage(t, `container:
  image:
    source: registry
    repo: limeight/ha-svc-a
    tag: "1.0.0-deadbee"
`)
	run := env.runDeploy(t)
	res := run.Results["svc-a"]
	if res.Outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s, detail = %s", res.Outcome, res.Detail)
	}
	if len(env.fake.pulledRefs) != 1 || env.fake.pulledRefs[0] != "limeight/ha-svc-a:1.0.0-deadbee" {
		t.Fatalf("pull ref = %v, want tag fallback", env.fake.pulledRefs)
	}
}

// 拉取失败：错误原样带进运行记录（真实驱动侧另有分类，见 docker 包测试）。
func TestContainerDeployRegistryPullErrorRecorded(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	env := newContainerEnvWithImage(t, registryImageSection(digest))
	env.fake.pullErr = errors.New("registry 认证失败（401）：拉取需要先 docker login")

	run := env.runDeploy(t)
	res := run.Results["svc-a"]
	if res.Outcome != OutcomeFailed {
		t.Fatalf("pull failure must fail the deploy, got %s", res.Outcome)
	}
	joined := ""
	for _, s := range res.Steps {
		joined += s.Detail + "\n"
	}
	if !strings.Contains(joined, "docker login") {
		t.Fatalf("steps must carry the readable pull error: %s", joined)
	}
	if len(env.fake.creates) != 0 {
		t.Fatalf("no container may be created when pull fails, got %d", len(env.fake.creates))
	}
}
