package orchestrator

// 容器运行时流水线。与裸金属的对应关系：
//   - releases/<v>/ + current 软链  ->  镜像标签 <project>:<v>（镜像即版本）；
//   - Tinyvisor 重启                ->  同名容器重建（create+start）；
//   - pruneReleases                 ->  pruneImages（保留最近 N 个 + 生效标签）；
//   - shared_data 卷                ->  container.volumes（宿主目录在镜像之外，
//     换镜像不触碰，配置/数据跨升级保留）。
// 直接沿用：运行状态机/续跑、Plan 排序与失败隔离、健康探测、冒烟断言、
// 自动回滚触发条件（健康/冒烟判败）。孤儿清理为本层新增：默认只报不删。
//
// 回滚定位铁律：一律按镜像内容摘要（digest），绝不按标签。标签是可移动
// 的引用——同版本重打一次标签，按标签回滚就会静默落到另一份内容上，
// 且不会报任何错，这是最坏的一类静默错误。因此：运行记录存 digest
// （ProjectResult.ActiveImageID）；回滚先校验该 digest 的镜像仍在、再按
// digest 重建（docker create 接受 sha256 ID 作为镜像引用）；镜像已不存在
// 必须明确报"目标镜像已不存在"，绝不退回去用标签、绝不落到别的东西上。
// 标签在记录里只作为人类可读的名字出现。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"HA-Cupbearer/internal/docker"
	"HA-Cupbearer/internal/manifest"
)

// containerKeepImages 是镜像保留数（类比 keep_releases）。
const containerKeepImages = 5

// containerImageRef 给出容器项目的镜像标签：<project>:<service_version>。
func containerImageRef(m *manifest.Manifest) string {
	return m.Project + ":" + m.ServiceVersion
}

// registryPullRef 组装 registry 拉取引用：声明了 digest 一律
// repo@sha256:...（digest 优先，tag 可变、只作可读别名）；
// 仅有 tag 时退回 repo:tag。
func registryPullRef(img manifest.ImageSpec) string {
	if img.Digest != "" {
		return img.Repo + "@" + img.Digest
	}
	return img.Repo + ":" + img.Tag
}

// containsRepoDigest 判断实际摘要列表是否包含声明的 digest。
// 只比 sha256 值本身，忽略 repo 前缀拼写差异（如 docker.io/ 归一化）。
func containsRepoDigest(digests []string, want string) bool {
	want = strings.ToLower(strings.TrimPrefix(strings.ToLower(want), "sha256:"))
	for _, d := range digests {
		if _, value, ok := strings.Cut(d, "@"); ok {
			if strings.TrimPrefix(strings.ToLower(value), "sha256:") == want {
				return true
			}
		}
	}
	return false
}

// shortID 截取容器/镜像 ID 用于可读记录。
func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// containerActiveVersionAndImage 返回当前容器运行着的版本与镜像摘要
// （无容器时均为空）。按 digest 定位创建的容器（钉死回滚的产物）其镜像
// 引用就是摘要本身，此时版本经"指向同一摘要的标签"反查——标签只是
// 人类可读的名字，反查不到也不影响定位。
func (o *Orchestrator) containerActiveVersionAndImage(ctx context.Context, m *manifest.Manifest) (string, string) {
	cur, err := o.docker.Inspect(ctx, m.Project)
	if err != nil {
		return "", ""
	}
	version := ""
	if rest, ok := strings.CutPrefix(cur.Image, m.Project+":"); ok {
		version = rest
	} else if tags, terr := o.docker.ImageTags(ctx, m.Project); terr == nil {
		for _, t := range tags {
			if id, ierr := o.docker.ImageID(ctx, m.Project+":"+t); ierr == nil && id == cur.ImageID {
				version = t
				break
			}
		}
	}
	return version, cur.ImageID
}

