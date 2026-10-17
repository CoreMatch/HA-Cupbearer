package models

type Config struct {
	Version  string         `yaml:"version" json:"version"`
	Server   ServerConfig   `yaml:"server" json:"server"`
	Storage  StorageConfig  `yaml:"storage" json:"storage"`
	Manifest ManifestConfig `yaml:"manifest" json:"manifest"`
}

type ServerConfig struct {
	Listen string `yaml:"listen" json:"listen"`
}

type StorageConfig struct {
	ArtifactsRoot string `yaml:"artifacts_root" json:"artifacts_root"`
}

type ManifestConfig struct {
	Path string `yaml:"path" json:"path"`
}

func DefaultConfig() *Config {
	return &Config{
		Version: "1",
		Server: ServerConfig{
			Listen: ":2770",
		},
		Storage: StorageConfig{
			ArtifactsRoot: "./data/artifacts",
		},
		Manifest: ManifestConfig{
			Path: "./data/manifest.json",
		},
	}
}
