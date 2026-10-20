package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"path/filepath"

	"govoice/examples/envload"
	"govoice/server"
	"govoice/smartturn"
	"govoice/vad"
)

// fatal logs through slog (JSON in production) and exits 1.
func fatal(msg string, err error) {
	slog.Error(msg, slog.String("err", err.Error()))
	os.Exit(1)
}

func main() {
	// JSON until the configured logger exists, so no startup line is unstructured.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	configPath := flag.String("config", "config/development.yaml", "path to YAML configuration file")
	profileFlag := flag.String("profile", "", "override profile: development|production")
	vadModeFlag := flag.String("vad-mode", "", "override VAD mode: process|inprocess")
	flag.Parse()

	cfg, err := server.LoadConfigProfile(*configPath, server.Profile(*profileFlag))
	if err != nil {
		fatal("failed to load config from "+*configPath, err)
	}
	// .env files are a development convenience: production gets its environment from the orchestrator,
	// so a stray .env in the working directory can never supply a DSN, key or token secret.
	if cfg.Profile != server.ProfileProduction {
		_ = envload.Load()
	}
	if *vadModeFlag != "" {
		cfg.VAD.Mode = *vadModeFlag
		if err := server.ValidateConfig(cfg); err != nil { // e.g. inprocess in production
			fatal("invalid -vad-mode", err)
		}
	}

	srv, err := server.NewServer(cfg)
	if err != nil {
		fatal("failed to initialize server", err)
	}
	logger := srv.Logger()
	slog.SetDefault(logger) // provider and registry warnings share the configured (JSON) format

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
		fatal("vad executor failed closed (no stub fallback)", err)
	}

	if vadExec != nil {
		srv.SetVADExecutor(vadExec)
	}

	if cfg.LoadsSmartTurn() {
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
			fatal("smart-turn model failed closed (hash/worker check)", err)
		}
		srv.SetTurnBackend(be)
	}

	if err := srv.EnableManagement(context.Background()); err != nil {
		fatal("failed to enable management plane", err)
	}

	if err := srv.Start(); err != nil {
		fatal("failed to start server", err)
	}

	// Wait for SIGINT or SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info("received signal: initiating graceful shutdown", slog.String("signal", sig.String()))

	shutdownTimeout := server.ShutdownBudget(cfg.Limits.DrainTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	err = srv.Shutdown(shutdownCtx) // closes the VAD executor and turn backend first
	vad.ShutdownORT()               // only after every ORT session is closed
	if err != nil {
		logger.Error("error during shutdown", slog.String("err", err.Error()))
		os.Exit(1)
	}

	logger.Info("server stopped cleanly")
	os.Exit(0)
}
