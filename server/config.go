package server

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Profile represents development or production deployment profile.
type Profile string

const (
	ProfileDevelopment Profile = "development"
	ProfileProduction  Profile = "production"
)

// InsecureOverrides allows explicit bypassing of production safety checks.
type InsecureOverrides struct {
	AllowPlainWSInProduction bool `yaml:"allow_plain_ws_in_production"`
	SkipCredentialPermsCheck bool `yaml:"skip_credential_perms_check"`
}

// Config represents complete server configuration matching spec §28.
type Config struct {
	Profile           Profile           `yaml:"profile"`
	InsecureOverrides InsecureOverrides `yaml:"insecure_overrides"`

	// Network listeners
	PublicListenAddr string `yaml:"public_listen_addr"` // e.g. "127.0.0.1:8080"
	AdminListenAddr  string `yaml:"admin_listen_addr"`  // e.g. "127.0.0.1:9090"

	// TLS & Network Policy
	TLS struct {
		CertFile         string   `yaml:"cert_file"`
		KeyFile          string   `yaml:"key_file"`
		TrustedProxyMode bool     `yaml:"trusted_proxy_mode"`
		TrustedProxies   []string `yaml:"trusted_proxies"`
	} `yaml:"tls"`

	OriginAllowlist []string `yaml:"origin_allowlist"`
	Management      struct {
		APIKeyEnv string `yaml:"api_key_env"`
	} `yaml:"management"`

	// Durable management plane (agents, provider credentials, MCP, session
	// history). Disabled when Storage.Dialect is empty; retention: see Retention.
	Storage struct {
		Dialect string `yaml:"dialect"` // "sqlite" | "postgres"
		DSNEnv  string `yaml:"dsn_env"` // env var holding the DSN (default GOVOICE_DATABASE_DSN)
	} `yaml:"storage"`
	Credentials struct {
		KeyringEnv string `yaml:"keyring_env"` // env var: "id:base64key[,id:base64key]", first is active (default GOVOICE_CREDENTIAL_KEYS)
	} `yaml:"credentials"`

	// Admission Token
	Token struct {
		SecretEnv        string        `yaml:"secret_env"` // Env var holding HS256 secret (default "TOKEN_SECRET")
		Secret           string        `yaml:"secret"`     // Direct secret (dev only)
		Issuer           string        `yaml:"issuer"`
		Audience         string        `yaml:"audience"`
		MaxLifetime      time.Duration `yaml:"max_lifetime"`
		ClockSkew        time.Duration `yaml:"clock_skew"`
		ReplayCacheLimit int           `yaml:"replay_cache_limit"`
		KeyID            string        `yaml:"key_id"` // Expected kid header in production (default "v1")
	} `yaml:"token"`

	// Limits & Admission
	Limits struct {
		MaxPendingHandshakes    int           `yaml:"max_pending_handshakes"`
		MaxSessions             int           `yaml:"max_sessions"`
		MaxSessionsPerPrincipal int           `yaml:"max_sessions_per_principal"`
		MaxSessionsPerIP        int           `yaml:"max_sessions_per_ip"`
		UpgradeTimeout          time.Duration `yaml:"upgrade_timeout"`
		ReadyTimeout            time.Duration `yaml:"ready_timeout"`
		MaxCallDuration         time.Duration `yaml:"max_call_duration"`
		IdleTimeout             time.Duration `yaml:"idle_timeout"`
		DrainTimeout            time.Duration `yaml:"drain_timeout"`
		ImmediateCancel         bool          `yaml:"immediate_cancel"`
	} `yaml:"limits"`

	// VAD Configuration
	VAD struct {
		Mode       string `yaml:"mode"` // "process" | "inprocess"
		Workers    int    `yaml:"workers"`
		Batch      int    `yaml:"batch"`
		ModelPath  string `yaml:"model_path"`
		OrtLibPath string `yaml:"ort_lib_path"`
		QueueCap   int    `yaml:"queue_cap"`
	} `yaml:"vad"`

	// Provider Configurations
	// Provider infrastructure only. What a call runs (models, voices, prompts)
	// and its credentials come from the stored agent, never from YAML.
	Providers struct {
		GoogleSTT struct {
			ProjectID                  string        `yaml:"project_id"`
			Location                   string        `yaml:"location"`
			MaxStreams                 int           `yaml:"max_streams"`
			RotationThreshold          time.Duration `yaml:"rotation_threshold"`
			RotationHeadroomMultiplier float64       `yaml:"rotation_headroom_multiplier"`
		} `yaml:"google_stt"`

		GoogleTTS struct {
			SampleRate int `yaml:"sample_rate"`
		} `yaml:"google_tts"`

		XAILLM struct {
			Endpoint string `yaml:"endpoint"`
		} `yaml:"xai_llm"`

		XAISTT struct {
			Endpoint string `yaml:"endpoint"`
		} `yaml:"xai_stt"`

		XAITTS struct {
			Endpoint string `yaml:"endpoint"`
		} `yaml:"xai_tts"`
	} `yaml:"providers"`

	// Observability
	Observability struct {
		LogLevel          string        `yaml:"log_level"`
		JSONFormat        bool          `yaml:"json_format"`
		TimelineMaxEvents int           `yaml:"timeline_max_events"`
		TimelineTTL       time.Duration `yaml:"timeline_ttl"`
	} `yaml:"observability"`

	// Retention
	Retention RetentionConfig `yaml:"retention"`

	// OpenTelemetry
}

