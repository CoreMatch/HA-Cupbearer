package manifest

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	validGroups    = map[string]bool{"infra": true, "core": true, "satellite": true, "build": true, "ops": true, "game": true, "edge": true, "web": true}
	validManagers  = map[string]bool{"tinyvisor": true, "aapanel": true, "manual": true}
	validPolicies  = map[string]bool{"never": true, "always": true, "unless-stopped": true, "on-failure": true}
	validSources   = map[string]bool{"ci_release": true, "local_path": true}
	projectNameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	binaryNameRe   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	versionTokenRe = regexp.MustCompile(`\{service_version\}`)
	// smokeSecretRefRe 匹配 ${secret:<name>} 引用；冒烟请求中凡出现 ${...}
	// 必须是该形态，密钥值本身绝不进 Manifest。
	smokeSecretRefRe  = regexp.MustCompile(`^\$\{secret:[A-Za-z0-9_-]+\}$`)
	smokeSecretAnyRe  = regexp.MustCompile(`\$\{secret:[A-Za-z0-9_-]+\}`)
	smokeBraceTokenRe = regexp.MustCompile(`\$\{[^}]*\}`)
	validSmokeMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "DELETE": true}

	// validImageSources 是 container.image.source 的合法取值。
	validImageSources = map[string]bool{"build": true, "local_image": true, "registry": true}
)

// Report 是一次聚合校验的结果。Errors 为空即可执行（OK）；
// Warnings 只提示不阻断。
type Report struct {
	Errors    []string    `json:"errors"`
	Warnings  []string    `json:"warnings"`
	Manifests []*Manifest `json:"-"`
}

func (r *Report) OK() bool { return len(r.Errors) == 0 }

func (r *Report) addf(format string, args ...interface{}) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

func (r *Report) addwf(format string, args ...interface{}) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// AddError / AddWarning 供调用方（编排器）注入上下文相关的结论，
// 如重名取舍在严格开关下的升降级。
func (r *Report) AddError(msg string)   { r.Errors = append(r.Errors, msg) }
func (r *Report) AddWarning(msg string) { r.Warnings = append(r.Warnings, msg) }

// ValidateAggregate 对装载结果做全量校验：单清单字段规则 +
// 跨清单一致性（依赖存在性、依赖环、端口冲突）。
// 入参应已去重（LoadAggregate 按确定性规则取舍重名并返回
// DuplicateGroup；重名的升降级由调用方按开关处理，不在这里硬阻断）。
// 装载错误以 loadErrors 传入，一并体现在报告中。
func ValidateAggregate(manifests []*Manifest, loadErrors []error) *Report {
	rep := &Report{Manifests: manifests, Warnings: []string{}}
	for _, err := range loadErrors {
		rep.addf("load: %v", err)
	}

	byProject := make(map[string]*Manifest)
	for _, m := range manifests {
		validateSingle(rep, m)
		if m.Project == "" {
			continue
		}
		byProject[m.Project] = m
	}

	validatePortsCrossProject(rep, manifests)
	validateDependencies(rep, byProject)
	return rep
}

func validateSingle(rep *Report, m *Manifest) {
	ctx := m.Project
	if ctx == "" {
		ctx = m.SourcePath
	}

	if m.ManifestVersion != SchemaVersion {
		rep.addf("%s: manifest_version 必须为 %d（当前 %d）", ctx, SchemaVersion, m.ManifestVersion)
	}
	switch m.Kind {
	case "service", "external", "plugin":
	default:
		rep.addf("%s: kind 必须为 service、external 或 plugin（当前 %q）", ctx, m.Kind)
	}
	if m.Project == "" {
		rep.addf("%s: project 不能为空", m.SourcePath)
	} else if !projectNameRe.MatchString(m.Project) {
		rep.addf("%s: project 只能包含小写字母、数字与连字符且以字母数字开头", ctx)
	}
	switch m.Runtime {
	case "", "baremetal", "container":
	default:
		rep.addf("%s: runtime 必须为 baremetal 或 container（当前 %q）", ctx, m.Runtime)
	}
	if m.Container.Enabled {
		rep.addf("%s: container.enabled 已退役（2026-11-08 全仓扫描确认无清单在用），请改用顶层 runtime 选择器", ctx)
	}

	if m.IsExternal() {
		validateExternal(rep, m)
		return
	}

	if m.DisplayName == "" {
		rep.addf("%s: display_name 不能为空", ctx)
	}
	if !validGroups[m.Group] {
		rep.addf("%s: group 非法（当前 %q）", ctx, m.Group)
	}
	if m.ServiceVersion == "" {
		rep.addf("%s: service_version 不能为空", ctx)
	}

	if !m.IsPlugin() && m.Plugin != nil {
		rep.addf("%s: 仅 kind=plugin 清单可声明 plugin 段", ctx)
	}

	validateFacts(rep, m)
	if m.IsPlugin() {
		validatePlugin(rep, m)
		return
	}
	// 两段共存、按选择器校验生效的一段；未生效段为信息性声明不校验。
	if m.Runtime == "container" {
		validateContainer(rep, m)
	} else {
		validateBaremetal(rep, m)
	}
}

