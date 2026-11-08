package orchestrator

// 部署前置检查（preflight）：把能在开局查出来的问题一次查全、一次报全，
// 而不是让人跑到流水线第三步才撞见。
//
// 输出分级：
//   - blocker（阻断项）——会导致部署失败，直接拒绝执行；
//   - warning（警告项）——能继续但有风险。
// 每条结果必须带具体路径/端口/地址，不允许只说"检查失败"。
//
// 检查面（deploy 模式）：安装根目录/版本目录/共享数据目录可写性、
// current 软链形态、workdir、env 文件存在可读、
// smoke 引用的密钥可在凭据文件解析、目标端口占用（跳过"就是它自己"）、
// 磁盘空间、Manifest 声明的依赖可达性、Tinyvisor 可达与 apiKey 有效。
// rollback 模式不查产物与磁盘（不拉新产物），其余相同。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"

	"HA-Services-Deployer/internal/artifact"
	"HA-Services-Deployer/internal/manifest"
	"HA-Services-Deployer/internal/tinyvisor"
)

// preflightTimeout 限制一次前置检查的总耗时（单项探测本身都是短超时，此为兜底）。
const preflightTimeout = 60 * time.Second

// POSIX access(2) 模式常量（Go 的 syscall 包未导出）。
const (
	accessROK = 0x4 // R_OK
	accessWOK = 0x2 // W_OK
)

// CheckLevel 是检查结果的分级。
type CheckLevel string

const (
	LevelBlocker CheckLevel = "blocker"
	LevelWarning CheckLevel = "warning"
)

// CheckResult 是单条检查结果。失败时 Level 给出分级，Detail 必须落到
// 具体路径/端口/地址。Project 为空表示全局检查（如 Tinyvisor）。
type CheckResult struct {
	Project string     `json:"project,omitempty"`
	Name    string     `json:"name"`
	Level   CheckLevel `json:"level,omitempty"`
	OK      bool       `json:"ok"`
	Detail  string     `json:"detail"`
}

// PreflightReport 是一次前置检查的完整报告。
type PreflightReport struct {
	CheckedAt time.Time     `json:"checked_at"`
	Mode      string        `json:"mode"` // deploy | rollback
	Scope     []string      `json:"scope"`
	Results   []CheckResult `json:"results"`
	Blockers  []CheckResult `json:"blockers"`
	Warnings  []CheckResult `json:"warnings"`
}

func (r *PreflightReport) HasBlockers() bool { return len(r.Blockers) > 0 }

func (r *PreflightReport) add(res CheckResult) {
	if !res.OK && res.Level == "" {
		// 失败必须分级；未分级的一律按阻断处理，宁可拦下也不静默放行。
		res.Level = LevelBlocker
	}
	r.Results = append(r.Results, res)
	if res.OK {
		return
	}
	if res.Level == LevelBlocker {
		r.Blockers = append(r.Blockers, res)
	} else {
		r.Warnings = append(r.Warnings, res)
	}
}

// ErrPreflightBlocked 表示前置检查发现阻断项，运行被拒绝。
// 完整报告随错误返回，端点原样输出。
type ErrPreflightBlocked struct {
	Report *PreflightReport
}

func (e *ErrPreflightBlocked) Error() string {
	return fmt.Sprintf("preflight found %d blocker(s); run refused", len(e.Report.Blockers))
}

// diskSpaceCheck 是磁盘空间预检注入点（真实实现走 artifact 包），测试可替换。
var diskSpaceCheck = artifact.EnsureDiskSpaceFor

// Preflight 对指定范围（项目列表或分组，空=全部）做一次前置检查。
// 只读：不改任何状态，也不受单飞限制（运行进行中也可查）。
func (o *Orchestrator) Preflight(ctx context.Context, projects []string, group, mode string) (*PreflightReport, error) {
	ms, rep := o.Load()
	if !rep.OK() {
		return nil, &ErrValidation{Errors: rep.Errors}
	}
	order, err := Plan(ms, projects, group)
	if err != nil {
		return nil, err
	}
	if mode == "" {
		mode = "deploy"
	}
	return o.preflightScope(ctx, ms, order, mode), nil
}

