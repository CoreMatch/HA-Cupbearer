// Package manifest 定义各项目向编排器提交的部署声明（deploy-manifest.yaml）
// 的模型、装载与校验。
//
// 声明分两层：
//   - facts：运行时无关的事实（端口、健康端点、依赖、分组、数据目录等），
//     后续的容器编排复用同一层；
//   - baremetal / container：运行时相关的执行方式。container 段当前冻结
//     （enabled 恒为 false），仅为 schema 预留。
//
// 每个项目在自己仓库根目录放一份 deploy-manifest.yaml；无仓库归属的
// 外部依赖（如 mysql/redis）声明放在 Deployer 仓的外部清单目录。
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/goccy/go-yaml"
)

// FileName 是各项目仓库根目录下的清单文件名。
const FileName = "deploy-manifest.yaml"

// SchemaVersion 是当前清单 schema 版本。
const SchemaVersion = 1

// Manifest 是一个项目的完整部署声明。
type Manifest struct {
	ManifestVersion int       `yaml:"manifest_version" json:"manifest_version"`
	Kind            string    `yaml:"kind" json:"kind"` // service | external
	Project         string    `yaml:"project" json:"project"`
	DisplayName     string    `yaml:"display_name" json:"display_name"`
	Group           string    `yaml:"group" json:"group"`
	ServiceVersion  string    `yaml:"service_version" json:"service_version"`
	Facts           Facts     `yaml:"facts" json:"facts"`
	Baremetal       Baremetal `yaml:"baremetal" json:"baremetal"`
	Container       Container `yaml:"container" json:"container"`

	// SourcePath 记录装载来源（校验报告用），不参与序列化。
	SourcePath string `yaml:"-" json:"source_path"`
}

// Facts 为运行时无关事实，容器编排阶段原样复用。
type Facts struct {
	Ports        []Port       `yaml:"ports" json:"ports"`
	Health       Health       `yaml:"health" json:"health"`
	DependsOn    []Dependency `yaml:"depends_on" json:"depends_on"`
	Migration    Migration    `yaml:"migration" json:"migration"`
	DataDirs     []string     `yaml:"data_dirs" json:"data_dirs"`
	Config       []ConfigFile `yaml:"config" json:"config"`
	StaticAssets string       `yaml:"static_assets" json:"static_assets"`
	// External 仅 kind=external 清单使用：可达性检查的主机。
	External *ExternalFacts `yaml:"external" json:"external"`
}

type ExternalFacts struct {
	Host string `yaml:"host" json:"host"`
}

type Port struct {
	Name     string `yaml:"name" json:"name"`
	Port     int    `yaml:"port" json:"port"`
	Exposure string `yaml:"exposure" json:"exposure"` // public | loopback
}

type Health struct {
	HTTP *HTTPProbe `yaml:"http" json:"http"`
}

type HTTPProbe struct {
	PortName        string `yaml:"port_name" json:"port_name"`
	Path            string `yaml:"path" json:"path"`
	ExpectStatus    int    `yaml:"expect_status" json:"expect_status"`
	TimeoutSeconds  int    `yaml:"timeout_seconds" json:"timeout_seconds"`
	IntervalSeconds int    `yaml:"interval_seconds" json:"interval_seconds"`
	Retries         int    `yaml:"retries" json:"retries"`
}

type Dependency struct {
	Project string `yaml:"project" json:"project"`
	Kind    string `yaml:"kind" json:"kind"` // required | optional
}

type Migration struct {
	Mode string `yaml:"mode" json:"mode"` // auto_on_startup | manual | none
}

type ConfigFile struct {
	Path            string `yaml:"path" json:"path"`
	TemplateVersion string `yaml:"template_version" json:"template_version"`
}

// Baremetal 为裸金属执行方式。
type Baremetal struct {
	Process  ProcessSpec  `yaml:"process" json:"process"`
	Install  InstallSpec  `yaml:"install" json:"install"`
	Artifact ArtifactSpec `yaml:"artifact" json:"artifact"`
}

