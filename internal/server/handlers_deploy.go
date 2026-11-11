package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"HA-Services-Deployer/internal/orchestrator"

	"github.com/gin-gonic/gin"
)

// 部署编排端点。设计边界：
//   - 只读面（manifests/validate/status/runs）与现有只读端点一致，不加鉴权，
//     服务默认仅绑回环的既有约束不变；
//   - 变更面（run/rollback）必须携带 X-Deploy-Token，令牌来自本地凭据文件；
//     未配置令牌时一律 503（fail-closed），任何情况下不放开为无鉴权。
//     运维面板（MaintenanceView）作为操作面时由服务端持有并转发该令牌，
//     浏览器端不接触凭据——因此校验只认请求头，不依赖任何前端行为。

func (h *Handler) orchestrationDisabled(c *gin.Context) bool {
	if h.orch == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"success": false,
			"message": "orchestration disabled (config: orchestration.enabled=false)",
		})
		return true
	}
	return false
}

// deployAuth 是变更端点的令牌校验中间件（仅认 X-Deploy-Token 请求头）。
func (h *Handler) deployAuth(c *gin.Context) {
	if h.orch == nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"success": false,
			"message": "orchestration disabled (config: orchestration.enabled=false)",
		})
		return
	}
	if h.deployToken == "" {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"success": false,
			"message": "deploy token not configured; refusing unauthenticated deployment (fail-closed)",
		})
		return
	}
	got := c.GetHeader("X-Deploy-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.deployToken)) != 1 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "invalid or missing X-Deploy-Token",
		})
		return
	}
	c.Next()
}

func (h *Handler) GetDeployManifests(c *gin.Context) {
	if h.orchestrationDisabled(c) {
		return
	}
	manifests, rep := h.orch.Load()

	type view struct {
		Project         string `json:"project"`
		Kind            string `json:"kind"`
		DisplayName     string `json:"display_name"`
		Group           string `json:"group"`
		ServiceVersion  string `json:"service_version"`
		Manager         string `json:"manager"`
		SourcePath      string `json:"source_path"`
	}
	views := make([]view, 0, len(manifests))
	for _, m := range manifests {
		manager := m.Baremetal.Process.Manager
		if m.IsExternal() {
			manager = ""
		}
		if m.IsPlugin() {
			manager = "plugin"
		}
		views = append(views, view{
			Project:        m.Project,
			Kind:           m.Kind,
			DisplayName:    m.DisplayName,
			Group:          m.Group,
			ServiceVersion: m.ServiceVersion,
			Manager:        manager,
			SourcePath:     m.SourcePath,
		})
	}

	loadErrors := make([]string, 0, len(rep.Errors))
	loadErrors = append(loadErrors, rep.Errors...)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "manifests aggregated",
		"data": gin.H{
			"count":    len(manifests),
			"projects": views,
			"errors":   loadErrors,
		},
	})
}

func (h *Handler) GetDeployValidate(c *gin.Context) {
	if h.orchestrationDisabled(c) {
		return
	}
	_, rep := h.orch.Load()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "validation executed",
		"data": gin.H{
			"ok":       rep.OK(),
			"errors":   rep.Errors,
			"warnings": rep.Warnings,
		},
	})
}

func (h *Handler) GetDeployStatus(c *gin.Context) {
	if h.orchestrationDisabled(c) {
		return
	}
	report, err := h.orch.Status(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "failed to collect status",
			"error":   err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "status collected",
		"data":    report,
	})
}

func (h *Handler) GetDeployRuns(c *gin.Context) {
	if h.orchestrationDisabled(c) {
		return
	}
	summaries, err := h.orch.ListRuns()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "failed to list runs",
			"error":   err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "runs listed",
		"data": gin.H{
			"runs": summaries,
		},
	})
}

