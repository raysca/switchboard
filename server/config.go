package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"govoice/core/redact"
	coreturn "govoice/core/turn"
	"govoice/server/otelexport"

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
		MaxSessionsPerAgent     int           `yaml:"max_sessions_per_agent"`    // 0 = unlimited
		StartRatePerPrincipal   float64       `yaml:"start_rate_per_principal"`  // session starts/sec per principal; 0 = unlimited
		StartBurstPerPrincipal  int           `yaml:"start_burst_per_principal"` // bucket size; required >= 1 when rate > 0
		HandshakeRatePerIP      float64       `yaml:"handshake_rate_per_ip"`     // WebSocket upgrades/sec per source IP (burst = max(5, 2x)); default 20
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

	// STT tunes failover for agents with STT fallbacks.
	STT struct {
		// ReplayBuffer is the audio kept so a failover can replay the utterance
		// in progress (default 10s, max 60s; 16 kHz PCM16 mono, ~32 KB/s per call).
		ReplayBuffer time.Duration `yaml:"replay_buffer"`
	} `yaml:"stt"`

	// Failover tunes how long an agent stage with fallbacks waits on its
	// primary (see docs/operations_runbook.md). Zero means the default.
	Failover struct {
		// LLMBudget: no fallback is started later than this after the
		// generation began (default 60s).
		LLMBudget time.Duration `yaml:"llm_budget"`
		// LLMFirstToken: a primary LLM silent this long is abandoned for the
		// fallback; a generation that has started is never cut (default 8s).
		LLMFirstToken time.Duration `yaml:"llm_first_token"`
	} `yaml:"failover"`

	// Turn routes live calls through the turn manager (core/turn.Processor).
	// Off by default: user turns then end on raw VAD stop as before.
	Turn struct {
		Manager  bool `yaml:"manager"`
		MinWords int  `yaml:"min_words"` // >0: barge-in needs this many words while the bot speaks
		// Stop is the any-stop chain that ends a user turn (first decisive wins):
		// speech-timeout | eager; a non-empty chain must include speech-timeout.
		// Empty keeps the built-in silence timeout. SpeechTimeout overrides its
		// 700ms default. "eager" ends the turn after EagerGrace (default 200ms)
		// of silence once the final transcript ends a sentence. There is no
		// retraction after that: "Book a table. For two." becomes two user
		// turns and the second barges in on the reply to the first. Only use
		// it with punctuating STT and when latency beats occasional false cuts.
		Stop []string `yaml:"stop"`
		// Completion tunes the "llm-completion" entry of Stop (ignored otherwise):
		// the model opens each reply with a completion marker; an incomplete
		// turn is held silently for short_wait (default 5s) or long_wait
		// (default 10s), then the model is re-prompted (nudge overrides the
		// default wording, prompt the protocol instructions appended to the
		// system prompt). Costs prompt tokens on every turn and relies on the
		// model following the protocol; unmarked replies are spoken as-is.
		Completion    CompletionConfig `yaml:"completion"`
		SpeechTimeout time.Duration    `yaml:"speech_timeout"`
		EagerGrace    time.Duration    `yaml:"eager_grace"`
		// Mute lists user mute strategies (always, first_speech,
		// until_first_bot_complete, function_call, critical_output). While any
		// is muting, VAD starts/stops and transcripts are dropped (audio still
		// flows to VAD/STT/recording): no turn starts, no barge-in, nothing
		// reaches the context. Each strategy's continuous mute is capped at
		// MaxMute (default 30s) so a lost signal cannot silence the user.
		// until_first_bot_complete mutes until the bot first speaks, so use it
		// only with an opening greeting (the cap is the only other release).
		Mute    []string      `yaml:"mute"`
		MaxMute time.Duration `yaml:"max_mute"`
		// Analyzer configures the Smart Turn v3 model behind turn.stop:
		// [turn-analyzer, speech-timeout]. The model ends a turn as soon as it
		// judges the user done; "incomplete" holds the turn up to StopSecs of
		// silence (Pipecat stop_secs, default 3s). When the model cannot answer
		// (worker crash, overload, no verdict within Budget) the turn ends at
		// speech_timeout like before and a fallback metric is counted. Mode ""
		// follows vad.mode (process = crash-isolated worker; production needs it).
		Analyzer struct {
			Mode      string        `yaml:"mode"` // "" | process | inprocess
			Workers   int           `yaml:"workers"`
			ModelPath string        `yaml:"model_path"`
			StopSecs  time.Duration `yaml:"stop_secs"`
			Budget    time.Duration `yaml:"budget"` // per-verdict latency budget, default 150ms
		} `yaml:"analyzer"`
	} `yaml:"turn"`

	// FirstClauseChars > 0 lets the first synthesis unit of a reply end at a
	// clause boundary (comma, semicolon, colon, dash) once it has this many
	// characters, so audio starts before the first sentence is complete, at
	// some cost to prosody. 0 (default) splits on sentences only. Try 40.
	FirstClauseChars int `yaml:"first_clause_chars"`

	// Audio configures inbound audio conditioning. InputFilters runs in the
	// listed order between the DTMF detector (which hears unfiltered audio)
	// and VAD; empty (default) leaves the pipeline unchanged. Known filters:
	// denoise. DenoiseStrength is 0..1; 0 means the default (0.5). Recordings
	// tap the transport and keep the raw audio. Per-agent filter settings are
	// not supported yet: this is server-wide.
	Audio struct {
		InputFilters    []string `yaml:"input_filters"`
		DenoiseStrength float64  `yaml:"denoise_strength"`
		// OutputGainDB scales the bot's audio, -30..+6 dB; 0 (default) inserts
		// no stage. Background mixes a looping WAV under the speech and keeps
		// playing in silences (continuous WS traffic, ~48 kB/s at 24 kHz); off
		// unless File is set. See docs/audio-filters.md.
		OutputGainDB float64 `yaml:"output_gain_db"`
		Background   struct {
			File   string   `yaml:"file"`
			GainDB *float64 `yaml:"gain_db"` // -60..0, default -30 when unset
			Loop   *bool    `yaml:"loop"`    // default true
		} `yaml:"background"`
	} `yaml:"audio"`

	// Idle: user-idle prompts/hang-up and the stuck-bot watchdog (see IdleConfig).
	Idle IdleConfig `yaml:"idle"`

	// DTMF is off by default. Detect finds keypad tones in inbound audio;
	// Aggregate turns key presses (detected, or sent by the client as RTVI dtmf
	// messages) into user messages for the LLM; SendTool gives the LLM a
	// send_dtmf tool to press keys toward the caller (IVR navigation).
	DTMF struct {
		Detect           bool          `yaml:"detect"`
		Aggregate        bool          `yaml:"aggregate"`
		SendTool         bool          `yaml:"send_tool"`
		AggregateTimeout time.Duration `yaml:"aggregate_timeout"` // idle flush; 0 = 2s
	} `yaml:"dtmf"`

	// Provider Configurations
	// Provider infrastructure only. What a call runs (models, voices, prompts)
	// and its credentials come from the stored agent, never from YAML.
	Providers struct {
		GoogleSTT struct {
			ProjectID         string        `yaml:"project_id"`
			Location          string        `yaml:"location"`
			RotationThreshold time.Duration `yaml:"rotation_threshold"`
		} `yaml:"google_stt"`

		GoogleTTS struct {
			SampleRate int `yaml:"sample_rate"`
		} `yaml:"google_tts"`

		XAILLM struct {
			Endpoint string `yaml:"endpoint"`
		} `yaml:"xai_llm"`

		// OpenAILLM base URLs come from stored credentials, so the server
		// refuses http and loopback/private/link-local targets (SSRF) unless
		// the operator opts in here for self-hosted vLLM. Metadata and
		// link-local addresses stay blocked regardless.
		OpenAILLM struct {
			AllowPrivateBaseURL bool `yaml:"allow_private_base_url"`
		} `yaml:"openai_llm"`

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

	// Recording is the global switch for call recording (default off); an
	// agent must also opt in. See docs/privacy.md.
	Recording RecordingConfig `yaml:"recording"`

	// Privacy holds the sink redaction policy (docs/privacy.md). DTMF digits
	// are masked at sinks by default; text rules are opt-in.
	Privacy struct {
		Redaction redact.Config `yaml:"redaction"`
	} `yaml:"privacy"`

	// OTel is optional OpenTelemetry trace export; off unless otel.enabled.
	OTel otelexport.Config `yaml:"otel"`

	// Warmup tunes provider connection reuse and pre-dialing (P2-9); see
	// warmup.go. The zero value is the safe default: shared transports on,
	// pre-dial off.
	Warmup WarmupConfig `yaml:"warmup"`

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

// RecordingConfig enables stereo call recordings written to a local directory.
type RecordingConfig struct {
	Enabled       bool          `yaml:"enabled"`
	Dir           string        `yaml:"dir"`            // created 0700
	RetentionDays int           `yaml:"retention_days"` // recordings older than this are deleted
	MaxDuration   time.Duration `yaml:"max_duration"`   // per call; 0 = 1h
	MaxBytes      int64         `yaml:"max_bytes"`      // per call; 0 = 256 MiB
}

// TurnAnalyzerMode is the effective smart-turn backend: process or inprocess.
func (c *Config) TurnAnalyzerMode() string {
	if m := c.Turn.Analyzer.Mode; m != "" {
		return m
	}
	if c.VAD.Mode == "inprocess" {
		return "inprocess"
	}
	return "process"
}

// DefaultConfig returns the base configuration LoadConfig decodes YAML over
// (the only production path; direct callers are tests). Its origin allowlist
// includes "*", so LoadConfig clears it, and the storage dialect, for the
// production profile: those must be set explicitly.
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

	cfg.Idle.MaxPrompts = 2

	// Limits
	cfg.Limits.MaxPendingHandshakes = 50
	cfg.Limits.MaxSessions = 100
	cfg.Limits.MaxSessionsPerPrincipal = 5
	cfg.Limits.HandshakeRatePerIP = 20
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
	// Model paths stay empty: the vad and smartturn packages resolve their
	// default artifacts (and GOVOICE_*_MODEL_PATH) themselves.
	cfg.Turn.Analyzer.Workers = 1
	cfg.Turn.Analyzer.StopSecs = 3 * time.Second
	cfg.Turn.Analyzer.Budget = 150 * time.Millisecond

	// Providers
	cfg.Providers.GoogleSTT.Location = "us-central1"
	cfg.Providers.GoogleSTT.RotationThreshold = 240 * time.Second

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
	cfg.Recording.RetentionDays = 7
	cfg.Recording.MaxDuration = time.Hour
	cfg.Recording.MaxBytes = 256 << 20

	cfg.STT.ReplayBuffer = 10 * time.Second
	cfg.Failover.LLMBudget, cfg.Failover.LLMFirstToken = defaultLLMFailoverBudget, defaultLLMFirstToken
	cfg.Idle.BotMaxStuck = 3
	cfg.DTMF.AggregateTimeout = 2 * time.Second
	cfg.Audio.DenoiseStrength = 0.5
	cfg.Warmup.Timeout = 3 * time.Second
	cfg.Warmup.SpareTTL = 10 * time.Second
	cfg.OTel.QueueSize = 2048
	cfg.OTel.ShutdownTimeout = 5 * time.Second
	// Not defaulted on purpose: turn.speech_timeout, eager_grace, max_mute and
	// completion waits (setting them would trip "needs turn.manager"; the turn
	// code owns those fallbacks), warmup.enabled and audio.background.gain_db/loop
	// (nil means default), otel.sampling_ratio (nil means 1).

	return cfg
}