type ProcessSpec struct {
	// Manager 声明该项目进程归谁管：
	//   tinyvisor — 编排器可驱动（登记/重启/状态）；
	//   aapanel   — 宝塔面板管理，编排器只读（foreign）；
	//   manual    — 人工管理，编排器只读（foreign）。
	Manager   string        `yaml:"manager" json:"manager"`
	Tinyvisor TinyvisorSpec `yaml:"tinyvisor" json:"tinyvisor"`
}

type TinyvisorSpec struct {
	RestartPolicy string `yaml:"restart_policy" json:"restart_policy"` // never|always|unless-stopped|on-failure
	AutoStart     bool   `yaml:"auto_start" json:"auto_start"`
	// EnvFile 为可选的环境变量文件；启动脚本在 exec 前以 set -a 方式注入。
	EnvFile string `yaml:"env_file" json:"env_file"`
}

type InstallSpec struct {
	// Root 为部署根目录；releases/<service_version>/ 存放各版本产物，
	// current 软链指向生效版本。配置与数据在 release 目录之外。
	Root         string `yaml:"root" json:"root"`
	Layout       string `yaml:"layout" json:"layout"` // releases
	BinaryName   string `yaml:"binary_name" json:"binary_name"`
	KeepReleases int    `yaml:"keep_releases" json:"keep_releases"`
}

type ArtifactSpec struct {
	// Source 为产物来源：
	//   ci_release  — CI 构建后发布的 Release 资产（生产正路）；
	//   local_path  — 本机/挂载路径上的产物文件（隔离环境与本地验证）。
	Source    string      `yaml:"source" json:"source"`
	Release   ReleaseSpec `yaml:"release" json:"release"`
	LocalPath string      `yaml:"local_path" json:"local_path"`
}

// ReleaseSpec 描述 CI 发布资产位置。资产命名硬约定：
// <项目名>-linux-amd64 与 <项目名>-linux-amd64.sha256 双资产；
// asset / checksum_asset 留空时按该约定从 project 名推导。
type ReleaseSpec struct {
	Repo          string `yaml:"repo" json:"repo"`
	Tag           string `yaml:"tag" json:"tag"` // 支持 {service_version} 占位
	Asset         string `yaml:"asset" json:"asset"`
	ChecksumAsset string `yaml:"checksum_asset" json:"checksum_asset"`
}

// Container 为容器执行方式预留段，当前冻结不实现。
type Container struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// IsExternal 报告该清单是否为外部依赖（无进程、只做可达性检查）。
func (m *Manifest) IsExternal() bool {
	return m.Kind == "external"
}

// LoadFile 读取并解析单个清单文件（不做跨清单校验）。
func LoadFile(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m.Kind == "" {
		m.Kind = "service"
	}
	m.SourcePath = path
	return &m, nil
}

// LoadAggregate 从扫描根与外部清单目录装载全部清单。
// scanRoots 下按 <root>/<repo>/deploy-manifest.yaml 规则查找；
// externalDir 下装载全部 *.yaml。返回装载成功列表与逐项装载错误。
func LoadAggregate(scanRoots []string, externalDir string) ([]*Manifest, []error) {
	var manifests []*Manifest
	var errs []error

	for _, root := range scanRoots {
		entries, err := os.ReadDir(root)
		if err != nil {
			errs = append(errs, fmt.Errorf("scan root %s: %w", root, err))
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			path := filepath.Join(root, e.Name(), FileName)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			m, err := LoadFile(path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			manifests = append(manifests, m)
		}
	}

	if externalDir != "" {
		pattern := filepath.Join(externalDir, "*.yaml")
		files, err := filepath.Glob(pattern)
		if err != nil {
			errs = append(errs, fmt.Errorf("external manifests %s: %w", externalDir, err))
		}
		sort.Strings(files)
		for _, path := range files {
			m, err := LoadFile(path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			manifests = append(manifests, m)
		}
	}

	return manifests, errs
}