// preflightScope 对给定执行序做一次查全：全局检查在前，随后逐项目。
func (o *Orchestrator) preflightScope(ctx context.Context, ms []*manifest.Manifest, scope []string, mode string) *PreflightReport {
	report := &PreflightReport{
		CheckedAt: time.Now(),
		Mode:      mode,
		Scope:     append([]string(nil), scope...),
		Results:   []CheckResult{},
		Blockers:  []CheckResult{},
		Warnings:  []CheckResult{},
	}
	byProject := indexByProject(ms)
	scopeSet := make(map[string]bool, len(scope))
	for _, name := range scope {
		scopeSet[name] = true
	}

	resolved := make([]*manifest.Manifest, 0, len(scope))
	needTinyvisor := false
	needDocker := false
	for _, name := range scope {
		m := o.resolveManifest(byProject[name])
		resolved = append(resolved, m)
		eff := o.effective(m)
		if m.IsExternal() {
			continue
		}
		if eff.runtime == "container" {
			needDocker = true
		} else if eff.manager == "tinyvisor" {
			needTinyvisor = true
		}
	}

	if needTinyvisor {
		o.checkTinyvisor(ctx, report)
	}
	if needDocker {
		o.checkDocker(ctx, report)
	}

	for _, m := range resolved {
		if m.IsExternal() {
			o.checkExternalSelf(ctx, m, report)
			continue
		}
		eff := o.effective(m)
		if eff.runtime == "container" {
			if mode == "deploy" {
				o.checkContainerImage(m, report)
			}
			o.checkContainerVolumes(m, eff, report)
			o.checkSmokeSecrets(m, report)
			o.checkPorts(ctx, m, eff, report)
			o.checkDependencies(ctx, m, byProject, scopeSet, report)
			continue
		}
		if eff.manager != "tinyvisor" {
			// foreign：编排器只读，没有写入动作可检查。
			report.add(CheckResult{Project: m.Project, Name: "foreign", OK: true,
				Detail: fmt.Sprintf("manager=%s: read-only, write-side checks skipped", eff.manager)})
			continue
		}
		o.checkInstallPaths(m, eff, mode, report)
		o.checkEnvFile(m, report)
		o.checkSmokeSecrets(m, report)
		o.checkPorts(ctx, m, eff, report)
		if mode == "deploy" {
			o.checkDiskSpace(ctx, m, eff, report)
		}
		o.checkDependencies(ctx, m, byProject, scopeSet, report)
	}
	return report
}

// checkDocker 检查容器运行时可达且当前用户有权操作（全局一次）。
func (o *Orchestrator) checkDocker(ctx context.Context, report *PreflightReport) {
	if err := o.docker.Info(ctx); err != nil {
		report.add(CheckResult{Name: "docker", Level: LevelBlocker,
			Detail: fmt.Sprintf("docker daemon unreachable or not permitted: %v", err)})
		return
	}
	report.add(CheckResult{Name: "docker", OK: true, Detail: "daemon reachable with current credentials"})
}

