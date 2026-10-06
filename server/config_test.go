package server

import (
	"os"
	"path/filepath"
	"strings"
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
			name: "no agent store configured",
			modify: func(c *Config) {
				c.Storage.Dialect = ""
			},
			wantErr: "storage.dialect is required",
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

// YAML pipelines and per-provider models/credentials were removed: loading a
// config that still has them fails with an actionable message.
func TestConfig_RejectsRemovedPipelineAndAgentKeys(t *testing.T) {
	base, err := os.ReadFile("../config/development.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ old, new, want string }{
		{"\nobservability:", "\npipelines:\n  default:\n    stt: google.stt\n\nobservability:", "`pipelines` is no longer supported"},
		{"\nobservability:", "\npipelines:\n\nobservability:", "`pipelines` is no longer supported"},
		{"\nobservability:", "\notel:\n  enabled: true\n\nobservability:", "`otel` is not supported"},
		{"\n  google_tts:\n", "\n  google_llm:\n    model: gemini\n  google_tts:\n", "providers.google_llm.model is no longer supported"},
	} {
		doc := strings.Replace(string(base), tc.old, tc.new, 1)
		path := filepath.Join(t.TempDir(), "cfg.yaml")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q: err=%v, want %q", tc.new, err, tc.want)
		}
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
	if cfg.Storage.Dialect == "" {
		t.Fatalf("development config must enable the agent store")
	}
}

func TestConfig_LoadProductionYAML(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "production-test-secret-not-the-dev-key-123456")
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

func TestResolveTokenSecret_ProductionRejectsPublicKey(t *testing.T) {
	cfg := DefaultConfig(ProfileProduction)
	cfg.Token.Secret = DevInsecureTokenSecret
	if _, err := ResolveTokenSecret(cfg); err == nil || !strings.Contains(err.Error(), "repository-public") {
		t.Fatalf("expected public development secret to be refused, got %v", err)
	}
	cfg.Token.Secret = "short"
	if _, err := ResolveTokenSecret(cfg); err == nil || !strings.Contains(err.Error(), "32") {
		t.Fatalf("expected short secret to be refused, got %v", err)
	}
	cfg.Token.Secret = "production-test-secret-not-the-dev-key-123456"
	got, err := ResolveTokenSecret(cfg)
	if err != nil || got != cfg.Token.Secret {
		t.Fatalf("expected the configured secret, got %q %v", got, err)
	}
}

func TestConfig_Retention(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)

	if cfg.Retention.MetadataRetentionDays != 30 {
		t.Errorf("expected default metadata retention 30 days, got %d", cfg.Retention.MetadataRetentionDays)
	}
	if cfg.Retention.ContentRetentionDays != 7 {
		t.Errorf("expected default content retention 7 days, got %d", cfg.Retention.ContentRetentionDays)
	}
	if cfg.Retention.BatchSize <= 0 {
		t.Errorf("expected positive retention batch size, got %d", cfg.Retention.BatchSize)
	}

	// Validation tests
	invalidRetention := DefaultConfig(ProfileDevelopment)
	invalidRetention.Retention.Enabled = true
	invalidRetention.Retention.MetadataRetentionDays = -1
	if err := ValidateConfig(invalidRetention); err == nil {
		t.Errorf("expected error for negative metadata retention days")
	}

	invalidContent := DefaultConfig(ProfileDevelopment)
	invalidContent.Retention.Enabled = true
	invalidContent.Retention.MetadataRetentionDays = 7
	invalidContent.Retention.ContentRetentionDays = 30
	if err := ValidateConfig(invalidContent); err == nil {
		t.Errorf("expected error when content retention exceeds metadata retention")
	}
}
