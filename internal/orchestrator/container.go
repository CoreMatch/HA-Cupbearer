package orchestrator

// 容器运行时流水线。与裸金属的对应关系：
//   - releases/<v>/ + current 软链  ->  镜像标签 <project>:<v>（镜像即版本）；
//   - Tinyvisor 重启                ->  同名容器重建（create+start）；
//   - pruneReleases                 ->  pruneImages（保留最近 N 个 + 生效标签）；
//   - shared_data 卷                ->  container.volumes（宿主目录在镜像之外，
//     换镜像不触碰，配置/数据跨升级保留）。
// 直接沿用：运行状态机/续跑、Plan 排序与失败隔离、健康探测、冒烟断言、
// 自动回滚触发条件（健康/冒烟判败）。孤儿清理为本层新增：默认只报不删。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"HA-Services-Deployer/internal/docker"
	"HA-Services-Deployer/internal/manifest"
)

// containerKeepImages 是镜像保留数（类比 keep_releases）。
const containerKeepImages = 5

// containerImageRef 给出容器项目的镜像标签：<project>:<service_version>。
func containerImageRef(m *manifest.Manifest) string {
	return m.Project + ":" + m.ServiceVersion
}

// shortID 截取容器/镜像 ID 用于可读记录。
func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// containerActiveVersion 返回当前容器运行着的版本（无容器返回空）。
func (o *Orchestrator) containerActiveVersion(ctx context.Context, m *manifest.Manifest) string {
	cur, err := o.docker.Inspect(ctx, m.Project)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(cur.Image, m.Project+":")
}

