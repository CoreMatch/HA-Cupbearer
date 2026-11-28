package discovery

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"HA-Cupbearer/internal/manifest"

	"github.com/goccy/go-yaml"
)

var excludedDirectories = map[string]bool{
	"build": true, "dist": true, "out": true, "target": true,
	"tmp": true, "temp": true, "node_modules": true, "vendor": true,
	"data": true, "logs": true, "log": true, "backup": true, "backups": true,
}

var configNames = map[string]bool{
	"config.yaml": true, "config.yml": true, "config.json": true,
	"application.yaml": true, "application.yml": true, "Config.json": true,
}

var healthPaths = []string{"/healthz", "/health", "/actuator/health", "/v1/health", "/v1/rooms", "/status"}

type signals struct {
	processes   []Process
	scripts     []Script
	containers  []Container
	executables []string
	configs     []string
}

func Scan(ctx context.Context, roots []string, importedDir string, snapshot Snapshot, existing []*manifest.Manifest) DiscoveryReport {
	report := DiscoveryReport{GeneratedAt: time.Now().UTC(), Roots: []RootReport{}, Warnings: []string{}}
	existingProjects := make(map[string]string)
	existingRoots := make(map[string]string)
	for _, m := range existing {
		existingProjects[m.Project] = m.SourcePath
		if m.Baremetal.Install.Root != "" {
			if root, err := canonicalPath(m.Baremetal.Install.Root); err == nil {
				existingRoots[root] = m.Project
			}
		}
	}

	for _, configuredRoot := range roots {
		rootReport := RootReport{Root: configuredRoot, DiscoveryDepth: 1, Candidates: []Candidate{}, Errors: []string{}}
		root, err := canonicalRoot(configuredRoot)
		if err != nil {
			rootReport.Errors = append(rootReport.Errors, err.Error())
			report.Roots = append(report.Roots, rootReport)
			continue
		}
		rootReport.Root = root
		depth, err := discoveryDepth(root)
		if err != nil {
			rootReport.Errors = append(rootReport.Errors, err.Error())
			report.Roots = append(report.Roots, rootReport)
			continue
		}
		rootReport.DiscoveryDepth = depth
		dirs, truncated, err := discoverDirectories(root, depth)
		rootReport.Truncated = truncated
		if err != nil {
			rootReport.Errors = append(rootReport.Errors, err.Error())
		}

		byDir := make(map[string]*signals)
		for _, dir := range dirs {
			byDir[dir] = &signals{}
		}
		collectRuntimeSignals(byDir, snapshot)

		for _, dir := range dirs {
			sig := byDir[dir]
			sig.executables = topLevelExecutables(dir)
			sig.configs = topLevelConfigs(dir)
			_, manifestErr := os.Stat(filepath.Join(dir, manifest.FileName))
			if len(sig.processes) == 0 && len(sig.scripts) == 0 && len(sig.containers) == 0 && len(sig.executables) == 0 && len(sig.configs) == 0 && manifestErr != nil {
				continue
			}
			candidate := buildCandidate(ctx, dir, importedDir, sig, existingProjects, existingRoots)
			rootReport.Candidates = append(rootReport.Candidates, candidate)
		}
		report.Roots = append(report.Roots, rootReport)
	}
	return report
}

