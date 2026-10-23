package server

import (
	"slices"
	"testing"
	"time"
)

func TestCloudRunConfigLoadsInProduction(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "cloudrun-test-token-secret-0123456789abcdef")
	t.Setenv("GOVOICE_DATABASE_DSN", "postgres://switchboard:x@/switchboard?host=/cloudsql/p:r:i&sslmode=disable")
	cfg, err := LoadConfig("../config/cloudrun.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != ProfileProduction || cfg.Storage.Dialect != "postgres" {
		t.Fatalf("profile=%s dialect=%s", cfg.Profile, cfg.Storage.Dialect)
	}
	if !cfg.PublicAdmin.Enabled || !cfg.OriginAllowSameHost || !cfg.TLS.TrustedProxyMode || !slices.Equal(cfg.TLS.TrustedProxies, []string{"169.254.0.0/16"}) {
		t.Fatalf("one-port settings missing: %+v", cfg.PublicAdmin)
	}
	if cfg.PublicListenAddr != "0.0.0.0:8080" || cfg.ProbeListenAddr != "" || cfg.AdminListenAddr != "127.0.0.1:9090" {
		t.Fatalf("listeners: public=%s admin=%s probe=%q", cfg.PublicListenAddr, cfg.AdminListenAddr, cfg.ProbeListenAddr)
	}
	if cfg.MCP.AllowLocalHTTP || cfg.Recording.Enabled {
		t.Fatal("a public demo has no loopback MCP and no recording")
	}
	if cfg.Limits.MaxSessions > 20 || cfg.Limits.MaxCallDuration > 30*time.Minute {
		t.Fatalf("a one-instance demo is bounded: sessions=%d duration=%v", cfg.Limits.MaxSessions, cfg.Limits.MaxCallDuration)
	}
}

// The deployed Cloud Run demo: same shape as the Cloud SQL variant, but a SQLite store in the
// container's tmpfs (wiped on restart) instead of Postgres.
func TestCloudRunSQLiteConfigLoadsInProduction(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "cloudrun-test-token-secret-0123456789abcdef")
	t.Setenv("GOVOICE_DATABASE_DSN", "/tmp/switchboard.db")
	cfg, err := LoadConfig("../config/cloudrun-sqlite.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profile != ProfileProduction || cfg.Storage.Dialect != "sqlite" {
		t.Fatalf("profile=%s dialect=%s", cfg.Profile, cfg.Storage.Dialect)
	}
	if !cfg.PublicAdmin.Enabled || !cfg.OriginAllowSameHost || !cfg.TLS.TrustedProxyMode || !slices.Equal(cfg.TLS.TrustedProxies, []string{"169.254.0.0/16"}) {
		t.Fatalf("one-port settings missing: %+v", cfg.PublicAdmin)
	}
	if cfg.PublicListenAddr != "0.0.0.0:8080" || cfg.ProbeListenAddr != "" || cfg.AdminListenAddr != "127.0.0.1:9090" {
		t.Fatalf("listeners: public=%s admin=%s probe=%q", cfg.PublicListenAddr, cfg.AdminListenAddr, cfg.ProbeListenAddr)
	}
	if cfg.MCP.AllowLocalHTTP || cfg.Recording.Enabled {
		t.Fatal("a public demo has no loopback MCP and no recording")
	}
	if cfg.Limits.MaxSessions > 20 || cfg.Limits.MaxCallDuration > 30*time.Minute {
		t.Fatalf("a one-instance demo is bounded: sessions=%d duration=%v", cfg.Limits.MaxSessions, cfg.Limits.MaxCallDuration)
	}
}
