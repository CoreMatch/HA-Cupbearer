package manifest

import (
	"fmt"
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
)

// Report 是一次聚合校验的结果。Errors 为空即全部通过。
type Report struct {
	Errors    []string    `json:"errors"`
	Manifests []*Manifest `json:"-"`
}

func (r *Report) OK() bool { return len(r.Errors) == 0 }

func (r *Report) addf(format string, args ...interface{}) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

// ValidateAggregate 对装载结果做全量校验：单清单字段规则 +
// 跨清单一致性（重名、依赖存在性、依赖环、端口冲突）。
// 装载错误以 loadErrors 传入，一并体现在报告中。
func ValidateAggregate(manifests []*Manifest, loadErrors []error) *Report {
	rep := &Report{Manifests: manifests}
	for _, err := range loadErrors {
		rep.addf("load: %v", err)
	}

	byProject := make(map[string]*Manifest)
	for _, m := range manifests {
		validateSingle(rep, m)
		if m.Project == "" {
			continue
		}
		if prev, dup := byProject[m.Project]; dup {
			rep.addf("duplicate project %q: %s 与 %s", m.Project, prev.SourcePath, m.SourcePath)
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
	case "service", "external":
	default:
		rep.addf("%s: kind 必须为 service 或 external（当前 %q）", ctx, m.Kind)
	}
	if m.Project == "" {
		rep.addf("%s: project 不能为空", m.SourcePath)
	} else if !projectNameRe.MatchString(m.Project) {
		rep.addf("%s: project 只能包含小写字母、数字与连字符且以字母数字开头", ctx)
	}
	if m.Container.Enabled {
		rep.addf("%s: container 段当前冻结，enabled 必须为 false", ctx)
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

	validateFacts(rep, m)
	validateBaremetal(rep, m)
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
		rep.addf("%s: facts.health.http 必填（编排器健康判定与状态可见性依赖它）", ctx)
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

func validateExternal(rep *Report, m *Manifest) {
	ctx := m.Project
	if len(m.Facts.Ports) == 0 {
		rep.addf("%s: external 清单必须声明 facts.ports（可达性检查目标）", ctx)
	}
	if m.Facts.External == nil || m.Facts.External.Host == "" {
		rep.addf("%s: external 清单必须声明 facts.external.host", ctx)
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
