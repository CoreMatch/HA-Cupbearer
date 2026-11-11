package server

import (
	"net/http"
	"os"

	"HA-Cupbearer/internal/bootstrap"
	"HA-Cupbearer/internal/models"
	"HA-Cupbearer/internal/orchestrator"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	cfg         *models.Config
	orch        *orchestrator.Orchestrator // nil = 编排关闭（零回归）
	deployToken string
	boot        *bootstrap.Engine  // nil = 初始化层未装配
	mv          MVStatusSource     // nil = MV 监督未装配
}

func NewHandler(cfg *models.Config, orch *orchestrator.Orchestrator, deployToken string, boot *bootstrap.Engine, mv MVStatusSource) *Handler {
	return &Handler{cfg: cfg, orch: orch, deployToken: deployToken, boot: boot, mv: mv}
}

func NewRouter(cfg *models.Config, orch *orchestrator.Orchestrator, deployToken string, boot *bootstrap.Engine, mv MVStatusSource) *gin.Engine {
	router := gin.Default()
	handler := NewHandler(cfg, orch, deployToken, boot, mv)

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
			deploy.POST("/container/trigger", handler.deployAuth, handler.PostDeployContainerTrigger)
		}

		// 首次运行初始化面（独立于 orchestration.enabled；变更面经
		// bootstrapAuth 强制 X-Deploy-Token，fail-closed）。组前缀写全路径，
		// 与 HA-Contract 的路由级 drift 提取（单层 Group 前缀解析）保持可校验。
		bootstrapGroup := router.Group("/api/v1/deploy/bootstrap")
		{
			bootstrapGroup.GET("/status", handler.GetBootstrapStatus)
			bootstrapGroup.GET("/infra", handler.GetBootstrapInfra)
			bootstrapGroup.POST("/infra/mysql", handler.bootstrapAuth, handler.PostBootstrapInfraMySQL)
			bootstrapGroup.POST("/infra/redis", handler.bootstrapAuth, handler.PostBootstrapInfraRedis)
			bootstrapGroup.POST("/plan", handler.bootstrapAuth, handler.PostBootstrapPlan)
			bootstrapGroup.POST("/run", handler.bootstrapAuth, handler.PostBootstrapRun)
			bootstrapGroup.GET("/runs", handler.GetBootstrapRuns)
			bootstrapGroup.GET("/runs/:id", handler.GetBootstrapRun)
		}
		api.GET("/deploy/maintenance-view", handler.GetMaintenanceView)
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
