package discovery

import (
	"time"

	"HA-Cupbearer/internal/manifest"
)

const (
	MaxDirectoryEntries = 256
	MaxDiscoveryDepth   = 8
	MaxConfigBytes      = 1 << 20
	MaxConfigPorts      = 32
	MaxProbePorts       = 4
)

type Process struct {
	PID   int
	Exe   string
	Cwd   string
	Ports []int
}

type Script struct {
	Name    string
	WorkDir string
	Content string
}

type Container struct {
	Name       string
	Project    string
	WorkingDir string
}

type Snapshot struct {
	Processes  []Process
	Scripts    []Script
	Containers []Container
}

type DiscoveryReport struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Roots       []RootReport `json:"roots"`
	Warnings    []string     `json:"warnings"`
}

type RootReport struct {
	Root           string      `json:"root"`
	DiscoveryDepth int         `json:"discovery_depth"`
	Candidates     []Candidate `json:"candidates"`
	Errors         []string    `json:"errors"`
	Truncated      bool        `json:"truncated"`
}

type Candidate struct {
	Directory        string   `json:"directory"`
	Project          string   `json:"project"`
	DisplayName      string   `json:"display_name"`
	DetectedManager  string   `json:"detected_manager"`
	ImportManager    string   `json:"import_manager"`
	Runtime          string   `json:"runtime"`
	BinaryPath       string   `json:"binary_path,omitempty"`
	Ports            []int    `json:"ports"`
	Port             int      `json:"port,omitempty"`
	HealthPath       string   `json:"health_path,omitempty"`
	HealthStatus     int      `json:"health_status,omitempty"`
	Confidence       string   `json:"confidence"`
	Evidence         []string `json:"evidence"`
	Blockers         []string `json:"blockers"`
	Importable       bool     `json:"importable"`
	ExistingManifest bool     `json:"existing_manifest"`
	Imported         bool     `json:"imported"`
}

type ImportItem struct {
	Directory    string `json:"directory"`
	Project      string `json:"project"`
	DisplayName  string `json:"display_name"`
	Manager      string `json:"manager"`
	Port         int    `json:"port"`
	HealthPath   string `json:"health_path"`
	HealthStatus int    `json:"health_status"`
}

type ImportRequest struct {
	Root            string       `json:"root"`
	Items           []ImportItem `json:"items"`
	ImportManifests bool         `json:"import_manifests,omitempty"`
}

type ImportedProject struct {
	Project    string `json:"project"`
	SourcePath string `json:"source_path"`
}

type ImportedManifestSkip struct {
	Directory string `json:"directory"`
	Project   string `json:"project,omitempty"`
	Reason    string `json:"reason"`
}

type ImportResult struct {
	Imported []ImportedProject      `json:"imported"`
	Skipped  []ImportedManifestSkip `json:"skipped"`
}

func BuildImportedManifest(item ImportItem, directory, sourcePath string) (*manifest.Manifest, error) {
	if !manifest.IsValidProjectName(item.Project) {
		return nil, &ValidationError{Field: "project", Message: "只能包含小写字母、数字与连字符，并以字母数字开头"}
	}
	if item.DisplayName == "" {
		return nil, &ValidationError{Field: "display_name", Message: "显示名不能为空"}
	}
	if item.Manager != "manual" && item.Manager != "aapanel" {
		return nil, &ValidationError{Field: "manager", Message: "第 1 期仅支持 manual 或 aapanel 只读导入"}
	}
	if item.Port < 1 || item.Port > 65535 {
		return nil, &ValidationError{Field: "port", Message: "端口必须在 1-65535 之间"}
	}
	if item.HealthPath == "" || item.HealthPath[0] != '/' {
		return nil, &ValidationError{Field: "health_path", Message: "健康路径必须以 / 开头"}
	}
	status := item.HealthStatus
	if status == 0 {
		status = 200
	}
	if status < 100 || status > 599 {
		return nil, &ValidationError{Field: "health_status", Message: "健康状态码必须在 100-599 之间"}
	}

	m := &manifest.Manifest{
		ManifestVersion: manifest.SchemaVersion,
		Kind:            "service",
		Project:         item.Project,
		DisplayName:     item.DisplayName,
		Group:           "ops",
		ServiceVersion:  "unknown",
		Runtime:         "baremetal",
		Facts: manifest.Facts{
			Ports: []manifest.Port{{Name: "http", Port: item.Port, Exposure: "loopback"}},
			Health: manifest.Health{HTTP: &manifest.HTTPProbe{
				PortName: "http", Path: item.HealthPath, ExpectStatus: status,
				TimeoutSeconds: 3, IntervalSeconds: 30, Retries: 3,
			}},
			DependsOn: []manifest.Dependency{},
			Migration: manifest.Migration{Mode: "none"},
			DataDirs:  []string{},
			Config:    []manifest.ConfigFile{},
		},
		Baremetal: manifest.Baremetal{
			Process: manifest.ProcessSpec{Manager: item.Manager},
			Install: manifest.InstallSpec{Root: directory, BinaryName: "unknown", KeepReleases: 5},
		},
		SourcePath: sourcePath,
	}
	if rep := manifest.ValidateAggregate([]*manifest.Manifest{m}, nil); !rep.OK() {
		return nil, &ValidationError{Field: "manifest", Message: rep.Errors[0]}
	}
	return m, nil
}

type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }
