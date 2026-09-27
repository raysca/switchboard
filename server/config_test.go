package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfig_Defaults(t *testing.T) {
	devCfg := DefaultConfig(ProfileDevelopment)
	if devCfg.Profile != ProfileDevelopment {
		t.Fatalf("expected profile development, got %s", devCfg.Profile)
	}
	if devCfg.VAD.Mode != "inprocess" {
		t.Fatalf("expected VAD mode inprocess for dev, got %s", devCfg.VAD.Mode)
	}
	if devCfg.Observability.JSONFormat != false {
		t.Fatalf("expected JSON format false for dev")
	}

	prodCfg := DefaultConfig(ProfileProduction)
	if prodCfg.Profile != ProfileProduction {
		t.Fatalf("expected profile production, got %s", prodCfg.Profile)
	}
	if prodCfg.VAD.Mode != "process" {
		t.Fatalf("expected VAD mode process for prod, got %s", prodCfg.VAD.Mode)
	}
	if prodCfg.Observability.JSONFormat != true {
		t.Fatalf("expected JSON format true for prod")
	}
}

func TestConfig_UnknownFieldsRejection(t *testing.T) {
	yamlWithUnknown := `
profile: development
public_listen_addr: "127.0.0.1:8080"
admin_listen_addr: "127.0.0.1:9090"
unknown_field_foo_bar: 12345
limits:
  max_sessions: 10
`
	tmpFile := filepath.Join(t.TempDir(), "unknown.yaml")
	if err := os.WriteFile(tmpFile, []byte(yamlWithUnknown), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(tmpFile)
	if err == nil {
		t.Fatal("expected error on unknown config field, got nil")
	}
}

func TestConfig_ValidationFailures(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(cfg *Config)
		wantErr string
	}{
		{
			name: "invalid profile",
			modify: func(c *Config) {
				c.Profile = "staging"
			},
			wantErr: "invalid profile",
		},
		{
			name: "missing public listen addr",
			modify: func(c *Config) {
				c.PublicListenAddr = ""
			},
			wantErr: "public_listen_addr must be specified",
		},
		{
			name: "identical public and admin listen addrs",
			modify: func(c *Config) {
				c.PublicListenAddr = "127.0.0.1:8080"
				c.AdminListenAddr = "127.0.0.1:8080"
			},
			wantErr: "must be separate listeners",
		},
		{
			name: "production profile without TLS or proxy",
			modify: func(c *Config) {
				c.Profile = ProfileProduction
				c.TLS.CertFile = ""
				c.TLS.KeyFile = ""
				c.TLS.TrustedProxyMode = false
				c.InsecureOverrides.AllowPlainWSInProduction = false
			},
			wantErr: "production profile requires TLS cert/key or trusted_proxy_mode",
		},
		{
			name: "invalid limits max sessions",
			modify: func(c *Config) {
				c.Limits.MaxSessions = 0
			},
			wantErr: "max_sessions must be > 0",
		},
		{
			name: "no pipelines configured",
			modify: func(c *Config) {
				c.Pipelines = nil
			},
			wantErr: "at least one pipeline must be configured",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig(ProfileDevelopment)
			tt.modify(cfg)
			err := ValidateConfig(cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
		})
	}
}

func TestConfig_ProductionFileSecurityCheck(t *testing.T) {
	tmpDir := t.TempDir()
	credPath := filepath.Join(tmpDir, "insecure_creds.json")

	// Create world-readable file (mode 0644)
	if err := os.WriteFile(credPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig(ProfileProduction)
	cfg.TLS.TrustedProxyMode = true
	cfg.Providers.GoogleSTT.CredentialsFile = credPath
	cfg.InsecureOverrides.SkipCredentialPermsCheck = false

	err := ValidateConfig(cfg)
	if err == nil {
		t.Fatal("expected failure on group/world-readable credential file in production profile")
	}

	// Change to secure mode 0600
	if err := os.Chmod(credPath, 0600); err != nil {
		t.Fatal(err)
	}

	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("expected success with 0600 permissions, got: %v", err)
	}
}

func TestConfig_LoadDevelopmentYAML(t *testing.T) {
	cfg, err := LoadConfig("../config/development.yaml")
	if err != nil {
		t.Fatalf("load development.yaml: %v", err)
	}
	if cfg.Profile != ProfileDevelopment {
		t.Fatalf("expected profile development, got %s", cfg.Profile)
	}
	if cfg.Pipelines["default"].STT != "google.stt" {
		t.Fatalf("expected default pipeline google.stt")
	}
	if cfg.Pipelines["xai"].LLM != "xai.llm" {
		t.Fatalf("expected xai pipeline xai.llm")
	}
}

func TestConfig_LoadProductionYAML(t *testing.T) {
	cfg, err := LoadConfig("../config/production.yaml")
	if err != nil {
		t.Fatalf("load production.yaml: %v", err)
	}
	if cfg.Profile != ProfileProduction {
		t.Fatalf("expected profile production, got %s", cfg.Profile)
	}
	if cfg.VAD.Mode != "process" {
		t.Fatalf("expected production VAD mode process")
	}
	if cfg.Limits.MaxCallDuration != 60*time.Minute {
		t.Fatalf("expected max call duration 60m")
	}
}
