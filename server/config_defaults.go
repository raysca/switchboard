package server

import (
	"time"
)

const DevInsecureTokenSecret = "dev-insecure-secret-key-for-testing-only-12345"

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
	cfg.Token.MaxLifetime = time.Minute
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
	cfg.Limits.PingInterval = 15 * time.Second
	cfg.Limits.PingTimeout = 10 * time.Second
	cfg.Limits.DrainTimeout = 30 * time.Second
	cfg.MCP.Timeout = 15 * time.Second

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
	cfg.VAD.StopMS = 200 // Pipecat VAD_STOP_SECS; the gated speech timeout guards mid-sentence cuts
	cfg.Turn.Manager = true
	// Model paths stay empty: the vad and smartturn packages resolve their
	// default artifacts (and GOVOICE_*_MODEL_PATH) themselves.
	cfg.Turn.Analyzer.Workers = 1
	cfg.Turn.Analyzer.StopSecs = 3 * time.Second
	cfg.Turn.Analyzer.Budget = 150 * time.Millisecond

	// Providers
	cfg.Providers.GoogleSTT.Location = "us-central1"
	cfg.Providers.GoogleSTT.RotationThreshold = 240 * time.Second

	cfg.Providers.GoogleTTS.SampleRate = 24000

	cfg.Providers.Echo.Mimic = "google"
	cfg.Providers.Echo.STT = "text"

	cfg.Providers.XAILLM.Endpoint = "https://api.x.ai/v1/responses"

	cfg.Providers.XAISTT.Endpoint = "wss://api.x.ai/v1/stt"

	cfg.Providers.XAITTS.Endpoint = "https://api.x.ai/v1/tts"

	// The local stack (offline/) is on by default for development only; production sets these on purpose.
	if profile != ProfileProduction {
		cfg.Providers.Offline.LLMEndpoint = "http://127.0.0.1:11434/v1/responses"
		cfg.Providers.Offline.STTEndpoint = "ws://127.0.0.1:8099/v1/stt"
		cfg.Providers.Offline.TTSEndpoint = "http://127.0.0.1:8099/v1/tts"
	}

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
	// Quiet-caller handling is on: a call nobody is on would otherwise cost money until max_call_duration.
	// idle.user_timeout: 0 turns it off; a flow can set its own (flow idle).
	cfg.Idle.UserTimeout, cfg.Idle.MaxPrompts = defaultUserTimeout, defaultMaxPrompts
	cfg.Idle.Prompt, cfg.Idle.Goodbye = defaultIdlePrompt, defaultIdleBye
	cfg.Idle.BotMaxStuck = 3
	cfg.Audio.AllowFlowBackground = profile == ProfileDevelopment
	cfg.DTMF.AggregateTimeout = 2 * time.Second
	cfg.Audio.DenoiseStrength = 0.5
	cfg.Warmup.Timeout = 3 * time.Second
	cfg.Warmup.SpareTTL = 10 * time.Second
	cfg.OTel.QueueSize = 2048
	cfg.OTel.ShutdownTimeout = 5 * time.Second
	// Not defaulted on purpose: turn.speech_timeout, eager_grace, max_mute and
	// completion waits (setting them would trip "needs turn.manager"; the turn
	// code owns those fallbacks), warmup.enabled and audio.backgrounds gain_db/loop
	// (nil means default), otel.sampling_ratio (nil means 1).

	return cfg
}