// checkContainerImage 按镜像来源检查部署可行性：
// build 要构建上下文与 Dockerfile 在场；local_image 要压缩包与旁路校验和；
// registry 本轮未实现运行时拉取，如实报阻断。
func (o *Orchestrator) checkContainerImage(m *manifest.Manifest, report *PreflightReport) {
	img := m.Container.Image
	switch img.Source {
	case "build":
		repoDir := filepath.Dir(m.SourcePath)
		contextDir := filepath.Join(repoDir, img.Context)
		if info, err := os.Stat(contextDir); err != nil || !info.IsDir() {
			report.add(CheckResult{Project: m.Project, Name: "image", Level: LevelBlocker,
				Detail: fmt.Sprintf("build context %s missing or not a directory", contextDir)})
			return
		}
		dockerfile := img.Dockerfile
		if dockerfile == "" {
			dockerfile = "Dockerfile"
		}
		if _, err := os.Stat(filepath.Join(contextDir, dockerfile)); err != nil {
			report.add(CheckResult{Project: m.Project, Name: "image", Level: LevelBlocker,
				Detail: fmt.Sprintf("dockerfile %s missing in build context %s", dockerfile, contextDir)})
			return
		}
		report.add(CheckResult{Project: m.Project, Name: "image", OK: true,
			Detail: fmt.Sprintf("build context %s with %s present", contextDir, dockerfile)})
	case "local_image":
		if _, err := os.Stat(img.LocalPath); err != nil {
			report.add(CheckResult{Project: m.Project, Name: "image", Level: LevelBlocker,
				Detail: fmt.Sprintf("local_image tarball %s missing", img.LocalPath)})
			return
		}
		if _, err := os.Stat(img.LocalPath + ".sha256"); err != nil {
			report.add(CheckResult{Project: m.Project, Name: "image", Level: LevelBlocker,
				Detail: fmt.Sprintf("local_image checksum sidecar %s.sha256 missing", img.LocalPath)})
			return
		}
		report.add(CheckResult{Project: m.Project, Name: "image", OK: true,
			Detail: fmt.Sprintf("local_image %s with checksum sidecar present", img.LocalPath)})
	case "registry":
		report.add(CheckResult{Project: m.Project, Name: "image", Level: LevelBlocker,
			Detail: fmt.Sprintf("registry source %q: authenticated pull not implemented this phase (schema/validation only)", img.Repo)})
	default:
		report.add(CheckResult{Project: m.Project, Name: "image", Level: LevelBlocker,
			Detail: fmt.Sprintf("unknown image source %q", img.Source)})
	}
}

// checkContainerVolumes 检查容器根与各卷宿主目录可写（不存在则须可创建）。
func (o *Orchestrator) checkContainerVolumes(m *manifest.Manifest, eff effective, report *PreflightReport) {
	if detail, err := probeWritable(eff.root); err != nil {
		report.add(CheckResult{Project: m.Project, Name: "container-root", Level: LevelBlocker, Detail: err.Error()})
	} else {
		report.add(CheckResult{Project: m.Project, Name: "container-root", OK: true, Detail: detail})
	}
	for i, v := range m.Container.Volumes {
		volDir := filepath.Join(eff.root, v.HostRelative)
		if detail, err := probeWritable(volDir); err != nil {
			report.add(CheckResult{Project: m.Project, Name: "volume", Level: LevelBlocker,
				Detail: fmt.Sprintf("volumes[%d] %s: %v", i, volDir, err)})
		} else {
			report.add(CheckResult{Project: m.Project, Name: "volume", OK: true,
				Detail: fmt.Sprintf("volumes[%d] %s -> %s (%s)", i, volDir, v.Container, briefDetail(detail))})
		}
	}
}

// checkTinyvisor 检查 Tinyvisor 可达与 apiKey 有效（全局一次）。
// /ping 免鉴权探可达；脚本列表走鉴权，401 即密钥无效。
func (o *Orchestrator) checkTinyvisor(ctx context.Context, report *PreflightReport) {
	url := o.cfg.Orchestration.Tinyvisor.URL
	if err := o.tv.Ping(ctx); err != nil {
		report.add(CheckResult{Name: "tinyvisor", Level: LevelBlocker,
			Detail: fmt.Sprintf("tinyvisor unreachable at %s: %v", url, err)})
		return
	}
	if _, err := o.tv.List(ctx); err != nil {
		if errors.Is(err, tinyvisor.ErrUnauthorized) {
			report.add(CheckResult{Name: "tinyvisor", Level: LevelBlocker,
				Detail: fmt.Sprintf("tinyvisor reachable at %s but apiKey is invalid (401 unauthorized)", url)})
			return
		}
		report.add(CheckResult{Name: "tinyvisor", Level: LevelBlocker,
			Detail: fmt.Sprintf("tinyvisor reachable at %s but listing scripts failed: %v", url, err)})
		return
	}
	report.add(CheckResult{Name: "tinyvisor", OK: true,
		Detail: fmt.Sprintf("reachable at %s, apiKey accepted", url)})
}

// checkExternalSelf 检查范围内的 external 项目自身可达性
// （执行期不可达即失败，故为阻断项）。
func (o *Orchestrator) checkExternalSelf(ctx context.Context, m *manifest.Manifest, report *PreflightReport) {
	if err := o.checkExternal(ctx, m); err != nil {
		report.add(CheckResult{Project: m.Project, Name: "reachability", Level: LevelBlocker,
			Detail: fmt.Sprintf("external dependency unreachable: %v", err)})
		return
	}
	report.add(CheckResult{Project: m.Project, Name: "reachability", OK: true,
		Detail: fmt.Sprintf("external reachable (%s)", m.Facts.External.Host)})
}

