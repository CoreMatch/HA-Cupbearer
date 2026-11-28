package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"HA-Cupbearer/internal/manifest"

	"github.com/goccy/go-yaml"
)

func ValidateRoot(configured []string, requested string) (string, error) {
	canonical, err := canonicalRoot(requested)
	if err != nil {
		return "", err
	}
	for _, root := range configured {
		configuredCanonical, err := canonicalRoot(root)
		if err != nil || configuredCanonical != canonical {
			continue
		}
		return canonical, nil
	}
	return "", &ValidationError{Field: "root", Message: "目录不在 orchestration.deployment_roots 中"}
}

func ValidateDirectory(root, requested string) (string, error) {
	return validateImportDirectory(root, requested, false)
}

func ValidateImportDirectory(root, requested string, allowExistingManifest bool) (string, error) {
	return validateImportDirectory(root, requested, allowExistingManifest)
}

func validateImportDirectory(root, requested string, allowExistingManifest bool) (string, error) {
	canonical, err := canonicalPath(requested)
	if err != nil {
		return "", &ValidationError{Field: "directory", Message: "目录不存在或无法解析"}
	}
	depth, err := discoveryDepth(root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, canonical)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &ValidationError{Field: "directory", Message: "导入目标不在部署根目录内"}
	}
	if level := len(strings.Split(rel, string(filepath.Separator))); level > depth {
		return "", &ValidationError{Field: "directory", Message: fmt.Sprintf("目标深度 %d 超过清单声明的 discovery.depth=%d", level, depth)}
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", &ValidationError{Field: "directory", Message: "导入目标不是目录"}
	}
	if _, err := os.Stat(filepath.Join(canonical, manifest.FileName)); err == nil && !allowExistingManifest {
		return "", &ValidationError{Field: "directory", Message: "目标目录已有 deploy-manifest.yaml，请使用清单批量导入"}
	}
	return canonical, nil
}

func WriteBatch(dir string, manifests []*manifest.Manifest) ([]string, error) {
	if len(manifests) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create imported manifests dir: %w", err)
	}
	canonicalDir, err := canonicalPath(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve imported manifests dir: %w", err)
	}

	created := make([]string, 0, len(manifests))
	for _, m := range manifests {
		data, err := yaml.Marshal(m)
		if err != nil {
			removeCreated(created)
			return nil, fmt.Errorf("marshal %s: %w", m.Project, err)
		}
		tmp, err := os.CreateTemp(canonicalDir, ".import-*.tmp")
		if err != nil {
			removeCreated(created)
			return nil, fmt.Errorf("create temp manifest: %w", err)
		}
		tmpPath := tmp.Name()
		if _, err := tmp.Write(data); err == nil {
			err = tmp.Sync()
		}
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(tmpPath)
			removeCreated(created)
			return nil, fmt.Errorf("write %s: %w", m.Project, err)
		}
		commitErr := os.Chmod(tmpPath, 0o644)
		if commitErr == nil {
			finalPath := filepath.Join(canonicalDir, m.Project+".yaml")
			commitErr = os.Link(tmpPath, finalPath)
		}
		_ = os.Remove(tmpPath)
		if commitErr != nil {
			removeCreated(created)
			return nil, fmt.Errorf("commit %s: %w", m.Project, commitErr)
		}
		created = append(created, filepath.Join(canonicalDir, m.Project+".yaml"))
	}
	return created, nil
}

func removeCreated(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func NormalizeImportRequest(input ImportRequest) ImportRequest {
	input.Root = strings.TrimSpace(input.Root)
	for i := range input.Items {
		input.Items[i].Directory = strings.TrimSpace(input.Items[i].Directory)
		input.Items[i].Project = strings.TrimSpace(input.Items[i].Project)
		input.Items[i].DisplayName = strings.TrimSpace(input.Items[i].DisplayName)
		input.Items[i].Manager = strings.TrimSpace(input.Items[i].Manager)
		input.Items[i].HealthPath = strings.TrimSpace(input.Items[i].HealthPath)
	}
	return input
}
