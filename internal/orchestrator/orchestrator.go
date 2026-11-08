// Package orchestrator 是 Deployer 的裸金属编排核心。
//
// 语义要点：
//   - 进程生命周期一律经 Tinyvisor（登记/重启/状态），编排器不常驻、
//     不当守护、不自己拉进程；
//   - 版本目录 + current 软链：发布 = 落版本目录 → 原子切链 → 重启 →
//     健康判定；只搬文件不重启不算部署；
//   - 幂等：版本未变、脚本无漂移且已健康 → 零动作（up-to-date）；
//   - 失败隔离：单项目失败只连锁其 required 下游，其余分支继续；
//   - 续跑：重跑即续跑——已健康且版本一致的项目自动零动作；
//   - 一键回滚：软链切回上一版本 + 重启 + 健康判定；
//   - manager 非 tinyvisor 的项目一律 foreign：只读、绝不触碰。
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"HA-Services-Deployer/internal/artifact"
	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/manifest"
	"HA-Services-Deployer/internal/models"
	"HA-Services-Deployer/internal/tinyvisor"
)

// ErrRunInProgress 表示已有运行中的编排任务（单飞）。
var ErrRunInProgress = errors.New("an orchestration run is already in progress")

// ErrValidation 表示聚合校验未通过，拒绝执行。
type ErrValidation struct {
	Errors []string
}

func (e *ErrValidation) Error() string {
	return fmt.Sprintf("manifest validation failed with %d error(s): %v", len(e.Errors), e.Errors)
}

type ProjectOutcome string

const (
	OutcomePending   ProjectOutcome = "pending"
	OutcomeUpToDate  ProjectOutcome = "up-to-date"
	OutcomeDeployed  ProjectOutcome = "deployed"
	OutcomeRolledBack ProjectOutcome = "rolled-back"
	OutcomeReachable ProjectOutcome = "reachable" // external 依赖可达
	OutcomeFailed    ProjectOutcome = "failed"
	OutcomeSkipped   ProjectOutcome = "skipped"
	OutcomeForeign   ProjectOutcome = "foreign"
	OutcomeDryRun    ProjectOutcome = "dry-run"
)

type StepRecord struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type ProjectResult struct {
	Project         string         `json:"project"`
	Outcome         ProjectOutcome `json:"outcome"`
	Detail          string         `json:"detail,omitempty"`
	DeclaredVersion string         `json:"declared_version"`
	ActiveVersion   string         `json:"active_version,omitempty"`
	Steps           []StepRecord   `json:"steps,omitempty"`
	StartedAt       time.Time      `json:"started_at"`
	FinishedAt      time.Time      `json:"finished_at"`
}

type RunOptions struct {
	Projects           []string `json:"projects,omitempty"`
	Group              string   `json:"group,omitempty"`
	DryRun             bool     `json:"dry_run,omitempty"`
	// AutoRollbackOnFail 默认 true：健康判定失败时自动切回上一版本并重启。
	AutoRollbackOnFail *bool `json:"auto_rollback_on_fail,omitempty"`
}