func validateFacts(rep *Report, m *Manifest) {
	ctx := m.Project

	portNames := make(map[string]bool)
	for _, p := range m.Facts.Ports {
		if p.Name == "" {
			rep.addf("%s: facts.ports 存在无名端口", ctx)
			continue
		}
		if portNames[p.Name] {
			rep.addf("%s: facts.ports 端口名 %q 重复", ctx, p.Name)
		}
		portNames[p.Name] = true
		if p.Port < 1 || p.Port > 65535 {
			rep.addf("%s: facts.ports[%s] 端口越界（%d）", ctx, p.Name, p.Port)
		}
		switch p.Exposure {
		case "", "public", "loopback":
		default:
			rep.addf("%s: facts.ports[%s] exposure 非法（%q）", ctx, p.Name, p.Exposure)
		}
	}

	probe := m.Facts.Health.HTTP
	if probe == nil {
		// 插件型产物没有自己的进程与健康面：豁免。
		if !m.IsPlugin() {
			rep.addf("%s: facts.health.http 必填（编排器健康判定与状态可见性依赖它）", ctx)
		}
	} else {
		if !portNames[probe.PortName] {
			rep.addf("%s: facts.health.http.port_name %q 未在 facts.ports 中声明", ctx, probe.PortName)
		}
		if !strings.HasPrefix(probe.Path, "/") {
			rep.addf("%s: facts.health.http.path 必须以 / 开头", ctx)
		}
		if probe.ExpectStatus == 0 {
			probe.ExpectStatus = 200
		}
		if probe.TimeoutSeconds <= 0 {
			probe.TimeoutSeconds = 5
		}
		if probe.IntervalSeconds <= 0 {
			probe.IntervalSeconds = 2
		}
		if probe.Retries <= 0 {
			probe.Retries = 30
		}
	}

	for _, d := range m.Facts.DependsOn {
		if d.Project == "" {
			rep.addf("%s: facts.depends_on 存在空依赖", ctx)
			continue
		}
		if d.Project == m.Project {
			rep.addf("%s: 不能依赖自身", ctx)
		}
		switch d.Kind {
		case "", "required", "optional":
		default:
			rep.addf("%s: facts.depends_on[%s] kind 非法（%q）", ctx, d.Project, d.Kind)
		}
	}

	switch m.Facts.Migration.Mode {
	case "", "auto_on_startup", "manual", "none":
	default:
		rep.addf("%s: facts.migration.mode 非法（%q）", ctx, m.Facts.Migration.Mode)
	}

	validateSmoke(rep, m, portNames)
}

