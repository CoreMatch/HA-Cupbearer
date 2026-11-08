package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"HA-Services-Deployer/internal/manifest"
	"HA-Services-Deployer/internal/probe"
	"HA-Services-Deployer/internal/tinyvisor"
)

// effective 解析项目在当下部署上下文中的执行参数：
// Manifest 声明为默认，编排器配置的 Overrides 按上下文覆盖
// （生产上下文不做任何覆盖）。
// resolveManifest 返回应用了部署上下文覆盖的清单副本：
// 仓库清单记录生产现状，覆盖项（manager/install_root/service_version/
// artifact_local_path）按上下文生效。生产上下文无任何覆盖时返回等值副本。
func (o *Orchestrator) resolveManifest(m *manifest.Manifest) *manifest.Manifest {
	clone := *m
	clone.Baremetal = m.Baremetal
	ov, ok := o.cfg.Orchestration.Overrides[m.Project]
	if !ok {
		return &clone
	}
	if ov.Runtime != "" {
		clone.Runtime = ov.Runtime
	}
	if ov.Manager != "" {
		clone.Baremetal.Process.Manager = ov.Manager
	}
	if ov.InstallRoot != "" {
		clone.Baremetal.Install.Root = ov.InstallRoot
	}
	if ov.ServiceVersion != "" {
		clone.ServiceVersion = ov.ServiceVersion
	}
	if ov.ArtifactLocalPath != "" {
		clone.Baremetal.Artifact.Source = "local_path"
		clone.Baremetal.Artifact.LocalPath = ov.ArtifactLocalPath
	}
	return &clone
}

// effective 解析项目在当下部署上下文中的执行参数（入参应为已 resolve 的清单）。
type effective struct {
	runtime    string // baremetal（默认）| container
	manager    string
	root       string
	healthPort int // 0 = 沿用 facts.ports 声明
}

func (o *Orchestrator) effective(m *manifest.Manifest) effective {
	runtime := m.Runtime
	if runtime == "" {
		runtime = "baremetal"
	}
	eff := effective{
		runtime: runtime,
		manager: m.Baremetal.Process.Manager,
		root:    m.Baremetal.Install.Root,
	}
	if ov, ok := o.cfg.Orchestration.Overrides[m.Project]; ok {
		eff.healthPort = ov.HealthPort
	}
	return eff
}

func currentLink(root string) string { return filepath.Join(root, "current") }

func releaseDir(root, version string) string {
	return filepath.Join(root, "releases", version)
}

// readActiveVersion 读取 current 软链指向的版本（不存在返回空）。
func readActiveVersion(root string) string {
	target, err := os.Readlink(currentLink(root))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// switchSymlink 原子切换 current 软链（临时链接 + rename）。
// target 使用相对路径（releases/<version>），保证部署根可整体搬迁。
func switchSymlink(root, version string) error {
	link := currentLink(root)
	tmp := fmt.Sprintf("%s.tmp-%d", link, time.Now().UnixNano())
	if err := os.Symlink(filepath.Join("releases", version), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// listReleases 按修改时间倒序返回版本目录名。
func listReleases(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "releases"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type item struct {
		name    string
		modTime time.Time
	}
	var items []item
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{name: e.Name(), modTime: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].modTime.After(items[j].modTime) })
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.name)
	}
	return names, nil
}

// renderScript 生成 Tinyvisor 登记的启动脚本。
// 约定：workdir 为部署根；脚本必须以 exec 结尾，保证 Tinyvisor 的
// SIGTERM 直达服务进程本身（而不是外层 shell）。
// 声明 shared_data 时，exec 行经 env 注入 avalonhelper.basePath 绝对路径
// （带点变量名无法经 env_file/shell 赋值注入，只能由 env 命令直接设置），
// 使进程的配置与数据落在版本目录之外的共享目录。
func renderScript(m *manifest.Manifest) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -e\n")
	if envFile := m.Baremetal.Process.Tinyvisor.EnvFile; envFile != "" {
		fmt.Fprintf(&b, "if [ -f %q ]; then set -a; . %q; set +a; fi\n", envFile, envFile)
	}
	bin := "./current/" + m.Baremetal.Install.BinaryName
	if shared := m.Baremetal.Install.SharedData; shared != "" {
		basePath := filepath.Join(m.Baremetal.Install.Root, shared)
		fmt.Fprintf(&b, "exec env %q %s\n", "avalonhelper.basePath="+basePath, bin)
		return b.String()
	}
	fmt.Fprintf(&b, "exec %s\n", bin)
	return b.String()
}

