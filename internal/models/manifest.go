package models

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ManifestSchemaVersion = 1
)

var (
	serviceNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	sha256Pattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Manifest struct {
	SchemaVersion int              `json:"schema_version"`
	GeneratedAt   string           `json:"generated_at"`
	Services      []ServiceRelease `json:"services"`
}

type ServiceRelease struct {
	Name        string            `json:"name"`
	DisplayName string            `json:"display_name,omitempty"`
	Channel     string            `json:"channel,omitempty"`
	Version     string            `json:"version"`
	PublishedAt string            `json:"published_at,omitempty"`
	Artifacts   []ArtifactRelease `json:"artifacts"`
}

type ArtifactRelease struct {
	ID           string `json:"id"`
	Role         string `json:"role"`
	OS           string `json:"os,omitempty"`
	Arch         string `json:"arch,omitempty"`
	FileName     string `json:"file_name"`
	DownloadPath string `json:"download_path"`
	SHA256       string `json:"sha256"`
	SizeBytes    int64  `json:"size_bytes"`
}

func NewEmptyManifest() *Manifest {
	return &Manifest{
		SchemaVersion: ManifestSchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Services:      []ServiceRelease{},
	}
}

func (m *Manifest) Normalize() {
	if m.SchemaVersion == 0 {
		m.SchemaVersion = ManifestSchemaVersion
	}
	if m.GeneratedAt == "" {
		m.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if m.Services == nil {
		m.Services = []ServiceRelease{}
	}
	for serviceIndex := range m.Services {
		if m.Services[serviceIndex].Artifacts == nil {
			m.Services[serviceIndex].Artifacts = []ArtifactRelease{}
		}
	}
}

func (m *Manifest) Validate() error {
	if m.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("unsupported manifest schema_version %d", m.SchemaVersion)
	}

	serviceSeen := make(map[string]struct{}, len(m.Services))
	for _, service := range m.Services {
		if !serviceNamePattern.MatchString(service.Name) {
			return fmt.Errorf("service name %q is invalid", service.Name)
		}
		if _, exists := serviceSeen[service.Name]; exists {
			return fmt.Errorf("service name %q is duplicated", service.Name)
		}
		serviceSeen[service.Name] = struct{}{}

		if strings.TrimSpace(service.Version) == "" {
			return fmt.Errorf("service %q version is required", service.Name)
		}
		if len(service.Artifacts) == 0 {
			return fmt.Errorf("service %q must declare at least one artifact", service.Name)
		}

		artifactSeen := make(map[string]struct{}, len(service.Artifacts))
		for _, artifact := range service.Artifacts {
			if strings.TrimSpace(artifact.ID) == "" {
				return fmt.Errorf("service %q has an artifact without id", service.Name)
			}
			if _, exists := artifactSeen[artifact.ID]; exists {
				return fmt.Errorf("service %q artifact id %q is duplicated", service.Name, artifact.ID)
			}
			artifactSeen[artifact.ID] = struct{}{}

			switch artifact.Role {
			case "binary", "archive", "asset":
			default:
				return fmt.Errorf("service %q artifact %q has unsupported role %q", service.Name, artifact.ID, artifact.Role)
			}

			if strings.TrimSpace(artifact.FileName) == "" {
				return fmt.Errorf("service %q artifact %q file_name is required", service.Name, artifact.ID)
			}
			if !sha256Pattern.MatchString(artifact.SHA256) {
				return fmt.Errorf("service %q artifact %q sha256 must be a lowercase 64-char hex string", service.Name, artifact.ID)
			}
			if artifact.SizeBytes < 0 {
				return fmt.Errorf("service %q artifact %q size_bytes must be >= 0", service.Name, artifact.ID)
			}
			if err := validateDownloadPath(service.Name, artifact.DownloadPath); err != nil {
				return fmt.Errorf("service %q artifact %q invalid download_path: %w", service.Name, artifact.ID, err)
			}
		}
	}

	return nil
}

func validateDownloadPath(serviceName, rawPath string) error {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return fmt.Errorf("path is required")
	}
	if strings.HasPrefix(trimmed, "/") {
		return fmt.Errorf("must be relative to /api/v1/artifacts/%s", serviceName)
	}

	cleaned := filepath.Clean(trimmed)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return fmt.Errorf("path escapes the service artifact root")
	}

	return nil
}
