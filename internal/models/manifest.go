package models

type Manifest struct {
	GeneratedAt string         `json:"generated_at"`
	Services    []ServiceEntry `json:"services"`
}

type ServiceEntry struct {
	Name      string          `json:"name"`
	Channel   string          `json:"channel"`
	Version   string          `json:"version"`
	Artifacts []ArtifactEntry `json:"artifacts"`
}

type ArtifactEntry struct {
	FileName    string `json:"file_name"`
	RelativeURL string `json:"relative_url"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
}
