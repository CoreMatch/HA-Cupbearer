package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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