// checkInstallPaths 检查安装根目录、版本目录可写性与 current 软链形态。
// 不存在的路径向上找最近存在的祖先探写权限：可写即"部署时将被创建"。
func (o *Orchestrator) checkInstallPaths(m *manifest.Manifest, eff effective, mode string, report *PreflightReport) {
	root := eff.root

	if detail, err := probeWritable(root); err != nil {
		report.add(CheckResult{Project: m.Project, Name: "install-root", Level: LevelBlocker, Detail: err.Error()})
	} else {
		report.add(CheckResult{Project: m.Project, Name: "install-root", OK: true, Detail: detail})
	}

	if mode == "deploy" {
		relDir := releaseDir(root, m.ServiceVersion)
		if detail, err := probeWritable(relDir); err != nil {
			report.add(CheckResult{Project: m.Project, Name: "version-dir", Level: LevelBlocker, Detail: err.Error()})
		} else {
			report.add(CheckResult{Project: m.Project, Name: "version-dir", OK: true, Detail: detail})
		}
	}

	// 共享数据目录（声明时）：启动脚本把它注入为进程数据根，部署时创建；
	// 不可创建即阻断（两种模式都查——回滚后进程同样要读写它）。
	if shared := m.Baremetal.Install.SharedData; shared != "" {
		sharedDir := filepath.Join(root, shared)
		if detail, err := probeWritable(sharedDir); err != nil {
			report.add(CheckResult{Project: m.Project, Name: "shared-data", Level: LevelBlocker, Detail: err.Error()})
		} else {
			report.add(CheckResult{Project: m.Project, Name: "shared-data", OK: true, Detail: detail})
		}
	}

	link := currentLink(root)
	if info, err := os.Lstat(link); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			report.add(CheckResult{Project: m.Project, Name: "current-link", Level: LevelBlocker,
				Detail: fmt.Sprintf("%s exists but is not a symlink (%s); atomic switch impossible", link, info.Mode())})
		} else {
			report.add(CheckResult{Project: m.Project, Name: "current-link", OK: true, Detail: link + " is a symlink"})
		}
	} else if os.IsNotExist(err) {
		report.add(CheckResult{Project: m.Project, Name: "current-link", OK: true,
			Detail: link + " absent (first deploy creates it)"})
	} else {
		report.add(CheckResult{Project: m.Project, Name: "current-link", Level: LevelBlocker,
			Detail: fmt.Sprintf("lstat %s: %v", link, err)})
	}

	// workdir：Tinyvisor 登记的工作目录就是部署根。流水线先创建目录再登记，
	// 因此结论与安装根一致——不可创建才阻断。
	if detail, err := probeWritable(root); err != nil {
		report.add(CheckResult{Project: m.Project, Name: "workdir", Level: LevelBlocker,
			Detail: fmt.Sprintf("workdir %s cannot be created: %v", root, err)})
	} else {
		report.add(CheckResult{Project: m.Project, Name: "workdir", OK: true,
			Detail: fmt.Sprintf("workdir %s (%s); created by pipeline before script registration", root, briefDetail(detail))})
	}
}

// briefDetail 截取检查结论的主句，避免嵌套详情过长。
func briefDetail(detail string) string {
	for i, r := range detail {
		if r == '(' {
			return detail[:i]
		}
	}
	return detail
}

// probeWritable 校验路径可写并给出人类可读结论：
// 路径存在须是目录且可写；不存在则向上找最近存在的祖先探写权限。
func probeWritable(path string) (string, error) {
	p := path
	for {
		info, err := os.Stat(p)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%s exists but is not a directory", p)
			}
			if err := syscall.Access(p, accessWOK); err != nil {
				return "", fmt.Errorf("%s is not writable", p)
			}
			if p == path {
				return path + " exists and is writable", nil
			}
			return fmt.Sprintf("%s will be created (nearest existing parent %s is writable)", path, p), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat %s: %v", p, err)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fmt.Errorf("%s does not exist and has no creatable parent", path)
		}
		p = parent
	}
}