// validateSmoke 校验 facts.smoke 段：请求要素合法、密钥只以
// ${secret:<name>} 形态出现（值从凭据文件解析，绝不写进 Manifest）。
func validateSmoke(rep *Report, m *Manifest, portNames map[string]bool) {
	for i, s := range m.Facts.Smoke {
		ctx := fmt.Sprintf("%s: facts.smoke[%d]", m.Project, i)
		if s.Name != "" && strings.TrimSpace(s.Name) == "" {
			rep.addf("%s: name 不能为空白", ctx)
		}
		method := strings.ToUpper(s.Method)
		if method == "" {
			method = "GET"
		}
		if !validSmokeMethods[method] {
			rep.addf("%s: method 非法（%q），支持 GET|POST|PUT|DELETE", ctx, s.Method)
		}
		if !strings.HasPrefix(s.Path, "/") {
			rep.addf("%s: path 必须以 / 开头", ctx)
		}
		if s.PortName != "" && !portNames[s.PortName] {
			rep.addf("%s: port_name %q 未在 facts.ports 中声明", ctx, s.PortName)
		}
		if s.ExpectStatus == 0 {
			s.ExpectStatus = 200
		}
		if s.ExpectStatus < 100 || s.ExpectStatus > 599 {
			rep.addf("%s: expect_status 越界（%d）", ctx, s.ExpectStatus)
		}
		if s.TimeoutSeconds < 0 {
			rep.addf("%s: timeout_seconds 不能为负", ctx)
		}
		for k, v := range s.Headers {
			// 头值：要么纯字面量，要么整值密钥引用（不允许半截嵌入，
			// 避免误把密钥片段当普通值提交进仓库）。
			if strings.Contains(v, "${") && !smokeSecretRefRe.MatchString(v) {
				rep.addf("%s: headers[%s] 含 ${...} 但不是完整的 ${secret:<name>} 引用", ctx, k)
			}
		}
		// 请求体：任何 ${...} 都必须是合法密钥引用。
		for _, tok := range smokeBraceTokenRe.FindAllString(s.Body, -1) {
			if !smokeSecretAnyRe.MatchString(tok) {
				rep.addf("%s: body 含非法占位符 %q（只允许 ${secret:<name>}）", ctx, tok)
			}
		}
	}
}

// validateContainer 校验 runtime=container 时的执行段。
// 密钥边界：清单任何位置都不允许出现疑似凭据值——装载层
// (LoadFile scanCredentials) 已做代码级拦截，这里不重复。
func validateContainer(rep *Report, m *Manifest) {
	ctx := m.Project
	c := m.Container

	// install.root 对容器项目同样必填：它是卷宿主侧的根目录。
	if m.Baremetal.Install.Root == "" || !strings.HasPrefix(m.Baremetal.Install.Root, "/") {
		rep.addf("%s: 容器项目同样要求 baremetal.install.root 为绝对路径（卷的宿主侧根）", ctx)
	}

	if !validImageSources[c.Image.Source] {
		rep.addf("%s: container.image.source 非法（当前 %q），支持 build | local_image | registry", ctx, c.Image.Source)
	}
	switch c.Image.Source {
	case "build":
		if c.Image.Context == "" {
			rep.addf("%s: container.image.context 必填（build 来源）", ctx)
		} else if filepath.IsAbs(c.Image.Context) || strings.HasPrefix(filepath.Clean(c.Image.Context), "..") {
			rep.addf("%s: container.image.context 必须为仓库根内的相对路径", ctx)
		}
	case "local_image":
		if c.Image.LocalPath == "" {
			rep.addf("%s: container.image.local_path 必填（local_image 来源）", ctx)
		}
	case "registry":
		if c.Image.Repo == "" {
			rep.addf("%s: container.image.repo 必填（registry 来源）", ctx)
		}
	}

	portNames := make(map[string]bool)
	for _, p := range m.Facts.Ports {
		portNames[p.Name] = true
	}
	for i, v := range c.Volumes {
		if !strings.HasPrefix(v.Container, "/") {
			rep.addf("%s: container.volumes[%d].container 必须为容器内绝对路径", ctx, i)
		}
		if v.HostRelative == "" {
			rep.addf("%s: container.volumes[%d].host_relative 必填", ctx, i)
			continue
		}
		if cleaned := filepath.Clean(v.HostRelative); filepath.IsAbs(v.HostRelative) || cleaned != v.HostRelative || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			rep.addf("%s: container.volumes[%d].host_relative 必须为相对容器根的路径（不允许 ..）", ctx, i)
		}
	}
	for name := range c.ContainerPorts {
		if !portNames[name] {
			rep.addf("%s: container.container_ports 端口名 %q 未在 facts.ports 中声明", ctx, name)
		}
	}
}

func validateExternal(rep *Report, m *Manifest) {
	ctx := m.Project
	if len(m.Facts.Ports) == 0 {
		rep.addf("%s: external 清单必须声明 facts.ports（可达性检查目标）", ctx)
	}
	if m.Facts.External == nil || m.Facts.External.Host == "" {
		rep.addf("%s: external 清单必须声明 facts.external.host", ctx)
	}
}

