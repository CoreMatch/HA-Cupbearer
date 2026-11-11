package artifact

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/manifest"
)

// fakeGrail 是一个进程内的假 Grail 节点：真实的 Ed25519 根密钥与项目
// 密钥、真实签名的密钥环/版本清单、内容寻址的字节供应。
type fakeGrail struct {
	ts *httptest.Server

	rootPub     ed25519.PublicKey
	rootPriv    ed25519.PrivateKey
	projectPub  ed25519.PublicKey
	projectPriv ed25519.PrivateKey

	project string
	channel string
	version string
	payload []byte
	digest  string

	yanked bool // 置位后 resolve 返回 yanked 标记
}

func newFakeGrail(t *testing.T, project, channel, version string, payload []byte) *fakeGrail {
	t.Helper()
	rootPub, rootPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	projectPub, projectPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(payload)
	f := &fakeGrail{
		rootPub: rootPub, rootPriv: rootPriv,
		projectPub: projectPub, projectPriv: projectPriv,
		project: project, channel: channel, version: version,
		payload: payload,
		digest:  "sha256:" + hex.EncodeToString(h[:]),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/keyring", f.handleKeyring)
	mux.HandleFunc(fmt.Sprintf("/v1/projects/%s/channels/%s/resolve", project, channel), f.handleResolve)
	mux.HandleFunc(fmt.Sprintf("/v1/projects/%s/channels/%s/versions/%s", project, channel, version), f.handleVersion)
	mux.HandleFunc(fmt.Sprintf("/%s/%s/%s/linux-amd64", project, channel, version), f.handleBlob)
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

// signTestEnvelope 组装签名信封的 JSON 原文（签名覆盖 payload 字节）。
func signTestEnvelope(payload any, keyID string, priv ed25519.PrivateKey) []byte {
	raw, _ := json.Marshal(payload)
	sig := ed25519.Sign(priv, raw)
	env := grailEnvelope{
		Payload:   raw,
		Signature: grailSignature{KeyID: keyID, Alg: "ed25519", Value: base64.StdEncoding.EncodeToString(sig)},
	}
	out, _ := json.Marshal(env)
	return out
}

func (f *fakeGrail) handleKeyring(w http.ResponseWriter, r *http.Request) {
	payload := map[string]any{
		"seq": 1,
		"root": map[string]any{
			"key_id": "root-1", "alg": "ed25519",
			"public_key": base64.StdEncoding.EncodeToString(f.rootPub),
		},
		"projects": map[string]any{
			f.project: map[string]any{
				"key_id": f.project + "-1", "alg": "ed25519",
				"public_key": base64.StdEncoding.EncodeToString(f.projectPub),
			},
		},
	}
	w.Write(signTestEnvelope(payload, "root-1", f.rootPriv))
}

func (f *fakeGrail) manifestPayload() map[string]any {
	return map[string]any{
		"project": f.project, "channel": f.channel, "version": f.version,
		"seq": 1, "platforms": []string{"linux-amd64"},
		"resources": []map[string]any{{
			"name": f.project, "platform": "linux-amd64",
			"digest": f.digest, "size": len(f.payload),
			"url": fmt.Sprintf("/%s/%s/%s/linux-amd64", f.project, f.channel, f.version),
		}},
		"provenance":   map[string]any{"same_content": false},
		"published_at": "2026-11-12T00:00:00Z",
	}
}

func (f *fakeGrail) handleResolve(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"project": f.project, "channel": f.channel, "version": f.version,
		"seq": 1, "yanked": f.yanked, "resolved_from": "latest",
		"platforms":  []string{"linux-amd64"},
		"resources":  f.manifestPayload()["resources"],
		"provenance": map[string]any{"same_content": false},
		"signature":  map[string]any{"key_id": f.project + "-1", "alg": "ed25519", "value": "resolve-carries-envelope-signature"},
	}
	json.NewEncoder(w).Encode(resp)
}

func (f *fakeGrail) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Write(signTestEnvelope(f.manifestPayload(), f.project+"-1", f.projectPriv))
}

