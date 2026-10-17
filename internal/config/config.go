package config

import (
	"os"

	"HA-Services-Deployer/internal/models"
	"github.com/goccy/go-yaml"
)

func Load(path string) (*models.Config, error) {
	cfg := models.DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
