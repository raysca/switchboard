package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	// Admission Token
	Token struct {
		SecretEnv        string        `yaml:"secret_env"` // Env var holding HS256 secret (default "TOKEN_SECRET")
		Secret           string        `yaml:"secret"`     // Direct secret (dev only)
		Issuer           string        `yaml:"issuer"`
		Audience         string        `yaml:"audience"`
		MaxLifetime      time.Duration `yaml:"max_lifetime"`
		ClockSkew        time.Duration `yaml:"clock_skew"`
		ReplayCacheLimit int           `yaml:"replay_cache_limit"`
	} `yaml:"token"`

	// Limits & Admission
	Limits struct {
		MaxPendingHandshakes    int           `yaml:"max_pending_handshakes"`
		MaxSessions             int           `yaml:"max_sessions"`
		MaxSessionsPerPrincipal int           `yaml:"max_sessions_per_principal"`
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
	Providers struct {
		GoogleSTT struct {
			ProjectID                  string        `yaml:"project_id"`
			Location                   string        `yaml:"location"`
			Model                      string        `yaml:"model"`
			LanguageCode               string        `yaml:"language_code"`
			MaxStreams                 int           `yaml:"max_streams"`
			RotationThreshold          time.Duration `yaml:"rotation_threshold"`
			RotationHeadroomMultiplier float64       `yaml:"rotation_headroom_multiplier"`
			CredentialsFile            string        `yaml:"credentials_file"`
		} `yaml:"google_stt"`

		GoogleLLM struct {
			Model        string `yaml:"model"`
			AuthMode     string `yaml:"auth_mode"` // "vertex_adc" | "gemini_api_key"
			SystemPrompt string `yaml:"system_prompt"`
			APIKeyEnv    string `yaml:"api_key_env"`
		} `yaml:"google_llm"`

		GoogleTTS struct {
			Voice           string `yaml:"voice"`
			SampleRate      int    `yaml:"sample_rate"`
			CredentialsFile string `yaml:"credentials_file"`
		} `yaml:"google_tts"`

		XAILLM struct {
			Model        string `yaml:"model"`
			Endpoint     string `yaml:"endpoint"`
			APIKeyEnv    string `yaml:"api_key_env"`
			SystemPrompt string `yaml:"system_prompt"`
		} `yaml:"xai_llm"`
	} `yaml:"providers"`

	// Pipeline Definitions
	Pipelines map[string]PipelineConfig `yaml:"pipelines"`

	// Observability
	Observability struct {
		LogLevel                string        `yaml:"log_level"`
		JSONFormat              bool          `yaml:"json_format"`
		EnableTranscriptLogging bool          `yaml:"enable_transcript_logging"`
		TimelineMaxEvents       int           `yaml:"timeline_max_events"`
		TimelineTTL             time.Duration `yaml:"timeline_ttl"`
	} `yaml:"observability"`
}

// PipelineConfig defines composition of a named pipeline.
type PipelineConfig struct {
	STT         string `yaml:"stt"` // e.g. "google.stt"
	LLM         string `yaml:"llm"` // e.g. "google.llm" or "xai.llm"
	TTS         string `yaml:"tts"` // e.g. "google.tts"
	MaxMessages int    `yaml:"max_messages"`
	MaxTokens   int    `yaml:"max_tokens"`
}

// DefaultConfig returns a production-ready configuration with safe defaults.
func DefaultConfig(profile Profile) *Config {
	cfg := &Config{
		Profile:          profile,
		PublicListenAddr: "127.0.0.1:8080",
		AdminListenAddr:  "127.0.0.1:9090",
		OriginAllowlist:  []string{"http://localhost:3000", "https://localhost:3000", "*"},
		Pipelines:        make(map[string]PipelineConfig),
	}

	// Token defaults
	cfg.Token.SecretEnv = "TOKEN_SECRET"
	cfg.Token.Issuer = "govoice"
	cfg.Token.Audience = "govoice-rtvi"
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
	cfg.Providers.GoogleSTT.Model = "chirp_2"
	cfg.Providers.GoogleSTT.Location = "us-central1"
	cfg.Providers.GoogleSTT.LanguageCode = "en-US"
	cfg.Providers.GoogleSTT.MaxStreams = 100
	cfg.Providers.GoogleSTT.RotationThreshold = 240 * time.Second
	cfg.Providers.GoogleSTT.RotationHeadroomMultiplier = 2.0

	cfg.Providers.GoogleLLM.Model = "gemini-2.5-flash"
	cfg.Providers.GoogleLLM.AuthMode = "vertex_adc"
	cfg.Providers.GoogleLLM.APIKeyEnv = "GOOGLE_API_KEY"

	cfg.Providers.GoogleTTS.Voice = "en-US-Journey-F"
	cfg.Providers.GoogleTTS.SampleRate = 24000

	cfg.Providers.XAILLM.Model = "grok-2-1212"
	cfg.Providers.XAILLM.Endpoint = "https://api.x.ai/v1/responses"
	cfg.Providers.XAILLM.APIKeyEnv = "XAI_API_KEY"

	// Default pipeline
	cfg.Pipelines["default"] = PipelineConfig{
		STT:         "google.stt",
		LLM:         "google.llm",
		TTS:         "google.tts",
		MaxMessages: 20,
		MaxTokens:   4000,
	}
	cfg.Pipelines["xai"] = PipelineConfig{
		STT:         "google.stt",
		LLM:         "xai.llm",
		TTS:         "google.tts",
		MaxMessages: 20,
		MaxTokens:   4000,
	}

	// Observability
	cfg.Observability.LogLevel = "info"
	cfg.Observability.EnableTranscriptLogging = false
	cfg.Observability.TimelineMaxEvents = 500
	cfg.Observability.TimelineTTL = 10 * time.Minute

	return cfg
}

// LoadConfig parses and validates YAML configuration from disk.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
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
		if cfg.VAD.Mode == "inprocess" && !cfg.InsecureOverrides.AllowPlainWSInProduction {
			return errors.New("production profile requires VAD mode 'process' (worker isolation)")
		}
	}

	// Credential file security permissions check
	if cfg.Profile == ProfileProduction && !cfg.InsecureOverrides.SkipCredentialPermsCheck {
		credFiles := []string{
			cfg.Providers.GoogleSTT.CredentialsFile,
			cfg.Providers.GoogleTTS.CredentialsFile,
		}
		for _, file := range credFiles {
			if file != "" {
				if err := checkFileSecurity(file); err != nil {
					return fmt.Errorf("credential file %s security check failed: %w", file, err)
				}
			}
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

	// Pipeline validation
	if len(cfg.Pipelines) == 0 {
		return errors.New("at least one pipeline must be configured")
	}
	for name, p := range cfg.Pipelines {
		if p.STT == "" || p.LLM == "" || p.TTS == "" {
			return fmt.Errorf("pipeline %q must specify stt, llm, and tts providers", name)
		}
		if p.MaxMessages <= 0 {
			return fmt.Errorf("pipeline %q max_messages must be > 0", name)
		}
		if p.MaxTokens <= 0 {
			return fmt.Errorf("pipeline %q max_tokens must be > 0", name)
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

func checkFileSecurity(filePath string) error {
	cleanPath := filepath.Clean(filePath)
	info, err := os.Stat(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file does not exist: %s", cleanPath)
		}
		return err
	}
	mode := info.Mode().Perm()
	// Refuse group- or world-readable (mask 0077)
	if mode&0077 != 0 {
		return fmt.Errorf("file permissions %#o are insecure; must not be group- or world-accessible (e.g. 0600 or 0400)", mode)
	}
	return nil
}