// RetentionConfig defines trace and session retention policy.
type RetentionConfig struct {
	Enabled               bool          `yaml:"enabled"`
	Interval              time.Duration `yaml:"interval"`
	MetadataRetentionDays int           `yaml:"metadata_retention_days"`
	ContentRetentionDays  int           `yaml:"content_retention_days"`
	BatchSize             int           `yaml:"batch_size"`
}

// DefaultConfig returns a production-ready configuration with safe defaults.
func DefaultConfig(profile Profile) *Config {
	cfg := &Config{
		Profile:          profile,
		PublicListenAddr: "127.0.0.1:8080",
		AdminListenAddr:  "127.0.0.1:9090",
		OriginAllowlist:  []string{"http://localhost:3000", "https://localhost:3000", "*"},
	}

	// Token defaults
	cfg.Storage.Dialect = "sqlite" // DSN comes from GOVOICE_DATABASE_DSN when enabled
	cfg.Token.SecretEnv = "TOKEN_SECRET"
	cfg.Management.APIKeyEnv = "GOVOICE_MANAGEMENT_API_KEY"
	cfg.Token.Issuer = "govoice-v1"
	cfg.Token.Audience = "govoice-v1-client"
	cfg.Token.KeyID = "v1"
	cfg.Token.MaxLifetime = 5 * time.Minute
	cfg.Token.ClockSkew = 30 * time.Second
	cfg.Token.ReplayCacheLimit = 10000

	// Limits
	cfg.Limits.MaxPendingHandshakes = 50
	cfg.Limits.MaxSessions = 100
	cfg.Limits.MaxSessionsPerPrincipal = 5
	cfg.Limits.UpgradeTimeout = 5 * time.Second
	cfg.Limits.ReadyTimeout = 10 * time.Second
	cfg.Limits.MaxCallDuration = 30 * time.Minute
	cfg.Limits.IdleTimeout = 60 * time.Second
	cfg.Limits.DrainTimeout = 30 * time.Second

	// VAD defaults based on profile
	if profile == ProfileProduction {
		cfg.VAD.Mode = "process"
		cfg.Observability.JSONFormat = true
	} else {
		cfg.VAD.Mode = "inprocess"
		cfg.Observability.JSONFormat = false
	}
	cfg.VAD.Workers = 4
	cfg.VAD.Batch = 8
	cfg.VAD.QueueCap = 8192
	cfg.VAD.ModelPath = "third_party/silero_vad.onnx"

	// Providers
	cfg.Providers.GoogleSTT.Location = "us-central1"
	cfg.Providers.GoogleSTT.MaxStreams = 100
	cfg.Providers.GoogleSTT.RotationThreshold = 240 * time.Second
	cfg.Providers.GoogleSTT.RotationHeadroomMultiplier = 2.0

	cfg.Providers.GoogleTTS.SampleRate = 24000

	cfg.Providers.XAILLM.Endpoint = "https://api.x.ai/v1/responses"

	cfg.Providers.XAISTT.Endpoint = "wss://api.x.ai/v1/stt"

	cfg.Providers.XAITTS.Endpoint = "https://api.x.ai/v1/tts"

	// Default pipeline

	// Observability
	cfg.Observability.LogLevel = "info"
	cfg.Observability.TimelineMaxEvents = 500
	cfg.Observability.TimelineTTL = 10 * time.Minute

	// Retention defaults (sane defaults)
	cfg.Retention.Enabled = false
	cfg.Retention.Interval = 1 * time.Hour
	cfg.Retention.MetadataRetentionDays = 30
	cfg.Retention.ContentRetentionDays = 7
	cfg.Retention.BatchSize = 500

	return cfg
}

// LoadConfig parses and validates YAML configuration from disk.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	if err := rejectRemovedKeys(data); err != nil {
		return nil, err
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // Reject unknown fields per spec §28 & Phase 8 checklist
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}

	if err := ValidateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

