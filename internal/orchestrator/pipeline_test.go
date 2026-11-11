package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"HA-Cupbearer/internal/manifest"
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

// 声明 shared_data 时，exec 行必须经 env 注入 avalonhelper.basePath 绝对路径：
// 带点变量名无法经 env_file/shell 赋值注入，只能由 env 命令直接设置。
func TestRenderScript_SharedDataInjection(t *testing.T) {
	m := &manifest.Manifest{
		Project: "avalon",
		Baremetal: manifest.Baremetal{
			Process: manifest.ProcessSpec{
				Tinyvisor: manifest.TinyvisorSpec{EnvFile: "/opt/avl/.env"},
			},
			Install: manifest.InstallSpec{Root: "/opt/avl", BinaryName: "avalon", SharedData: "shared"},
		},
	}
	script := renderScript(m)
	lines := strings.Split(strings.TrimSpace(script), "\n")
	last := lines[len(lines)-1]
	want := `exec env "avalonhelper.basePath=/opt/avl/shared" ./current/avalon`
	if last != want {
		t.Errorf("shared_data exec line:\n got %q\nwant %q", last, want)
	}
	// env 注入必须在 env file 注入之后（后者优先级更高）。
	envIdx := strings.Index(script, `. "/opt/avl/.env"`)
	injectIdx := strings.Index(script, "avalonhelper.basePath=")
	if envIdx < 0 || injectIdx < 0 || injectIdx < envIdx {
		t.Errorf("basePath injection must come after env file sourcing, got:\n%s", script)
	}

	// 未声明 shared_data 时 exec 行保持原样（不引入 env 包装）。
	m.Baremetal.Install.SharedData = ""
	plain := renderScript(m)
	if lastPlain := strings.Split(strings.TrimSpace(plain), "\n"); lastPlain[len(lastPlain)-1] != "exec ./current/avalon" {
		t.Errorf("without shared_data the exec line must stay bare, got:\n%s", plain)
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
