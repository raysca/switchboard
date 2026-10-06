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

	"govoice/examples/envload"
	"govoice/server"
	"govoice/vad"
)

func main() {
	_ = envload.Load()

	configPath := flag.String("config", "config/development.yaml", "path to YAML configuration file")
	profileFlag := flag.String("profile", "", "override profile: development|production")
	vadModeFlag := flag.String("vad-mode", "", "override VAD mode: process|inprocess")
	flag.Parse()

	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config from %s: %v", *configPath, err)
	}

	if *profileFlag != "" {
		cfg.Profile = server.Profile(*profileFlag)
	}
	if *vadModeFlag != "" {
		cfg.VAD.Mode = *vadModeFlag
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

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("error during shutdown: %v", err)
		os.Exit(1)
	}

	fmt.Println("Server stopped cleanly.")
	os.Exit(0)
}
