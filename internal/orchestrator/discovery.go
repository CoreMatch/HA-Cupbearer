package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"HA-Cupbearer/internal/discovery"
	"HA-Cupbearer/internal/docker"
	"HA-Cupbearer/internal/manifest"
)

func (o *Orchestrator) Discover(ctx context.Context) (discovery.DiscoveryReport, []string) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	snapshot, warnings := o.discoverySnapshot(ctx)
	existing, rep := o.Load()
	if !rep.OK() {
		warnings = append(warnings, rep.Errors...)
	}
	return discovery.Scan(ctx, o.cfg.Orchestration.DeploymentRoots, o.importedManifestsDir(), snapshot, existing), warnings
}

func (o *Orchestrator) discoverySnapshot(ctx context.Context) (discovery.Snapshot, []string) {
	var snapshot discovery.Snapshot
	var warnings []string

	processes, err := discovery.ReadProcesses("/proc")
	if err != nil {
		warnings = append(warnings, "读取 /proc 失败："+err.Error())
	} else {
		snapshot.Processes = processes
	}
	if o.cfg.Orchestration.Tinyvisor.URL != "" {
		scripts, err := o.tv.List(ctx)
		if err != nil {
			warnings = append(warnings, "读取 Tinyvisor 脚本失败："+err.Error())
		} else {
			for _, script := range scripts {
				snapshot.Scripts = append(snapshot.Scripts, discovery.Script{
					Name: script.Name, WorkDir: script.WorkDir, Content: script.Content,
				})
			}
		}
	}
	if lister, ok := o.docker.(interface {
		ListComposeContainers(context.Context) ([]docker.ComposeContainerRef, error)
	}); ok {
		containers, err := lister.ListComposeContainers(ctx)
		if err != nil {
			warnings = append(warnings, "读取 Compose 容器失败："+err.Error())
		} else {
			for _, container := range containers {
				snapshot.Containers = append(snapshot.Containers, discovery.Container{
					Name: container.Name, Project: container.Project, WorkingDir: container.WorkingDir,
				})
			}
		}
	}
	return snapshot, warnings
}

