// grail 来源：从 Grail 上游解析签名清单并取件（HA-Contract grail.yaml）。
//
// 验签链（端到端，失败必须显式，禁止降级）：
//
//	本地信任锚（凭据文件里的根公钥）
//	  → 验证密钥环（根密钥签名）
//	    → 按项目取公钥，验证版本清单签名
//	      → 取清单中的 digest，流式校验字节
//
// 消费方纪律（D10）：默认信任上游，但兜底限制不可省——哈希校验是硬要求，
// 字节流被大小上限约束；不代管任何外部凭据。
package artifact

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/manifest"
)

// grail 线上结构（与 HA-Contract grail.yaml 对齐；只取 Deployer 需要的字段）。
type grailSignature struct {
	KeyID string `json:"key_id"`
	Alg   string `json:"alg"`
	Value string `json:"value"`
}

type grailEnvelope struct {
	Payload   json.RawMessage `json:"payload"`
	Signature grailSignature  `json:"signature"`
}

type grailKeyEntry struct {
	KeyID     string `json:"key_id"`
	Alg       string `json:"alg"`
	PublicKey string `json:"public_key"`
}

type grailKeyringPayload struct {
	Seq      int64                    `json:"seq"`
	Root     grailKeyEntry            `json:"root"`
	Projects map[string]grailKeyEntry `json:"projects"`
}

type grailResource struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	URL      string `json:"url"`
}

type grailGraceInfo struct {
	RemoveAt string `json:"remove_at"`
}

type grailResolveResponse struct {
	Project   string          `json:"project"`
	Channel   string          `json:"channel"`
	Version   string          `json:"version"`
	Seq       int64           `json:"seq"`
	Yanked    bool            `json:"yanked"`
	Grace     *grailGraceInfo `json:"grace"`
	Resources []grailResource `json:"resources"`
	Signature grailSignature  `json:"signature"`
}

type grailVersionManifest struct {
	Project   string          `json:"project"`
	Channel   string          `json:"channel"`
	Version   string          `json:"version"`
	Seq       int64           `json:"seq"`
	Resources []grailResource `json:"resources"`
}

// grailArtifact 是一次 grail 解析的结果。
type grailArtifact struct {
	digestHex string // 期望的 sha256 十六进制
	fetchURL  string // 规范取件 URL（绝对）
	size      int64  // 清单声明大小（用于空间预检与流式上限）
	version   string // 实际解析到的版本（latest 时是动态结果）
	graceNote string // 非空 = 版本处于宽限期（警告，钉住部署允许继续）
}

// grailClient 是面向单个 Grail 节点的最小客户端。
type grailClient struct {
	baseURL string
	token   string // 读凭据，可为空（公开节点的 release 匿名读）
	http    *http.Client
}

func newGrailClient(baseURL, token string) *grailClient {
	return &grailClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Minute},
	}
}

