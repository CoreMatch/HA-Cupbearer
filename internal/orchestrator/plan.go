package orchestrator

import (
	"fmt"
	"sort"

	"HA-Cupbearer/internal/manifest"
)

// groupOrder 是分组推进顺序。依赖拓扑序优先于分组序（见 Plan），
// 分组序只在无依赖关系的节点之间起确定性排序作用。
var groupOrder = []string{"infra", "core", "satellite", "build", "ops", "game", "edge", "web"}

func groupRank(g string) int {
	for i, name := range groupOrder {
		if name == g {
			return i
		}
	}
	return len(groupOrder)
}

// Plan 依据范围（项目列表或分组，空=全部）计算执行顺序：
// 对选中集合做拓扑排序（Kahn），平局按（分组序, 项目名）确定性取最小。
// 范围外但被依赖的项目不参与排序（其状态在执行期另行判定）。
func Plan(manifests []*manifest.Manifest, projects []string, group string) ([]string, error) {
	byProject := make(map[string]*manifest.Manifest)
	for _, m := range manifests {
		byProject[m.Project] = m
	}

	inScope := make(map[string]bool)
	for _, m := range manifests {
		if len(projects) > 0 {
			for _, want := range projects {
				if want == m.Project {
					inScope[m.Project] = true
					break
				}
			}
			continue
		}
		if group != "" {
			if m.Group == group {
				inScope[m.Project] = true
			}
			continue
		}
		inScope[m.Project] = true
	}

	for _, want := range projects {
		if _, ok := byProject[want]; !ok {
			return nil, fmt.Errorf("project %q not found in aggregated manifests", want)
		}
	}

	// 入度 = 范围内 required+optional 依赖数量（零依赖节点也要登记，否则排不出）。
	indeg := make(map[string]int)
	for name := range inScope {
		indeg[name] = 0
	}
	dependents := make(map[string][]string)
	for name := range inScope {
		m := byProject[name]
		for _, d := range m.Facts.DependsOn {
			if inScope[d.Project] {
				indeg[name]++
				dependents[d.Project] = append(dependents[d.Project], name)
			}
		}
	}

	ready := func() []string {
		var out []string
		for name, deg := range indeg {
			if deg == 0 && inScope[name] {
				out = append(out, name)
			}
		}
		sort.Slice(out, func(i, j int) bool {
			gi, gj := groupRank(byProject[out[i]].Group), groupRank(byProject[out[j]].Group)
			if gi != gj {
				return gi < gj
			}
			return out[i] < out[j]
		})
		return out
	}

	var order []string
	for {
		batch := ready()
		if len(batch) == 0 {
			break
		}
		next := batch[0]
		order = append(order, next)
		delete(inScope, next)
		delete(indeg, next)
		for _, dep := range dependents[next] {
			if _, ok := indeg[dep]; ok {
				indeg[dep]--
			}
		}
	}

	if len(inScope) > 0 {
		// 仍有节点未排出：范围内存在依赖环（聚合校验应已拦截，双保险）。
		var stuck []string
		for name := range inScope {
			stuck = append(stuck, name)
		}
		sort.Strings(stuck)
		return nil, fmt.Errorf("dependency cycle within scope: %v", stuck)
	}

	return order, nil
}

// requiredDependents 返回直接以 required 方式依赖 project 的项目名（有序）。
func requiredDependents(manifests []*manifest.Manifest, project string) []string {
	var out []string
	for _, m := range manifests {
		for _, d := range m.Facts.DependsOn {
			if d.Project == project && d.Kind != "optional" {
				out = append(out, m.Project)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
