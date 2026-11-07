package main

import (
	"log"

	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/orchestrator"
	"HA-Services-Deployer/internal/server"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// 编排能力按配置开启；关闭时（默认）不构造编排器，
	// 服务行为与纯下发模式完全一致（零回归）。
	var orch *orchestrator.Orchestrator
	var deployToken string
	if cfg.Orchestration.Enabled {
		creds, err := config.LoadCredentials(cfg.Orchestration.CredentialsPath)
		if err != nil {
			log.Fatalf("load orchestration credentials: %v", err)
		}
		orch = orchestrator.New(cfg, creds)
		deployToken = creds.DeployToken
		log.Printf("orchestration enabled: scan_roots=%v tinyvisor=%s",
			cfg.Orchestration.ScanRoots, cfg.Orchestration.Tinyvisor.URL)
	}

	router := server.NewRouter(cfg, orch, deployToken)
	log.Printf("HA Services Deployer listening on %s", cfg.Server.Listen)

	if err := router.Run(cfg.Server.Listen); err != nil {
		log.Fatalf("run server: %v", err)
	}
}
