package server

import (
	"net/http"
	"os"

	"HA-Services-Deployer/internal/models"
	"github.com/gin-gonic/gin"
)

func NewRouter(cfg *models.Config) *gin.Engine {
	router := gin.Default()
	handler := NewHandler(cfg)

	router.GET("/healthz", handler.Healthz)

	api := router.Group("/api/v1")
	{
		api.GET("/healthz", handler.Healthz)
		api.GET("/manifest", handler.GetManifest)
		api.GET("/projects", handler.GetProjects)
		api.GET("/artifacts/:service/*filepath", handler.GetArtifact)
	}

	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "route not found",
			"path":    c.Request.URL.Path,
		})
	})

	_ = os.MkdirAll(cfg.Storage.ArtifactsRoot, 0o755)

	return router
}