// validatePlugin 校验 kind=plugin 清单的安装目标与产物声明。
// 缺字段一律响亮失败：插件条目宁可拒绝装载，也不允许含糊登记。
func validatePlugin(rep *Report, m *Manifest) {
	ctx := m.Project
	p := m.Plugin
	if p == nil {
		rep.addf("%s: kind=plugin 清单必须声明 plugin 段（runtime/plugins_dir/artifact）", ctx)
		return
	}

	// 插件没有自己的进程：裸金属/容器执行段对它无意义，出现即报错。
	if m.Baremetal.Process.Manager != "" {
		rep.addf("%s: kind=plugin 清单不应携带 baremetal.process.manager（插件没有自己的进程）", ctx)
	}
	if m.Runtime == "container" {
		rep.addf("%s: kind=plugin 清单的 runtime 不能为 container（插件装进目标运行时，不是容器）", ctx)
	}

	if strings.TrimSpace(p.Runtime) == "" {
		rep.addf("%s: plugin.runtime 必填（承载插件的目标运行时，如 velocity-proxy / bukkit-server）", ctx)
	}
	if p.TargetProject == m.Project {
		rep.addf("%s: plugin.target_project 不能指向自身", ctx)
	}

	dir := p.PluginsDir
	if strings.TrimSpace(dir) == "" {
		rep.addf("%s: plugin.plugins_dir 必填（插件产物放入的目录，相对目标运行时工作目录，如 plugins）", ctx)
	} else if filepath.IsAbs(dir) {
		rep.addf("%s: plugin.plugins_dir 必须为相对目标运行时工作目录的路径", ctx)
	} else if cleaned := filepath.Clean(dir); cleaned != dir || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		rep.addf("%s: plugin.plugins_dir 非法（不允许 .. 或非规范路径）", ctx)
	}

	art := p.Artifact
	if !validSources[art.Source] {
		rep.addf("%s: plugin.artifact.source 非法（当前 %q），支持 ci_release | local_path", ctx, art.Source)
		return
	}
	switch art.Source {
	case "ci_release":
		if art.Release.Repo == "" {
			rep.addf("%s: plugin.artifact.release.repo 必填", ctx)
		}
	case "local_path":
		if art.LocalPath == "" {
			rep.addf("%s: plugin.artifact.local_path 必填", ctx)
		}
	}
}

func validateBaremetal(rep *Report, m *Manifest) {
	ctx := m.Project
	bm := m.Baremetal

	switch bm.Process.Manager {
	case "":
		rep.addf("%s: baremetal.process.manager 必填", ctx)
		return
	case "aapanel", "manual":
		// foreign：编排器只读，执行段可选；若声明了仍做基本检查。
		if bm.Process.Manager == "manual" {
			return
		}
	case "tinyvisor":
	default:
		rep.addf("%s: baremetal.process.manager 非法（%q）", ctx, bm.Process.Manager)
		return
	}

	tv := bm.Process.Tinyvisor
	switch tv.RestartPolicy {
	case "", "never", "always", "unless-stopped", "on-failure":
	default:
		rep.addf("%s: baremetal.process.tinyvisor.restart_policy 非法（%q）", ctx, tv.RestartPolicy)
	}

	inst := bm.Install
	if bm.Process.Manager == "tinyvisor" {
		// 编排器实际执行的项目：严格要求。
		if inst.Root == "" || !strings.HasPrefix(inst.Root, "/") {
			rep.addf("%s: baremetal.install.root 必须为绝对路径", ctx)
		}
		if inst.BinaryName == "" {
			rep.addf("%s: baremetal.install.binary_name 必填（release 目录内的可执行文件名）", ctx)
		} else if !binaryNameRe.MatchString(inst.BinaryName) || strings.Contains(inst.BinaryName, "..") {
			rep.addf("%s: baremetal.install.binary_name 非法", ctx)
		}
	} else {
		// foreign（aapanel/manual）：执行段为信息性声明，允许以 "unknown"
		// 如实记录未定项（现状文档化策略）；仅在有值且明显非法时报错。
		if inst.Root != "" && inst.Root != "unknown" && !strings.HasPrefix(inst.Root, "/") {
			rep.addf("%s: baremetal.install.root 必须为绝对路径或 unknown", ctx)
		}
		if inst.BinaryName != "" && inst.BinaryName != "unknown" &&
			(!binaryNameRe.MatchString(inst.BinaryName) || strings.Contains(inst.BinaryName, "..")) {
			rep.addf("%s: baremetal.install.binary_name 非法", ctx)
		}
	}
	if inst.Layout != "" && inst.Layout != "releases" {
		rep.addf("%s: baremetal.install.layout 当前仅支持 releases", ctx)
	}
	if inst.KeepReleases <= 0 {
		inst.KeepReleases = 5
	}
	if inst.SharedData != "" {
		// 必须相对部署根、不含 ..（防止逃出部署根或被版本清理波及）。
		if filepath.IsAbs(inst.SharedData) {
			rep.addf("%s: baremetal.install.shared_data 必须为相对部署根的路径", ctx)
		} else if cleaned := filepath.Clean(inst.SharedData); cleaned != inst.SharedData || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			rep.addf("%s: baremetal.install.shared_data 非法（不允许 .. 或非规范路径）", ctx)
		}
	}

	art := bm.Artifact
	if bm.Process.Manager != "tinyvisor" {
		return
	}
	if !validSources[art.Source] {
		rep.addf("%s: baremetal.artifact.source 非法（%q），支持 ci_release | local_path", ctx, art.Source)
		return
	}
	switch art.Source {
	case "ci_release":
		if art.Release.Repo == "" {
			rep.addf("%s: baremetal.artifact.release.repo 必填", ctx)
		}
	case "local_path":
		if art.LocalPath == "" {
			rep.addf("%s: baremetal.artifact.local_path 必填", ctx)
		}
	}
}

