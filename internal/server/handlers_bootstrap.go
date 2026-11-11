package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"HA-Services-Deployer/internal/bootstrap"

	"github.com/gin-gonic/gin"
)

// 首次运行初始化端点（契约：ha-services-deployer.yaml v1.3.0
// /api/v1/deploy/bootstrap/*）。设计边界：
//   - 初始化层独立于 orchestration.enabled——全新机器上编排尚未启用时
//     同样可用；变更面（plan/run）仍强制 X-Deploy-Token（fail-closed）；
//   - 只读面（status/runs）与既有只读端点一致不加鉴权；响应里任何机密值
//     只以指纹出现，绝不回显明文；
//   - plan = dry-run，绝不落盘；run = 异步执行，逐项落运行记录，重跑即续跑。

// MVStatusSource 提供 MaintenanceView 监督状态（由 mvmanager 实现；
// 接口定义在 server 侧，避免依赖方向倒挂）。
type MVStatusSource interface {
	Status(ctx context.Context) MVStatus
}

// MVStatus 是 MV 监督的只读视图。
type MVStatus struct {
	Mode       string `json:"mode"` // managed | reused | disabled | unavailable
	Address    string `json:"address,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Healthy    *bool  `json:"healthy,omitempty"`
	BinaryPath string `json:"binary_path,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// bootstrapAuth 是初始化变更端点的令牌校验（只认 X-Deploy-Token，
// 与编排面同样的 fail-closed；不依赖 orchestration.enabled）。
func (h *Handler) bootstrapAuth(c *gin.Context) {
	if h.deployToken == "" {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"success": false,
			"message": "deploy token not configured; refusing unauthenticated bootstrap (fail-closed)",
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

func (h *Handler) bootstrapUnavailable(c *gin.Context) bool {
	if h.boot == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"success": false,
			"message": "bootstrap layer not initialized",
		})
		return true
	}
	return false
}

// GetBootstrapStatus 返回初始化层现状与向导元数据（只读，无机密明文）。
func (h *Handler) GetBootstrapStatus(c *gin.Context) {
	if h.bootstrapUnavailable(c) {
		return
	}
	st, err := h.boot.Store().LoadState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "failed to load bootstrap state",
			"error":   err.Error(),
		})
		return
	}

	secrets := gin.H{}
	for _, key := range bootstrap.SecretKeyNames() {
		if v := st.Secrets[key]; v != "" {
			secrets[key] = bootstrap.Mask(v)
		} else {
			secrets[key] = "(absent)"
		}
	}

	envFile := bootstrap.DefaultEnvFile
	secretsDir := bootstrap.DefaultSecretsDir
	artifacts := gin.H{
		"env_file":            filePresent(envFile),
		"signing_private_key": filePresent(filepath.Join(secretsDir, "private_key.pem")),
		"signing_public_key":  filePresent(filepath.Join(secretsDir, "public_key.pem")),
	}

	templates, _ := bootstrap.ListTemplates(bootstrap.DefaultTemplateDir)

	// 裸金属渲染输出的预填：清单在场时按 安装根+facts.config 路径 推导；
	// 否则只给配置文件名约定（输出根由向导里的人填）。
	configFiles := map[string]string{}
	outputs := map[string]string{}
	for _, tmpl := range templates {
		configFiles[tmpl] = bootstrap.ConventionalConfigFile(tmpl)
	}
	if h.orch != nil {
		if ms, _ := h.orch.Load(); len(ms) > 0 {
			byProject := map[string]int{}
			for i, m := range ms {
				byProject[m.Project] = i
			}
			for svc := range configFiles {
				idx, ok := byProject[svc]
				if !ok {
					continue
				}
				m := ms[idx]
				if len(m.Facts.Config) > 0 && m.Facts.Config[0].Path != "" {
					configFiles[svc] = m.Facts.Config[0].Path
				}
				root := m.Baremetal.Install.Root
				if root != "" && root != "unknown" {
					outputs[svc] = root + "/" + configFiles[svc]
				}
			}
		}
	}

	// 基础设施现状（只读探测）：向导据此决定"一键拉起"按钮是否出现。
	var probeIn bootstrap.Inputs
	probeIn.Prepare()
	infra := h.boot.ProbeInfra(c.Request.Context(), &probeIn)

	mv := gin.H{"mode": "disabled", "detail": "maintenance view supervision not wired"}
	if h.mv != nil {
		s := h.mv.Status(c.Request.Context())
		mv = gin.H{
			"mode": s.Mode, "address": s.Address, "pid": s.PID,
			"healthy": s.Healthy, "binary_path": s.BinaryPath, "detail": s.Detail,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "bootstrap status",
		"data": gin.H{
			"state_exists":         len(st.Secrets) > 0,
			"secrets":              secrets,
			"signing_key_fingerprint": st.SigningKeyFingerprint,
			"artifacts":            artifacts,
			"deploy_token_configured": h.deployToken != "",
			"templates":            templates,
			"infra":                infra,
			"defaults": gin.H{
				"client_ids": gin.H{
					"super": bootstrap.DefaultSuperClientID,
					"public": bootstrap.DefaultPublicClientID,
					"winnerproxy": bootstrap.DefaultWinnerProxyID,
					"ygg": bootstrap.DefaultYGGClientID,
					"haskinproxy": bootstrap.DefaultSkinProxyID,
					"sdkhandler": bootstrap.DefaultSDKHandlerID,
					"haskinlib": bootstrap.DefaultSkinLibID,
				},
				"db_names": gin.H{
					"hrpauth": bootstrap.DefaultHRPAuthDB,
					"ygg": bootstrap.DefaultYGGDB,
					"haskinlib": bootstrap.DefaultHASkinLibDB,
					"mca": bootstrap.DefaultMCADB,
				},
				"app_db_user": bootstrap.DefaultAppDBUser,
				"config_files": configFiles,
				"baremetal_outputs": outputs,
			},
			"maintenance_view": mv,
		},
	})
}