// ValidateConfig enforces production safety and constraint validation.
func ValidateConfig(cfg *Config) error {
	if cfg.Profile != ProfileDevelopment && cfg.Profile != ProfileProduction {
		return fmt.Errorf("invalid profile %q: must be 'development' or 'production'", cfg.Profile)
	}
	if cfg.Token.MaxLifetime != 0 && cfg.Token.MaxLifetime < 2*time.Second {
		return errors.New("token.max_lifetime must be at least 2 seconds when configured")
	}

	switch cfg.Storage.Dialect {
	case "", "sqlite", "postgres": // empty is reported below as required
	default:
		return fmt.Errorf("invalid storage.dialect %q: must be 'sqlite' or 'postgres'", cfg.Storage.Dialect)
	}
	if cfg.PublicListenAddr == "" {
		return errors.New("public_listen_addr must be specified")
	}
	if cfg.AdminListenAddr == "" {
		return errors.New("admin_listen_addr must be specified")
	}
	if cfg.PublicListenAddr == cfg.AdminListenAddr {
		return fmt.Errorf("public_listen_addr (%s) and admin_listen_addr (%s) must be separate listeners",
			cfg.PublicListenAddr, cfg.AdminListenAddr)
	}

	// Production TLS requirements
	if cfg.Profile == ProfileProduction {
		hasTLS := cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != ""
		hasProxy := cfg.TLS.TrustedProxyMode && len(cfg.TLS.TrustedProxies) > 0
		if cfg.TLS.TrustedProxyMode && len(cfg.TLS.TrustedProxies) == 0 {
			return errors.New("production trusted_proxy_mode requires tls.trusted_proxies")
		}
		if !hasTLS && !hasProxy && !cfg.InsecureOverrides.AllowPlainWSInProduction {
			return errors.New("production profile requires TLS cert/key or trusted_proxy_mode enabled (or explicit insecure override allow_plain_ws_in_production)")
		}
		if _, err := ResolveTokenSecret(cfg); err != nil {
			return err
		}

		// Production VAD default
		if cfg.VAD.Mode == "inprocess" {
			return errors.New("production profile requires VAD mode 'process' (worker isolation)")
		}
	}

	// Limits validation
	if cfg.Limits.MaxPendingHandshakes <= 0 {
		return errors.New("max_pending_handshakes must be > 0")
	}
	if cfg.Limits.MaxSessions <= 0 {
		return errors.New("max_sessions must be > 0")
	}
	if cfg.Limits.MaxSessionsPerPrincipal <= 0 {
		return errors.New("max_sessions_per_principal must be > 0")
	}
	if cfg.Limits.MaxCallDuration <= 0 {
		return errors.New("max_call_duration must be > 0")
	}

	// Calls run stored agents only: the agent store is mandatory.
	if cfg.Storage.Dialect == "" {
		return errors.New("storage.dialect is required: calls run stored agents (set sqlite or postgres)")
	}

	// Retention validation
	if cfg.Retention.Enabled {
		if cfg.Retention.MetadataRetentionDays < 0 {
			return errors.New("retention.metadata_retention_days must be >= 0")
		}
		if cfg.Retention.ContentRetentionDays < 0 {
			return errors.New("retention.content_retention_days must be >= 0")
		}
		if cfg.Retention.MetadataRetentionDays > 0 && cfg.Retention.ContentRetentionDays > cfg.Retention.MetadataRetentionDays {
			return errors.New("retention.content_retention_days cannot exceed retention.metadata_retention_days")
		}
		if cfg.Retention.BatchSize < 0 {
			return errors.New("retention.batch_size must be >= 0")
		}
	}

	return nil
}

const DevInsecureTokenSecret = "dev-insecure-secret-key-for-testing-only-12345"

// ResolveTokenSecret returns the HMAC secret. Production refuses an empty
// secret and the repository-public development key.
func ResolveTokenSecret(cfg *Config) (string, error) {
	secret := cfg.Token.Secret
	if secret == "" && cfg.Token.SecretEnv != "" {
		secret = os.Getenv(cfg.Token.SecretEnv)
	}
	if cfg.Profile == ProfileProduction {
		if secret == "" {
			return "", errors.New("production profile requires token.secret or token.secret_env")
		}
		if secret == DevInsecureTokenSecret {
			return "", errors.New("production profile refuses the repository-public development token secret")
		}
		if len(secret) < 32 {
			return "", errors.New("production token secret must be at least 32 characters")
		}
		return secret, nil
	}
	if secret == "" {
		secret = DevInsecureTokenSecret
	}
	return secret, nil
}

// rejectRemovedKeys turns keys removed with YAML pipelines into an actionable
// error instead of a generic unknown-field failure.
func rejectRemovedKeys(data []byte) error {
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil // the strict decode reports syntax errors
	}
	var doc struct {
		Providers map[string]map[string]any `yaml:"providers"`
	}
	_ = yaml.Unmarshal(data, &doc)
	if _, ok := top["otel"]; ok {
		return errors.New("config: `otel` is not supported; there is no trace exporter")
	}
	if _, ok := top["pipelines"]; ok {
		return errors.New("config: `pipelines` is no longer supported; calls run stored agents. Create agents via /api/v1/agents and remove the pipelines section")
	}
	for provider, settings := range doc.Providers {
		for _, key := range []string{"model", "language_code", "language", "voice", "system_prompt", "api_key_env", "credentials_file", "auth_mode"} {
			if _, ok := settings[key]; ok {
				return fmt.Errorf("config: providers.%s.%s is no longer supported; models, voices, prompts and credentials belong to stored agents and provider credentials", provider, key)
			}
		}
	}
	return nil
}
