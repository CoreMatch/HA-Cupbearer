// Package artifact 负责按 Manifest 解析产物来源、下载/复制并校验 sha256，
// 幂等地把产物落到版本目录。来源三种：
//   - ci_release：CI 构建后发布的 Release 资产（资产命名硬约定：
//     <项目名>-linux-amd64 与 <项目名>-linux-amd64.sha256 双资产）；
//   - local_path：本机/挂载路径上的产物文件（校验和文件为同名 + .sha256）；
//   - grail：Grail 产物分发上游（签名清单 + 规范 URL 取件，见 grail.go）。
//
// 健壮性约定（失败必须说清楚，不静默降级）：
//   - 落盘前做磁盘空间预检：能确定产物大小时，可用空间不足直接拒绝并给出差额；
//   - 产物先写临时文件、整体校验 sha256 通过后才原子改名到目标，下载中断不会
//     留下"看似完整实则残缺"的文件被当成已存在跳过；
//   - 每次落盘前清理上次中断遗留的孤儿临时文件。
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/manifest"
)

// spaceMargin 是在产物大小之外额外要求的余量（文件系统开销），避免贴着上限写满。
const spaceMargin int64 = 1 << 20 // 1 MiB

// tmpPrefix 是落盘临时文件前缀；凡此前缀的文件都是未校验完成的中间产物。
const tmpPrefix = ".artifact-"

// releaseBaseURL 是 Release 资产下载基址；测试可指向本地 HTTP 服务。
var releaseBaseURL = "https://github.com"

// availableBytes 返回 path 所在文件系统当前可用空间（字节）。
// 定义为变量以便测试替换。
var availableBytes = func(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// Stage 把项目产物落到 destDir/<binary_name>。
// 幂等：目标已存在且 sha256 与期望一致时直接跳过。
// 落盘前做磁盘空间预检；产物经临时文件 + sha256 校验后原子改名。
func Stage(ctx context.Context, m *manifest.Manifest, destDir string, creds *config.Credentials) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(destDir, m.Baremetal.Install.BinaryName)

	expectSHA, source, fetchURL, err := resolveChecksum(ctx, m, creds)
	if err != nil {
		return "", err
	}

	if existing, err := fileSHA256(dest); err == nil && existing == expectSHA {
		return expectSHA, nil // 幂等跳过
	}

	// 磁盘空间预检：能确定产物大小时，不足则明确拒绝并给出差额。
	size, known, err := artifactSize(ctx, m, creds)
	if err != nil {
		return "", err
	}
	if known {
		if err := ensureDiskSpace(destDir, size+spaceMargin); err != nil {
			return "", err
		}
	}

	// 清理上次中断遗留的孤儿临时文件（未校验完成，可安全删除）。
	cleanOrphanTempFiles(destDir)

	tmp, err := os.CreateTemp(destDir, tmpPrefix)
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	switch m.Baremetal.Artifact.Source {
	case "local_path":
		err = copyLocal(m.ResolveLocalPath(), tmp)
	case "ci_release":
		err = downloadStream(ctx, fetchURL, githubTokenOf(creds), tmp)
	case "grail":
		err = downloadStream(ctx, fetchURL, grailReadTokenOf(creds), tmp)
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
		return "", fmt.Errorf("sha256 mismatch: expect %s, got %s (partial/corrupted fetch rejected, target not written)", expectSHA, got)
	}

	if err := os.Chmod(tmpName, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return "", err
	}
	return expectSHA, nil
}

// VerifyLocalArtifact 校验 local_path 来源的产物与其校验和旁路：
// 文件必须存在且是普通文件；同名 .sha256 旁路必须存在；旁路声明的
// 校验和必须与产物实际内容一致。任一失败返回带具体路径的错误。
// 部署流水线（Stage）对这三者都是硬要求——在前置检查阶段查出来，
// 而不是等流水线执行到 artifact 步才失败。
func VerifyLocalArtifact(m *manifest.Manifest) error {
	p := m.ResolveLocalPath()
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("local_path artifact path is empty")
	}
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("local_path artifact missing: %s: %w", p, err)
	}
	if info.IsDir() {
		return fmt.Errorf("local_path artifact %s is a directory, not a file", p)
	}
	sumPath := p + ".sha256"
	data, err := os.ReadFile(sumPath)
	if err != nil {
		return fmt.Errorf("checksum sidecar missing: %s (local_path 部署硬要求同名 .sha256 旁路；产物 %s 在位但旁路缺失): %w",
			sumPath, p, err)
	}
	expect := parseChecksum(string(data))
	if expect == "" {
		return fmt.Errorf("checksum sidecar %s is empty or unparseable", sumPath)
	}
	got, err := fileSHA256(p)
	if err != nil {
		return fmt.Errorf("hash local artifact %s: %w", p, err)
	}
	if got != expect {
		return fmt.Errorf("checksum sidecar %s mismatch: sidecar declares %s, artifact actually hashes to %s (stale sidecar or modified artifact)",
			sumPath, expect, got)
	}
	return nil
}