func discoveryDepth(root string) (int, error) {
	depth := 1
	path := filepath.Join(root, manifest.FileName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return depth, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read discovery manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > MaxConfigBytes {
		return 0, fmt.Errorf("%s 必须是大小不超过 %d 字节的普通文件", path, MaxConfigBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("read discovery manifest: %w", err)
	}
	defer file.Close()
	var control struct {
		Discovery manifest.DiscoverySpec `yaml:"discovery"`
	}
	decoder := yaml.NewDecoder(io.LimitReader(file, MaxConfigBytes+1))
	if err := decoder.Decode(&control); err != nil {
		return 0, fmt.Errorf("解析 %s 失败：%w", path, err)
	}
	if control.Discovery.Depth < 0 || control.Discovery.Depth > MaxDiscoveryDepth {
		return 0, fmt.Errorf("%s: discovery.depth 必须在 0-%d 之间", path, MaxDiscoveryDepth)
	}
	if control.Discovery.Depth > 0 {
		depth = control.Discovery.Depth
	}
	return depth, nil
}

func discoverDirectories(root string, depth int) ([]string, bool, error) {
	dirs := []string{}
	truncated := false
	var walk func(string, int) error
	walk = func(current string, level int) error {
		entries, err := os.ReadDir(current)
		if err != nil {
			return fmt.Errorf("read discovery directory %s: %w", current, err)
		}
		for _, entry := range entries {
			if len(dirs) >= MaxDirectoryEntries {
				truncated = true
				return nil
			}
			name := entry.Name()
			if !entry.IsDir() || strings.HasPrefix(name, ".") || excludedDirectories[name] {
				continue
			}
			dir := filepath.Join(current, name)
			info, err := os.Lstat(dir)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			dirs = append(dirs, dir)
			if level < depth {
				if err := walk(dir, level+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, 1); err != nil {
		return dirs, truncated, err
	}
	return dirs, truncated, nil
}

func canonicalRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("deployment root 不能为空")
	}
	canonical, err := canonicalPath(path)
	if err != nil {
		return "", fmt.Errorf("deployment root %s: %w", path, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("deployment root %s: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("deployment root %s 不是目录", path)
	}
	return canonical, nil
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func collectRuntimeSignals(byDir map[string]*signals, snapshot Snapshot) {
	for _, process := range snapshot.Processes {
		if dir := deepestOwnerDirectory(process.Cwd, byDir); dir != "" {
			byDir[dir].processes = append(byDir[dir].processes, process)
		} else if dir := deepestOwnerDirectory(process.Exe, byDir); dir != "" {
			byDir[dir].processes = append(byDir[dir].processes, process)
		}
	}
	for _, script := range snapshot.Scripts {
		if sig := byDir[deepestOwnerDirectory(script.WorkDir, byDir)]; sig != nil {
			sig.scripts = append(sig.scripts, script)
		}
	}
	for _, container := range snapshot.Containers {
		if sig := byDir[deepestOwnerDirectory(container.WorkingDir, byDir)]; sig != nil {
			sig.containers = append(sig.containers, container)
		}
	}
}

func deepestOwnerDirectory(path string, byDir map[string]*signals) string {
	if path == "" {
		return ""
	}
	canonical, err := canonicalPath(path)
	if err != nil {
		canonical = filepath.Clean(path)
	}
	owner := ""
	longest := -1
	for dir := range byDir {
		rel, err := filepath.Rel(dir, canonical)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(dir) > longest {
			owner = dir
			longest = len(dir)
		}
	}
	return owner
}

func ownerDirectory(root, path string) string {
	if path == "" {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	first := strings.Split(rel, string(filepath.Separator))[0]
	if first == "" || first == "." || first == ".." {
		return ""
	}
	return filepath.Join(root, first)
}

func topLevelExecutables(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var preferred, scripts []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".sh") {
			scripts = append(scripts, path)
		} else {
			preferred = append(preferred, path)
		}
	}
	sort.Strings(preferred)
	sort.Strings(scripts)
	return append(preferred, scripts...)
}

func topLevelConfigs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var configs []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !configNames[entry.Name()] {
			continue
		}
		configs = append(configs, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(configs)
	return configs
}

func applyManifestCandidate(candidate *Candidate, m *manifest.Manifest) {
	candidate.Project = m.Project
	candidate.DisplayName = m.DisplayName
	candidate.Runtime = m.Runtime
	candidate.DetectedManager = m.Baremetal.Process.Manager
	candidate.ImportManager = m.Baremetal.Process.Manager
	candidate.Confidence = "high"
	for _, port := range m.Facts.Ports {
		candidate.Ports = appendUniqueInts(candidate.Ports, port.Port)
	}
	if probe := m.Facts.Health.HTTP; probe != nil {
		for _, port := range m.Facts.Ports {
			if port.Name == probe.PortName {
				candidate.Port = port.Port
				break
			}
		}
		candidate.HealthPath = probe.Path
		candidate.HealthStatus = probe.ExpectStatus
		candidate.Evidence = append(candidate.Evidence, fmt.Sprintf("清单健康配置 port_name=%s port=%d path=%s expect=%d", probe.PortName, candidate.Port, probe.Path, probe.ExpectStatus))
	}
	if m.Baremetal.Install.BinaryName != "" {
		candidate.BinaryPath = filepath.Join(candidate.Directory, m.Baremetal.Install.BinaryName)
	}
	switch {
	case m.Kind != "service":
		candidate.Blockers = append(candidate.Blockers, "仅支持 kind=service 清单的只读导入")
	case m.Runtime == "container":
		candidate.Runtime = "container"
		candidate.Blockers = append(candidate.Blockers, "容器项目需要确认镜像与卷，第 1 期不自动导入")
	case m.Baremetal.Process.Manager != "manual" && m.Baremetal.Process.Manager != "aapanel":
		candidate.Blockers = append(candidate.Blockers, "清单由 "+m.Baremetal.Process.Manager+" 管理；批量导入仅登记 manual/aapanel 只读项目，请使用 scan_roots 原生装载")
	}
}

func buildCandidate(ctx context.Context, dir, importedDir string, sig *signals, existingProjects, existingRoots map[string]string) Candidate {
	candidate := Candidate{
		Directory:       dir,
		DisplayName:     humanize(filepath.Base(dir)),
		ImportManager:   "manual",
		Runtime:         "baremetal",
		Ports:           []int{},
		Evidence:        []string{},
		Blockers:        []string{},
		DetectedManager: "manual",
		Confidence:      "low",
	}

	manifestPath := filepath.Join(dir, manifest.FileName)
	if info, err := os.Lstat(manifestPath); err == nil {
		candidate.ExistingManifest = true
		candidate.Evidence = append(candidate.Evidence, "已有 "+manifestPath)
		if !info.Mode().IsRegular() || info.Size() > MaxConfigBytes {
			candidate.Blockers = append(candidate.Blockers, fmt.Sprintf("部署清单必须是大小不超过 %d 字节的普通文件", MaxConfigBytes))
		} else if m, err := manifest.LoadFile(manifestPath); err != nil {
			candidate.Blockers = append(candidate.Blockers, "现有清单无法装载："+err.Error())
		} else if rep := manifest.ValidateAggregate([]*manifest.Manifest{m}, nil); !rep.OK() {
			candidate.Blockers = append(candidate.Blockers, "现有清单校验失败："+strings.Join(rep.Errors, "；"))
		} else {
			applyManifestCandidate(&candidate, m)
		}
	}

	for _, process := range sig.processes {
		candidate.Evidence = append(candidate.Evidence, fmt.Sprintf("进程 PID=%d cwd=%s exe=%s", process.PID, process.Cwd, process.Exe))
		candidate.Ports = appendUniqueInts(candidate.Ports, process.Ports...)
		if candidate.BinaryPath == "" && ownerDirectory(filepath.Dir(dir), process.Exe) == dir {
			candidate.BinaryPath = process.Exe
		}
	}
	sort.Slice(sig.scripts, func(i, j int) bool { return sig.scripts[i].Name < sig.scripts[j].Name })
	for _, script := range sig.scripts {
		candidate.DetectedManager = "tinyvisor"
		candidate.Evidence = append(candidate.Evidence, fmt.Sprintf("Tinyvisor 脚本 %s workDir=%s", script.Name, script.WorkDir))
		if candidate.Project == "" {
			candidate.Project = slug(script.Name)
		}
		if candidate.BinaryPath == "" {
			candidate.BinaryPath = scriptBinary(script)
		}
	}
	for _, container := range sig.containers {
		candidate.DetectedManager = "container"
		candidate.Runtime = "container"
		candidate.Evidence = append(candidate.Evidence, fmt.Sprintf("Compose 容器 %s project=%s working_dir=%s", container.Name, container.Project, container.WorkingDir))
	}
	for _, executable := range sig.executables {
		candidate.Evidence = append(candidate.Evidence, "可执行文件 "+executable)
		if candidate.BinaryPath == "" {
			candidate.BinaryPath = executable
		}
	}
	for _, config := range sig.configs {
		candidate.Evidence = append(candidate.Evidence, "配置文件 "+config)
		candidate.Ports = appendUniqueInts(candidate.Ports, portsFromConfig(config)...)
	}
	candidate.Ports = sortedUniqueInts(candidate.Ports)
	if candidate.Project == "" {
		if candidate.BinaryPath != "" {
			candidate.Project = slug(strings.TrimSuffix(filepath.Base(candidate.BinaryPath), filepath.Ext(candidate.BinaryPath)))
		} else {
			candidate.Project = slug(filepath.Base(dir))
		}
	}
	if candidate.Project == "" {
		candidate.Project = "service"
	}

	if !candidate.ExistingManifest && candidate.Runtime != "container" && len(candidate.Ports) > 0 {
		port, path, status, ok := probeHealth(ctx, candidate.Ports)
		if ok {
			candidate.Port = port
			candidate.HealthPath = path
			candidate.HealthStatus = status
			candidate.Evidence = append(candidate.Evidence, fmt.Sprintf("HTTP 健康探测 %d%s -> %d", port, path, status))
		}
	}

	if canonical, err := canonicalPath(dir); err == nil {
		if project := existingRoots[canonical]; project != "" {
			candidate.Imported = true
			candidate.Evidence = append(candidate.Evidence, "已由项目 "+project+" 声明安装根")
		}
	}
	if source := existingProjects[candidate.Project]; source != "" {
		candidate.Evidence = append(candidate.Evidence, "项目名已存在于 "+source)
	}
	if isImportedSource(importedDir, existingProjects[candidate.Project]) {
		candidate.Imported = true
	}
	if candidate.ExistingManifest && len(sig.scripts) > 0 {
		candidate.Blockers = append(candidate.Blockers, "运行时证据显示 Tinyvisor 管理，与清单声明的只读导入方式冲突")
	}
	if candidate.ExistingManifest && len(sig.containers) > 0 {
		candidate.Blockers = append(candidate.Blockers, "运行时证据显示容器管理，与清单声明的只读导入方式冲突")
	}
	if !candidate.Imported && existingProjects[candidate.Project] != "" {
		candidate.Blockers = append(candidate.Blockers, "项目名已被现有清单占用")
	}

	switch {
	case candidate.Imported:
		candidate.Blockers = append(candidate.Blockers, "已导入或已被现有清单声明")
	case candidate.ExistingManifest:
		if len(candidate.Blockers) == 0 {
			candidate.Importable = true
		}
	case candidate.Runtime == "container":
		candidate.Blockers = append(candidate.Blockers, "容器项目需要确认镜像与卷，第 1 期不自动导入")
	case len(sig.processes) == 0 && len(sig.scripts) == 0 && len(sig.executables) == 0:
		candidate.Blockers = append(candidate.Blockers, "未发现可执行文件或运行进程")
	}
	if !candidate.ExistingManifest && !candidate.Imported && candidate.Runtime != "container" {
		if candidate.Port == 0 {
			candidate.Blockers = append(candidate.Blockers, "未发现可用 HTTP 健康端点，请填写端口与路径")
		}
	}
	if len(candidate.Evidence) > 16 {
		candidate.Evidence = candidate.Evidence[:16]
	}
	candidate.Importable = len(candidate.Blockers) == 0
	if candidate.ExistingManifest && candidate.Importable {
		candidate.Confidence = "high"
	} else if (len(sig.processes) > 0 || len(sig.scripts) > 0) && candidate.Port != 0 {
		candidate.Confidence = "high"
	} else if len(sig.processes) > 0 || len(sig.scripts) > 0 || candidate.Port != 0 {
		candidate.Confidence = "medium"
	}
	return candidate
}

func isImportedSource(importedDir, source string) bool {
	if importedDir == "" || source == "" {
		return false
	}
	canonicalDir, err := canonicalPath(importedDir)
	if err != nil {
		return false
	}
	canonicalSource, err := canonicalPath(source)
	if err != nil {
		return false
	}
	return filepath.Dir(canonicalSource) == canonicalDir
}

func scriptBinary(script Script) string {
	for _, line := range strings.Split(script.Content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "exec ") {
			continue
		}
		for _, field := range strings.Fields(strings.TrimPrefix(line, "exec ")) {
			if field == "env" || strings.Contains(field, "=") {
				continue
			}
			field = strings.Trim(field, "\"'")
			if strings.HasPrefix(field, "./") || strings.HasPrefix(field, "/") {
				path := field
				if !filepath.IsAbs(path) {
					path = filepath.Join(script.WorkDir, path)
				}
				return filepath.Clean(path)
			}
		}
	}
	return ""
}

func probeHealth(ctx context.Context, ports []int) (int, string, int, bool) {
	if len(ports) > MaxProbePorts {
		ports = ports[:MaxProbePorts]
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   500 * time.Millisecond,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for _, port := range ports {
		if port <= 0 {
			continue
		}
		for _, path := range healthPaths {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+path, nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return 0, "", 0, false
				}
				continue
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return port, path, resp.StatusCode, true
			}
		}
	}
	return 0, "", 0, false
}

func portsFromConfig(path string) []int {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxConfigBytes {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var value interface{}
	if err := yaml.Unmarshal(data, &value); err != nil {
		return nil
	}
	set := make(map[int]bool)
	collectConfigPorts(value, set)
	return sortedUniqueInts(setToInts(set))
}

func collectConfigPorts(value interface{}, out map[int]bool) {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			lower := strings.ToLower(key)
			if lower == "port" || strings.HasSuffix(lower, "_port") {
				out[asPort(child)] = true
			}
			if lower == "listen" || lower == "addr" || lower == "address" {
				out[hostPort(child)] = true
			}
			collectConfigPorts(child, out)
		}
	case map[interface{}]interface{}:
		for key, child := range typed {
			name, ok := key.(string)
			if !ok {
				continue
			}
			lower := strings.ToLower(name)
			if lower == "port" || strings.HasSuffix(lower, "_port") {
				out[asPort(child)] = true
			}
			if lower == "listen" || lower == "addr" || lower == "address" {
				out[hostPort(child)] = true
			}
			collectConfigPorts(child, out)
		}
	case []interface{}:
		for _, child := range typed {
			collectConfigPorts(child, out)
		}
	}
}

func asPort(value interface{}) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case uint64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		port, _ := strconv.Atoi(strings.TrimSpace(typed))
		return port
	default:
		return 0
	}
}

func hostPort(value interface{}) int {
	text, ok := value.(string)
	if !ok {
		return 0
	}
	text = strings.TrimSpace(text)
	if port, err := strconv.Atoi(text); err == nil {
		return port
	}
	_, portText, err := net.SplitHostPort(text)
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(portText)
	return port
}

func appendUniqueInts(dst []int, values ...int) []int {
	for _, value := range values {
		if value > 0 && value <= 65535 && !containsInt(dst, value) {
			dst = append(dst, value)
		}
	}
	return dst
}

func sortedUniqueInts(values []int) []int {
	set := make(map[int]bool)
	for _, value := range values {
		if value > 0 && value <= 65535 {
			set[value] = true
		}
	}
	out := make([]int, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

func setToInts(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	return out
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func slug(value string) string {
	var out strings.Builder
	dash := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if dash && out.Len() > 0 {
				out.WriteByte('-')
			}
			dash = false
			if r <= unicode.MaxASCII {
				out.WriteRune(r)
			}
		} else {
			dash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func humanize(value string) string {
	value = strings.NewReplacer("-", " ", "_", " ").Replace(value)
	for _, r := range value {
		return strings.ToUpper(string(r)) + value[len(string(r)):]
	}
	return value
}