// checkEnvFile 检查声明的 env 文件：缺失为警告（启动脚本容忍缺失），
// 存在但不可读为阻断（set -e 下 . file 失败即启动失败）。
func (o *Orchestrator) checkEnvFile(m *manifest.Manifest, report *PreflightReport) {
	envFile := m.Baremetal.Process.Tinyvisor.EnvFile
	if envFile == "" {
		return
	}
	info, err := os.Stat(envFile)
	if err != nil {
		if os.IsNotExist(err) {
			report.add(CheckResult{Project: m.Project, Name: "env-file", Level: LevelWarning,
				Detail: fmt.Sprintf("%s missing; startup script tolerates absence (service starts without it)", envFile)})
			return
		}
		report.add(CheckResult{Project: m.Project, Name: "env-file", Level: LevelBlocker,
			Detail: fmt.Sprintf("stat %s: %v", envFile, err)})
		return
	}
	if info.IsDir() {
		report.add(CheckResult{Project: m.Project, Name: "env-file", Level: LevelBlocker,
			Detail: envFile + " is a directory, not a file"})
		return
	}
	if err := syscall.Access(envFile, accessROK); err != nil {
		report.add(CheckResult{Project: m.Project, Name: "env-file", Level: LevelBlocker,
			Detail: fmt.Sprintf("%s exists but is not readable; startup script (set -e) would fail on it", envFile)})
		return
	}
	report.add(CheckResult{Project: m.Project, Name: "env-file", OK: true, Detail: envFile + " exists and is readable"})
}

// checkSmokeSecrets 校验 smoke 引用的 ${secret:<name>} 都能在凭据文件解析。
// 缺失则运行时必失败，报阻断项并点名（部署与回滚都会跑冒烟，两种模式都查）。
func (o *Orchestrator) checkSmokeSecrets(m *manifest.Manifest, report *PreflightReport) {
	if len(m.Facts.Smoke) == 0 {
		return
	}
	missingSet := map[string]bool{}
	collect := func(text string) {
		for _, name := range smokeSecretNames(text) {
			if o.creds == nil || o.creds.SmokeSecrets == nil {
				missingSet[name] = true
				continue
			}
			if v, ok := o.creds.SmokeSecrets[name]; !ok || v == "" {
				missingSet[name] = true
			}
		}
	}
	for _, chk := range m.Facts.Smoke {
		for _, v := range chk.Headers {
			collect(v)
		}
		collect(chk.Body)
	}
	if len(missingSet) > 0 {
		names := make([]string, 0, len(missingSet))
		for n := range missingSet {
			names = append(names, n)
		}
		sort.Strings(names)
		report.add(CheckResult{Project: m.Project, Name: "smoke-secrets", Level: LevelBlocker,
			Detail: fmt.Sprintf("smoke secret(s) %v referenced by facts.smoke not configured in credentials file (smoke_secrets)", names)})
		return
	}
	report.add(CheckResult{Project: m.Project, Name: "smoke-secrets", OK: true,
		Detail: fmt.Sprintf("all smoke secrets for %d check(s) resolve from credentials file", len(m.Facts.Smoke))})
}