// GetDeployPreflight 只读前置检查：把部署前能查的问题一次查全、一次报全，
// 输出分级为阻断项（拒绝执行）与警告项（可继续但有风险）。
// 查询参数：projects（逗号分隔）与/或 group 指定范围，空=全部。
func (h *Handler) GetDeployPreflight(c *gin.Context) {
	if h.orchestrationDisabled(c) {
		return
	}
	var projects []string
	for _, p := range strings.Split(c.Query("projects"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			projects = append(projects, p)
		}
	}

	report, err := h.orch.Preflight(c.Request.Context(), projects, c.Query("group"), "deploy")
	if err != nil {
		var verr *orchestrator.ErrValidation
		if errors.As(err, &verr) {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": "manifest validation failed",
				"data":    gin.H{"errors": verr.Errors},
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	msg := "preflight passed"
	if report.HasBlockers() {
		msg = fmt.Sprintf("preflight found %d blocker(s); a run would be refused", len(report.Blockers))
	} else if len(report.Warnings) > 0 {
		msg = fmt.Sprintf("preflight passed with %d warning(s)", len(report.Warnings))
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": msg,
		"data":    report,
	})
}

func (h *Handler) GetDeployRun(c *gin.Context) {
	if h.orchestrationDisabled(c) {
		return
	}
	run, err := h.orch.GetRun(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "run not found",
			"error":   err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "run loaded",
		"data":    run,
	})
}

func (h *Handler) PostDeployRun(c *gin.Context) {
	var opts orchestrator.RunOptions
	if err := c.ShouldBindJSON(&opts); err != nil {
		// 允许空 body（= 全量部署）。
		opts = orchestrator.RunOptions{}
	}

	run, err := h.orch.StartRun(opts)
	if err != nil {
		if errors.Is(err, orchestrator.ErrRunInProgress) {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
		var verr *orchestrator.ErrValidation
		if errors.As(err, &verr) {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": "manifest validation failed; run refused",
				"data":    gin.H{"errors": verr.Errors},
			})
			return
		}
		var perr *orchestrator.ErrPreflightBlocked
		if errors.As(err, &perr) {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": "preflight blocked; run refused",
				"data":    gin.H{"preflight": perr.Report},
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"success": true,
		"message": "run started",
		"data":    run,
	})
}

func (h *Handler) PostDeployRollback(c *gin.Context) {
	var body struct {
		Project string `json:"project"`
		To      string `json:"to"`
		Cascade bool   `json:"cascade"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Project == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "project is required",
		})
		return
	}

	run, err := h.orch.Rollback(body.Project, body.To, body.Cascade)
	if err != nil {
		if errors.Is(err, orchestrator.ErrRunInProgress) {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
		var verr *orchestrator.ErrValidation
		if errors.As(err, &verr) {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": "manifest validation failed; rollback refused",
				"data":    gin.H{"errors": verr.Errors},
			})
			return
		}
		var perr *orchestrator.ErrPreflightBlocked
		if errors.As(err, &perr) {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": "preflight blocked; rollback refused",
				"data":    gin.H{"preflight": perr.Report},
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"success": true,
		"message": "rollback started",
		"data":    run,
	})
}

// PostDeployContainerTrigger 一键触发整套容器编排（容器路径的默认交互）：
// 按清单已发布镜像引用生成 override，叠加平台 compose 模板执行
// `docker compose up -d`，结果如实记入运行记录。编排器在此路径不做
// 逐容器管理（那是 runtime=container 的可选能力）。
func (h *Handler) PostDeployContainerTrigger(c *gin.Context) {
	run, err := h.orch.TriggerContainerStack()
	if err != nil {
		if errors.Is(err, orchestrator.ErrRunInProgress) {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"message": err.Error(),
			})
			return
		}
		var verr *orchestrator.ErrValidation
		if errors.As(err, &verr) {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"message": "manifest validation failed; trigger refused",
				"data":    gin.H{"errors": verr.Errors},
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"success": true,
		"message": "container trigger started",
		"data":    run,
	})
}
