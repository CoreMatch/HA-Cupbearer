package main

import (
	"log"

	"HA-Services-Deployer/internal/config"
	"HA-Services-Deployer/internal/server"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	router := server.NewRouter(cfg)
	log.Printf("HA Services Deployer listening on %s", cfg.Server.Listen)

	if err := router.Run(cfg.Server.Listen); err != nil {
		log.Fatalf("run server: %v", err)
	}
}
