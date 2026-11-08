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
	"strings"

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
	// Smoke 可选：部署后冒烟验证请求列表。健康探测只证明进程活着，
	// 冒烟用真实请求证明服务"真的在干活"。未声明的项目维持只看健康。
	Smoke []SmokeCheck `yaml:"smoke,omitempty" json:"smoke,omitempty"`
	// External 仅 kind=external 清单使用：可达性检查的主机。
	External *ExternalFacts `yaml:"external" json:"external"`
}

// SmokeCheck 描述一条部署后冒烟请求。
// 密钥值绝不写进 Manifest：头/体中只允许 ${secret:<name>} 引用，
// 由编排器从凭据文件的 smoke_secrets 解析；缺失即失败并报清楚。
type SmokeCheck struct {
	// Name 为检查名（报告与步骤记录用）；留空时按序号标记。
	Name   string `yaml:"name" json:"name"`
	Method string `yaml:"method" json:"method"` // 默认 GET
	// PortName 指定请求端口；留空时沿用 health.http.port_name。
	PortName string `yaml:"port_name" json:"port_name"`
	Path     string `yaml:"path" json:"path"`
	// Headers 为固定请求头；值可以是字面量或整值 ${secret:<name>} 引用。
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	// Body 为可选请求体，支持内联 ${secret:<name>} 替换。
	Body string `yaml:"body,omitempty" json:"body,omitempty"`
	// ContentType 在带 Body 时生效，默认 application/json。
	ContentType string `yaml:"content_type,omitempty" json:"content_type,omitempty"`
	// ExpectStatus 为期望状态码，默认 200（允许 404 等确定性负结果）。
	ExpectStatus int `yaml:"expect_status" json:"expect_status"`
	// Assert 为可选响应体断言。
	Assert     SmokeAssert `yaml:"assert,omitempty" json:"assert,omitempty"`
	TimeoutSeconds int   `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"` // 默认 5
}

// SmokeAssert 是响应体断言（三个开关可组合）。
type SmokeAssert struct {
	// Contains 要求响应体包含该子串。
	Contains string `yaml:"contains,omitempty" json:"contains,omitempty"`
	// JSONField 要求响应为 JSON 且点路径字段存在（如 data.version）。
	JSONField string `yaml:"json_field,omitempty" json:"json_field,omitempty"`
	// JSONNotEmpty 要求点路径字段存在且非空（数组/对象/字符串非空，
	// 数值/布尔存在即视为非空，null 视为空）。
	JSONNotEmpty string `yaml:"json_not_empty,omitempty" json:"json_not_empty,omitempty"`
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
	// SharedData 声明共享数据目录（相对部署根）：不随版本升级重置的配置
	// 与数据（配置文件、地图、模组、运行数据）落在这里，releases/ 只放
	// 二进制。编排器部署时幂等创建该目录；版本切换与 prune 从不触碰它。
	// 声明后启动脚本以 env 注入 avalonhelper.basePath=<根>/<SharedData>
	// 绝对路径——带点变量名无法经 env_file 注入（shell 赋值语法不允许
	// 点号），只能由脚本内 env 命令直接设置。变量名当前为 Avalon 专有
	// （其 internal/util/file resolveBasePath 读取该键）；他服务如需共享
	// 数据目录，再泛化为按服务声明变量名。
	SharedData string `yaml:"shared_data" json:"shared_data"`
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

// excludedRepoDirs 是扫描时默认排除的候选目录名：构建产物与临时目录。
// 隐藏目录（以 . 开头，含 .trae/.git 及其 worktrees）另行按前缀排除。
// 排除只是第一道筛子；真正的兜底是重名确定性取舍（见 LoadAggregate）——
// 部署链路不得依赖"目录里恰好没有干扰副本"。
var excludedRepoDirs = map[string]bool{
	"build": true, "dist": true, "out": true, "target": true,
	"tmp": true, "temp": true, "node_modules": true, "vendor": true,
}

// DuplicateGroup 记录同一项目名出现多份清单时的取舍结果。
type DuplicateGroup struct {
	Project string   `json:"project"`
	Kept    string   `json:"kept"`    // 取胜清单的 SourcePath
	Ignored []string `json:"ignored"` // 被忽略副本的 SourcePath（按装载顺序）
}

// LoadAggregate 从扫描根与外部清单目录装载清单，并按确定性规则解决重名。
// scanRoots 下按 <root>/<repo>/deploy-manifest.yaml 规则查找；
// externalDir 下装载全部 *.yaml。
//
// 重名取舍规则（确定性，不依赖目录现场）：
//  1. 装载顺序 = scan_roots 配置顺序（同一根内按目录名排序）→ 外部清单目录；
//  2. 同名项目先装载者取胜，其余副本不进生效列表，记入 DuplicateGroup；
//  3. 隐藏目录与 excludedRepoDirs 中的目录不作为仓库候选。
//
// 重名本身不是错误：并行 worktree、检出副本都会造成重名，硬阻断会让
// "开个分支干活"瘫痪整条部署链路。需要严格语义（重名即失败）时，由
// 调用方依据返回的 DuplicateGroup 拒绝（编排器 strict_project_names 开关）。
// 返回去重后的生效清单、重名取舍记录与逐项装载错误。
func LoadAggregate(scanRoots []string, externalDir string) ([]*Manifest, []DuplicateGroup, []error) {
	var manifests []*Manifest
	var dups []DuplicateGroup
	var errs []error
	keptIdx := make(map[string]int)  // project -> 取胜者在 manifests 的下标
	groupIdx := make(map[string]int) // project -> dups 下标

	register := func(m *Manifest) {
		if m.Project == "" {
			manifests = append(manifests, m) // 空项目名由校验报错
			return
		}
		if _, seen := keptIdx[m.Project]; !seen {
			keptIdx[m.Project] = len(manifests)
			manifests = append(manifests, m)
			return
		}
		gi, ok := groupIdx[m.Project]
		if !ok {
			dups = append(dups, DuplicateGroup{
				Project: m.Project,
				Kept:    manifests[keptIdx[m.Project]].SourcePath,
			})
			gi = len(dups) - 1
			groupIdx[m.Project] = gi
		}
		dups[gi].Ignored = append(dups[gi].Ignored, m.SourcePath)
	}

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
			if name := e.Name(); strings.HasPrefix(name, ".") || excludedRepoDirs[name] {
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
			register(m)
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
			register(m)
		}
	}

	return manifests, dups, errs
}
