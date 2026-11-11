package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Credentials 是编排链路的本地凭据（tinyvisor apiKey、部署端点令牌、
// 可选的 GitHub 下载令牌）。只从编排器配置指定的本地文件读取：
// 文件不进仓库，权限建议 0600。任何凭据都不得写入 Manifest 或配置文件。
type Credentials struct {
	TinyvisorAPIKey string `json:"tinyvisor_api_key"`
	DeployToken     string `json:"deploy_token"`
	GithubToken     string `json:"github_token"`
	// SmokeSecrets 存放部署后冒烟验证引用的密钥值：
	// Manifest 的 facts.smoke 以 ${secret:<name>} 引用这里的键，
	// 密钥本身只存在于凭据文件，绝不进 Manifest。
	SmokeSecrets map[string]string `json:"smoke_secrets,omitempty"`
}

// LoadCredentials 读取凭据文件。文件不存在时返回空凭据（不视为错误），
// 由调用方按 fail-closed 语义处理（例如部署端点拒绝服务）。
func LoadCredentials(path string) (*Credentials, error) {
	creds := &Credentials{}
	if path == "" {
		return creds, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return creds, nil
		}
		return nil, err
	}

	if err := json.Unmarshal(data, creds); err != nil {
		return nil, err
	}
	return creds, nil
}

// SaveCredentials 原子写入凭据文件（0600，目录 0700）。
// 用于首跑时把自动生成的 deploy_token 落盘（只记指纹到日志，不写明文）。
func SaveCredentials(path string, creds *Credentials) error {
	if path == "" {
		return fmt.Errorf("credentials path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
