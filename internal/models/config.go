package models

type Config struct {
	Version       string              `yaml:"version" json:"version"`
	Server        ServerConfig        `yaml:"server" json:"server"`
	Storage       StorageConfig       `yaml:"storage" json:"storage"`
	Manifest      ManifestConfig      `yaml:"manifest" json:"manifest"`
	Orchestration OrchestrationConfig `yaml:"orchestration" json:"orchestration"`
	// MaintenanceView 描述 Cupbearer 对运维面板的自动拉起与监督
	//（config v3 引入；零值 = 全部默认，行为见 internal/mvmanager）。
	MaintenanceView MaintenanceViewConfig `yaml:"maintenance_view" json:"maintenance_view"`
}

// MaintenanceViewConfig 自动拉起运维面板的配置。
// Enabled 为 nil 时按启用处理（默认自动拉起；找不到二进制只告警不致命）。
type MaintenanceViewConfig struct {
	Enabled    *bool  `yaml:"enabled" json:"enabled,omitempty"`
	BinaryPath string `yaml:"binary_path" json:"binary_path"` // 空 = 自动探测
	Listen     string `yaml:"listen" json:"listen"`           // 默认 127.0.0.1:18081
	WorkDir    string `yaml:"work_dir" json:"work_dir"`       // 默认 ./data/maintenance-view
}

// MVEnabled 报告是否启用 MV 自动拉起（nil = 启用）。
func (c *Config) MVEnabled() bool {
	return c.MaintenanceView.Enabled == nil || *c.MaintenanceView.Enabled
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

// OrchestrationConfig 描述 Cupbearer 作为裸金属编排器的运行参数。
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
	ProbeHost string                `yaml:"probe_host" json:"probe_host"`
	Tinyvisor TinyvisorTargetConfig `yaml:"tinyvisor" json:"tinyvisor"`
	// CredentialsPath 指向本地凭据文件（0600，不进仓库）：
	// tinyvisor_api_key / deploy_token / github_token。
	CredentialsPath string `yaml:"credentials_path" json:"credentials_path"`
	// Overrides 按部署上下文覆盖单个项目的执行参数（如本地验证环境
	// 把 manager 覆盖为 tinyvisor、install_root 指向本地目录）。
	// 生产上下文不做任何覆盖。
	Overrides map[string]ProjectOverride `yaml:"overrides" json:"overrides"`
	// StrictProjectNames 为 true 时项目清单重名按校验失败处理（拒绝运行）；
	// 默认 false：重名按装载顺序取第一个并记警告（并行 worktree 常态）。
	StrictProjectNames bool `yaml:"strict_project_names" json:"strict_project_names"`
	// Container 为容器运行时的全局参数（仅对 runtime=container 项目生效）。
	Container ContainerRuntimeConfig `yaml:"container" json:"container"`
}

// ContainerRuntimeConfig 描述如何连接容器运行时。
type ContainerRuntimeConfig struct {
	// DockerHost 指定 docker 端点（如 unix:///run/user/1000/docker.sock）。
	// 留空时按序探测：环境变量 DOCKER_HOST → $XDG_RUNTIME_DIR/docker.sock
	// （rootless 用户单元，非特权验证的默认形态）→ /var/run/docker.sock。
	DockerHost string `yaml:"docker_host" json:"docker_host"`
	// OrphanPolicy 处置带本编排器标签但清单中已不存在的项目容器：
	// report（默认，只报不删）| remove。
	OrphanPolicy string `yaml:"orphan_policy" json:"orphan_policy"`
	// ComposeFile 是容器路径"一键触发"使用的平台 compose 模板位置
	//（相对路径按进程工作目录解析）。模板承载 env/配置模板等整套接线，
	// 触发时编排器按清单里已发布的镜像引用生成 override 叠加其上。
	// 留空时默认 deploy/docker-compose.yml。
	ComposeFile string `yaml:"compose_file" json:"compose_file"`
}

type TinyvisorTargetConfig struct {
	URL string `yaml:"url" json:"url"`
}

type ProjectOverride struct {
	// Runtime 覆盖清单的 runtime 选择器（如生产裸金属、隔离环境容器）。
	Runtime     string `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Manager     string `yaml:"manager,omitempty" json:"manager,omitempty"`
	InstallRoot string `yaml:"install_root,omitempty" json:"install_root,omitempty"`
	// HealthPort 覆盖健康探测端口（本地验证用独立端口，不改生产 Manifest）。
	// 0 表示不覆盖，沿用 facts.ports 声明。
	HealthPort int `yaml:"health_port,omitempty" json:"health_port,omitempty"`
	// Ports 按端口名重映射声明端口（隔离环境绕开本机已被占用的端口）。
	// 作用于 preflight 的端口占用检查；健康探测/冒烟的目标端口仍由
	// HealthPort 控制。未列出的端口名沿用 facts.ports 声明。
	Ports map[string]int `yaml:"ports,omitempty" json:"ports,omitempty"`
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
