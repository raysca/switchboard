package server

import (
	"os"
	"slices"
	"strings"
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
		t.Fatal("a public deployment has no loopback MCP and no recording")
	}
	if cfg.Limits.MaxSessions > 20 || cfg.Limits.MaxCallDuration > 30*time.Minute {
		t.Fatalf("a one-instance demo is bounded: sessions=%d duration=%v", cfg.Limits.MaxSessions, cfg.Limits.MaxCallDuration)
	}
}

// The deployed Cloud Run SQLite profile: same shape as the Cloud SQL variant, but a SQLite store in
// the container's tmpfs (wiped on restart) instead of Postgres. The clean-room server starts empty
// and the optional demo pipeline seeds it afterwards.
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
		t.Fatal("a public deployment has no loopback MCP and no recording")
	}
	if cfg.Limits.MaxSessions > 20 || cfg.Limits.MaxCallDuration > 30*time.Minute {
		t.Fatalf("a one-instance demo is bounded: sessions=%d duration=%v", cfg.Limits.MaxSessions, cfg.Limits.MaxCallDuration)
	}
}

// The shipped Cloud Run configs are clean-room: no demo install block anywhere (a stored config that
// still names one fails startup as an unknown field, which is the documented breaking change).
func TestCloudRunConfigsHaveNoDemoBlock(t *testing.T) {
	for _, path := range []string{"../config/cloudrun.yaml", "../config/cloudrun-sqlite.yaml", "../deploy/rehearsal/server.yaml"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"auto_install", "\ndemo:", "DENTAL_MCP_URL", "ATLAS_DEMO_URL"} {
			if strings.Contains(string(raw), banned) {
				t.Errorf("%s must not carry demo config %q (the server is clean-room; demos are seeded by the demo pipeline)", path, banned)
			}
		}
	}
}

// The shipped Cloudflare profile stays on ephemeral SQLite; the external Postgres profile a demo
// deployment needs is a separate file the Worker selects from the DSN. Both must load.
func TestCloudflareProfilesLoad(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "cloudflare-test-token-secret-0123456789abcdef")
	t.Setenv("GOVOICE_DATABASE_DSN", "/tmp/switchboard.db")
	sqlite, err := LoadConfig("../config/cloudflare-demo.yaml")
	if err != nil {
		t.Fatalf("the shipped Cloudflare SQLite profile does not load: %v", err)
	}
	if sqlite.Storage.Dialect != "sqlite" {
		t.Fatalf("the shipped Cloudflare profile must stay SQLite (ephemeral): %q", sqlite.Storage.Dialect)
	}
	t.Setenv("GOVOICE_DATABASE_DSN", "postgres://switchboard:x@db.example.com:5432/switchboard?sslmode=require")
	postgres, err := LoadConfig("../deploy/cloudflare/cloudflare-demo-postgres.yaml")
	if err != nil {
		t.Fatalf("the Cloudflare Postgres profile does not load: %v", err)
	}
	if postgres.Storage.Dialect != "postgres" {
		t.Fatalf("the Cloudflare demo profile must be Postgres: %q", postgres.Storage.Dialect)
	}
	if len(postgres.TLS.TrustedProxies) != 2 || !postgres.PublicAdmin.Enabled {
		t.Fatalf("the Cloudflare profiles must stay one-port Worker-fronted: %+v", postgres.PublicAdmin)
	}
}
