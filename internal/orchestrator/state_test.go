package orchestrator

import (
	"os"
	"strings"
	"testing"
	"time"

	"HA-Services-Deployer/internal/models"
)

func newTestOrchestrator(dir string) *Orchestrator {
	return &Orchestrator{store: NewStore(dir), cfg: &models.Config{}}
}

func seedRun(t *testing.T, s *Store, id, project, outcome, version string, started time.Time) {
	t.Helper()
	finished := started.Add(time.Minute)
	run := &Run{
		ID:         id,
		Type:       "deploy",
		State:      "done",
		StartedAt:  started,
		FinishedAt: &finished,
		Results: map[string]*ProjectResult{
			project: {Project: project, Outcome: ProjectOutcome(outcome), ActiveVersion: version},
		},
	}
	if err := s.SaveRun(run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

func TestLoadStateRecovered_CorruptRebuildFromRuns(t *testing.T) {
	dir := t.TempDir()
	o := newTestOrchestrator(dir)

	now := time.Now()
	seedRun(t, o.store, "r-old", "svc-a", "deployed", "1.0.0", now.Add(-2*time.Hour))
	seedRun(t, o.store, "r-new", "svc-a", "deployed", "1.1.0", now.Add(-1*time.Hour))

	if err := os.WriteFile(o.store.statePath(), []byte("{this is not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	state, notes := o.loadStateRecovered()
	if state == nil {
		t.Fatal("state must not be nil after recovery")
	}
	got, ok := state.Projects["svc-a"]
	if !ok {
		t.Fatalf("rebuilt state missing svc-a; notes=%v", notes)
	}
	if got.ActiveVersion != "1.1.0" {
		t.Errorf("rebuilt active = %q, want latest 1.1.0", got.ActiveVersion)
	}
	if len(notes) == 0 {
		t.Fatal("expected recovery notes to be reported, got none")
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "corrupt") {
		t.Errorf("notes should mention corruption: %v", notes)
	}
	if !strings.Contains(joined, "rebuilt from run records") {
		t.Errorf("notes should mention rebuild source: %v", notes)
	}
	if _, err := os.Stat(o.store.statePath()); !os.IsNotExist(err) {
		t.Error("corrupt state.json should be quarantined (renamed away)")
	}
}

func TestLoadStateRecovered_CorruptNoRunsDegrades(t *testing.T) {
	dir := t.TempDir()
	o := newTestOrchestrator(dir)

	if err := os.WriteFile(o.store.statePath(), []byte("{{{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	state, notes := o.loadStateRecovered()
	if state == nil {
		t.Fatal("state must not be nil even when degraded")
	}
	if len(state.Projects) != 0 {
		t.Errorf("expected empty (full re-run) state, got %v", state.Projects)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "full re-run") {
		t.Errorf("notes must explicitly report degradation to full re-run: %v", notes)
	}
}

func TestLoadStateRecovered_MissingFileIsNormal(t *testing.T) {
	dir := t.TempDir()
	o := newTestOrchestrator(dir)

	state, notes := o.loadStateRecovered()
	if state == nil {
		t.Fatal("state must not be nil")
	}
	if len(notes) != 0 {
		t.Errorf("clean load must produce no recovery notes, got %v", notes)
	}
}

func TestLoadStateRecovered_ValidFileNoNotes(t *testing.T) {
	dir := t.TempDir()
	o := newTestOrchestrator(dir)

	if err := o.store.SaveState(&GlobalState{Projects: map[string]*ProjectState{
		"svc-a": {Outcome: "deployed", ActiveVersion: "1.0.0"},
	}}); err != nil {
		t.Fatal(err)
	}

	state, notes := o.loadStateRecovered()
	if state == nil {
		t.Fatal("nil state")
	}
	if len(notes) != 0 {
		t.Errorf("valid state must produce no notes, got %v", notes)
	}
	if state.Projects["svc-a"].ActiveVersion != "1.0.0" {
		t.Errorf("state not loaded correctly")
	}
}
