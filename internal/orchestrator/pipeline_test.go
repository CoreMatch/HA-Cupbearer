package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"HA-Services-Deployer/internal/manifest"
)

func TestSwitchSymlinkAtomic(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "releases", "1.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "releases", "2.0.0"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := switchSymlink(root, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if got := readActiveVersion(root); got != "1.0.0" {
		t.Fatalf("active = %q, want 1.0.0", got)
	}

	if err := switchSymlink(root, "2.0.0"); err != nil {
		t.Fatal(err)
	}
	if got := readActiveVersion(root); got != "2.0.0" {
		t.Fatalf("active = %q, want 2.0.0", got)
	}

	// 无残留临时链接
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp link: %s", e.Name())
		}
	}
}

func TestRenderScript_ExecRule(t *testing.T) {
	m := &manifest.Manifest{
		Project: "svc-a",
		Baremetal: manifest.Baremetal{
			Process: manifest.ProcessSpec{
				Tinyvisor: manifest.TinyvisorSpec{EnvFile: "/opt/svc-a/.env"},
			},
			Install: manifest.InstallSpec{BinaryName: "svc-a"},
		},
	}
	script := renderScript(m)
	lines := strings.Split(strings.TrimSpace(script), "\n")
	last := lines[len(lines)-1]
	if last != "exec ./current/svc-a" {
		t.Errorf("script must end with exec line, got %q", last)
	}
	if !strings.Contains(script, `. "/opt/svc-a/.env"`) {
		t.Errorf("script must source env file with set -a, got:\n%s", script)
	}
}

func TestListReleasesAndPrune(t *testing.T) {
	root := t.TempDir()
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0", "2.0.0"} {
		if err := os.MkdirAll(releaseDir(root, v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 按 mtime 排序；prune 保留 2 个，且永不删除当前生效版本。
	removed := pruneReleases(root, "1.0.0", 2)
	entries, _ := os.ReadDir(filepath.Join(root, "releases"))
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["1.0.0"] {
		t.Errorf("active version 1.0.0 must survive pruning, removed=%v", removed)
	}
	if len(entries) > 3 {
		t.Errorf("expect at most 3 releases kept (keep=2 + protected active), got %v", names)
	}
}