// checkPorts 以绑定测试检查声明端口是否被占用；占用者就是项目自己
// （Tinyvisor 状态 running）时不算冲突——部署流水线会重启它。
// 部署上下文覆盖了端口时检查生效端口（与探测/冒烟一致）：
// overrides.ports 按名重映射优先，其次 health_port 对健康端口生效，
// 那才是本上下文里服务真正要绑定的口。
func (o *Orchestrator) checkPorts(ctx context.Context, m *manifest.Manifest, eff effective, report *PreflightReport) {
	selfRunning := false
	if eff.runtime == "container" {
		if cur, err := o.docker.Inspect(ctx, m.Project); err == nil && cur.Running {
			selfRunning = true
		}
	} else if s, err := o.tv.GetByName(ctx, m.Project); err == nil && s.Status == "running" {
		selfRunning = true
	}
	healthPortName := ""
	if m.Facts.Health.HTTP != nil {
		healthPortName = m.Facts.Health.HTTP.PortName
	}
	ovPorts := o.cfg.Orchestration.Overrides[m.Project].Ports
	for _, p := range m.Facts.Ports {
		port := p.Port
		if ovPort, ok := ovPorts[p.Name]; ok && ovPort > 0 {
			port = ovPort
		} else if eff.healthPort > 0 && p.Name == healthPortName {
			port = eff.healthPort
		}
		addr := net.JoinHostPort("0.0.0.0", strconv.Itoa(port))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			report.add(CheckResult{Project: m.Project, Name: "port", OK: true,
				Detail: fmt.Sprintf("port %d (%s) is free", port, p.Name)})
			continue
		}
		if selfRunning {
			report.add(CheckResult{Project: m.Project, Name: "port", OK: true,
				Detail: fmt.Sprintf("port %d (%s) held by %s itself (running; will be restarted/recreated)", port, p.Name, m.Project)})
			continue
		}
		report.add(CheckResult{Project: m.Project, Name: "port", Level: LevelBlocker,
			Detail: fmt.Sprintf("port %d (%s) already in use by another process (bind %s: %v)", port, p.Name, addr, err)})
	}
}

// checkDiskSpace 复用 artifact 包的磁盘预检（含差额报告）。
// 产物大小未知时降级为警告：落盘安全性仍由临时文件 + sha256 保证。
func (o *Orchestrator) checkDiskSpace(ctx context.Context, m *manifest.Manifest, eff effective, report *PreflightReport) {
	destDir := releaseDir(eff.root, m.ServiceVersion)
	checked, err := diskSpaceCheck(ctx, m, destDir, o.creds.GithubToken)
	if err != nil {
		report.add(CheckResult{Project: m.Project, Name: "disk-space", Level: LevelBlocker, Detail: err.Error()})
		return
	}
	if !checked {
		report.add(CheckResult{Project: m.Project, Name: "disk-space", Level: LevelWarning,
			Detail: fmt.Sprintf("artifact size unknown for source %s; disk space not prechecked", m.Baremetal.Artifact.Source)})
		return
	}
	report.add(CheckResult{Project: m.Project, Name: "disk-space", OK: true, Detail: "enough free space for artifact at " + destDir})
}

// checkDependencies 检查 Manifest 声明的依赖服务当前是否可达：
// required 不可达且不在本次范围内 → 阻断（执行期该项目会被跳过）；
// required 不可达但在本次范围内（本运行会先起它）→ 警告；
// optional 不可达 → 警告。
func (o *Orchestrator) checkDependencies(ctx context.Context, m *manifest.Manifest, byProject map[string]*manifest.Manifest, scopeSet map[string]bool, report *PreflightReport) {
	for _, d := range m.Facts.DependsOn {
		depRaw, ok := byProject[d.Project]
		if !ok {
			// 聚合校验应已拦截依赖缺失；双保险。
			report.add(CheckResult{Project: m.Project, Name: "dependency", Level: LevelBlocker,
				Detail: fmt.Sprintf("declared dependency %q not found in aggregated manifests", d.Project)})
			continue
		}
		dep := o.resolveManifest(depRaw)
		err := o.checkDependencyLive(ctx, dep)
		if err == nil {
			report.add(CheckResult{Project: m.Project, Name: "dependency", OK: true,
				Detail: fmt.Sprintf("dependency %s (%s) reachable", d.Project, d.Kind)})
			continue
		}
		if d.Kind == "optional" {
			report.add(CheckResult{Project: m.Project, Name: "dependency", Level: LevelWarning,
				Detail: fmt.Sprintf("optional dependency %s not reachable: %v", d.Project, err)})
			continue
		}
		if scopeSet[d.Project] && !dep.IsExternal() {
			report.add(CheckResult{Project: m.Project, Name: "dependency", Level: LevelWarning,
				Detail: fmt.Sprintf("required dependency %s not reachable now (%v), but it is in this run's scope and will be deployed first", d.Project, err)})
			continue
		}
		report.add(CheckResult{Project: m.Project, Name: "dependency", Level: LevelBlocker,
			Detail: fmt.Sprintf("required dependency %s not reachable and not started by this run: %v", d.Project, err)})
	}
}
