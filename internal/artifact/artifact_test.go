package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"HA-Services-Deployer/internal/manifest"
)

func localPathManifest(t *testing.T, content []byte) (*manifest.Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "svc-a-linux-amd64")
	if err := os.WriteFile(src, content, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if err := os.WriteFile(src+".sha256", []byte(hex.EncodeToString(sum[:])+"  svc-a-linux-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Project:        "svc-a",
		ServiceVersion: "1.0.0",
		Baremetal: manifest.Baremetal{
			Install:  manifest.InstallSpec{BinaryName: "svc-a"},
			Artifact: manifest.ArtifactSpec{Source: "local_path", LocalPath: src},
		},
	}
	return m, src
}

func TestStage_LocalPath_VerifyAndIdempotent(t *testing.T) {
	content := []byte("binary-v1")
	m, _ := localPathManifest(t, content)
	destDir := t.TempDir()

	sha1, err := Stage(context.Background(), m, destDir, "")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	dest := filepath.Join(destDir, "svc-a")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "binary-v1" {
		t.Fatalf("staged content = %q", data)
	}
	info, _ := os.Stat(dest)
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("staged artifact must be executable, mode=%v", info.Mode())
	}

	// 幂等：再次 Stage 不报错且不改写文件
	if _, err := Stage(context.Background(), m, destDir, ""); err != nil {
		t.Fatalf("idempotent stage: %v", err)
	}
	if sha1 == "" {
		t.Error("sha must not be empty")
	}
}