// deployContainer 是容器项目的完整流水线：
// 备镜像 → 卷目录 → 幂等判定 → 重建 → 健康判定 → 冒烟 → 旧镜像清理
// （健康/冒烟判败时自动回滚到切换前的镜像标签）。
func (o *Orchestrator) deployContainer(ctx context.Context, run *Run, m *manifest.Manifest, eff effective, res *ProjectResult) {
	addStep := func(name string, err error, detail string) bool {
		step := StepRecord{Name: name, OK: err == nil, Detail: detail}
		if err != nil {
			step.Detail = fmt.Sprintf("%s: %v", detail, err)
		}
		res.Steps = append(res.Steps, step)
		return err == nil
	}

	tag := containerImageRef(m)

	// 1. 镜像：按来源备齐并打成 <project>:<version> 标签。
	switch m.Container.Image.Source {
	case "build":
		repoDir := filepath.Dir(m.SourcePath)
		contextDir := filepath.Join(repoDir, m.Container.Image.Context)
		err := o.docker.Build(ctx, tag, contextDir, m.Container.Image.Dockerfile)
		if !addStep("image", err, fmt.Sprintf("build %s -> %s", contextDir, tag)) {
			res.Outcome = OutcomeFailed
			res.Detail = "image build failed"
			return
		}
	case "local_image":
		err := verifyLocalImageTarball(m.Container.Image.LocalPath)
		if err == nil {
			err = o.docker.LoadAndTag(ctx, m.Container.Image.LocalPath, tag)
		}
		if !addStep("image", err, fmt.Sprintf("load %s -> %s", m.Container.Image.LocalPath, tag)) {
			res.Outcome = OutcomeFailed
			res.Detail = "image load failed"
			return
		}
	case "registry":
		err := errors.New("registry 来源本轮未实现运行时拉取（仅 schema 与校验）")
		addStep("image", err, m.Container.Image.Repo)
		res.Outcome = OutcomeFailed
		res.Detail = "registry image source not implemented"
		return
	default:
		addStep("image", fmt.Errorf("unknown image source %q", m.Container.Image.Source), "")
		res.Outcome = OutcomeFailed
		res.Detail = "invalid image source"
		return
	}

	// 2. 卷目录：宿主侧幂等创建；版本切换从不触碰其内容。
	for _, v := range m.Container.Volumes {
		volDir := filepath.Join(eff.root, v.HostRelative)
		if err := os.MkdirAll(volDir, 0o755); !addStep("ensure-volumes", err, volDir) {
			res.Outcome = OutcomeFailed
			res.Detail = "volume dir creation failed"
			return
		}
	}

	// 3. 幂等：容器在 + 镜像标签一致 + 健康 → 零动作，容器不重建。
	if cur, ierr := o.docker.Inspect(ctx, m.Project); ierr == nil && cur.Image == tag {
		if o.probeOnce(ctx, m, eff.healthPort) == nil {
			res.Outcome = OutcomeUpToDate
			res.ActiveVersion = m.ServiceVersion
			res.Detail = fmt.Sprintf("container %s on %s healthy: no action", shortID(cur.ID), tag)
			addStep("activate", nil, fmt.Sprintf("skipped (up-to-date, container %s not recreated)", shortID(cur.ID)))
			o.pruneImages(ctx, m, tag, res)
			return
		}
	}

	// 切换前状态：回滚目标与证据（容器 ID + 镜像摘要）。
	activeBefore := o.containerActiveVersion(ctx, m)
	if cur, ierr := o.docker.Inspect(ctx, m.Project); ierr == nil {
		addStep("inspect-before", nil, fmt.Sprintf("container %s on %s (image id %s)", shortID(cur.ID), cur.Image, shortID(cur.ImageID)))
	} else {
		addStep("inspect-before", nil, "no existing container")
	}

	// 4. 重建：删旧 → 建 → 启动。
	if err := o.docker.Remove(ctx, m.Project); !addStep("remove-old", err, m.Project) {
		res.Outcome = OutcomeFailed
		res.Detail = "remove old container failed"
		return
	}
	id, err := o.docker.Create(ctx, o.containerCreateOptions(m, eff, tag))
	if !addStep("create", err, fmt.Sprintf("%s (%s) on %s", m.Project, shortID(id), tag)) {
		res.Outcome = OutcomeFailed
		res.Detail = "container create failed"
		return
	}
	if err := o.docker.Start(ctx, m.Project); !addStep("start", err, m.Project) {
		res.Outcome = OutcomeFailed
		res.Detail = "container start failed"
		return
	}
	res.ActiveVersion = m.ServiceVersion

	// 5. 健康判定。
	if err := o.probeUntilHealthy(ctx, m, eff.healthPort); !addStep("health", err, "probe until healthy") {
		res.Outcome = OutcomeFailed
		res.Detail = "health check failed after container start"
		o.autoRollbackContainer(ctx, m, eff, res, activeBefore)
		return
	}

	// 6. 冒烟验证（复用裸金属同一套）。
	if len(m.Facts.Smoke) > 0 {
		if err := o.runSmoke(ctx, m, eff.healthPort); !addStep("smoke", err, fmt.Sprintf("%d smoke check(s)", len(m.Facts.Smoke))) {
			res.Outcome = OutcomeFailed
			res.Detail = "smoke verification failed after health"
			o.autoRollbackContainer(ctx, m, eff, res, activeBefore)
			return
		}
	}

	// 证据：落容器 ID 与镜像摘要，供核对"跑的确实是这个版本"。
	if cur, ierr := o.docker.Inspect(ctx, m.Project); ierr == nil {
		res.Detail = fmt.Sprintf("container %s on %s (image id %s) active and healthy", shortID(cur.ID), tag, shortID(cur.ImageID))
	} else {
		res.Detail = fmt.Sprintf("container on %s active and healthy", tag)
	}
	o.pruneImages(ctx, m, tag, res)
	res.Outcome = OutcomeDeployed
}

// containerCreateOptions 组装容器创建参数：
// 端口复用 facts.ports（宿主侧经 overrides.ports 重映射；容器口可经
// container_ports 覆盖；loopback 声明绑 127.0.0.1），卷宿主侧落在容器根。
func (o *Orchestrator) containerCreateOptions(m *manifest.Manifest, eff effective, tag string) docker.CreateOptions {
	opts := docker.CreateOptions{
		Name:          m.Project,
		Image:         tag,
		RestartPolicy: "unless-stopped",
		Labels: map[string]string{
			docker.LabelProject:   m.Project,
			docker.LabelManagedBy: "ha-services-deployer",
		},
	}
	ovPorts := o.cfg.Orchestration.Overrides[m.Project].Ports
	for _, p := range m.Facts.Ports {
		hostPort := p.Port
		if rp, ok := ovPorts[p.Name]; ok && rp > 0 {
			hostPort = rp
		}
		containerPort := p.Port
		if cp, ok := m.Container.ContainerPorts[p.Name]; ok && cp > 0 {
			containerPort = cp
		}
		hostIP := ""
		if p.Exposure == "loopback" {
			hostIP = "127.0.0.1"
		}
		opts.Publish = append(opts.Publish, docker.PortMapping{HostIP: hostIP, HostPort: hostPort, ContainerPort: containerPort})
	}
	for _, v := range m.Container.Volumes {
		opts.Volumes = append(opts.Volumes, docker.VolumeMount{
			Host:      filepath.Join(eff.root, v.HostRelative),
			Container: v.Container,
		})
	}
	return opts
}