// EnsureDiskSpaceFor 是部署前置检查（preflight）复用的磁盘空间预检：
// 能确定产物大小时校验 destDir 可用空间（含余量），不足返回带差额的错误。
// 返回值 checked 表示空间是否实际得到校验——产物大小未知（如 ci_release
// 的 HEAD 读不到 Content-Length）时为 false，调用方应报警告而非阻断
// （落盘安全性仍由临时文件 + sha256 保证）。
func EnsureDiskSpaceFor(ctx context.Context, m *manifest.Manifest, destDir string, creds *config.Credentials) (checked bool, err error) {
	size, known, err := artifactSize(ctx, m, creds)
	if err != nil {
		return false, err
	}
	if !known {
		return false, nil
	}
	return true, ensureDiskSpace(destDir, size+spaceMargin)
}

// ensureDiskSpace 校验 dir 所在文件系统可用空间不低于 required，
// 不足时返回带差额的明确错误。dir 尚不存在时（preflight 场景）
// 向上取最近存在的祖先目录做统计。
func ensureDiskSpace(dir string, required int64) error {
	probe := dir
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return fmt.Errorf("check disk space at %s: no existing ancestor found", dir)
		}
		probe = parent
	}
	avail, err := availableBytes(probe)
	if err != nil {
		return fmt.Errorf("check disk space at %s: %w", dir, err)
	}
	if avail < required {
		return fmt.Errorf("insufficient disk space at %s: need %s, available %s, short by %s",
			dir, humanSize(required), humanSize(avail), humanSize(required-avail))
	}
	return nil
}

// artifactSize 返回产物大小（字节）及是否已知。
// local_path 直接 stat 源文件；ci_release 通过 HEAD 读取 Content-Length，
// 读不到时视为未知（下载仍以临时文件 + 校验保证安全，不会写坏目标）；
// grail 的大小来自签名清单声明（解析即验签，见 grail.go）。
func artifactSize(ctx context.Context, m *manifest.Manifest, creds *config.Credentials) (int64, bool, error) {
	switch m.Baremetal.Artifact.Source {
	case "local_path":
		info, err := os.Stat(m.ResolveLocalPath())
		if err != nil {
			return 0, false, fmt.Errorf("stat local artifact: %w", err)
		}
		return info.Size(), true, nil
	case "ci_release":
		url := releaseAssetURL(m, m.EffectiveAsset())
		size, ok, err := headContentLength(ctx, url, githubTokenOf(creds))
		if err != nil || !ok {
			return 0, false, nil // 大小未知：不阻断，落盘安全性由临时文件+校验保证
		}
		return size, true, nil
	case "grail":
		art, err := resolveGrailArtifact(ctx, m, creds)
		if err != nil {
			return 0, false, err
		}
		if art.size <= 0 {
			return 0, false, nil
		}
		return art.size, true, nil
	default:
		return 0, false, fmt.Errorf("unsupported artifact source %q", m.Baremetal.Artifact.Source)
	}
}

func githubTokenOf(creds *config.Credentials) string {
	if creds == nil {
		return ""
	}
	return creds.GithubToken
}

func grailReadTokenOf(creds *config.Credentials) string {
	if creds == nil {
		return ""
	}
	return creds.GrailReadToken
}

func headContentLength(ctx context.Context, url, token string) (int64, bool, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, false, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("HEAD %s -> %d", url, resp.StatusCode)
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength, true, nil
	}
	return 0, false, nil
}

// cleanOrphanTempFiles 删除 dir 下遗留的未校验临时文件。
func cleanOrphanTempFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// resolveChecksum 解析产物的期望校验和，并顺带给出取件地址：
// 返回值 fetchURL 对 ci_release 是资产下载地址，对 grail 是规范取件
// URL（已完成端到端验签），对 local_path 为空。
func resolveChecksum(ctx context.Context, m *manifest.Manifest, creds *config.Credentials) (sha string, source string, fetchURL string, err error) {
	switch m.Baremetal.Artifact.Source {
	case "local_path":
		sumPath := m.ResolveLocalPath() + ".sha256"
		data, err := os.ReadFile(sumPath)
		if err != nil {
			return "", sumPath, "", fmt.Errorf("read checksum file: %w", err)
		}
		return parseChecksum(string(data)), sumPath, "", nil
	case "ci_release":
		url := releaseAssetURL(m, m.EffectiveChecksumAsset())
		body, err := fetchBytes(ctx, url, githubTokenOf(creds))
		if err != nil {
			return "", url, "", err
		}
		return parseChecksum(string(body)), url, releaseAssetURL(m, m.EffectiveAsset()), nil
	case "grail":
		art, err := resolveGrailArtifact(ctx, m, creds)
		if err != nil {
			return "", "grail", "", err
		}
		if art.graceNote != "" {
			log.Printf("artifact: %s", art.graceNote)
		}
		return art.digestHex, art.fetchURL, art.fetchURL, nil
	default:
		return "", "", "", fmt.Errorf("unsupported artifact source %q", m.Baremetal.Artifact.Source)
	}
}

func releaseAssetURL(m *manifest.Manifest, asset string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s",
		releaseBaseURL, m.Baremetal.Artifact.Release.Repo, m.EffectiveTag(), asset)
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

// downloadStream 以流式方式把 url 内容写入 dst（不全量读入内存），
// 大产物不会撑爆内存；网络中断时返回错误，已写入的临时文件由调用方清理。
func downloadStream(ctx context.Context, url, token string, dst *os.File) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s -> %d", url, resp.StatusCode)
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		return err
	}
	return nil
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

// humanSize 把字节数格式化为易读字符串（用于错误信息）。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
