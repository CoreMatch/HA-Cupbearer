package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ErrStateCorrupt 表示 state.json 存在但无法解析（截断/写坏）。
// 调用方据此走恢复流程，而不是让后续操作全部失败。
var ErrStateCorrupt = errors.New("state file corrupt")

// Store 持久化运行状态（续跑依据）与运行记录。
// 布局：<dir>/state.json 与 <dir>/runs/<run-id>.json。
type Store struct {
	dir string
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// GlobalState 记录每个项目最近一次的编排结果。
type GlobalState struct {
	Projects  map[string]*ProjectState `json:"projects"`
	UpdatedAt time.Time                `json:"updated_at"`
}

type ProjectState struct {
	Outcome       string    `json:"outcome"`
	ActiveVersion string    `json:"active_version,omitempty"`
	// ImageID 是容器项目实际运行的镜像内容摘要（sha256:...）：
	// 回滚的唯一可定位依据；标签只是人类可读的名字，标签会撒谎，摘要不会。
	ImageID   string    `json:"image_id,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) statePath() string { return filepath.Join(s.dir, "state.json") }

// LoadState 读取 state.json。
//   - 文件不存在：返回空状态，无错误（首次运行）；
//   - 解析失败：返回 ErrStateCorrupt 包装的错误（调用方走恢复流程）；
//   - 其它读取错误（如权限）：原样返回。
func (s *Store) LoadState() (*GlobalState, error) {
	state := &GlobalState{Projects: map[string]*ProjectState{}}
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if state.Projects == nil {
		state.Projects = map[string]*ProjectState{}
	}
	return state, nil
}

// RebuildStateFromRuns 从 runs/*.json 重建 GlobalState：按时间正序遍历运行
// 记录，每个项目取最近一次结果。用于 state.json 损坏后的恢复。
// 无运行记录时返回空状态（等价于"全部重跑"）。
func (s *Store) RebuildStateFromRuns() (*GlobalState, error) {
	state := &GlobalState{Projects: map[string]*ProjectState{}}
	entries, err := os.ReadDir(filepath.Join(s.dir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return nil, err
	}

	var runs []*Run
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		run, err := s.LoadRun(e.Name()[:len(e.Name())-len(".json")])
		if err != nil {
			continue
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.Before(runs[j].StartedAt) })

	for _, run := range runs {
		ts := run.StartedAt
		if run.FinishedAt != nil {
			ts = *run.FinishedAt
		}
		for name, res := range run.Results {
			state.Projects[name] = &ProjectState{
				Outcome:       string(res.Outcome),
				ActiveVersion: res.ActiveVersion,
				ImageID:       res.ActiveImageID,
				Detail:        res.Detail,
				UpdatedAt:     ts,
			}
		}
	}
	state.UpdatedAt = time.Now()
	return state, nil
}

// quarantineState 把损坏的 state.json 改名保留为证据，返回新路径。
func (s *Store) quarantineState() (string, error) {
	src := s.statePath()
	dst := fmt.Sprintf("%s.corrupt-%d", src, time.Now().UnixNano())
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func (s *Store) SaveState(state *GlobalState) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	state.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.statePath(), data)
}

func (s *Store) runPath(id string) string { return filepath.Join(s.dir, "runs", id+".json") }

func (s *Store) SaveRun(run *Run) error {
	if err := os.MkdirAll(filepath.Join(s.dir, "runs"), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.runPath(run.ID), data)
}

func (s *Store) LoadRun(id string) (*Run, error) {
	data, err := os.ReadFile(s.runPath(id))
	if err != nil {
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// ImageIDForVersion 返回运行记录中"该项目最近一次以指定版本运行"时记下的
// 镜像内容摘要；无记录返回空。回滚按此钉死目标：标签可以重打移动，
// 记录里的摘要是"那个版本当时跑的确实是这份内容"的不可动证据。
func (s *Store) ImageIDForVersion(project, version string) string {
	entries, err := os.ReadDir(filepath.Join(s.dir, "runs"))
	if err != nil {
		return ""
	}
	var runs []*Run
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		run, err := s.LoadRun(e.Name()[:len(e.Name())-len(".json")])
		if err != nil {
			continue
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	for _, run := range runs {
		res, ok := run.Results[project]
		if ok && res.ActiveVersion == version && res.ActiveImageID != "" {
			return res.ActiveImageID
		}
	}
	return ""
}

// RunSummary 是运行列表的精简视图。
type RunSummary struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Outcomes   map[string]int `json:"outcomes"`
}

func (s *Store) ListRuns() ([]RunSummary, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return []RunSummary{}, nil
		}
		return nil, err
	}

	var summaries []RunSummary
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		run, err := s.LoadRun(e.Name()[:len(e.Name())-len(".json")])
		if err != nil {
			continue
		}
		summary := RunSummary{
			ID:         run.ID,
			Type:       run.Type,
			State:      run.State,
			StartedAt:  run.StartedAt,
			FinishedAt: run.FinishedAt,
			Outcomes:   map[string]int{},
		}
		for _, r := range run.Results {
			summary.Outcomes[string(r.Outcome)]++
		}
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].StartedAt.After(summaries[j].StartedAt)
	})
	return summaries, nil
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
