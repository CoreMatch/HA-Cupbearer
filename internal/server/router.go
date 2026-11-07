package server

import (
	"net/http"
	"os"

	"HA-Services-Deployer/internal/models"
	"HA-Services-Deployer/internal/orchestrator"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	cfg         *models.Config
	orch        *orchestrator.Orchestrator // nil = 编排关闭（零回归）
	deployToken string
}

func NewHandler(cfg *models.Config, orch *orchestrator.Orchestrator, deployToken string) *Handler {
	return &Handler{cfg: cfg, orch: orch, deployToken: deployToken}
}

func NewRouter(cfg *models.Config, orch *orchestrator.Orchestrator, deployToken string) *gin.Engine {
	router := gin.Default()
	handler := NewHandler(cfg, orch, deployToken)

	router.GET("/healthz", handler.Healthz)

	api := router.Group("/api/v1")
	{
		api.GET("/healthz", handler.Healthz)
		api.GET("/manifest", handler.GetManifest)
		api.GET("/projects", handler.GetProjects)
		api.GET("/artifacts/:service/*filepath", handler.GetArtifact)

		// 部署编排面。只读端点不加鉴权（与既有只读面一致）；
		// 变更端点经 deployAuth 强制 X-Deploy-Token（fail-closed）。
		// orch == nil（orchestration.enabled=false）时全部返回 503，
		// 既有 4 个只读端点不受任何影响。
		// 组前缀写全路径（不嵌套在 api 下），与 HA-Contract 的
		// 路由级 drift 提取（单层 Group 前缀解析）保持可校验。
		deploy := router.Group("/api/v1/deploy")
		{
			deploy.GET("/manifests", handler.GetDeployManifests)
			deploy.GET("/validate", handler.GetDeployValidate)
			deploy.GET("/status", handler.GetDeployStatus)
			deploy.GET("/preflight", handler.GetDeployPreflight)
			deploy.GET("/runs", handler.GetDeployRuns)
			deploy.GET("/runs/:id", handler.GetDeployRun)
			deploy.POST("/run", handler.deployAuth, handler.PostDeployRun)
			deploy.POST("/rollback", handler.deployAuth, handler.PostDeployRollback)
		}
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