// containerActiveVersion 返回当前容器运行着的版本（无容器返回空）。
func (o *Orchestrator) containerActiveVersion(ctx context.Context, m *manifest.Manifest) string {
	v, _ := o.containerActiveVersionAndImage(ctx, m)
	return v
}

// deployContainer 是容器项目的完整流水线：
// 备镜像 → 摘要解析 → 卷目录 → 幂等判定 → 重建 → 健康判定 → 冒烟 → 旧镜像清理
// （健康/冒烟判败时自动回滚，目标按切换前的镜像摘要钉死）。
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
		// digest 优先：声明了 digest 就按 repo@sha256:... 拉——拉取阶段即
		// 钉死内容，绝不"按 tag 拉完再比对"（tag 可变，比对发生在内容已
		// 落地之后，是事后补救）。仅声明 tag（schema 允许、不推荐）才按
		// tag 拉。鉴权沿用宿主 docker 登录态，编排器不持有任何凭据。
		ref := registryPullRef(m.Container.Image)
		if err := o.docker.Pull(ctx, ref); !addStep("image", err, "pull "+ref) {
			res.Outcome = OutcomeFailed
			res.Detail = "registry image pull failed"
			return
		}
		// 对照校验：拉到的镜像实际 registry 摘要必须包含清单声明的 digest，
		// 不一致响亮失败、绝不继续——防止清单漂移或 registry 侧内容被换。
		if m.Container.Image.Digest != "" {
			digests, derr := o.docker.RepoDigests(ctx, ref)
			if derr != nil {
				addStep("image", derr, "inspect repo digests of "+ref)
				res.Outcome = OutcomeFailed
				res.Detail = "pulled image digest verification failed"
				return
			}
			if !containsRepoDigest(digests, m.Container.Image.Digest) {
				err := fmt.Errorf("image digest mismatch: manifest declares %s, pulled image repo digests are %v — refusing to continue",
					m.Container.Image.Digest, digests)
				addStep("image", err, "verify digest of "+ref)
				res.Outcome = OutcomeFailed
				res.Detail = "pulled image digest does not match manifest declaration"
				return
			}
		}
		// 打本地编排标签 <project>:<version>（镜像即版本，与 build/local_image
		// 同语义；后续幂等/回滚/清理都走这个标签与它背后的摘要）。
		if err := o.docker.Tag(ctx, ref, tag); !addStep("image", err, fmt.Sprintf("tag %s -> %s", ref, tag)) {
			res.Outcome = OutcomeFailed
			res.Detail = "registry image tagging failed"
			return
		}
	default:
		addStep("image", fmt.Errorf("unknown image source %q", m.Container.Image.Source), "")
		res.Outcome = OutcomeFailed
		res.Detail = "invalid image source"
		return
	}

	// 1.5 解析目标镜像的内容摘要：幂等判定与运行记录都以摘要为准。
	// 标签可以重打移动（指向另一份内容），摘要不会撒谎。
	targetImageID, err := o.docker.ImageID(ctx, tag)
	if !addStep("image-id", err, fmt.Sprintf("resolve digest of %s -> %s", tag, shortID(targetImageID))) {
		res.Outcome = OutcomeFailed
		res.Detail = "image digest resolution failed"
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

	// 3. 幂等：容器在 + 运行镜像的内容摘要与目标一致 + 健康 → 零动作，容器不重建。
	// 按摘要而非引用字符串比较：钉死回滚创建的容器其引用是摘要本身，
	// 只要内容一致就应判幂等；反之标签即使同名，内容变了就必须重建。
	if cur, ierr := o.docker.Inspect(ctx, m.Project); ierr == nil && cur.ImageID == targetImageID {
		if o.probeOnce(ctx, m, eff.healthPort) == nil {
			res.Outcome = OutcomeUpToDate
			res.ActiveVersion = m.ServiceVersion
			res.ActiveImageID = cur.ImageID
			res.Detail = fmt.Sprintf("container %s on %s (image id %s) healthy: no action", shortID(cur.ID), tag, shortID(cur.ImageID))
			addStep("activate", nil, fmt.Sprintf("skipped (up-to-date, container %s not recreated)", shortID(cur.ID)))
			o.pruneImages(ctx, m, tag, res)
			return
		}
	}

	// 切换前状态：回滚目标（版本 + 摘要，摘要才是定位依据）与证据（容器 ID + 镜像摘要）。
	activeBefore, activeBeforeImageID := o.containerActiveVersionAndImage(ctx, m)
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
	res.ActiveImageID = targetImageID

	// 5. 健康判定。
	if err := o.probeUntilHealthy(ctx, m, eff.healthPort); !addStep("health", err, "probe until healthy") {
		res.Outcome = OutcomeFailed
		res.Detail = "health check failed after container start"
		o.autoRollbackContainer(ctx, m, eff, res, activeBefore, activeBeforeImageID, targetImageID)
		return
	}

	// 6. 冒烟验证（复用裸金属同一套）。
	if len(m.Facts.Smoke) > 0 {
		if err := o.runSmoke(ctx, m, eff.healthPort); !addStep("smoke", err, fmt.Sprintf("%d smoke check(s)", len(m.Facts.Smoke))) {
			res.Outcome = OutcomeFailed
			res.Detail = "smoke verification failed after health"
			o.autoRollbackContainer(ctx, m, eff, res, activeBefore, activeBeforeImageID, targetImageID)
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

// autoRollbackContainer 健康/冒烟判败时重建回切换前的镜像。
// 目标按切换前的镜像摘要钉死，绝不按标签：标签是可移动的引用，同版本
// 重打一次标签，按标签回滚就会静默落到另一份内容上。摘要相同则无需
// 回滚（内容没变）；目标摘要的镜像已不存在则明确拒绝，不退化为任何
// 其它镜像。证据（新旧容器 ID + 镜像摘要）记入步骤，等价裸金属的软链回切。
func (o *Orchestrator) autoRollbackContainer(ctx context.Context, m *manifest.Manifest, eff effective, res *ProjectResult, activeBefore, activeBeforeImageID, targetImageID string) {
	if o.runAutoRollbackDisabled() {
		return
	}
	if activeBeforeImageID == "" {
		res.Detail += "; auto-rollback skipped: no previous image"
		return
	}
	if activeBeforeImageID == targetImageID {
		res.Detail += "; auto-rollback skipped: previous image content identical to current"
		return
	}
	if exists, err := o.docker.ImageExists(ctx, activeBeforeImageID); err != nil || !exists {
		res.Detail += fmt.Sprintf("; auto-rollback skipped: 目标镜像已不存在（摘要 %s），拒绝回滚到任何其它镜像", shortID(activeBeforeImageID))
		return
	}
	if err := o.docker.Remove(ctx, m.Project); err != nil {
		res.Detail += "; auto-rollback remove failed: " + err.Error()
		return
	}
	id, err := o.docker.Create(ctx, o.containerCreateOptions(m, eff, activeBeforeImageID))
	if err != nil {
		res.Detail += "; auto-rollback create failed: " + err.Error()
		return
	}
	if err := o.docker.Start(ctx, m.Project); err != nil {
		res.Detail += "; auto-rollback start failed: " + err.Error()
		return
	}
	detail := fmt.Sprintf("recreated on digest %s (version %s) (container %s", shortID(activeBeforeImageID), orNone(activeBefore), shortID(id))
	if cur, ierr := o.docker.Inspect(ctx, m.Project); ierr == nil {
		detail += ", image id " + shortID(cur.ImageID)
	}
	res.ActiveVersion = activeBefore
	res.ActiveImageID = activeBeforeImageID
	res.Detail += fmt.Sprintf("; auto-rolled back to version %s (pinned by digest %s)", orNone(activeBefore), shortID(activeBeforeImageID))
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

// executeRollbackContainer 执行容器项目的一键回滚：
// 按目标版本在运行记录中钉死的镜像摘要重建（标签只是人类可读的名字）。
// cascade 时连带处理直接依赖方：容器依赖方按其自身当前运行摘要重建、
// 裸金属依赖方经 Tinyvisor 重启——两侧都回到一致且健康的状态，
// 绝不存在"接受 cascade 参数却只回滚一个"的静默降级。
func (o *Orchestrator) executeRollbackContainer(run *Run, m *manifest.Manifest, eff effective, target, targetDigest string, cascade bool) {
	ctx := context.Background()
	now := time.Now()
	res := &ProjectResult{Project: m.Project, DeclaredVersion: m.ServiceVersion, StartedAt: now}
	activeBefore, activeBeforeImageID := o.containerActiveVersionAndImage(ctx, m)

	finishFailed := func(detail string) {
		res.Outcome = OutcomeFailed
		res.Detail = detail
		res.FinishedAt = time.Now()
		run.Results[m.Project] = res
		o.finishRollbackRun(run)
	}

	if activeBeforeImageID != "" {
		res.Steps = append(res.Steps, StepRecord{Name: "inspect-before", OK: true,
			Detail: fmt.Sprintf("version %s, image digest %s", orNone(activeBefore), shortID(activeBeforeImageID))})
	}

	// 复核目标镜像仍在（入口校验与执行之间存在时间窗）。已不存在则明确
	// 报错——绝不退回按标签定位，绝不落到别的东西上。
	if exists, err := o.docker.ImageExists(ctx, targetDigest); err != nil || !exists {
		finishFailed(fmt.Sprintf("目标镜像已不存在：版本 %s 的镜像摘要 %s 不在本机——拒绝回滚，不会回滚到任何其它镜像", target, shortID(targetDigest)))
		return
	}
	if err := o.docker.Remove(ctx, m.Project); err != nil {
		finishFailed("remove old container failed: " + err.Error())
		return
	}
	res.Steps = append(res.Steps, StepRecord{Name: "remove-old", OK: true, Detail: m.Project})
	id, err := o.docker.Create(ctx, o.containerCreateOptions(m, eff, targetDigest))
	if err != nil {
		finishFailed("container create failed: " + err.Error())
		return
	}
	if err := o.docker.Start(ctx, m.Project); err != nil {
		finishFailed("container start failed: " + err.Error())
		return
	}
	res.Steps = append(res.Steps, StepRecord{Name: "recreate", OK: true,
		Detail: fmt.Sprintf("%s (%s) on %s (pinned by digest %s)", m.Project, shortID(id), m.Project+":"+target, shortID(targetDigest))})

	if err := o.probeUntilHealthy(ctx, m, eff.healthPort); err != nil {
		res.ActiveVersion = target
		res.ActiveImageID = targetDigest
		finishFailed("health check failed after rollback: " + err.Error())
		return
	}
	res.Steps = append(res.Steps, StepRecord{Name: "health", OK: true, Detail: "healthy after rollback"})

	if len(m.Facts.Smoke) > 0 {
		if err := o.runSmoke(ctx, m, eff.healthPort); err != nil {
			res.ActiveVersion = target
			res.ActiveImageID = targetDigest
			finishFailed(fmt.Sprintf("rolled back to %s, healthy, but smoke failed: %v", target, err))
			return
		}
		res.Steps = append(res.Steps, StepRecord{Name: "smoke", OK: true,
			Detail: fmt.Sprintf("%d smoke check(s) passed after rollback", len(m.Facts.Smoke))})
	}

	res.Outcome = OutcomeRolledBack
	res.ActiveVersion = target
	res.ActiveImageID = targetDigest
	detail := fmt.Sprintf("rolled back %s -> %s (pinned by digest %s), healthy", orNone(activeBefore), target, shortID(targetDigest))
	if cur, ierr := o.docker.Inspect(ctx, m.Project); ierr == nil {
		detail = fmt.Sprintf("rolled back %s -> %s (container %s, image id %s, pinned by digest %s), healthy",
			orNone(activeBefore), target, shortID(cur.ID), shortID(cur.ImageID), shortID(targetDigest))
	}
	res.Detail = detail
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
			depRes := o.restartDependentAny(ctx, dep)
			run.Results[depName] = depRes
			_ = o.store.SaveRun(run)
		}
	}
	o.finishRollbackRun(run)
}

// restartDependentAny 按运行时执行 cascade 依赖方重启：
// 裸金属经 Tinyvisor 重启；容器按其当前运行镜像的摘要重建（钉死，
// 不受标签移动影响）。两侧都必须回到健康（含冒烟）才算连带完成。
func (o *Orchestrator) restartDependentAny(ctx context.Context, rawDep *manifest.Manifest) *ProjectResult {
	m := o.resolveManifest(rawDep)
	if o.effective(m).runtime == "container" {
		return o.restartDependentContainer(ctx, m)
	}
	return o.restartDependent(ctx, rawDep)
}

// restartDependentContainer 连带重建容器依赖方：依赖方不换版本，
// 只按自己当前运行的镜像摘要重建，恢复与被回滚上游的一致连接。
// 当前镜像已不存在则明确拒绝（不退化到任何标签）。
func (o *Orchestrator) restartDependentContainer(ctx context.Context, m *manifest.Manifest) *ProjectResult {
	res := &ProjectResult{Project: m.Project, DeclaredVersion: m.ServiceVersion, StartedAt: time.Now()}
	eff := o.effective(m)

	cur, err := o.docker.Inspect(ctx, m.Project)
	if err != nil {
		res.Outcome = OutcomeFailed
		res.Detail = "cascade recreate failed: no existing container to pin: " + err.Error()
		res.FinishedAt = time.Now()
		return res
	}
	pinID := cur.ImageID
	if exists, eerr := o.docker.ImageExists(ctx, pinID); eerr != nil || !exists {
		res.Outcome = OutcomeFailed
		res.Detail = fmt.Sprintf("cascade recreate refused: 目标镜像已不存在（摘要 %s），不会按任何标签重建", shortID(pinID))
		res.FinishedAt = time.Now()
		return res
	}
	if err := o.docker.Remove(ctx, m.Project); err != nil {
		res.Outcome = OutcomeFailed
		res.Detail = "cascade recreate failed: remove: " + err.Error()
		res.FinishedAt = time.Now()
		return res
	}
	id, err := o.docker.Create(ctx, o.containerCreateOptions(m, eff, pinID))
	if err != nil {
		res.Outcome = OutcomeFailed
		res.Detail = "cascade recreate failed: create: " + err.Error()
		res.FinishedAt = time.Now()
		return res
	}
	if err := o.docker.Start(ctx, m.Project); err != nil {
		res.Outcome = OutcomeFailed
		res.Detail = "cascade recreate failed: start: " + err.Error()
		res.FinishedAt = time.Now()
		return res
	}
	if err := o.probeUntilHealthy(ctx, m, eff.healthPort); err != nil {
		res.Outcome = OutcomeFailed
		res.Detail = "cascade health check failed: " + err.Error()
		res.ActiveImageID = pinID
		res.FinishedAt = time.Now()
		return res
	}
	if len(m.Facts.Smoke) > 0 {
		if err := o.runSmoke(ctx, m, eff.healthPort); err != nil {
			res.Outcome = OutcomeFailed
			res.Detail = "cascade smoke verification failed: " + err.Error()
			res.ActiveImageID = pinID
			res.FinishedAt = time.Now()
			return res
		}
	}
	version, _ := o.containerActiveVersionAndImage(ctx, m)
	res.Outcome = OutcomeDeployed
	res.ActiveVersion = version
	res.ActiveImageID = pinID
	res.Detail = fmt.Sprintf("cascade recreated on digest %s (container %s), healthy", shortID(pinID), shortID(id))
	res.FinishedAt = time.Now()
	return res
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
