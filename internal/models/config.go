package models

type Config struct {
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
		Server: ServerConfig{
			Listen: ":18080",
		},
		Storage: StorageConfig{
			ArtifactsRoot: "./data/artifacts",
		},
		Manifest: ManifestConfig{
			Path: "./data/manifest.json",
		},
	}
}
