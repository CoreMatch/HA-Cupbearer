package orchestrator

// 容器路径的一键触发——容器形态的默认交互。
//
// Deployer 的设计意图是"在不使用容器的情况下编排微服务"：裸金属编排
//（版本目录+软链、Tinyvisor 重启、健康判定、回滚）是本体。容器路径在
// 此之上提供两种形态，职责不同：
//
//  1. 一键触发（本文件）：把整套容器编排交出去执行——按各清单已发布
//     的镜像引用（digest 优先）生成 override，叠加在平台 compose 模板上
//     执行 `docker compose up -d`，然后把结果如实回报。整套生命周期
//     （创建、网络、健康、重启策略）由 compose 承担，编排器不做逐容器
//     管理动作。
//  2. 容器级管理（container.go，可选能力）：runtime=container 的单项目
//     流水线——按 digest 拉取、逐容器重建、健康判定、冒烟、按摘要回滚。
//     这是已实现并保留的能力，不是主线。
//
// 两者互不冲突：触发消费的是清单 container 段"要触发什么"（已发布的
// 镜像引用）；容器级管理消费的是同一段的执行声明。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"HA-Services-Deployer/internal/manifest"
)

// OutcomeTriggered 表示该项目经一键触发交给 compose 执行（结果如实回报）。
const OutcomeTriggered ProjectOutcome = "triggered"

// defaultComposeFile 是平台 compose 模板的默认位置（相对进程工作目录）。
const defaultComposeFile = "deploy/docker-compose.yml"

// triggerTimeout 是一次触发的总时限（含整套镜像拉取）。
const triggerTimeout = 30 * time.Minute

// composeFilePath 返回平台 compose 模板位置（配置优先，缺省用默认值）。
func (o *Orchestrator) composeFilePath() string {
	if p := o.cfg.Orchestration.Container.ComposeFile; p != "" {
		return p
	}
	return defaultComposeFile
}

// publishedImageRefs 提取全部清单中已发布的 registry 镜像引用
//（digest 优先，见 registryPullRef）。清单 container 段承载"要触发
// 什么"：只有 source=registry 是已发布引用（build/local_image 是本地
// 来源，不属于一键触发的消费范围）。
func publishedImageRefs(ms []*manifest.Manifest) map[string]string {
	refs := make(map[string]string)
	for _, m := range ms {
		if m.IsExternal() || m.IsPlugin() {
			continue
		}
		if m.Container.Image.Source != "registry" || m.Container.Image.Repo == "" {
			continue
		}
		refs[m.Project] = registryPullRef(m.Container.Image)
	}
	return refs
}

// composeTemplateServices 返回平台 compose 模板声明的服务名集合。
// 以 compose 自身为权威（`config --services`）：锚点/别名、变量插值等
// 边界全部由 compose 处理，编排器不重新解析 compose YAML。
func (o *Orchestrator) composeTemplateServices(ctx context.Context, path string) (map[string]bool, error) {
	names, err := o.docker.ComposeServices(ctx, []string{path})
	if err != nil {
		return nil, fmt.Errorf("compose 模板服务发现失败（%s）: %v", path, err)
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set, nil
}

// renderComposeOverride 生成 override YAML：把模板中与项目名同名的服务
// 的 image 钉到清单声明的已发布引用（digest 优先）。只覆盖 image，
// 其余接线（env/配置模板/端口/依赖）沿用模板——模板是整套容器形态的
// 事实来源，清单只贡献镜像引用。
func renderComposeOverride(runID string, pinned map[string]string) string {
	var b strings.Builder
	b.WriteString("# 由 HA-Services-Deployer 一键触发自动生成（run " + runID + "）——勿手工编辑。\n")
	b.WriteString("# 镜像引用来自各项目 deploy-manifest.yaml 的已发布引用（digest 优先）。\n")
	b.WriteString("services:\n")
	names := make([]string, 0, len(pinned))
	for n := range pinned {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString(fmt.Sprintf("  %s:\n    image: %s\n", n, pinned[n]))
	}
	return b.String()
}

// tailText 截取输出尾部用于运行记录（避免超长输出撑爆记录文件）。
func tailText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-max:]
}