type Run struct {
	ID         string                    `json:"id"`
	Type       string                    `json:"type"` // deploy | rollback
	State      string                    `json:"state"` // running | done | failed
	Options    RunOptions                `json:"options"`
	Order      []string                  `json:"order"`
	Results    map[string]*ProjectResult `json:"results"`
	// Preflight 记录本次运行开局时的前置检查报告。
	// 阻断项已在入口处拦截（不会进入运行），这里留存的是放行时的报告。
	Preflight *PreflightReport `json:"preflight,omitempty"`
	// Notes 记录本次运行需要上报的异常事件（如 state.json 损坏后的恢复动作）。
	// 失败与降级必须可被看见，不静默。
	Notes      []string   `json:"notes,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type Orchestrator struct {
	cfg   *models.Config
	creds *config.Credentials
	tv    *tinyvisor.Client
	store *Store

	mu      sync.Mutex
	current *Run
}

func New(cfg *models.Config, creds *config.Credentials) *Orchestrator {
	stateDir := cfg.Orchestration.StateDir
	if stateDir == "" {
		stateDir = "./data/deploy"
	}
	return &Orchestrator{
		cfg:   cfg,
		creds: creds,
		tv:    tinyvisor.New(cfg.Orchestration.Tinyvisor.URL, creds.TinyvisorAPIKey),
		store: NewStore(stateDir),
	}
}

// Load 装载并聚合校验全部清单。
func (o *Orchestrator) Load() ([]*manifest.Manifest, *manifest.Report) {
	ms, errs := manifest.LoadAggregate(o.cfg.Orchestration.ScanRoots, o.cfg.Orchestration.ExternalManifests)
	return ms, manifest.ValidateAggregate(ms, errs)
}

func indexByProject(ms []*manifest.Manifest) map[string]*manifest.Manifest {
	out := make(map[string]*manifest.Manifest)
	for _, m := range ms {
		out[m.Project] = m
	}
	return out
}

// loadStateRecovered 加载 GlobalState，并在 state.json 损坏时恢复。
// 返回可用状态与恢复说明（正常时为空）。恢复策略：隔离损坏文件 → 从 runs
// 记录重建；重建无结果则显式降级为"全部重跑"。全过程记录并上报，不静默。
func (o *Orchestrator) loadStateRecovered() (*GlobalState, []string) {
	empty := func() *GlobalState { return &GlobalState{Projects: map[string]*ProjectState{}} }

	state, err := o.store.LoadState()
	if err == nil {
		return state, nil
	}

	var notes []string
	note := func(f string, a ...interface{}) {
		msg := fmt.Sprintf(f, a...)
		notes = append(notes, msg)
		log.Printf("orchestrator: state recovery: %s", msg)
	}

	if !errors.Is(err, ErrStateCorrupt) {
		note("state.json unreadable (%v); degraded to full re-run", err)
		return empty(), notes
	}

	note("state.json is corrupt (%v)", err)
	if qpath, qerr := o.store.quarantineState(); qerr != nil {
		note("failed to quarantine corrupt state.json: %v", qerr)
	} else {
		note("corrupt state.json quarantined to %s", qpath)
	}

	rebuilt, rerr := o.store.RebuildStateFromRuns()
	if rerr != nil {
		note("rebuild from runs failed (%v); degraded to full re-run", rerr)
		return empty(), notes
	}
	if len(rebuilt.Projects) == 0 {
		note("no run records available to rebuild state; degraded to full re-run (all projects will be re-checked)")
		return empty(), notes
	}
	note("state rebuilt from run records (%d project(s))", len(rebuilt.Projects))
	return rebuilt, notes
}

// StartRun 启动一次异步编排运行（单飞）。返回运行快照，进度经
// GetRun/CurrentRun 查询，逐项落盘保证中断后可续跑。
func (o *Orchestrator) StartRun(opts RunOptions) (*Run, error) {
	ms, _, order, err := o.prepare(opts.Projects, opts.Group)
	if err != nil {
		return nil, err
	}

	// 前置检查门：能查的一次查全，发现阻断项直接拒绝（dry_run 同样拒绝）。
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	report := o.preflightScope(ctx, ms, order, "deploy")
	cancel()
	if report.HasBlockers() {
		return nil, &ErrPreflightBlocked{Report: report}
	}

	run := &Run{
		ID:        time.Now().Format("20060102-150405"),
		Type:      "deploy",
		State:     "running",
		Options:   opts,
		Order:     order,
		Results:   map[string]*ProjectResult{},
		Preflight: report,
		StartedAt: time.Now(),
	}

	o.mu.Lock()
	o.current = run
	o.mu.Unlock()
	_ = o.store.SaveRun(run)

	// 快照先于执行协程：之后 run 会被执行侧并发写入。
	snap := snapshotRun(run)
	go o.execute(run, ms)
	return snap, nil
}

// Rollback 一键回滚：软链切回上一版本（或指定版本）+ 重启 + 健康判定。
// cascade 时按依赖序连带重启直接依赖方。
func (o *Orchestrator) Rollback(project, to string, cascade bool) (*Run, error) {
	ms, rep := o.Load()
	if !rep.OK() {
		return nil, &ErrValidation{Errors: rep.Errors}
	}
	byProject := indexByProject(ms)
	rawM, ok := byProject[project]
	if !ok {
		return nil, fmt.Errorf("project %q not found", project)
	}
	if rawM.IsExternal() {
		return nil, fmt.Errorf("project %q is external, nothing to roll back", project)
	}
	m := o.resolveManifest(rawM)
	eff := o.effective(m)
	if eff.manager != "tinyvisor" {
		return nil, fmt.Errorf("project %q manager is %q (foreign): rollback refused", project, eff.manager)
	}

	active := readActiveVersion(eff.root)
	target := to
	if target == "" {
		releases, err := listReleases(eff.root)
		if err != nil {
			return nil, err
		}
		for _, name := range releases {
			if name != active {
				target = name
				break
			}
		}
	}
	if target == "" {
		return nil, fmt.Errorf("no previous release to roll back to (root %s)", eff.root)
	}
	if target == active {
		return nil, fmt.Errorf("release %q is already active", target)
	}

	order := []string{project}
	if cascade {
		order = append(order, requiredDependents(ms, project)...)
	}

	// 单飞先行：已有运行就不必再跑前置检查。
	o.mu.Lock()
	busy := o.current != nil && o.current.State == "running"
	o.mu.Unlock()
	if busy {
		return nil, ErrRunInProgress
	}

	// 前置检查门（rollback 模式：不查产物/磁盘，其余相同）。
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	report := o.preflightScope(ctx, ms, order, "rollback")
	cancel()
	if report.HasBlockers() {
		return nil, &ErrPreflightBlocked{Report: report}
	}

	run := &Run{
		ID:        time.Now().Format("20060102-150405") + "-rb",
		Type:      "rollback",
		State:     "running",
		Options:   RunOptions{Projects: order},
		Order:     order,
		Results:   map[string]*ProjectResult{},
		Preflight: report,
		StartedAt: time.Now(),
	}

	o.mu.Lock()
	if o.current != nil && o.current.State == "running" {
		o.mu.Unlock()
		return nil, ErrRunInProgress
	}
	o.current = run
	o.mu.Unlock()
	_ = o.store.SaveRun(run)

	// 快照先于执行协程：之后 run 会被执行侧并发写入。
	snap := snapshotRun(run)
	go o.executeRollback(run, m, target, cascade)
	return snap, nil
}

func (o *Orchestrator) prepare(projects []string, group string) ([]*manifest.Manifest, *manifest.Report, []string, error) {
	o.mu.Lock()
	if o.current != nil && o.current.State == "running" {
		o.mu.Unlock()
		return nil, nil, nil, ErrRunInProgress
	}
	o.mu.Unlock()

	ms, rep := o.Load()
	if !rep.OK() {
		return nil, rep, nil, &ErrValidation{Errors: rep.Errors}
	}
	order, err := Plan(ms, projects, group)
	if err != nil {
		return nil, rep, nil, err
	}
	return ms, rep, order, nil
}

// execute 按依赖序执行部署流水线，逐项落盘。
func (o *Orchestrator) execute(run *Run, ms []*manifest.Manifest) {
	ctx := context.Background()
	byProject := indexByProject(ms)
	state, notes := o.loadStateRecovered()
	if len(notes) > 0 {
		run.Notes = append(run.Notes, notes...)
		_ = o.store.SaveRun(run)
	}

	for _, name := range run.Order {
		m := byProject[name]
		res := o.executeProject(ctx, run, m, byProject)
		res.FinishedAt = time.Now()
		run.Results[name] = res

		if !run.Options.DryRun {
			state.Projects[name] = &ProjectState{
				Outcome:       string(res.Outcome),
				ActiveVersion: res.ActiveVersion,
				Detail:        res.Detail,
				UpdatedAt:     time.Now(),
			}
			_ = o.store.SaveState(state)
		}
		_ = o.store.SaveRun(run)
	}

	now := time.Now()
	run.FinishedAt = &now
	run.State = "done"
	for _, res := range run.Results {
		if res.Outcome == OutcomeFailed {
			run.State = "failed"
			break
		}
	}
	_ = o.store.SaveRun(run)

	o.mu.Lock()
	if o.current == run {
		o.current = run
	}
	o.mu.Unlock()
}

// executeProject 执行单项目流水线（含失败隔离与 foreign 只读语义）。
func (o *Orchestrator) executeProject(ctx context.Context, run *Run, raw *manifest.Manifest, byProject map[string]*manifest.Manifest) *ProjectResult {
	m := o.resolveManifest(raw)
	res := &ProjectResult{
		Project:         m.Project,
		Outcome:         OutcomePending,
		DeclaredVersion: m.ServiceVersion,
		StartedAt:       time.Now(),
	}

	// 失败隔离：required 依赖在本次运行中失败/跳过 → 本项目跳过。
	for _, d := range m.Facts.DependsOn {
		if d.Kind == "optional" {
			continue
		}
		depRes, inRun := run.Results[d.Project]
		if inRun && (depRes.Outcome == OutcomeFailed || depRes.Outcome == OutcomeSkipped) {
			res.Outcome = OutcomeSkipped
			res.Detail = fmt.Sprintf("required dependency %q %s", d.Project, depRes.Outcome)
			return res
		}
		// 依赖不在本次范围内（含 foreign / external）：用健康/可达判定兜底。
		if !inRun {
			if dep, ok := byProject[d.Project]; ok {
				if err := o.checkDependencyLive(ctx, dep); err != nil {
					res.Outcome = OutcomeSkipped
					res.Detail = fmt.Sprintf("required dependency %q not healthy: %v", d.Project, err)
					return res
				}
			}
		}
	}

	if m.IsExternal() {
		if err := o.checkExternal(ctx, m); err != nil {
			res.Outcome = OutcomeFailed
			res.Detail = err.Error()
		} else {
			res.Outcome = OutcomeReachable
		}
		return res
	}

	eff := o.effective(m)
	res.ActiveVersion = readActiveVersion(eff.root)

	if eff.manager != "tinyvisor" {
		// foreign：只读不碰。仅附一次被动探测用于状态可见性。
		res.Outcome = OutcomeForeign
		if err := o.probeOnce(ctx, m, eff.healthPort); err != nil {
			res.Detail = fmt.Sprintf("manager=%s (read-only); passive probe: %v", eff.manager, err)
		} else {
			res.Detail = fmt.Sprintf("manager=%s (read-only); passive probe: healthy", eff.manager)
		}
		return res
	}

	if run.Options.DryRun {
		res.Outcome = OutcomeDryRun
		res.Steps = append(res.Steps,
			StepRecord{Name: "artifact", OK: true, Detail: "dry-run: stage " + m.Baremetal.Artifact.Source},
		)
		if shared := m.Baremetal.Install.SharedData; shared != "" {
			res.Steps = append(res.Steps, StepRecord{Name: "ensure-shared", OK: true,
				Detail: "dry-run: mkdir " + filepath.Join(eff.root, shared)})
		}
		res.Steps = append(res.Steps,
			StepRecord{Name: "symlink", OK: true, Detail: "dry-run: current -> releases/" + m.ServiceVersion},
			StepRecord{Name: "ensure-script", OK: true, Detail: "dry-run: upsert " + m.Project},
			StepRecord{Name: "restart", OK: true, Detail: "dry-run: restart via tinyvisor"},
			StepRecord{Name: "health", OK: true, Detail: "dry-run: probe " + mustProbeURL(o, m)},
		)
		if len(m.Facts.Smoke) > 0 {
			res.Steps = append(res.Steps, StepRecord{Name: "smoke", OK: true,
				Detail: fmt.Sprintf("dry-run: %d smoke check(s)", len(m.Facts.Smoke))})
		}
		return res
	}

	o.deployManaged(ctx, run, m, eff, res)
	return res
}

// deployManaged 是 tinyvisor 管理项目的完整流水线：
// 拉产物 → 校验 → 落版本目录 → 切软链 → 登记脚本 → 重启 → 健康判定（→ 自动回滚）。
func (o *Orchestrator) deployManaged(ctx context.Context, run *Run, m *manifest.Manifest, eff effective, res *ProjectResult) {
	addStep := func(name string, err error, detail string) bool {
		step := StepRecord{Name: name, OK: err == nil, Detail: detail}
		if err != nil {
			step.Detail = fmt.Sprintf("%s: %v", detail, err)
		}
		res.Steps = append(res.Steps, step)
		return err == nil
	}

	// 1. 产物：下载/复制 + sha256 校验 + 幂等落版本目录。
	relDir := releaseDir(eff.root, m.ServiceVersion)
	sha, err := artifact.Stage(ctx, m, relDir, o.creds.GithubToken)
	if !addStep("artifact", err, fmt.Sprintf("stage %s (sha256 %s)", m.Baremetal.Artifact.Source, shortSHA(sha))) {
		res.Outcome = OutcomeFailed
		res.Detail = "artifact stage failed"
		return
	}

	// 1.5 共享数据目录：声明 shared_data 时，配置与数据落在版本目录之外。
	// 幂等确保存在；版本切换与 prune 从不触碰该目录，升级不重置数据。
	if shared := m.Baremetal.Install.SharedData; shared != "" {
		sharedDir := filepath.Join(eff.root, shared)
		if err = os.MkdirAll(sharedDir, 0o755); !addStep("ensure-shared", err, sharedDir) {
			res.Outcome = OutcomeFailed
			res.Detail = "shared data dir creation failed"
			return
		}
	}

	// 2. 软链：原子切换 current -> releases/<version>。
	activeBefore := readActiveVersion(eff.root)
	linkChanged := activeBefore != m.ServiceVersion
	if linkChanged {
		err = switchSymlink(eff.root, m.ServiceVersion)
	}
	if !addStep("symlink", err, fmt.Sprintf("current: %s -> %s", orNone(activeBefore), m.ServiceVersion)) {
		res.Outcome = OutcomeFailed
		res.Detail = "symlink switch failed"
		return
	}
	res.ActiveVersion = m.ServiceVersion

	// 3. 脚本：按名幂等登记（内容漂移才写）。
	scriptDrift, err := o.upsertScript(ctx, m, eff)
	if !addStep("ensure-script", err, fmt.Sprintf("upsert %s (drift=%v)", m.Project, scriptDrift)) {
		res.Outcome = OutcomeFailed
		res.Detail = "tinyvisor script upsert failed"
		return
	}

	// 4. 激活判定：版本未变 + 无漂移 + 已健康 → 幂等零动作。
	healthyBefore := o.probeOnce(ctx, m, eff.healthPort) == nil
	if !linkChanged && !scriptDrift && healthyBefore {
		res.Outcome = OutcomeUpToDate
		res.Detail = "version active, script aligned, health ok: no action"
		addStep("activate", nil, "skipped (up-to-date)")
		pruneReleases(eff.root, m.ServiceVersion, m.Baremetal.Install.KeepReleases)
		return
	}

	// 5. 重启（软链变化/脚本漂移/不健康都必须重启——只搬文件不算部署）。
	if _, err := o.tv.RestartByName(ctx, m.Project); !addStep("restart", err, "restart via tinyvisor") {
		res.Outcome = OutcomeFailed
		res.Detail = "restart failed"
		o.autoRollback(ctx, m, eff, res, activeBefore)
		return
	}

	// 6. 健康判定。
	if err := o.probeUntilHealthy(ctx, m, eff.healthPort); !addStep("health", err, "probe until healthy") {
		res.Outcome = OutcomeFailed
		res.Detail = "health check failed after restart"
		o.autoRollback(ctx, m, eff, res, activeBefore)
		return
	}

	// 7. 冒烟验证（仅声明了 facts.smoke 的项目）：健康只证明进程活着，
	// 冒烟用真实请求证明服务真的在干活；失败同样视为部署失败 → 自动回滚。
	if len(m.Facts.Smoke) > 0 {
		err := o.runSmoke(ctx, m, eff.healthPort)
		if !addStep("smoke", err, fmt.Sprintf("%d smoke check(s)", len(m.Facts.Smoke))) {
			res.Outcome = OutcomeFailed
			res.Detail = "smoke verification failed after health"
			o.autoRollback(ctx, m, eff, res, activeBefore)
			return
		}
	}

	pruneReleases(eff.root, m.ServiceVersion, m.Baremetal.Install.KeepReleases)
	res.Outcome = OutcomeDeployed
	res.Detail = fmt.Sprintf("version %s active and healthy", m.ServiceVersion)
}

// autoRollback 健康判定失败时切回上一版本并重启（默认开启）。
func (o *Orchestrator) autoRollback(ctx context.Context, m *manifest.Manifest, eff effective, res *ProjectResult, activeBefore string) {
	if o.runAutoRollbackDisabled() {
		return
	}
	target := activeBefore
	if target == "" {
		releases, err := listReleases(eff.root)
		if err != nil {
			return
		}
		for _, name := range releases {
			if name != m.ServiceVersion {
				target = name
				break
			}
		}
	}
	if target == "" {
		res.Detail += "; auto-rollback skipped: no previous release"
		return
	}

	if err := switchSymlink(eff.root, target); err != nil {
		res.Detail += "; auto-rollback symlink failed: " + err.Error()
		return
	}
	res.ActiveVersion = target
	if _, err := o.tv.RestartByName(ctx, m.Project); err != nil {
		res.Detail += "; auto-rollback restart failed: " + err.Error()
		return
	}
	res.Detail += fmt.Sprintf("; auto-rolled back to %s", target)
	res.Steps = append(res.Steps, StepRecord{Name: "auto-rollback", OK: true, Detail: "switched back to " + target})
}

func (o *Orchestrator) runAutoRollbackDisabled() bool {
	// 由当前运行选项决定；缺省为开启自动回滚。
	o.mu.Lock()
	run := o.current
	o.mu.Unlock()
	if run != nil && run.Options.AutoRollbackOnFail != nil {
		return !*run.Options.AutoRollbackOnFail
	}
	return false
}

// executeRollback 执行回滚运行：目标项目切链+重启+健康判定，
// cascade 时对直接依赖方做连带重启。
func (o *Orchestrator) executeRollback(run *Run, m *manifest.Manifest, target string, cascade bool) {
	ctx := context.Background()
	res := &ProjectResult{
		Project:         m.Project,
		DeclaredVersion: m.ServiceVersion,
		StartedAt:       time.Now(),
	}
	eff := o.effective(m)
	activeBefore := readActiveVersion(eff.root)

	if err := switchSymlink(eff.root, target); err != nil {
		res.Outcome = OutcomeFailed
		res.Detail = "symlink switch failed: " + err.Error()
		res.FinishedAt = time.Now()
		run.Results[m.Project] = res
		o.finishRollbackRun(run)
		return
	}
	res.Steps = append(res.Steps, StepRecord{Name: "symlink", OK: true, Detail: fmt.Sprintf("current: %s -> %s", orNone(activeBefore), target)})

	if _, err := o.tv.RestartByName(ctx, m.Project); err != nil {
		res.Outcome = OutcomeFailed
		res.Detail = "restart failed: " + err.Error()
		res.ActiveVersion = target
		res.FinishedAt = time.Now()
		run.Results[m.Project] = res
		o.finishRollbackRun(run)
		return
	}
	res.Steps = append(res.Steps, StepRecord{Name: "restart", OK: true, Detail: "restart via tinyvisor"})

	if err := o.probeUntilHealthy(ctx, m, eff.healthPort); err != nil {
		res.Outcome = OutcomeFailed
		res.Detail = "health check failed after rollback: " + err.Error()
		res.ActiveVersion = target
		res.FinishedAt = time.Now()
		run.Results[m.Project] = res
		o.finishRollbackRun(run)
		return
	}
	res.Steps = append(res.Steps, StepRecord{Name: "health", OK: true, Detail: "healthy after rollback"})

	// 回滚目标同样要过冒烟（若声明）：回滚不是目的，回到能用的版本才是。
	// 冒烟失败不再切链（已在回滚目标上），如实报失败。
	if len(m.Facts.Smoke) > 0 {
		if err := o.runSmoke(ctx, m, eff.healthPort); err != nil {
			res.Outcome = OutcomeFailed
			res.Detail = fmt.Sprintf("rolled back %s -> %s, healthy, but smoke failed: %v", orNone(activeBefore), target, err)
			res.ActiveVersion = target
			res.FinishedAt = time.Now()
			run.Results[m.Project] = res
			o.finishRollbackRun(run)
			return
		}
		res.Steps = append(res.Steps, StepRecord{Name: "smoke", OK: true,
			Detail: fmt.Sprintf("%d smoke check(s) passed after rollback", len(m.Facts.Smoke))})
	}

	res.Outcome = OutcomeRolledBack
	res.ActiveVersion = target
	res.Detail = fmt.Sprintf("rolled back %s -> %s, healthy", orNone(activeBefore), target)
	res.FinishedAt = time.Now()
	run.Results[m.Project] = res

	if cascade {
		ms, _ := o.Load()
		byProject := indexByProject(ms)
		for _, depName := range run.Order[1:] {
			dep, ok := byProject[depName]
			if !ok {
				continue
			}
			depRes := o.restartDependent(ctx, dep)
			run.Results[depName] = depRes
			_ = o.store.SaveRun(run)
		}
	}
	o.finishRollbackRun(run)
}

func (o *Orchestrator) restartDependent(ctx context.Context, rawDep *manifest.Manifest) *ProjectResult {
	m := o.resolveManifest(rawDep)
	res := &ProjectResult{Project: m.Project, DeclaredVersion: m.ServiceVersion, StartedAt: time.Now()}
	eff := o.effective(m)
	if eff.manager != "tinyvisor" {
		res.Outcome = OutcomeForeign
		res.Detail = fmt.Sprintf("manager=%s (read-only): not restarted", eff.manager)
		res.FinishedAt = time.Now()
		return res
	}
	if _, err := o.tv.RestartByName(ctx, m.Project); err != nil {
		res.Outcome = OutcomeFailed
		res.Detail = "cascade restart failed: " + err.Error()
		res.FinishedAt = time.Now()
		return res
	}
	if err := o.probeUntilHealthy(ctx, m, eff.healthPort); err != nil {
		res.Outcome = OutcomeFailed
		res.Detail = "cascade health check failed: " + err.Error()
		res.FinishedAt = time.Now()
		return res
	}
	if len(m.Facts.Smoke) > 0 {
		if err := o.runSmoke(ctx, m, eff.healthPort); err != nil {
			res.Outcome = OutcomeFailed
			res.Detail = "cascade smoke verification failed: " + err.Error()
			res.FinishedAt = time.Now()
			return res
		}
	}
	res.Outcome = OutcomeDeployed
	res.ActiveVersion = readActiveVersion(eff.root)
	res.Detail = "cascade restart healthy"
	res.FinishedAt = time.Now()
	return res
}

func (o *Orchestrator) finishRollbackRun(run *Run) {
	now := time.Now()
	run.FinishedAt = &now
	run.State = "done"
	for _, r := range run.Results {
		if r.Outcome == OutcomeFailed {
			run.State = "failed"
			break
		}
	}
	_ = o.store.SaveRun(run)
	state, notes := o.loadStateRecovered()
	if len(notes) > 0 {
		run.Notes = append(run.Notes, notes...)
		_ = o.store.SaveRun(run)
	}
	for name, r := range run.Results {
		if r.Outcome == OutcomeRolledBack || r.Outcome == OutcomeDeployed {
			state.Projects[name] = &ProjectState{
				Outcome:       string(r.Outcome),
				ActiveVersion: r.ActiveVersion,
				Detail:        r.Detail,
				UpdatedAt:     time.Now(),
			}
		}
	}
	_ = o.store.SaveState(state)
	o.mu.Lock()
	if o.current == run {
		o.current = run
	}
	o.mu.Unlock()
}

// checkDependencyLive 对范围外依赖做存活判定：
// external → TCP 可达；服务 → 被动健康探测。
func (o *Orchestrator) checkDependencyLive(ctx context.Context, rawDep *manifest.Manifest) error {
	dep := o.resolveManifest(rawDep)
	if dep.IsExternal() {
		return o.checkExternal(ctx, dep)
	}
	return o.probeOnce(ctx, dep, o.effective(dep).healthPort)
}

func (o *Orchestrator) checkExternal(ctx context.Context, m *manifest.Manifest) error {
	if m.Facts.External == nil || m.Facts.External.Host == "" {
		return fmt.Errorf("external host not declared")
	}
	for _, p := range m.Facts.Ports {
		addr := net.JoinHostPort(m.Facts.External.Host, fmt.Sprintf("%d", p.Port))
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			return fmt.Errorf("tcp %s: %w", addr, err)
		}
		conn.Close()
	}
	return nil
}

// ProjectStatusView 是状态端点的单项目视图。
type ProjectStatusView struct {
	Project         string `json:"project"`
	Kind            string `json:"kind"`
	Group           string `json:"group"`
	Manager         string `json:"manager"`
	Foreign         bool   `json:"foreign"`
	DeclaredVersion string `json:"declared_version"`
	ActiveVersion   string `json:"active_version,omitempty"`
	TinyvisorState  string `json:"tinyvisor_state,omitempty"`
	Healthy         *bool  `json:"healthy,omitempty"`
	Detail          string `json:"detail,omitempty"`
}

type StatusReport struct {
	GeneratedAt        time.Time           `json:"generated_at"`
	TinyvisorReachable bool                `json:"tinyvisor_reachable"`
	Projects           []ProjectStatusView `json:"projects"`
}

// Status 汇总声明 + 实时状态（Tinyvisor 存活 + 被动健康探测），只读。
func (o *Orchestrator) Status(ctx context.Context) (*StatusReport, error) {
	ms, rep := o.Load()
	report := &StatusReport{GeneratedAt: time.Now()}
	if !rep.OK() {
		for _, e := range rep.Errors {
			report.Projects = append(report.Projects, ProjectStatusView{Detail: "validation: " + e})
		}
	}

	tvReachable := o.tv.Ping(ctx) == nil
	report.TinyvisorReachable = tvReachable

	var tvScripts map[string]string
	if tvReachable {
		if scripts, err := o.tv.List(ctx); err == nil {
			tvScripts = make(map[string]string)
			for _, s := range scripts {
				tvScripts[s.Name] = s.Status
			}
		}
	}

	sort.Slice(ms, func(i, j int) bool { return ms[i].Project < ms[j].Project })
	for _, raw := range ms {
		m := o.resolveManifest(raw)
		view := ProjectStatusView{
			Project:         m.Project,
			Kind:            m.Kind,
			Group:           m.Group,
			DeclaredVersion: m.ServiceVersion,
		}

		if m.IsExternal() {
			view.Manager = "external"
			if err := o.checkExternal(ctx, m); err != nil {
				healthy := false
				view.Healthy = &healthy
				view.Detail = err.Error()
			} else {
				healthy := true
				view.Healthy = &healthy
			}
			report.Projects = append(report.Projects, view)
			continue
		}

		eff := o.effective(m)
		view.Manager = eff.manager
		view.ActiveVersion = readActiveVersion(eff.root)

		if eff.manager != "tinyvisor" {
			view.Foreign = true
		} else if tvScripts != nil {
			view.TinyvisorState = tvScripts[m.Project]
		}

		if err := o.probeOnce(ctx, m, eff.healthPort); err != nil {
			healthy := false
			view.Healthy = &healthy
			view.Detail = err.Error()
		} else {
			healthy := true
			view.Healthy = &healthy
		}
		report.Projects = append(report.Projects, view)
	}
	return report, nil
}

// CurrentRun / GetRun / ListRuns 暴露运行记录。

func (o *Orchestrator) CurrentRun() *Run {
	o.mu.Lock()
	defer o.mu.Unlock()
	return snapshotRun(o.current)
}

func (o *Orchestrator) GetRun(id string) (*Run, error) {
	if cur := o.CurrentRun(); cur != nil && cur.ID == id {
		return cur, nil
	}
	return o.store.LoadRun(id)
}

func (o *Orchestrator) ListRuns() ([]RunSummary, error) {
	return o.store.ListRuns()
}

func snapshotRun(run *Run) *Run {
	if run == nil {
		return nil
	}
	clone := *run
	clone.Results = make(map[string]*ProjectResult, len(run.Results))
	for k, v := range run.Results {
		vv := *v
		clone.Results[k] = &vv
	}
	clone.Order = append([]string(nil), run.Order...)
	return &clone
}

func mustProbeURL(o *Orchestrator, m *manifest.Manifest) string {
	url, err := o.probeURL(m, o.effective(m).healthPort)
	if err != nil {
		return "(invalid health spec)"
	}
	return url
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
