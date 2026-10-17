package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"HA-Services-Deployer/internal/models"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	cfg *models.Config
}

func NewHandler(cfg *models.Config) *Handler {
	return &Handler{cfg: cfg}
}

func (h *Handler) Healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "ok",
		"data": gin.H{
			"service": "ha-services-deployer",
			"time":    time.Now().UTC().Format(time.RFC3339),
		},
	})
}

func (h *Handler) GetManifest(c *gin.Context) {
	manifest, err := h.loadManifest()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "failed to load manifest",
			"error":   err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "manifest loaded",
		"data":    manifest,
	})
}

func (h *Handler) GetArtifact(c *gin.Context) {
	service := c.Param("service")
	artifactPath := strings.TrimPrefix(c.Param("filepath"), "/")
	cleaned := filepath.Clean(artifactPath)
	if cleaned == "." || strings.HasPrefix(cleaned, "..") {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "invalid artifact path",
		})
		return
	}

	fullPath := filepath.Join(h.cfg.Storage.ArtifactsRoot, service, cleaned)
	if _, err := os.Stat(fullPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, gin.H{
				"success": false,
				"message": "artifact not found",
				"path":    fullPath,
			})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "failed to read artifact",
			"error":   err.Error(),
		})
		return
	}

	c.File(fullPath)
}

func (h *Handler) loadManifest() (*models.Manifest, error) {
	data, err := os.ReadFile(h.cfg.Manifest.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &models.Manifest{
				GeneratedAt: time.Now().UTC().Format(time.RFC3339),
				Services:    []models.ServiceEntry{},
			}, nil
		}
		return nil, err
	}

	var manifest models.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}

	if manifest.GeneratedAt == "" {
		manifest.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if manifest.Services == nil {
		manifest.Services = []models.ServiceEntry{}
	}

	return &manifest, nil
}
