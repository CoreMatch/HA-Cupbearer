package config

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"

	"HA-Services-Deployer/internal/models"

	"github.com/goccy/go-yaml"
)

func Load(path string) (*models.Config, error) {
	cfg := models.DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Save default config if it doesn't exist
			if err := save(path, cfg); err != nil {
				return nil, err
			}
			return cfg, nil
		}
		return nil, err
	}

	// Parse into map for migration
	var raw map[string]interface{}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	// 迁移会原地修改 map，先记下迁移前版本用于备份命名。
	oldVersion, _ := raw["version"].(string)
	if oldVersion == "" {
		oldVersion = "unknown"
	}

	// Migrate config
	migrated, changed, err := MigrateConfig(raw, GenerateToken)
	if err != nil {
		return nil, err
	}

	if changed {
		if err := BackupConfigFile(path, oldVersion); err != nil {
			log.Printf("Warning: failed to backup config: %v", err)
		}
		if err := save(path, migrated); err != nil {
			return nil, err
		}
	}

	// Final unmarshal into struct
	migratedData, err := yaml.Marshal(migrated)
	if err != nil {
		return nil, err
	}

	if err := yaml.Unmarshal(migratedData, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func save(path string, cfg interface{}) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func GenerateToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}