// LoadConfig parses and validates YAML configuration from disk.
func LoadConfig(path string) (*Config, error) { return LoadConfigProfile(path, "") }

// LoadConfigProfile is LoadConfig with the YAML profile overridden (the
// -profile flag): the override picks the defaults and is what gets validated,
// so -profile production cannot inherit development defaults or skip checks.
func LoadConfigProfile(path string, profile Profile) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	if err := rejectRemovedKeys(data); err != nil {
		return nil, err
	}

	// Defaults depend on the profile, so read it first. yaml.v3 decodes into the
	// pre-populated struct, replacing only keys present (lists are replaced
	// whole, maps merge, an explicit zero overwrites).
	var head struct {
		Profile Profile `yaml:"profile"`
	}
	_ = yaml.Unmarshal(data, &head) // syntax errors are reported by the strict decode
	if profile != "" {
		head.Profile = profile
	}
	cfg := DefaultConfig(head.Profile)
	if head.Profile == ProfileProduction {
		cfg.OriginAllowlist, cfg.Storage.Dialect = nil, ""
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // Reject unknown fields per spec §28 & Phase 8 checklist
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}
	cfg.Profile = head.Profile

	if err := ValidateConfig(cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return cfg, nil
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
	if cfg.Limits.MaxSessionsPerAgent < 0 || cfg.Limits.StartRatePerPrincipal < 0 {
		return errors.New("max_sessions_per_agent and start_rate_per_principal must be >= 0")
	}
	if cfg.Limits.StartRatePerPrincipal > 0 && cfg.Limits.StartBurstPerPrincipal < 1 {
		return errors.New("start_burst_per_principal must be >= 1 when start_rate_per_principal > 0")
	}
	if cfg.Limits.HandshakeRatePerIP < 0 {
		return errors.New("handshake_rate_per_ip must be >= 0 (0 = default 20)")
	}
	if cfg.Limits.MaxCallDuration <= 0 {
		return errors.New("max_call_duration must be > 0")
	}
	if cfg.STT.ReplayBuffer < 0 || cfg.STT.ReplayBuffer > maxSTTReplay {
		return fmt.Errorf("stt.replay_buffer must be between 0 (default 10s) and %v", maxSTTReplay)
	}
	if err := validateLLMFailoverTiming(cfg.Failover.LLMBudget, cfg.Failover.LLMFirstToken); err != nil {
		return err
	}

	// Calls run stored agents only: the agent store is mandatory.
	if cfg.Storage.Dialect == "" {
		return errors.New("storage.dialect is required: calls run stored agents (set sqlite or postgres)")
	}

	if _, err := turnStopDetector(cfg.Turn.Stop, cfg.Turn.SpeechTimeout, cfg.Turn.EagerGrace, func(time.Duration) coreturn.UserTurnStopStrategy { return nil }); err != nil {
		return err
	}
	// Every TTS provider (and so every failover candidate) is built at this one
	// rate; these are the pcm rates Cartesia and ElevenLabs both offer.
	if r := cfg.Providers.GoogleTTS.SampleRate; r != 0 && !slices.Contains([]int{8000, 16000, 22050, 24000, 44100, 48000}, r) {
		return fmt.Errorf("providers.google_tts.sample_rate %d must be one of 8000, 16000, 22050, 24000, 44100, 48000 (the rates every TTS provider can output)", r)
	}
	if a := cfg.Turn.Analyzer; slices.Contains(cfg.Turn.Stop, "turn-analyzer") {
		if a.Mode != "" && a.Mode != "process" && a.Mode != "inprocess" {
			return fmt.Errorf("turn.analyzer.mode %q must be process or inprocess", a.Mode)
		}
		if a.StopSecs < 0 || a.Budget < 0 || a.Workers < 0 {
			return errors.New("turn.analyzer.stop_secs, budget and workers must be >= 0")
		}
		if cfg.Profile == ProfileProduction && cfg.TurnAnalyzerMode() != "process" {
			return errors.New("production profile requires turn.analyzer mode 'process' (worker isolation)")
		}
	}
	if err := cfg.Idle.validate(cfg.DTMF.Aggregate, cfg.DTMF.AggregateTimeout); err != nil {
		return err
	}
	if err := validateAudio(cfg); err != nil {
		return err
	}
	if cfg.FirstClauseChars < 0 {
		return errors.New("first_clause_chars must be >= 0")
	}
	if cfg.Turn.SpeechTimeout < 0 || cfg.Turn.EagerGrace < 0 {
		return errors.New("turn.speech_timeout and turn.eager_grace must be >= 0")
	}
	if _, err := coreturn.ParseMuteStrategies(cfg.Turn.Mute, nil); err != nil {
		return err
	}
	if k := cfg.Turn.Completion; k.ShortWait < 0 || k.LongWait < 0 {
		return errors.New("turn.completion.short_wait and long_wait must be >= 0")
	}
	if cfg.Turn.MaxMute < 0 {
		return errors.New("turn.max_mute must be >= 0")
	}
	if !cfg.Turn.Manager && (len(cfg.Turn.Mute) > 0 || cfg.Turn.MaxMute > 0) {
		return errors.New("turn.mute and turn.max_mute need turn.manager: true")
	}
	if !cfg.Turn.Manager && (len(cfg.Turn.Stop) > 0 || cfg.Turn.SpeechTimeout > 0 || cfg.Turn.EagerGrace > 0 || cfg.Turn.MinWords > 0 || cfg.Turn.Completion != (CompletionConfig{})) {
		return errors.New("turn.stop, turn.speech_timeout, turn.eager_grace and turn.min_words need turn.manager: true")
	}
	if cfg.Turn.Completion != (CompletionConfig{}) && !slices.Contains(cfg.Turn.Stop, "llm-completion") {
		return errors.New("turn.completion needs llm-completion in turn.stop")
	}

	if err := cfg.Privacy.Redaction.Validate(); err != nil {
		return err
	}
	if err := cfg.OTel.Validate(); err != nil {
		return err
	}
	if err := cfg.Warmup.Validate(); err != nil {
		return err
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

	if rc := cfg.Recording; rc.Enabled {
		if rc.Dir == "" {
			return errors.New("recording.dir is required when recording.enabled")
		}
		if rc.RetentionDays <= 0 {
			return errors.New("recording.retention_days must be > 0 when recording.enabled")
		}
		if rc.MaxDuration < 0 || rc.MaxBytes < 0 {
			return errors.New("recording.max_duration and recording.max_bytes must be >= 0")
		}
		// Recordings and their transcripts are conversation content.
		if r := cfg.Retention; r.Enabled && r.ContentRetentionDays > 0 && rc.RetentionDays > r.ContentRetentionDays {
			return errors.New("recording.retention_days cannot exceed retention.content_retention_days")
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
		Overrides map[string]any            `yaml:"insecure_overrides"`
	}
	_ = yaml.Unmarshal(data, &doc)
	if _, ok := doc.Overrides["skip_credential_perms_check"]; ok {
		return errors.New("config: insecure_overrides.skip_credential_perms_check is no longer supported; there is no credential file check. Remove the key")
	}
	if _, ok := top["pipelines"]; ok {
		return errors.New("config: `pipelines` is no longer supported; calls run stored agents. Create agents via /api/v1/agents and remove the pipelines section")
	}
	for provider, settings := range doc.Providers {
		for _, key := range []string{"max_streams", "rotation_headroom_multiplier"} {
			if _, ok := settings[key]; ok {
				return fmt.Errorf("config: providers.%s.%s is no longer supported; limits.max_sessions caps concurrent calls (keep it within your STT stream quota)", provider, key)
			}
		}
		for _, key := range []string{"model", "language_code", "language", "voice", "system_prompt", "api_key_env", "credentials_file", "auth_mode"} {
			if _, ok := settings[key]; ok {
				return fmt.Errorf("config: providers.%s.%s is no longer supported; models, voices, prompts and credentials belong to stored agents and provider credentials", provider, key)
			}
		}
	}
	return nil
}
