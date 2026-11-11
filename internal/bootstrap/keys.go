package bootstrap

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// 签名密钥对：形态与 HRPAuth-Yggdrasil-API 的 generateKeyPair 逐字一致
//（RSA 4096；私钥 PKCS1 "RSA PRIVATE KEY" 0600，公钥 PKIX "PUBLIC KEY" 0644）。
// YGG 模板经 ${SIGNATURE_KEY_DIR}/private_key.pem|public_key.pem 消费。

// KeyResult 是密钥步骤的结论。
type KeyResult struct {
	Dir         string `json:"dir"`
	Generated   bool   `json:"generated"`             // 本次新生成
	Reused      bool   `json:"reused"`                // 两个文件都在，原样保留
	PublicDerived bool `json:"public_derived,omitempty"` // 仅私钥在，公钥由私钥推导补齐
	Fingerprint string `json:"fingerprint"`           // 私钥指纹
}

// EnsureSigningKeys 幂等地保证密钥对在场：
//   - 两个文件都在 → 原样保留（绝不覆盖），返回指纹；
//   - 只有私钥 → 由私钥推导公钥补齐（缺值补齐）；
//   - 只有公钥 → 硬失败（无法安全恢复私钥，绝不静默另造一对造成错配）；
//   - 都不在 → 生成新对（0600/0644）。
func EnsureSigningKeys(dir string) (*KeyResult, error) {
	privPath := filepath.Join(dir, "private_key.pem")
	pubPath := filepath.Join(dir, "public_key.pem")

	privErr := fileAccessible(privPath)
	pubErr := fileAccessible(pubPath)

	if privErr == nil && pubErr == nil {
		fp, err := privateKeyFingerprint(privPath)
		if err != nil {
			return nil, err
		}
		return &KeyResult{Dir: dir, Reused: true, Fingerprint: fp}, nil
	}
	if privErr != nil && pubErr == nil {
		return nil, fmt.Errorf("%s 存在但私钥 %s 缺失：无法从公钥恢复私钥，"+
			"也不会另造一对造成错配——请人工核对密钥来源后再继续", pubPath, privPath)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create secrets dir: %w", err)
	}

	if privErr == nil {
		// 私钥在、公钥缺：由私钥推导补齐。
		priv, err := loadPrivateKey(privPath)
		if err != nil {
			return nil, err
		}
		if err := writePublicKey(pubPath, &priv.PublicKey); err != nil {
			return nil, err
		}
		fp, err := privateKeyFingerprint(privPath)
		if err != nil {
			return nil, err
		}
		return &KeyResult{Dir: dir, PublicDerived: true, Fingerprint: fp}, nil
	}

	key, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return nil, fmt.Errorf("generate RSA key: %w", err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write private key: %w", err)
	}
	if err := writePublicKey(pubPath, &key.PublicKey); err != nil {
		return nil, err
	}
	return &KeyResult{Dir: dir, Generated: true, Fingerprint: Fingerprint(string(privPEM))}, nil
}

func writePublicKey(path string, pub *rsa.PublicKey) error {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return fmt.Errorf("marshal public key: %w", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	if err := os.WriteFile(path, pubPEM, 0o644); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	return nil
}

func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s 不是合法 PEM", path)
	}
	if block.Type == "RSA PRIVATE KEY" {
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key %s: %w", path, err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s 不是 RSA 私钥", path)
	}
	return rsaKey, nil
}

func privateKeyFingerprint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read private key for fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])[:16], nil
}

func fileAccessible(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s 是目录不是文件", path)
	}
	return nil
}