// TriggerContainerStack 一键触发整套容器编排（容器路径的默认交互）：
// 按清单已发布镜像引用生成 override → 叠加平台 compose 模板 →
// `docker compose up -d` → 状态快照 → 如实回报。异步执行，进度经
// GetRun/CurrentRun 查询（运行类型 container-trigger），单飞约束与
// 部署/回滚共享。
//
// 编排器在此路径不做逐容器的创建编排、健康轮询或回滚——那是
// runtime=container 的容器级管理（可选能力）的职责。
func (o *Orchestrator) TriggerContainerStack() (*Run, error) {
	ms, rep := o.Load()
	if !rep.OK() {
		return nil, &ErrValidation{Errors: rep.Errors}
	}

	composeFile := o.composeFilePath()
	if _, err := os.Stat(composeFile); err != nil {
		return nil, fmt.Errorf("平台 compose 模板不可用（%s）: %v", composeFile, err)
	}
	if err := o.docker.Info(context.Background()); err != nil {
		return nil, fmt.Errorf("docker daemon unreachable or not permitted: %v", err)
	}
	services, err := o.composeTemplateServices(context.Background(), composeFile)
	if err != nil {
		return nil, err
	}

	refs := publishedImageRefs(ms)
	versions := make(map[string]string, len(ms))
	for _, m := range ms {
		versions[m.Project] = m.ServiceVersion
	}
	pinned := make(map[string]string)
	var noService []string
	for p, ref := range refs {
		if services[p] {
			pinned[p] = ref
		} else {
			noService = append(noService, p)
		}
	}
	if len(pinned) == 0 {
		return nil, fmt.Errorf("无可触发的已发布镜像引用：没有清单声明 source=registry 的容器镜像且项目名在 compose 模板（%s）中", composeFile)
	}

	o.mu.Lock()
	if o.current != nil && o.current.State == "running" {
		o.mu.Unlock()
		return nil, ErrRunInProgress
	}
	order := make([]string, 0, len(pinned))
	for p := range pinned {
		order = append(order, p)
	}
	sort.Strings(order)
	run := &Run{
		ID:        o.newRunID("-ct"),
		Type:      "container-trigger",
		State:     "running",
		Order:     order,
		Results:   map[string]*ProjectResult{},
		StartedAt: time.Now(),
	}
	o.current = run
	o.mu.Unlock()

	if len(noService) > 0 {
		sort.Strings(noService)
		run.Notes = append(run.Notes, fmt.Sprintf(
			"以下项目声明了已发布镜像引用但 compose 模板（%s）无同名服务，未触发：%s",
			composeFile, strings.Join(noService, ", ")))
	}
	// 模板有服务但清单无已发布引用的，按模板自身声明的镜像执行
	//（模板默认形态同样钉已发布 digest），记入备注保证可见。
	var templateOnly []string
	for s := range services {
		if _, ok := refs[s]; !ok {
			templateOnly = append(templateOnly, s)
		}
	}
	if len(templateOnly) > 0 {
		sort.Strings(templateOnly)
		run.Notes = append(run.Notes, fmt.Sprintf(
			"以下 compose 服务无清单已发布镜像引用，按模板自身声明的镜像执行（未经清单覆盖）：%s",
			strings.Join(templateOnly, ", ")))
	}
	_ = o.store.SaveRun(run)

	snap := snapshotRun(run)
	go o.executeContainerTrigger(run, composeFile, pinned, versions)
	return snap, nil
}

// executeContainerTrigger 执行触发：写 override → compose up → ps 快照。
// 每一步如实记录；compose 失败即运行失败，不做任何静默降级。
func (o *Orchestrator) executeContainerTrigger(run *Run, composeFile string, pinned map[string]string, versions map[string]string) {
	ctx, cancel := context.WithTimeout(context.Background(), triggerTimeout)
	defer cancel()

	now := time.Now()
	for p, ref := range pinned {
		run.Results[p] = &ProjectResult{
			Project:         p,
			Outcome:         OutcomeTriggered,
			DeclaredVersion: versions[p],
			Detail:          "image pinned: " + ref,
			StartedAt:       now,
			FinishedAt:      now,
		}
	}
	stack := &ProjectResult{Project: "compose-stack", StartedAt: now}
	run.Results["compose-stack"] = stack
	addStep := func(name string, err error, detail string) bool {
		step := StepRecord{Name: name, OK: err == nil, Detail: detail}
		if err != nil {
			step.Detail = fmt.Sprintf("%s: %v", detail, err)
		}
		stack.Steps = append(stack.Steps, step)
		return err == nil
	}

	// 1. override 落盘（状态目录下的 container-trigger/，可回溯）。
	overrideDir := filepath.Join(o.store.Dir(), "container-trigger")
	if err := os.MkdirAll(overrideDir, 0o755); err != nil {
		addStep("write-override", err, overrideDir)
		o.finishTriggerRun(run)
		return
	}
	overridePath := filepath.Join(overrideDir, "override-"+run.ID+".yml")
	if err := os.WriteFile(overridePath, []byte(renderComposeOverride(run.ID, pinned)), 0o644); !addStep("write-override", err, overridePath) {
		o.finishTriggerRun(run)
		return
	}

	// 2. 交给 compose：整套生命周期由它执行。
	files := []string{composeFile, overridePath}
	out, err := o.docker.ComposeUp(ctx, files)
	if !addStep("compose-up", err, fmt.Sprintf("docker compose -f %s -f %s up -d\n%s", composeFile, overridePath, tailText(out, 4000))) {
		stack.Outcome = OutcomeFailed
		stack.Detail = "compose up failed"
		o.finishTriggerRun(run)
		return
	}

	// 3. 状态快照：触发后的事实（健康与否由 compose 自身的健康检查
	// 持续判定；编排器不接管轮询）。
	psOut, psErr := o.docker.ComposePs(ctx, files)
	addStep("compose-ps", psErr, tailText(psOut, 4000))
	stack.Outcome = OutcomeTriggered
	stack.Detail = fmt.Sprintf("compose up ok (%d image reference(s) pinned by manifests); snapshot:\n%s", len(pinned), tailText(psOut, 2000))
	stack.FinishedAt = time.Now()
	o.finishTriggerRun(run)
}

// finishTriggerRun 收尾：落运行记录、更新当前运行指针。触发运行不写
// GlobalState（状态机属于裸金属/容器级管理两条执行路径）。
func (o *Orchestrator) finishTriggerRun(run *Run) {
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
	o.mu.Lock()
	if o.current == run {
		o.current = run
	}
	o.mu.Unlock()
}