// autoRollbackContainer 健康/冒烟判败时重建回切换前的镜像标签。
// 证据（新旧容器 ID + 镜像摘要）记入步骤，等价裸金属的软链回切。
func (o *Orchestrator) autoRollbackContainer(ctx context.Context, m *manifest.Manifest, eff effective, res *ProjectResult, activeBefore string) {
	if o.runAutoRollbackDisabled() {
		return
	}
	if activeBefore == "" || activeBefore == m.ServiceVersion {
		res.Detail += "; auto-rollback skipped: no previous image"
		return
	}
	prevTag := m.Project + ":" + activeBefore
	if exists, err := o.docker.ImageExists(ctx, prevTag); err != nil || !exists {
		res.Detail += fmt.Sprintf("; auto-rollback skipped: previous image %s unavailable", prevTag)
		return
	}
	if err := o.docker.Remove(ctx, m.Project); err != nil {
		res.Detail += "; auto-rollback remove failed: " + err.Error()
		return
	}
	id, err := o.docker.Create(ctx, o.containerCreateOptions(m, eff, prevTag))
	if err != nil {
		res.Detail += "; auto-rollback create failed: " + err.Error()
		return
	}
	if err := o.docker.Start(ctx, m.Project); err != nil {
		res.Detail += "; auto-rollback start failed: " + err.Error()
		return
	}
	detail := fmt.Sprintf("recreated on %s (container %s", prevTag, shortID(id))
	if cur, ierr := o.docker.Inspect(ctx, m.Project); ierr == nil {
		detail += ", image id " + shortID(cur.ImageID)
	}
	res.ActiveVersion = activeBefore
	res.Detail += "; auto-rolled back to " + prevTag
	res.Steps = append(res.Steps, StepRecord{Name: "auto-rollback", OK: true, Detail: detail + ")"})
}

// pruneImages 保留最近 containerKeepImages 个标签（docker images 按创建
// 时间倒序）与当前生效标签，删除更旧的。
func (o *Orchestrator) pruneImages(ctx context.Context, m *manifest.Manifest, activeTag string, res *ProjectResult) {
	tags, err := o.docker.ImageTags(ctx, m.Project)
	if err != nil || len(tags) <= containerKeepImages {
		return
	}
	var removed []string
	for _, t := range tags[containerKeepImages:] {
		ref := m.Project + ":" + t
		if ref == activeTag {
			continue
		}
		if err := o.docker.RemoveImage(ctx, ref); err == nil {
			removed = append(removed, ref)
		}
	}
	if len(removed) > 0 {
		res.Steps = append(res.Steps, StepRecord{Name: "prune-images", OK: true, Detail: "removed " + strings.Join(removed, ", ")})
	}
}

