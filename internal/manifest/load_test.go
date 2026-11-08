package manifest

// 装载层重名取舍与目录排除的测试。
// 背景：并行 worktree / 检出副本会带着清单副本出现在扫描根里；
// 重名必须确定性取舍并降级为警告，不能阻断运行（否则开个分支干活
// 就瘫痪整条部署链路）。规则见 LoadAggregate 注释。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAggregate_DuplicateWinnerByScanRootOrder(t *testing.T) {
	base := t.TempDir()
	root1 := filepath.Join(base, "first")
	root2 := filepath.Join(base, "second")
	writeManifest(t, root1, "repo-a", validManifest) // project: svc-a
	writeManifest(t, root2, "repo-b", validManifest) // 同名 svc-a

	// 配置顺序在先的扫描根取胜，与目录现场无关。
	manifests, dups, errs := LoadAggregate([]string{root1, root2}, "")
	if len(errs) != 0 {
		t.Fatalf("load errors: %v", errs)
	}
	if len(manifests) != 1 || manifests[0].Project != "svc-a" {
		t.Fatalf("want exactly one svc-a, got %+v", manifests)
	}
	if got := manifests[0].SourcePath; !strings.Contains(got, filepath.Join("first", "repo-a")) {
		t.Fatalf("winner must come from the first configured scan root, got %s", got)
	}
	if len(dups) != 1 {
		t.Fatalf("want 1 duplicate group, got %+v", dups)
	}
	g := dups[0]
	if g.Project != "svc-a" || !strings.Contains(g.Kept, filepath.Join("first", "repo-a")) {
		t.Fatalf("duplicate group wrong: %+v", g)
	}
	// 取舍记录必须列全被忽略副本的路径，让人知道重复在哪。
	if len(g.Ignored) != 1 || !strings.Contains(g.Ignored[0], filepath.Join("second", "repo-b")) {
		t.Fatalf("ignored paths must be listed in full: %+v", g)
	}

	// 交换配置顺序 → 取胜者随之改变（确定性来自配置顺序）。
	manifests, dups, errs = LoadAggregate([]string{root2, root1}, "")
	if len(errs) != 0 || len(manifests) != 1 {
		t.Fatalf("reversed order: manifests=%d errs=%v", len(manifests), errs)
	}
	if got := manifests[0].SourcePath; !strings.Contains(got, filepath.Join("second", "repo-b")) {
		t.Fatalf("reversed order must flip the winner, got %s", got)
	}
	if len(dups) != 1 || len(dups[0].Ignored) != 1 {
		t.Fatalf("reversed order must still record the duplicate: %+v", dups)
	}
}

func TestLoadAggregate_DuplicateWithinRootAlphabetical(t *testing.T) {
	// 同一扫描根内按目录名排序：主检出（Avalon）先于其 worktree
	// 副本（Avalon-fixes）装载，因而取胜——正是触发本规则的现实场景。
	dir := t.TempDir()
	writeManifest(t, dir, "Avalon-fixes", validManifest)
	writeManifest(t, dir, "Avalon", validManifest)

	manifests, dups, errs := LoadAggregate([]string{dir}, "")
	if len(errs) != 0 {
		t.Fatalf("load errors: %v", errs)
	}
	if len(manifests) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(manifests))
	}
	if !strings.Contains(manifests[0].SourcePath, string(filepath.Separator)+"Avalon"+string(filepath.Separator)) {
		t.Fatalf("alphabetically earlier dir must win, got %s", manifests[0].SourcePath)
	}
	if len(dups) != 1 || !strings.Contains(dups[0].Ignored[0], "Avalon-fixes") {
		t.Fatalf("ignored worktree copy must be recorded: %+v", dups)
	}
}

func TestLoadAggregate_ExcludesNonRepoDirs(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "real-repo", validManifest)
	// 隐藏目录（.trae worktrees 容器、.git 等）与构建产物/临时目录不得入选。
	for _, name := range []string{".trae", ".git", "build", "dist", "out", "target", "tmp", "temp", "node_modules", "vendor"} {
		writeManifest(t, dir, name, strings.Replace(validManifest, "project: svc-a", "project: svc-excluded", 1))
	}

	manifests, dups, errs := LoadAggregate([]string{dir}, "")
	if len(errs) != 0 {
		t.Fatalf("load errors: %v", errs)
	}
	if len(manifests) != 1 || manifests[0].Project != "svc-a" {
		t.Fatalf("excluded dirs must not be scanned, got %+v", manifests)
	}
	if len(dups) != 0 {
		t.Fatalf("excluded dirs must not even produce duplicates: %+v", dups)
	}
}

func TestLoadAggregate_ExternalDuplicateLosesToRepo(t *testing.T) {
	base := t.TempDir()
	scanRoot := filepath.Join(base, "scan")
	extDir := filepath.Join(base, "ext")
	writeManifest(t, scanRoot, "repo", validManifest) // project: svc-a
	// 外部清单目录在后装载：与仓库清单重名时让位。
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "svc-a.yaml"), []byte(validManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	manifests, dups, errs := LoadAggregate([]string{scanRoot}, extDir)
	if len(errs) != 0 {
		t.Fatalf("load errors: %v", errs)
	}
	if len(manifests) != 1 || !strings.Contains(manifests[0].SourcePath, "repo") {
		t.Fatalf("repo manifest must win over external duplicate, got %+v", manifests)
	}
	if len(dups) != 1 || !strings.Contains(dups[0].Ignored[0], filepath.Join("ext", "svc-a.yaml")) {
		t.Fatalf("external duplicate must be recorded: %+v", dups)
	}
}

func TestResolveLocalPath(t *testing.T) {
	tests := []struct {
		name       string
		localPath  string
		sourcePath string
		want       string
	}{
		{"absolute stays", "/srv/build/avalon", "/ws/Avalon/deploy-manifest.yaml", "/srv/build/avalon"},
		{"relative resolves against manifest dir", "build/avalon", "/ws/Avalon/deploy-manifest.yaml", filepath.Join("/ws/Avalon", "build", "avalon")},
		{"relative without source path keeps cwd semantics", "build/avalon", "", "build/avalon"},
		{"empty stays empty", "", "/ws/Avalon/deploy-manifest.yaml", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manifest{SourcePath: tt.sourcePath}
			m.Baremetal.Artifact.Source = "local_path"
			m.Baremetal.Artifact.LocalPath = tt.localPath
			if got := m.ResolveLocalPath(); got != tt.want {
				t.Errorf("ResolveLocalPath() = %q, want %q", got, tt.want)
			}
		})
	}
}
