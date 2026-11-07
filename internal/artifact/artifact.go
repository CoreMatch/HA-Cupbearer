// Package artifact 负责按 Manifest 解析产物来源、下载/复制并校验 sha256，
// 幂等地把产物落到版本目录。来源两种：
//   - ci_release：CI 构建后发布的 Release 资产（资产命名硬约定：
//     <项目名>-linux-amd64 与 <项目名>-linux-amd64.sha256 双资产）；
//   - local_path：本机/挂载路径上的产物文件（校验和文件为同名 + .sha256）。
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"HA-Services-Deployer/internal/manifest"
)

// Stage 把项目产物落到 destDir/<binary_name>。
// 幂等：目标已存在且 sha256 与期望一致时直接跳过。
func Stage(ctx context.Context, m *manifest.Manifest, destDir string, githubToken string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(destDir, m.Baremetal.Install.BinaryName)

	expectSHA, source, err := resolveChecksum(ctx, m, githubToken)
	if err != nil {
		return "", err
	}

	if existing, err := fileSHA256(dest); err == nil && existing == expectSHA {
		return expectSHA, nil // 幂等跳过
	}

	tmp, err := os.CreateTemp(destDir, ".artifact-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	switch m.Baremetal.Artifact.Source {
	case "local_path":
		err = copyLocal(m.Baremetal.Artifact.LocalPath, tmp)
	case "ci_release":
		url := releaseAssetURL(m, m.EffectiveAsset())
		err = download(ctx, url, githubToken, tmp)
	default:
		err = fmt.Errorf("unsupported artifact source %q", m.Baremetal.Artifact.Source)
	}
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("fetch artifact from %s: %w", source, err)
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return "", err
	}
	got, err := hashStream(tmp)
	if err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got != expectSHA {
		return "", fmt.Errorf("sha256 mismatch: expect %s, got %s", expectSHA, got)
	}

	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return "", err
	}
	return expectSHA, nil
}

func resolveChecksum(ctx context.Context, m *manifest.Manifest, githubToken string) (sha string, source string, err error) {
	switch m.Baremetal.Artifact.Source {
	case "local_path":
		sumPath := m.Baremetal.Artifact.LocalPath + ".sha256"
		data, err := os.ReadFile(sumPath)
		if err != nil {
			return "", sumPath, fmt.Errorf("read checksum file: %w", err)
		}
		return parseChecksum(string(data)), sumPath, nil
	case "ci_release":
		url := releaseAssetURL(m, m.EffectiveChecksumAsset())
		body, err := fetchBytes(ctx, url, githubToken)
		if err != nil {
			return "", url, err
		}
		return parseChecksum(string(body)), url, nil
	default:
		return "", "", fmt.Errorf("unsupported artifact source %q", m.Baremetal.Artifact.Source)
	}
}

func releaseAssetURL(m *manifest.Manifest, asset string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s",
		m.Baremetal.Artifact.Release.Repo, m.EffectiveTag(), asset)
}

// parseChecksum 兼容 "<hex>" 与 "<hex>  <filename>" 两种格式。
func parseChecksum(content string) string {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(fields[0])
}

func copyLocal(src string, dst *os.File) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if _, err := io.Copy(dst, in); err != nil {
		return err
	}
	return nil
}

func download(ctx context.Context, url, token string, dst *os.File) error {
	body, err := fetchBytes(ctx, url, token)
	if err != nil {
		return err
	}
	_, err = dst.Write(body)
	return err
}

func fetchBytes(ctx context.Context, url, token string) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s -> %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2<<30))
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashStream(f)
}

func hashStream(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
