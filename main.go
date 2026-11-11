package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"HA-Cupbearer/internal/bootstrap"
	"HA-Cupbearer/internal/config"
	"HA-Cupbearer/internal/docker"
	"HA-Cupbearer/internal/mvmanager"
	"HA-Cupbearer/internal/orchestrator"
	"HA-Cupbearer/internal/server"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// 凭据常载（不再以 orchestration.enabled 为前提）：初始化层的变更面
	// 同样 fail-closed 依赖 deploy_token。路径缺省回落本地数据目录。
	credsPath := cfg.Orchestration.CredentialsPath
	if credsPath == "" {
		credsPath = "./data/deploy/credentials.json"
	}
	creds, err := config.LoadCredentials(credsPath)
	if err != nil {
		log.Fatalf("load orchestration credentials: %v", err)
	}

	// 首跑闭环：凭据文件没有 deploy_token 时自动生成并落盘（0600）。
	// 令牌只经本地文件与 MV 服务端流转，绝不下发浏览器；日志只记指纹。
	if creds.DeployToken == "" {
		token := config.GenerateToken()
		if token == "" {
			log.Fatalf("generate deploy token: crypto/rand unavailable")
		}
		creds.DeployToken = token
		if err := config.SaveCredentials(credsPath, creds); err != nil {
			log.Fatalf("save generated deploy token: %v", err)
		}
		log.Printf("deploy token auto-generated and saved to %s (fingerprint %s)",
			credsPath, bootstrap.Fingerprint(token))
	}

	// 编排能力按配置开启；关闭时（默认）不构造编排器，
	// 服务行为与纯下发模式完全一致（零回归）。
	var orch *orchestrator.Orchestrator
	if cfg.Orchestration.Enabled {
		orch = orchestrator.New(cfg, creds)
		log.Printf("orchestration enabled: scan_roots=%v tinyvisor=%s",
			cfg.Orchestration.ScanRoots, cfg.Orchestration.Tinyvisor.URL)
	}

	// 首次运行初始化层：独立于编排开关，全新机器开箱即用。
	// 基础设施（MySQL/Redis 容器）经 docker CLI 驱动（端点探测与编排器同规则）。
	dockerHost := cfg.Orchestration.Container.DockerHost
	if dockerHost == "" {
		dockerHost = docker.DetectHost()
	}
	boot := bootstrap.NewEngine(bootstrap.NewStore("./data/bootstrap"), bootstrap.DefaultTemplateDir, docker.NewCLI(dockerHost))

	// 运维面板自动拉起：用户不需要单独部署 MV（已有一份健康的则复用）。
	mv := mvmanager.New(mvmanager.Config{
		Enabled:    cfg.MVEnabled(),
		BinaryPath: cfg.MaintenanceView.BinaryPath,
		Listen:     cfg.MaintenanceView.Listen,
		WorkDir:    cfg.MaintenanceView.WorkDir,
	}, cupbearerSelfURL(cfg.Server.Listen), creds.DeployToken)
	mv.Start(context.Background())
	defer mv.Stop()

	router := server.NewRouter(cfg, orch, creds.DeployToken, boot, mvAdapter{mv})
	log.Printf("HA Cupbearer listening on %s", cfg.Server.Listen)

	// 优雅退出：信号到达时先停 HTTP 面，再经 defer 停受管的 MV。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: cfg.Server.Listen, Handler: router}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("run server: %v", err)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Printf("HA Cupbearer stopped")
}

// cupbearerSelfURL 推导 MV 回连本服务的地址（listen 的端口 + 回环主机）。
func cupbearerSelfURL(listen string) string {
	port := "18080"
	if _, p, err := net.SplitHostPort(listen); err == nil {
		port = p
	} else if _, err := strconv.Atoi(listen); err == nil {
		port = listen
	}
	return "http://127.0.0.1:" + port
}

// mvAdapter 把 mvmanager.StatusView 适配成 server.MVStatus。
type mvAdapter struct{ m *mvmanager.Manager }

func (a mvAdapter) Status(ctx context.Context) server.MVStatus {
	v := a.m.Status(ctx)
	return server.MVStatus{
		Mode: v.Mode, Address: v.Address, PID: v.PID,
		Healthy: v.Healthy, BinaryPath: v.BinaryPath, Detail: v.Detail,
	}
}
