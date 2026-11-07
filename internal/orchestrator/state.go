package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

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
	Detail        string    `json:"detail,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (s *Store) statePath() string { return filepath.Join(s.dir, "state.json") }

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
		return nil, err
	}
	if state.Projects == nil {
		state.Projects = map[string]*ProjectState{}
	}
	return state, nil
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