func (o *Orchestrator) probeURL(m *manifest.Manifest, portOverride int) (string, error) {
	probeSpec := m.Facts.Health.HTTP
	if probeSpec == nil {
		return "", fmt.Errorf("facts.health.http missing")
	}
	var port int
	if portOverride > 0 {
		port = portOverride
	} else {
		for _, p := range m.Facts.Ports {
			if p.Name == probeSpec.PortName {
				port = p.Port
				break
			}
		}
	}
	if port == 0 {
		return "", fmt.Errorf("health port %q not declared", probeSpec.PortName)
	}
	host := o.cfg.Orchestration.ProbeHost
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d%s", host, port, probeSpec.Path), nil
}

// probeOnce 做一次短探测，仅用于状态判定（不阻塞重试）。
func (o *Orchestrator) probeOnce(ctx context.Context, m *manifest.Manifest, portOverride int) error {
	url, err := o.probeURL(m, portOverride)
	if err != nil {
		return err
	}
	expect := m.Facts.Health.HTTP.ExpectStatus
	if expect == 0 {
		expect = 200
	}
	return probe.CheckHTTP(ctx, url, expect, 3*time.Second)
}

// probeUntilHealthy 按 Manifest 探测参数轮询直至健康。
func (o *Orchestrator) probeUntilHealthy(ctx context.Context, m *manifest.Manifest, portOverride int) error {
	url, err := o.probeURL(m, portOverride)
	if err != nil {
		return err
	}
	p := m.Facts.Health.HTTP
	return probe.Poll(ctx, url, p.ExpectStatus,
		time.Duration(p.TimeoutSeconds)*time.Second,
		time.Duration(p.IntervalSeconds)*time.Second,
		p.Retries)
}

// upsertScript 按名幂等登记脚本；返回 (内容是否有变化, 错误)。
func (o *Orchestrator) upsertScript(ctx context.Context, m *manifest.Manifest, eff effective) (bool, error) {
	// Tinyvisor 的 validatePayload 会对 content/workDir 做 TrimSpace，
	// 比对与提交都用归一化后的值，避免尾部空白造成永久"漂移"误判。
	content := strings.TrimSpace(renderScript(m))
	policy := m.Baremetal.Process.Tinyvisor.RestartPolicy
	if policy == "" {
		policy = "unless-stopped"
	}

	existing, err := o.tv.GetByName(ctx, m.Project)
	if err != nil && err != tinyvisor.ErrNotFound {
		return false, err
	}

	payload := tinyvisor.ScriptPayload{
		Name:          m.Project,
		WorkDir:       eff.root,
		Content:       content,
		AutoStart:     m.Baremetal.Process.Tinyvisor.AutoStart,
		RestartPolicy: policy,
	}

	if err == nil && existing.WorkDir == payload.WorkDir && existing.Content == payload.Content &&
		existing.AutoStart == payload.AutoStart && existing.RestartPolicy == payload.RestartPolicy {
		return false, nil // 无漂移，不写
	}

	if _, _, err := o.tv.Upsert(ctx, payload); err != nil {
		return false, err
	}
	return true, nil
}

// pruneReleases 保留最近 keep 个版本目录，删除更旧的（从不删除当前生效版本）。
func pruneReleases(root, active string, keep int) []string {
	if keep <= 0 {
		keep = 5
	}
	releases, err := listReleases(root)
	if err != nil || len(releases) <= keep {
		return nil
	}
	var removed []string
	for _, name := range releases[keep:] {
		if name == active {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, "releases", name)); err == nil {
			removed = append(removed, name)
		}
	}
	return removed
}