// PostBootstrapPlan 计算 dry-run 计划（校验缺值当场拦截，绝不落盘）。
func (h *Handler) PostBootstrapPlan(c *gin.Context) {
	if h.bootstrapUnavailable(c) {
		return
	}
	var in bootstrap.Inputs
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "invalid bootstrap inputs payload: " + err.Error(),
		})
		return
	}
	if ferrs := in.Prepare(); len(ferrs) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "必填项缺失或不合法：请修正后重试",
			"data":    gin.H{"field_errors": ferrs},
		})
		return
	}
	plan, err := h.boot.Plan(c.Request.Context(), &in)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "plan computed (nothing written)",
		"data":    plan,
	})
}

// PostBootstrapRun 启动一次异步初始化运行（单飞；重跑即续跑）。
func (h *Handler) PostBootstrapRun(c *gin.Context) {
	if h.bootstrapUnavailable(c) {
		return
	}
	var in bootstrap.Inputs
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "invalid bootstrap inputs payload: " + err.Error(),
		})
		return
	}
	if ferrs := in.Prepare(); len(ferrs) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": "必填项缺失或不合法：请修正后重试",
			"data":    gin.H{"field_errors": ferrs},
		})
		return
	}
	run, err := h.boot.StartRun(c.Request.Context(), &in)
	if err != nil {
		if errors.Is(err, bootstrap.ErrRunInProgress) {
			c.JSON(http.StatusConflict, gin.H{
				"success": false,
				"message": err.Error(),
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
		"message": "bootstrap run started",
		"data":    run,
	})
}

// GetBootstrapRuns 列出初始化运行历史（值均已掩码）。
func (h *Handler) GetBootstrapRuns(c *gin.Context) {
	if h.bootstrapUnavailable(c) {
		return
	}
	summaries, err := h.boot.ListRuns()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"success": false,
			"message": "failed to list bootstrap runs",
			"error":   err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "bootstrap runs listed",
		"data":    gin.H{"runs": summaries},
	})
}

// GetBootstrapRun 读取单次初始化运行记录（断点续跑的可见面）。
func (h *Handler) GetBootstrapRun(c *gin.Context) {
	if h.bootstrapUnavailable(c) {
		return
	}
	run, err := h.boot.GetRun(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"success": false,
			"message": "bootstrap run not found",
			"error":   err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "bootstrap run loaded",
		"data":    run,
	})
}

// GetMaintenanceView 返回 MV 监督状态（managed/reused/disabled/unavailable）。
func (h *Handler) GetMaintenanceView(c *gin.Context) {
	if h.mv == nil {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "maintenance view supervision not wired",
			"data":    gin.H{"mode": "disabled"},
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "maintenance view status",
		"data":    h.mv.Status(c.Request.Context()),
	})
}

// GetBootstrapInfra 返回 MySQL/Redis 基础设施现状（只读探测，不拉起）：
// 每个实例报"被哪条链路用、裸金属与容器各自的连接地址、镜像/卷/健康"。
func (h *Handler) GetBootstrapInfra(c *gin.Context) {
	if h.bootstrapUnavailable(c) {
		return
	}
	var in bootstrap.Inputs
	in.Prepare()
	facts := h.boot.ProbeInfra(c.Request.Context(), &in)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "infra status",
		"data":    facts,
	})
}

// PostBootstrapInfraMySQL 一键确保 MySQL 可用（探测→接管→拉起容器，
// 等真能执行查询才返回）。幂等：已有可用实例不重复起。
func (h *Handler) PostBootstrapInfraMySQL(c *gin.Context) {
	h.postBootstrapInfra(c, "mysql")
}

// PostBootstrapInfraRedis 一键确保 Redis 可用（等真能 PING 才返回）。
func (h *Handler) PostBootstrapInfraRedis(c *gin.Context) {
	h.postBootstrapInfra(c, "redis")
}

func (h *Handler) postBootstrapInfra(c *gin.Context, kind string) {
	if h.bootstrapUnavailable(c) {
		return
	}
	var in bootstrap.Inputs
	// 允许只提交端口等少量字段；其余取默认。
	_ = c.ShouldBindJSON(&in)
	in.Prepare()

	ctx, cancel := context.WithTimeout(c.Request.Context(), 240*time.Second)
	defer cancel()

	var (
		inst *bootstrap.InfraInstance
		err  error
	)
	if kind == "mysql" {
		inst, err = h.boot.EnsureMySQLOnly(ctx, &in)
	} else {
		inst, err = h.boot.EnsureRedisOnly(ctx, &in)
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": kind + " ready",
		"data":    inst,
	})
}

func filePresent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