// executeRollbackContainer 执行容器项目的一键回滚：重建到目标镜像标签。
// 本轮不做 cascade 连带重启依赖方（容器依赖方的回滚连带另行设计）。
func (o *Orchestrator) executeRollbackContainer(run *Run, m *manifest.Manifest, eff effective, target string) {
	ctx := context.Background()
	now := time.Now()
	res := &ProjectResult{Project: m.Project, DeclaredVersion: m.ServiceVersion, StartedAt: now}
	activeBefore := o.containerActiveVersion(ctx, m)
	prevTag := m.Project + ":" + target

	finishFailed := func(detail string) {
		res.Outcome = OutcomeFailed
		res.Detail = detail
		res.FinishedAt = time.Now()
		run.Results[m.Project] = res
		o.finishRollbackRun(run)
	}

	if exists, err := o.docker.ImageExists(ctx, prevTag); err != nil || !exists {
		finishFailed(fmt.Sprintf("target image %s unavailable", prevTag))
		return
	}
	if err := o.docker.Remove(ctx, m.Project); err != nil {
		finishFailed("remove old container failed: " + err.Error())
		return
	}
	res.Steps = append(res.Steps, StepRecord{Name: "remove-old", OK: true, Detail: m.Project})
	id, err := o.docker.Create(ctx, o.containerCreateOptions(m, eff, prevTag))
	if err != nil {
		finishFailed("container create failed: " + err.Error())
		return
	}
	if err := o.docker.Start(ctx, m.Project); err != nil {
		finishFailed("container start failed: " + err.Error())
		return
	}
	res.Steps = append(res.Steps, StepRecord{Name: "recreate", OK: true, Detail: fmt.Sprintf("%s (%s) on %s", m.Project, shortID(id), prevTag)})

	if err := o.probeUntilHealthy(ctx, m, eff.healthPort); err != nil {
		res.ActiveVersion = target
		finishFailed("health check failed after rollback: " + err.Error())
		return
	}
	res.Steps = append(res.Steps, StepRecord{Name: "health", OK: true, Detail: "healthy after rollback"})

	if len(m.Facts.Smoke) > 0 {
		if err := o.runSmoke(ctx, m, eff.healthPort); err != nil {
			res.ActiveVersion = target
			finishFailed(fmt.Sprintf("rolled back to %s, healthy, but smoke failed: %v", target, err))
			return
		}
		res.Steps = append(res.Steps, StepRecord{Name: "smoke", OK: true,
			Detail: fmt.Sprintf("%d smoke check(s) passed after rollback", len(m.Facts.Smoke))})
	}

	res.Outcome = OutcomeRolledBack
	res.ActiveVersion = target
	detail := fmt.Sprintf("rolled back %s -> %s, healthy", orNone(activeBefore), target)
	if cur, ierr := o.docker.Inspect(ctx, m.Project); ierr == nil {
		detail = fmt.Sprintf("rolled back %s -> %s (container %s, image id %s), healthy",
			orNone(activeBefore), target, shortID(cur.ID), shortID(cur.ImageID))
	}
	res.Detail = detail
	res.FinishedAt = time.Now()
	run.Results[m.Project] = res
	o.finishRollbackRun(run)
}

// reconcileOrphanContainers 列出带本编排器标签、但项目已不在清单中的容器：
// 默认只报不删（run.Notes）；orphan_policy=remove 时删除并记录。
func (o *Orchestrator) reconcileOrphanContainers(ctx context.Context, run *Run, ms []*manifest.Manifest) {
	known := make(map[string]bool, len(ms))
	hasContainerProject := false
	for _, m := range ms {
		known[m.Project] = true
		if o.effective(m).runtime == "container" {
			hasContainerProject = true
		}
	}
	if !hasContainerProject {
		return // 纯裸金属站点不要求 docker daemon 在场
	}
	list, err := o.docker.ListManaged(ctx)
	if err != nil {
		run.Notes = append(run.Notes, fmt.Sprintf("orphan reconcile failed: %v", err))
		return
	}
	for _, c := range list {
		if c.Project == "" || known[c.Project] {
			continue
		}
		if o.cfg.Orchestration.Container.OrphanPolicy == "remove" {
			if err := o.docker.Remove(ctx, c.Name); err != nil {
				run.Notes = append(run.Notes, fmt.Sprintf("orphan container %s (project %s) remove failed: %v", c.Name, c.Project, err))
			} else {
				run.Notes = append(run.Notes, fmt.Sprintf("orphan container %s (project %s not in manifests) removed", c.Name, c.Project))
			}
			continue
		}
		run.Notes = append(run.Notes, fmt.Sprintf("orphan container %s (project %s not in manifests) — report-only, not removed", c.Name, c.Project))
	}
}

// verifyLocalImageTarball 校验 local_image 压缩包：存在 + 旁路 .sha256 一致。
func verifyLocalImageTarball(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("local_image tarball: %w", err)
	}
	sumData, err := os.ReadFile(path + ".sha256")
	if err != nil {
		return fmt.Errorf("local_image checksum sidecar missing: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(sumData)))
	if len(fields) == 0 {
		return fmt.Errorf("local_image checksum file empty: %s.sha256", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != fields[0] {
		return fmt.Errorf("local_image sha256 mismatch: want %s got %s", fields[0], got)
	}
	return nil
}
