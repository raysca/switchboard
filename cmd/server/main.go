package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"path/filepath"
	"slices"

	"govoice/examples/envload"
	"govoice/server"
	"govoice/smartturn"
	"govoice/vad"
)

func main() {
	_ = envload.Load()

	configPath := flag.String("config", "config/development.yaml", "path to YAML configuration file")
	profileFlag := flag.String("profile", "", "override profile: development|production")
	vadModeFlag := flag.String("vad-mode", "", "override VAD mode: process|inprocess")
	flag.Parse()

	cfg, err := server.LoadConfigProfile(*configPath, server.Profile(*profileFlag))
	if err != nil {
		log.Fatalf("failed to load config from %s: %v", *configPath, err)
	}
	if *vadModeFlag != "" {
		cfg.VAD.Mode = *vadModeFlag
		if err := server.ValidateConfig(cfg); err != nil { // e.g. inprocess in production
			log.Fatalf("invalid -vad-mode: %v", err)
		}
	}

	srv, err := server.NewServer(cfg)
	if err != nil {
		log.Fatalf("failed to initialize server: %v", err)
	}

	// Initialize VAD Executor based on configuration
	vadCfg := vad.DefaultConfig()
	vadCfg.Mode = cfg.VAD.Mode
	if vadCfg.Mode == "inprocess" {
		vadCfg.Mode = "sharded"
	}
	if cfg.VAD.Workers > 0 {
		vadCfg.Workers = cfg.VAD.Workers
	}
	if cfg.VAD.Batch > 0 {
		vadCfg.Batch = cfg.VAD.Batch
	}
	if cfg.VAD.QueueCap > 0 {
		vadCfg.QueueCap = cfg.VAD.QueueCap
	}
	if exe, err := os.Executable(); err == nil {
		vadCfg.WorkerBinary = filepath.Join(filepath.Dir(exe), "vadworker")
	} else {
		vadCfg.WorkerBinary = "bin/vadworker"
	}
	vad.ConfigureArtifacts(cfg.VAD.ModelPath, cfg.VAD.OrtLibPath)

	vadExec, err := vad.New(vadCfg)
	if err != nil {
		log.Fatalf("vad executor failed closed (no stub fallback): %v", err)
	}

	if vadExec != nil {
		srv.SetVADExecutor(vadExec)
	}

	if slices.Contains(cfg.Turn.Stop, "turn-analyzer") {
		smartturn.ConfigureModel(cfg.Turn.Analyzer.ModelPath)
		var be smartturn.Backend
		var err error
		if cfg.TurnAnalyzerMode() == "inprocess" {
			be, err = smartturn.NewInProcess(smartturn.ModelPath())
		} else {
			bin := "bin/turnworker"
			if exe, e := os.Executable(); e == nil {
				bin = filepath.Join(filepath.Dir(exe), "turnworker")
			}
			be, err = smartturn.NewPool(smartturn.PoolConfig{Binary: bin, Workers: cfg.Turn.Analyzer.Workers})
		}
		if err != nil {
			log.Fatalf("smart-turn model failed closed (hash/worker check): %v", err)
		}
		srv.SetTurnBackend(be)
	}

	if err := srv.EnableManagement(context.Background()); err != nil {
		log.Fatalf("failed to enable management plane: %v", err)
	}

	if err := srv.Start(); err != nil {
		log.Fatalf("failed to start server: %v", err)
	}

	fmt.Printf("Voice Framework V1 Server running\n")
	fmt.Printf("  Profile: %s\n", cfg.Profile)
	fmt.Printf("  Public:  %s\n", cfg.PublicListenAddr)
	fmt.Printf("  Admin:   %s\n", cfg.AdminListenAddr)

	// Wait for SIGINT or SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	fmt.Printf("\nReceived signal %v: initiating graceful shutdown...\n", sig)

	shutdownTimeout := server.ShutdownBudget(cfg.Limits.DrainTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	err = srv.Shutdown(shutdownCtx) // closes the VAD executor and turn backend first
	vad.ShutdownORT()               // only after every ORT session is closed
	if err != nil {
		log.Printf("error during shutdown: %v", err)
		os.Exit(1)
	}

	fmt.Println("Server stopped cleanly.")
	os.Exit(0)
}
