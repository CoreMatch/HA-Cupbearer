package bootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// State 是初始化层的本地持久状态：全部生成的机密值与关键事实。
// 文件权限 0600，目录 0700，绝不进仓库（.gitignore 已含 data/）。
// 幂等语义的载体：重跑时先读状态，已有值一律复用，绝不重新生成。
type State struct {
	Version   int               `json:"version"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Secrets   map[string]string `json:"secrets"`
	// InfraPorts 记录受管基础设施容器实际发布的宿主端口
	//（kind -> port）。复用时以此为准：换端口意味着重建容器，
	// 而本层不删容器——请求不同端口时沿用既有端口并如实说明。
	InfraPorts map[string]int `json:"infra_ports,omitempty"`
	// SigningKeyFingerprint 私钥指纹（公钥可由私钥推导；指纹用于核对
	// "盘上的密钥对是不是本层生成的那一对"）。
	SigningKeyFingerprint string `json:"signing_key_fingerprint,omitempty"`
}

// Store 负责状态与运行记录的落盘。
type Store struct {
	dir string
}

// NewStore 以 dir 为状态根（默认 <data>/bootstrap）。
func NewStore(dir string) *Store {
	if dir == "" {
		dir = "./data/bootstrap"
	}
	return &Store{dir: dir}
}

func (s *Store) statePath() string { return filepath.Join(s.dir, "state.json") }
func (s *Store) runsDir() string   { return filepath.Join(s.dir, "runs") }

// LoadState 读取状态；不存在返回空状态（首跑语义）。
func (s *Store) LoadState() (*State, error) {
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &State{Version: 1, Secrets: map[string]string{}}, nil
		}
		return nil, fmt.Errorf("read bootstrap state: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parse bootstrap state %s: %w", s.statePath(), err)
	}
	if st.Secrets == nil {
		st.Secrets = map[string]string{}
	}
	return &st, nil
}

// SaveState 原子写入状态（临时文件 + rename），0600 权限。
func (s *Store) SaveState(st *State) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create bootstrap state dir: %w", err)
	}
	st.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath())
}

// EnsureSecrets 幂等地取或生成全部机密值：状态里已有的原样复用
// （绝不重新生成、绝不覆盖），缺失的才生成。返回 (机密全集, 本次新生成的键, 错误)。
func (s *Store) EnsureSecrets() (map[string]string, []string, error) {
	st, err := s.LoadState()
	if err != nil {
		return nil, nil, err
	}
	var created []string
	for _, key := range secretKeys {
		if existing := st.Secrets[key]; existing != "" {
			continue
		}
		token, err := GenerateRandomToken()
		if err != nil {
			return nil, nil, err
		}
		st.Secrets[key] = token
		created = append(created, key)
	}
	if len(created) > 0 {
		if st.CreatedAt.IsZero() {
			st.CreatedAt = time.Now()
		}
		if err := s.SaveState(st); err != nil {
			return nil, nil, err
		}
	}
	out := make(map[string]string, len(secretKeys))
	for _, key := range secretKeys {
		out[key] = st.Secrets[key]
	}
	return out, created, nil
}

// AdoptSecrets 把既有目标（如已存在的 .env）里的机密值收编进状态：
//   - 状态无值而目标有值 → 采用目标值（reuse，不再生成）；
//   - 状态有值而目标值不同 → 目标值胜出（它是活值），状态改写为目标值，
//     记为漂移告警（绝不反向覆盖目标）。
//
// 收敛性：一次运行后状态与目标一致，重跑不再产生永久告警。
// 返回 (收编的键, 漂移的键, 错误)。
func (s *Store) AdoptSecrets(existing map[string]string) ([]string, []string, error) {
	st, err := s.LoadState()
	if err != nil {
		return nil, nil, err
	}
	var adopted, drifted []string
	changed := false
	for _, key := range secretKeys {
		target, ok := existing[key]
		if !ok || target == "" {
			continue
		}
		cur := st.Secrets[key]
		if cur == "" {
			st.Secrets[key] = target
			adopted = append(adopted, key)
			changed = true
		} else if cur != target {
			st.Secrets[key] = target
			drifted = append(drifted, key)
			changed = true
		}
	}
	if changed {
		if st.CreatedAt.IsZero() {
			st.CreatedAt = time.Now()
		}
		if err := s.SaveState(st); err != nil {
			return nil, nil, err
		}
	}
	return adopted, drifted, nil
}

// EnsureInfraSecret 幂等地取或生成单个基础设施机密（root/redis 口令）。
// 与平台机密分离：只在真正一键拉起基础设施时生成，避免无谓落盘。
func (s *Store) EnsureInfraSecret(key string) (string, bool, error) {
	st, err := s.LoadState()
	if err != nil {
		return "", false, err
	}
	if existing := st.Secrets[key]; existing != "" {
		return existing, false, nil
	}
	token, err := GenerateRandomToken()
	if err != nil {
		return "", false, err
	}
	st.Secrets[key] = token
	if st.CreatedAt.IsZero() {
		st.CreatedAt = time.Now()
	}
	if err := s.SaveState(st); err != nil {
		return "", false, err
	}
	return token, true, nil
}

// SetInfraPort 记录受管基础设施容器的宿主发布端口。
func (s *Store) SetInfraPort(kind string, port int) error {
	st, err := s.LoadState()
	if err != nil {
		return err
	}
	if st.InfraPorts == nil {
		st.InfraPorts = map[string]int{}
	}
	if st.InfraPorts[kind] == port {
		return nil
	}
	st.InfraPorts[kind] = port
	return s.SaveState(st)
}

// GetInfraPort 读取受管基础设施容器的宿主发布端口（未记录返回 0）。
func (s *Store) GetInfraPort(kind string) int {
	st, err := s.LoadState()
	if err != nil || st.InfraPorts == nil {
		return 0
	}
	return st.InfraPorts[kind]
}

// SetSigningKeyFingerprint 记录密钥对指纹（生成密钥步骤成功后调用）。
func (s *Store) SetSigningKeyFingerprint(fp string) error {
	st, err := s.LoadState()
	if err != nil {
		return err
	}
	if st.SigningKeyFingerprint == fp {
		return nil
	}
	st.SigningKeyFingerprint = fp
	return s.SaveState(st)
}

// --- 运行记录（断点续跑与进度可见性的载体） ---

// StepStatus 是单步状态。
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepOK      StepStatus = "ok"
	StepSkipped StepStatus = "skipped" // 已存在，幂等跳过（保留原值）
	StepWarn    StepStatus = "warn"    // 完成但有漂移/告警
	StepFailed  StepStatus = "failed"
)

// StepRecord 是单步结果。Detail 里只允许掩码/指纹，绝不允许明文机密。
type StepRecord struct {
	Name      string     `json:"name"`
	Status    StepStatus `json:"status"`
	Detail    string     `json:"detail,omitempty"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Run 是一次初始化运行的记录（形态对齐编排运行记录的可查询性）。
type Run struct {
	ID         string                `json:"id"`
	State      string                `json:"state"` // running | done | failed
	DryRun     bool                  `json:"dry_run"`
	Steps      map[string]StepRecord `json:"steps"`
	StepOrder  []string              `json:"step_order"`
	Secrets    map[string]string     `json:"secrets,omitempty"` // 只存指纹
	Notes      []string              `json:"notes,omitempty"`
	StartedAt  time.Time             `json:"started_at"`
	FinishedAt *time.Time            `json:"finished_at,omitempty"`
}

// RunSummary 是列表视图。
type RunSummary struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	DryRun     bool       `json:"dry_run"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	StepCounts map[string]int `json:"step_counts"`
}

// SaveRun 逐项落盘（中断后可续跑的证据链）。
func (s *Store) SaveRun(run *Run) error {
	if err := os.MkdirAll(s.runsDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.runsDir(), run.ID+".json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.runsDir(), run.ID+".json"))
}

// LoadRun 读取单次运行记录。
func (s *Store) LoadRun(id string) (*Run, error) {
	data, err := os.ReadFile(filepath.Join(s.runsDir(), id+".json"))
	if err != nil {
		return nil, err
	}
	var run Run
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// ListRuns 按开始时间倒序返回摘要。
func (s *Store) ListRuns() ([]RunSummary, error) {
	entries, err := os.ReadDir(s.runsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var runs []*Run
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-len(".json")]
		run, err := s.LoadRun(id)
		if err != nil {
			continue // 损坏记录跳过，列表仍可查
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	out := make([]RunSummary, 0, len(runs))
	for _, r := range runs {
		counts := map[string]int{}
		for _, step := range r.Steps {
			counts[string(step.Status)]++
		}
		out = append(out, RunSummary{
			ID: r.ID, State: r.State, DryRun: r.DryRun,
			StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, StepCounts: counts,
		})
	}
	return out, nil
}

// NextRunID 生成运行 ID（秒级时间戳 + 序号避让，同秒不覆盖）。
func (s *Store) NextRunID() string {
	base := time.Now().Format("20060102-150405")
	id := base
	for i := 2; ; i++ {
		if _, err := s.LoadRun(id); err != nil {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, i)
	}
}