func validatePortsCrossProject(rep *Report, manifests []*Manifest) {
	seen := make(map[int]string)
	for _, m := range manifests {
		for _, p := range m.Facts.Ports {
			if p.Port < 1 || p.Port > 65535 {
				continue
			}
			if prev, conflict := seen[p.Port]; conflict && prev != m.Project {
				rep.addf("port conflict: %d 同时被 %s 与 %s 声明", p.Port, prev, m.Project)
				continue
			}
			seen[p.Port] = m.Project
		}
	}
}

func validateDependencies(rep *Report, byProject map[string]*Manifest) {
	for name, m := range byProject {
		for _, d := range m.Facts.DependsOn {
			if _, ok := byProject[d.Project]; !ok {
				rep.addf("%s: 依赖 %q 不存在于聚合清单中", name, d.Project)
			}
		}
		// 插件的安装目标若指向编排体系内的项目，该项目必须存在。
		if m.IsPlugin() && m.Plugin != nil && m.Plugin.TargetProject != "" {
			if _, ok := byProject[m.Plugin.TargetProject]; !ok {
				rep.addf("%s: plugin.target_project %q 不存在于聚合清单中", name, m.Plugin.TargetProject)
			}
		}
	}

	// 依赖环检测（仅沿 required/optional 全部边）。
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int)
	var cycle []string
	var dfs func(n string) bool
	dfs = func(n string) bool {
		color[n] = gray
		cycle = append(cycle, n)
		if m, ok := byProject[n]; ok {
			deps := make([]string, 0, len(m.Facts.DependsOn))
			for _, d := range m.Facts.DependsOn {
				deps = append(deps, d.Project)
			}
			sort.Strings(deps)
			for _, dep := range deps {
				if _, exists := byProject[dep]; !exists {
					continue
				}
				switch color[dep] {
				case gray:
					cycle = append(cycle, dep)
					return true
				case white:
					if dfs(dep) {
						return true
					}
				}
			}
		}
		color[n] = black
		cycle = cycle[:len(cycle)-1]
		return false
	}
	names := make([]string, 0, len(byProject))
	for name := range byProject {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if color[name] == white {
			if dfs(name) {
				rep.addf("dependency cycle: %s", strings.Join(cycle, " -> "))
				break
			}
		}
	}
}

// EffectiveTag 解析 release.tag 中的 {service_version} 占位。
func (m *Manifest) EffectiveTag() string {
	tag := m.Baremetal.Artifact.Release.Tag
	if tag == "" {
		tag = "v{service_version}"
	}
	return versionTokenRe.ReplaceAllString(tag, m.ServiceVersion)
}

// EffectiveAsset 按硬约定推导资产名：<项目名>-linux-amd64。
func (m *Manifest) EffectiveAsset() string {
	if a := m.Baremetal.Artifact.Release.Asset; a != "" {
		return a
	}
	return m.Project + "-linux-amd64"
}

// EffectiveChecksumAsset 按硬约定推导校验和资产名：<项目名>-linux-amd64.sha256。
func (m *Manifest) EffectiveChecksumAsset() string {
	if a := m.Baremetal.Artifact.Release.ChecksumAsset; a != "" {
		return a
	}
	return m.EffectiveAsset() + ".sha256"
}