func (o *Orchestrator) ImportDiscovered(ctx context.Context, input discovery.ImportRequest) (discovery.ImportResult, error) {
	input = discovery.NormalizeImportRequest(input)
	if !input.ImportManifests && len(input.Items) == 0 {
		return discovery.ImportResult{}, &discovery.ValidationError{Field: "items", Message: "请至少选择一个项目"}
	}
	if len(input.Items) > discovery.MaxDirectoryEntries {
		return discovery.ImportResult{}, &discovery.ValidationError{Field: "items", Message: "单次最多导入 256 个项目"}
	}
	root, err := discovery.ValidateRoot(o.cfg.Orchestration.DeploymentRoots, input.Root)
	if err != nil {
		return discovery.ImportResult{}, err
	}

	o.importMu.Lock()
	defer o.importMu.Unlock()

	report, _ := o.Discover(ctx)
	candidates := make(map[string]discovery.Candidate)
	var skipped []discovery.ImportedManifestSkip
	if input.ImportManifests {
		input.Items = nil
	}
	for _, rootReport := range report.Roots {
		if rootReport.Root != root {
			continue
		}
		for _, candidate := range rootReport.Candidates {
			candidates[candidate.Directory] = candidate
			if input.ImportManifests && candidate.ExistingManifest && !candidate.Imported {
				if !candidate.Importable {
					skipped = append(skipped, discovery.ImportedManifestSkip{
						Directory: candidate.Directory,
						Project:   candidate.Project,
						Reason:    strings.Join(candidate.Blockers, "；"),
					})
					continue
				}
				input.Items = append(input.Items, discovery.ImportItem{
					Directory:    candidate.Directory,
					Project:      candidate.Project,
					DisplayName:  candidate.DisplayName,
					Manager:      candidate.ImportManager,
					Port:         candidate.Port,
					HealthPath:   candidate.HealthPath,
					HealthStatus: candidate.HealthStatus,
				})
			}
		}
	}
	if input.ImportManifests && len(input.Items) == 0 {
		return discovery.ImportResult{}, &discovery.ValidationError{Field: "items", Message: "没有发现可导入的 deploy-manifest.yaml"}
	}
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Directory < skipped[j].Directory })

	existing, rep := o.Load()
	if !rep.OK() {
		return discovery.ImportResult{}, fmt.Errorf("现有清单校验失败：%v", rep.Errors)
	}
	known := make(map[string]string)
	ports := make(map[int]string)
	for _, m := range existing {
		known[m.Project] = m.SourcePath
		for _, port := range m.Facts.Ports {
			if port.Port > 0 {
				ports[port.Port] = m.Project
			}
		}
	}

	manifests := make([]*manifest.Manifest, 0, len(input.Items))
	seenDirectories := make(map[string]bool)
	for _, item := range input.Items {
		directory, err := discovery.ValidateImportDirectory(root, item.Directory, input.ImportManifests)
		if err != nil {
			return discovery.ImportResult{}, err
		}
		if seenDirectories[directory] {
			return discovery.ImportResult{}, &discovery.ValidationError{Field: "directory", Message: "同一目录不能重复导入：" + directory}
		}
		seenDirectories[directory] = true
		candidate, ok := candidates[directory]
		if !ok {
			return discovery.ImportResult{}, &discovery.ValidationError{Field: "directory", Message: "目录未通过发现检查：" + directory}
		}
		if candidate.Imported || candidate.Runtime == "container" || (candidate.ExistingManifest && !input.ImportManifests) {
			return discovery.ImportResult{}, &discovery.ValidationError{Field: "directory", Message: "该目录当前不可导入：" + directory}
		}
		if input.ImportManifests && !candidate.ExistingManifest {
			return discovery.ImportResult{}, &discovery.ValidationError{Field: "directory", Message: "该目录没有 deploy-manifest.yaml：" + directory}
		}
		if previous := known[item.Project]; previous != "" {
			if input.ImportManifests {
				skipped = append(skipped, discovery.ImportedManifestSkip{Directory: directory, Project: item.Project, Reason: "项目名重复：" + item.Project + "（" + previous + "）"})
				continue
			}
			return discovery.ImportResult{}, &discovery.ValidationError{Field: "project", Message: "项目名已存在：" + item.Project + "（" + previous + "）"}
		}
		if previous := ports[item.Port]; previous != "" && previous != item.Project {
			if input.ImportManifests {
				skipped = append(skipped, discovery.ImportedManifestSkip{Directory: directory, Project: item.Project, Reason: fmt.Sprintf("端口 %d 已由项目 %s 声明", item.Port, previous)})
				continue
			}
			return discovery.ImportResult{}, &discovery.ValidationError{Field: "port", Message: fmt.Sprintf("端口 %d 已由项目 %s 声明", item.Port, previous)}
		}
		known[item.Project] = directory
		ports[item.Port] = item.Project
		m, err := discovery.BuildImportedManifest(item, directory, "")
		if err != nil {
			return discovery.ImportResult{}, err
		}
		manifests = append(manifests, m)
	}
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Directory < skipped[j].Directory })
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Project < manifests[j].Project })

	created, err := discovery.WriteBatch(o.importedManifestsDir(), manifests)
	if err != nil {
		return discovery.ImportResult{}, err
	}
	result := discovery.ImportResult{
		Imported: make([]discovery.ImportedProject, 0, len(created)),
		Skipped:  append([]discovery.ImportedManifestSkip{}, skipped...),
	}
	for i, path := range created {
		result.Imported = append(result.Imported, discovery.ImportedProject{
			Project: manifests[i].Project, SourcePath: strings.TrimSpace(path),
		})
	}
	return result, nil
}
