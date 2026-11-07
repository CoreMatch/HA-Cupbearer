package models

type Config struct {
	Version       string              `yaml:"version" json:"version"`
	Server        ServerConfig        `yaml:"server" json:"server"`
	Storage       StorageConfig       `yaml:"storage" json:"storage"`
	Manifest      ManifestConfig      `yaml:"manifest" json:"manifest"`
	Orchestration OrchestrationConfig `yaml:"orchestration" json:"orchestration"`
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

// OrchestrationConfig 描述 Deployer 作为裸金属编排器的运行参数。
// 零值（enabled=false）时编排能力整体关闭，服务行为与纯下发模式完全一致。
type OrchestrationConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ScanRoots 为同级检出目录列表，编排器扫描 <root>/<repo>/deploy-manifest.yaml。
	ScanRoots []string `yaml:"scan_roots" json:"scan_roots"`
	// ExternalManifests 目录存放无仓库归属的依赖声明（如 mysql/redis），*.yaml。
	ExternalManifests string `yaml:"external_manifests" json:"external_manifests"`
	// StateDir 存放运行状态与运行记录（续跑依据）。默认 <data>/deploy。
	StateDir string `yaml:"state_dir" json:"state_dir"`
	// ProbeHost 为健康探测目标主机；编排器与被部署服务同机，默认 127.0.0.1。
	ProbeHost string `yaml:"probe_host" json:"probe_host"`
	Tinyvisor TinyvisorTargetConfig `yaml:"tinyvisor" json:"tinyvisor"`
	// CredentialsPath 指向本地凭据文件（0600，不进仓库）：
	// tinyvisor_api_key / deploy_token / github_token。
	CredentialsPath string `yaml:"credentials_path" json:"credentials_path"`
	// Overrides 按部署上下文覆盖单个项目的执行参数（如本地验证环境
	// 把 manager 覆盖为 tinyvisor、install_root 指向本地目录）。
	// 生产上下文不做任何覆盖。
	Overrides map[string]ProjectOverride `yaml:"overrides" json:"overrides"`
}

type TinyvisorTargetConfig struct {
	URL string `yaml:"url" json:"url"`
}

type ProjectOverride struct {
	Manager     string `yaml:"manager,omitempty" json:"manager,omitempty"`
	InstallRoot string `yaml:"install_root,omitempty" json:"install_root,omitempty"`
	// HealthPort 覆盖健康探测端口（本地验证用独立端口，不改生产 Manifest）。
	// 0 表示不覆盖，沿用 facts.ports 声明。
	HealthPort int `yaml:"health_port,omitempty" json:"health_port,omitempty"`
	// ServiceVersion 覆盖目标版本（部署上下文相关：仓库 Manifest 记录现状，
	// 本地验证上下文在此声明要部署的版本）。空表示沿用 Manifest。
	ServiceVersion string `yaml:"service_version,omitempty" json:"service_version,omitempty"`
	// ArtifactLocalPath 覆盖 local_path 产物位置（随版本一起按上下文切换）。
	ArtifactLocalPath string `yaml:"artifact_local_path,omitempty" json:"artifact_local_path,omitempty"`
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
