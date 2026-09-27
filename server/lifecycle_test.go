package server

import (
	"context"
	"testing"
	"time"
)

func TestServer_GracefulShutdownLifecycle(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.PublicListenAddr = "127.0.0.1:18081"
	cfg.AdminListenAddr = "127.0.0.1:19091"
	cfg.Limits.DrainTimeout = 100 * time.Millisecond

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	mockExec := &mockVADExecutor{ready: true, available: 4, total: 4}
	srv.SetVADExecutor(mockExec)

	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	// Verify server is ready
	if !srv.ready.Load() {
		t.Fatal("expected server to be ready after start")
	}

	// Trigger shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Verify unready and shut down
	if srv.ready.Load() {
		t.Fatal("expected server to be unready after shutdown")
	}
	if !srv.shuttingDown.Load() {
		t.Fatal("expected shuttingDown flag to be true")
	}
}

func TestServer_ImmediateCancelShutdown(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.PublicListenAddr = "127.0.0.1:18082"
	cfg.AdminListenAddr = "127.0.0.1:19092"
	cfg.Limits.ImmediateCancel = true
	cfg.Limits.DrainTimeout = 10 * time.Second // Should be ignored because ImmediateCancel is true

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	start := time.Now()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("immediate cancel took too long: %v (expected < 500ms)", elapsed)
	}
}

func TestServer_ForcedShutdownTimeoutReturnsError(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.PublicListenAddr = "127.0.0.1:18083"
	cfg.AdminListenAddr = "127.0.0.1:19093"

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	// Expired context simulates forced shutdown timeout
	shutdownCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err = srv.Shutdown(shutdownCtx)
	if err == nil {
		t.Fatal("expected non-nil error when shutdown deadline is exceeded, got nil")
	}
}