func TestStage_LocalPath_ShaMismatch(t *testing.T) {
	content := []byte("binary-v1")
	m, src := localPathManifest(t, content)
	// 篡改校验和文件
	if err := os.WriteFile(src+".sha256", []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()
	if _, err := Stage(context.Background(), m, destDir, ""); err == nil {
		t.Fatal("expect sha256 mismatch error")
	}
	// 失败时不得留下半成品
	if _, err := os.Stat(filepath.Join(destDir, "svc-a")); !os.IsNotExist(err) {
		t.Error("failed stage must not leave destination file")
	}
}

func TestStage_LocalPath_MissingChecksum(t *testing.T) {
	content := []byte("binary-v1")
	m, src := localPathManifest(t, content)
	os.Remove(src + ".sha256")
	destDir := t.TempDir()
	if _, err := Stage(context.Background(), m, destDir, ""); err == nil {
		t.Fatal("expect error when checksum file missing (强制校验)")
	}
}

func TestParseChecksum(t *testing.T) {
	if got := parseChecksum("ABC123  file\n"); got != "abc123" {
		t.Errorf("parseChecksum = %q", got)
	}
	if got := parseChecksum("  ff00  \n"); got != "ff00" {
		t.Errorf("parseChecksum = %q", got)
	}
}

func TestStage_InsufficientDiskSpace(t *testing.T) {
	content := []byte("binary-v1")
	m, _ := localPathManifest(t, content)
	destDir := t.TempDir()

	orig := availableBytes
	availableBytes = func(path string) (int64, error) { return 10, nil } // 仅 10 字节可用
	defer func() { availableBytes = orig }()

	_, err := Stage(context.Background(), m, destDir, "")
	if err == nil {
		t.Fatal("expected insufficient disk space error")
	}
	if !strings.Contains(err.Error(), "insufficient disk space") {
		t.Errorf("error should state insufficient space: %v", err)
	}
	if !strings.Contains(err.Error(), "short by") {
		t.Errorf("error should state the shortfall: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(destDir, "svc-a")); !os.IsNotExist(statErr) {
		t.Error("dest must not be written when disk space is insufficient")
	}
}

func TestEnsureDiskSpace(t *testing.T) {
	dir := t.TempDir()
	if err := ensureDiskSpace(dir, 1); err != nil {
		t.Errorf("1 byte should be available in a fresh tempdir: %v", err)
	}
	orig := availableBytes
	availableBytes = func(path string) (int64, error) { return 100, nil }
	defer func() { availableBytes = orig }()
	err := ensureDiskSpace(dir, 1000)
	if err == nil {
		t.Fatal("expected error when required > available")
	}
	if !strings.Contains(err.Error(), "short by") {
		t.Errorf("error should include shortfall: %v", err)
	}
}

func TestEnsureDiskSpaceFor_InsufficientReportsShortfall(t *testing.T) {
	content := []byte("binary-v1")
	m, _ := localPathManifest(t, content)

	orig := availableBytes
	availableBytes = func(path string) (int64, error) { return 5, nil }
	defer func() { availableBytes = orig }()

	checked, err := EnsureDiskSpaceFor(context.Background(), m, t.TempDir(), "")
	if !checked || err == nil {
		t.Fatal("expected checked=true with insufficient-space error")
	}
	if !strings.Contains(err.Error(), "short by") {
		t.Errorf("error should include the shortfall: %v", err)
	}
}

func TestEnsureDiskSpaceFor_NonexistentDestUsesExistingAncestor(t *testing.T) {
	content := []byte("binary-v1")
	m, _ := localPathManifest(t, content)
	destDir := filepath.Join(t.TempDir(), "releases", "1.0.0") // 尚不存在（preflight 场景）

	var probed string
	orig := availableBytes
	availableBytes = func(path string) (int64, error) { probed = path; return 1 << 30, nil }
	defer func() { availableBytes = orig }()

	checked, err := EnsureDiskSpaceFor(context.Background(), m, destDir, "")
	if err != nil || !checked {
		t.Fatalf("expected pass, got checked=%v err=%v", checked, err)
	}
	if probed == "" || probed == destDir {
		t.Errorf("must stat an existing ancestor, probed=%q", probed)
	}
}

func TestEnsureDiskSpaceFor_UnknownSizeNotChecked(t *testing.T) {
	// ci_release 且 HEAD 不可达 → 大小未知 → 不阻断，返回 checked=false。
	m := ciReleaseManifest()
	origBase := releaseBaseURL
	releaseBaseURL = "http://127.0.0.1:1" // 立即拒绝
	defer func() { releaseBaseURL = origBase }()

	checked, err := EnsureDiskSpaceFor(context.Background(), m, t.TempDir(), "")
	if err != nil {
		t.Fatalf("unknown size must not error: %v", err)
	}
	if checked {
		t.Error("unknown artifact size must not be reported as checked")
	}
}

func TestStage_CleansOrphanTempFiles(t *testing.T) {
	content := []byte("binary-v1")
	m, _ := localPathManifest(t, content)
	destDir := t.TempDir()

	orphan := filepath.Join(destDir, tmpPrefix+"orphan123")
	if err := os.WriteFile(orphan, []byte("partial-junk"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Stage(context.Background(), m, destDir, ""); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan temp file from a crashed prior run should be removed")
	}
	if _, err := os.Stat(filepath.Join(destDir, "svc-a")); err != nil {
		t.Errorf("dest missing after stage: %v", err)
	}
}

func ciReleaseManifest() *manifest.Manifest {
	return &manifest.Manifest{
		Project:        "svc-a",
		ServiceVersion: "1.0.0",
		Baremetal: manifest.Baremetal{
			Install: manifest.InstallSpec{BinaryName: "svc-a"},
			Artifact: manifest.ArtifactSpec{
				Source:  "ci_release",
				Release: manifest.ReleaseSpec{Repo: "org/repo", Tag: "v{service_version}"},
			},
		},
	}
}

// ciReleaseServer 提供一个可控的 Release 资产服务器；assetBody 为实际下发内容。
func ciReleaseServer(t *testing.T, assetBody []byte) *httptest.Server {
	t.Helper()
	full := []byte("full-binary-content")
	sum := sha256.Sum256(full)
	sumHex := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/org/repo/releases/download/v1.0.0/svc-a-linux-amd64.sha256",
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, sumHex+"  svc-a-linux-amd64\n")
		})
	mux.HandleFunc("/org/repo/releases/download/v1.0.0/svc-a-linux-amd64",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(assetBody)))
				w.WriteHeader(http.StatusOK)
				return
			}
			w.Write(assetBody)
		})
	return httptest.NewServer(mux)
}

func TestStage_CiReleaseSuccess(t *testing.T) {
	full := []byte("full-binary-content")
	srv := ciReleaseServer(t, full)
	defer srv.Close()

	origBase := releaseBaseURL
	releaseBaseURL = srv.URL
	defer func() { releaseBaseURL = origBase }()

	destDir := t.TempDir()
	if _, err := Stage(context.Background(), ciReleaseManifest(), destDir, ""); err != nil {
		t.Fatalf("stage ci_release: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destDir, "svc-a"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(full) {
		t.Errorf("staged content mismatch")
	}
}

func TestStage_CiReleaseTruncatedDownload(t *testing.T) {
	// 服务端返回的内容短于校验和对应的完整内容，模拟下载中断导致的残缺文件。
	srv := ciReleaseServer(t, []byte("full")) // 残缺
	defer srv.Close()

	origBase := releaseBaseURL
	releaseBaseURL = srv.URL
	defer func() { releaseBaseURL = origBase }()

	destDir := t.TempDir()
	_, err := Stage(context.Background(), ciReleaseManifest(), destDir, "")
	if err == nil {
		t.Fatal("expected sha256 mismatch for truncated download")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("error should be a sha256 mismatch: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(destDir, "svc-a")); !os.IsNotExist(statErr) {
		t.Error("truncated download must not produce a dest file")
	}
	entries, _ := os.ReadDir(destDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			t.Errorf("orphan temp file left behind: %s", e.Name())
		}
	}
}