func (f *fakeGrail) handleBlob(w http.ResponseWriter, r *http.Request) {
	w.Write(f.payload)
}

func grailManifest(baseURL, project, channel string) *manifest.Manifest {
	m := &manifest.Manifest{Project: "svc-g", ServiceVersion: "0.6"}
	m.Baremetal.Install.BinaryName = "svc-g"
	m.Baremetal.Artifact.Source = "grail"
	m.Baremetal.Artifact.Grail = manifest.GrailSpec{
		BaseURL: baseURL, Project: project, Channel: channel,
	}
	return m
}

func TestStageGrailHappyPath(t *testing.T) {
	payload := []byte("grail artifact bytes for svc-g")
	f := newFakeGrail(t, "svc-g", "release", "0.6", payload)

	m := grailManifest(f.ts.URL, "svc-g", "release")
	creds := &config.Credentials{GrailRootPublicKey: base64.StdEncoding.EncodeToString(f.rootPub)}
	destDir := t.TempDir()

	sha, err := Stage(context.Background(), m, destDir, creds)
	if err != nil {
		t.Fatalf("Stage(grail): %v", err)
	}
	want := sha256.Sum256(payload)
	if sha != hex.EncodeToString(want[:]) {
		t.Fatalf("sha = %s, want %s", sha, hex.EncodeToString(want[:]))
	}
	got, err := os.ReadFile(filepath.Join(destDir, "svc-g"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("staged bytes mismatch")
	}

	// 幂等：再次 Stage 直接跳过且结果一致。
	if _, err := Stage(context.Background(), m, destDir, creds); err != nil {
		t.Fatalf("idempotent Stage: %v", err)
	}
}

func TestStageGrailWrongProjectKeyRejected(t *testing.T) {
	payload := []byte("payload")
	f := newFakeGrail(t, "svc-g", "release", "0.6", payload)
	// 用另一把密钥冒充项目密钥签名：密钥环里的公钥不变，签名方换人。
	_, evilPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.projectPriv = evilPriv

	m := grailManifest(f.ts.URL, "svc-g", "release")
	creds := &config.Credentials{GrailRootPublicKey: base64.StdEncoding.EncodeToString(f.rootPub)}
	if _, err := Stage(context.Background(), m, t.TempDir(), creds); err == nil {
		t.Fatal("forged project signature accepted — verification broken")
	}
}

func TestStageGrailYankedRejected(t *testing.T) {
	payload := []byte("payload")
	f := newFakeGrail(t, "svc-g", "release", "0.6", payload)
	f.yanked = true

	m := grailManifest(f.ts.URL, "svc-g", "release")
	creds := &config.Credentials{GrailRootPublicKey: base64.StdEncoding.EncodeToString(f.rootPub)}
	if _, err := Stage(context.Background(), m, t.TempDir(), creds); err == nil {
		t.Fatal("yanked version deployed — must refuse")
	}
}

func TestStageGrailMissingOrWrongAnchorRejected(t *testing.T) {
	payload := []byte("payload")
	f := newFakeGrail(t, "svc-g", "release", "0.6", payload)

	m := grailManifest(f.ts.URL, "svc-g", "release")
	// 无信任锚：fail-closed。
	if _, err := Stage(context.Background(), m, t.TempDir(), &config.Credentials{}); err == nil {
		t.Fatal("missing trust anchor accepted — must refuse")
	}
	// 错误的信任锚：密钥环验签失败。
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	creds := &config.Credentials{GrailRootPublicKey: base64.StdEncoding.EncodeToString(otherPub)}
	if _, err := Stage(context.Background(), m, t.TempDir(), creds); err == nil {
		t.Fatal("wrong trust anchor accepted — must refuse")
	}
}