func (g *grailClient) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	u := g.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("GET %s -> %d: %s", path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// verifyEnvelope 用公钥验证信封签名（只对信封内的 payload 字节验签），
// 并把负载反序列化到 out。
func verifyGrailEnvelope(env *grailEnvelope, pub ed25519.PublicKey, out any) error {
	if env.Signature.Alg != "ed25519" {
		return fmt.Errorf("unsupported grail signature alg %q", env.Signature.Alg)
	}
	sig, err := base64.StdEncoding.DecodeString(env.Signature.Value)
	if err != nil {
		return fmt.Errorf("decode grail signature: %w", err)
	}
	if !ed25519.Verify(pub, env.Payload, sig) {
		return fmt.Errorf("grail signature verification failed (key %s)", env.Signature.KeyID)
	}
	if out != nil {
		if err := json.Unmarshal(env.Payload, out); err != nil {
			return fmt.Errorf("parse grail payload: %w", err)
		}
	}
	return nil
}

func decodeGrailPublicKey(b64 string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode grail public key: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("grail public key size %d, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// VerifyGrailSource 是 grail 来源的前置检查：解析 + 端到端验签（只做
// 元数据层，不下载字节）。返回可读的核对摘要；任何环节失败显式返回错误。
func VerifyGrailSource(ctx context.Context, m *manifest.Manifest, creds *config.Credentials) (string, error) {
	art, err := resolveGrailArtifact(ctx, m, creds)
	if err != nil {
		return "", err
	}
	short := art.digestHex
	if len(short) > 12 {
		short = short[:12]
	}
	detail := fmt.Sprintf("grail %s/%s resolved version=%s (digest sha256:%s…, signature chain verified)",
		m.Baremetal.Artifact.Grail.Project, m.Baremetal.Artifact.Grail.Channel, art.version, short)
	if art.graceNote != "" {
		detail += " WARNING: version in grace period; pinned deploy proceeds"
	}
	return detail, nil
}

// resolveGrailArtifact 解析并端到端验签，返回待取件产物的描述。
// 任何一步失败都显式返回错误——部署中止优于假绿灯。
func resolveGrailArtifact(ctx context.Context, m *manifest.Manifest, creds *config.Credentials) (*grailArtifact, error) {
	if creds == nil || creds.GrailRootPublicKey == "" {
		return nil, fmt.Errorf("grail source requires grail_root_public_key in the credentials file (trust anchor)")
	}
	anchor, err := decodeGrailPublicKey(creds.GrailRootPublicKey)
	if err != nil {
		return nil, fmt.Errorf("grail trust anchor: %w", err)
	}

	g := m.Baremetal.Artifact.Grail
	var readToken string
	if creds != nil {
		readToken = creds.GrailReadToken
	}
	cl := newGrailClient(g.BaseURL, readToken)

	// 1. 密钥环：锚点验签。
	var krEnv grailEnvelope
	if err := cl.getJSON(ctx, "/v1/keyring", nil, &krEnv); err != nil {
		return nil, fmt.Errorf("fetch grail keyring: %w", err)
	}
	var kr grailKeyringPayload
	if err := verifyGrailEnvelope(&krEnv, anchor, &kr); err != nil {
		return nil, fmt.Errorf("grail keyring: %w", err)
	}
	entry, ok := kr.Projects[g.Project]
	if !ok {
		return nil, fmt.Errorf("grail keyring has no project %q (keyring seq=%d)", g.Project, kr.Seq)
	}
	projectPub, err := decodeGrailPublicKey(entry.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("grail project key: %w", err)
	}

	// 2. 解析（latest 由 Grail 按"跳过 yank/宽限"规则选择；钉住则精确取）。
	version := m.EffectiveGrailVersion()
	platform := m.EffectiveGrailPlatform()
	q := url.Values{"version": {version}, "platform": {platform}}
	var rr grailResolveResponse
	if err := cl.getJSON(ctx, fmt.Sprintf("/v1/projects/%s/channels/%s/resolve", g.Project, g.Channel), q, &rr); err != nil {
		return nil, fmt.Errorf("grail resolve (%s/%s version=%s): %w", g.Project, g.Channel, version, err)
	}
	if rr.Yanked {
		return nil, fmt.Errorf("grail version %s/%s/%s is yanked; refusing to deploy", g.Project, g.Channel, rr.Version)
	}

	// 3. 版本清单：项目密钥验签（签名覆盖 digest，字节的完整性由此传导）。
	var env grailEnvelope
	if err := cl.getJSON(ctx, fmt.Sprintf("/v1/projects/%s/channels/%s/versions/%s", g.Project, g.Channel, rr.Version), nil, &env); err != nil {
		return nil, fmt.Errorf("fetch grail version manifest: %w", err)
	}
	var vm grailVersionManifest
	if err := verifyGrailEnvelope(&env, projectPub, &vm); err != nil {
		return nil, fmt.Errorf("grail version manifest: %w", err)
	}
	if vm.Version != rr.Version || vm.Seq != rr.Seq {
		return nil, fmt.Errorf("grail resolve result disagrees with signed manifest (possible tampering mid-chain)")
	}

	// 4. 选平台资源（精确匹配或 any）。
	var res *grailResource
	for i := range vm.Resources {
		if vm.Resources[i].Platform == platform || vm.Resources[i].Platform == "any" {
			res = &vm.Resources[i]
			break
		}
	}
	if res == nil {
		return nil, fmt.Errorf("grail manifest %s/%s/%s has no resource for platform %s", g.Project, g.Channel, rr.Version, platform)
	}
	if !strings.HasPrefix(res.Digest, "sha256:") || len(res.Digest) != len("sha256:")+64 {
		return nil, fmt.Errorf("grail resource digest malformed: %q", res.Digest)
	}

	out := &grailArtifact{
		digestHex: strings.TrimPrefix(res.Digest, "sha256:"),
		fetchURL:  cl.baseURL + res.URL,
		size:      res.Size,
		version:   rr.Version,
	}
	if rr.Grace != nil {
		out.graceNote = fmt.Sprintf("grail version %s/%s/%s is in grace period (removal at %s); pinned deploy proceeds",
			g.Project, g.Channel, rr.Version, rr.Grace.RemoveAt)
	}
	return out, nil
}
