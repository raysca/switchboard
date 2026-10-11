package server

import (
	"time"

	"govoice/core/redact"
	coreturn "govoice/core/turn"
	"govoice/server/otelexport"
	"govoice/vad"
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
	AdminListenAddr  string `yaml:"admin_listen_addr"`  // e.g. "127.0.0.1:9090"; management API + /debug: keep on loopback
	// ProbeListenAddr optionally serves only /health, /ready and /metrics (no auth, no management API,
	// no /debug/timeline), so kubelet probes and Prometheus can reach a container whose admin listener
	// is on loopback. Empty = disabled. Never publish it to the internet.
	ProbeListenAddr string `yaml:"probe_listen_addr"`

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

	// Durable management plane (agents, MCP, session
	// history). Disabled when Storage.Dialect is empty; retention: see Retention.
	Storage struct {
		Dialect string `yaml:"dialect"` // "sqlite" | "postgres"
		DSNEnv  string `yaml:"dsn_env"` // env var holding the DSN (default GOVOICE_DATABASE_DSN)
	} `yaml:"storage"`

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
		IdleTimeout             time.Duration `yaml:"idle_timeout"`  // legacy data-silence read deadline; used only when ping_interval is 0
		PingInterval            time.Duration `yaml:"ping_interval"` // server WebSocket ping period (liveness); 0 = off, fall back to idle_timeout
		PingTimeout             time.Duration `yaml:"ping_timeout"`  // pong wait per ping; must be < ping_interval
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
		// StopMS is the silence after speech that ends the user's VAD turn
		// (rounded up to 32 ms chunks). Default 800; 200..2000.
		StopMS int `yaml:"stop_ms"`
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

	// Tools configures default tool loop limits across providers.
	Tools struct {
		// MaxParallel bounds concurrently executing calls within one round (1..16, 0 keeps default 1).
		MaxParallel int `yaml:"max_parallel"`
	} `yaml:"tools"`

	// Turn routes live calls through the turn manager (core/turn.Processor).
	// On by default: turns end on the transcript-gated speech timeout (Pipecat
	// parity). Off: turns end on the raw VAD stop, which needs vad.stop_ms ~800.
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
		Completion CompletionConfig `yaml:"completion"`
		// SpeechTimeout is the resume window after the VAD stop (Pipecat user_speech_timeout; 0: 600ms).
		SpeechTimeout time.Duration `yaml:"speech_timeout"`
		EagerGrace    time.Duration `yaml:"eager_grace"`
		// NoTranscriptTimeout ends a turn that never got a transcript after this much silence
		// (0: 5s), so a dead STT cannot hold a turn open until max_turn_duration.
		NoTranscriptTimeout time.Duration `yaml:"no_transcript_timeout"`
		// STTP99 overrides the built-in per-provider STT p99 (end of speech to final transcript)
		// the speech timeout waits for when the final is late.
		STTP99 map[string]time.Duration `yaml:"stt_p99"`
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

	// TTSMerge enables slack-aware unit merging for unary TTS providers (xai,
	// google; ignored for others): after the first unit, sentences are held
	// and merged while the merged request would still finish Margin before the
	// queued audio runs out. Fewer requests at the same characters. Off by
	// default. With it on and first_clause_chars 0, the first unit ends at a
	// clause after 40 chars. Cap 0 = 250 runes.
	TTSMerge struct {
		Enabled bool          `yaml:"enabled"`
		Margin  time.Duration `yaml:"margin"` // default 300ms
		Cap     int           `yaml:"cap"`
	} `yaml:"tts_merge"`

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
		// Cues are named sounds a graph agent plays while the call is between
		// agents (handoff_cue). See docs/audio-filters.md.
		Cues map[string]CueConfig `yaml:"cues"`
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
	// comes from the stored agent and its secrets from the environment, never from YAML.
	Providers struct {
		// CatalogDir is an optional directory of overlay files that add or replace
		// models, voices and languages in the provider catalog (server/catalog).
		CatalogDir string `yaml:"catalog_dir"`

		GoogleSTT struct {
			ProjectID         string        `yaml:"project_id"`
			Location          string        `yaml:"location"`
			RotationThreshold time.Duration `yaml:"rotation_threshold"`
		} `yaml:"google_stt"`

		GoogleTTS struct {
			SampleRate int `yaml:"sample_rate"`
			// DisableStreaming turns StreamingSynthesize off for Chirp 3 HD voices: they then use
			// the unary request like every other voice.
			DisableStreaming bool `yaml:"disable_streaming"`
		} `yaml:"google_tts"`

		XAILLM struct {
			Endpoint string `yaml:"endpoint"`
		} `yaml:"xai_llm"`

		// OpenAILLM base URLs come from the environment, so the server
		// refuses http and loopback/private/link-local targets (SSRF) unless
		// the operator opts in here for self-hosted vLLM. Metadata and
		// link-local addresses stay blocked regardless.
		OpenAILLM struct {
			AllowPrivateBaseURL bool `yaml:"allow_private_base_url"`
		} `yaml:"openai_llm"`

		// Nvidia: NIM base URLs and Riva endpoints come from the environment;
		// loopback/private targets (self-hosted NIM or Riva) and plaintext http
		// are refused unless the operator opts in. Link-local and metadata
		// addresses stay blocked regardless.
		Nvidia struct {
			AllowPrivate bool `yaml:"allow_private"`
		} `yaml:"nvidia"`

		XAISTT struct {
			Endpoint string `yaml:"endpoint"`
		} `yaml:"xai_stt"`

		XAITTS struct {
			Endpoint string `yaml:"endpoint"`
			// DisableStreaming turns the WebSocket off: every unit is one POST /v1/tts again.
			DisableStreaming bool `yaml:"disable_streaming"`
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

// VADProcessorConfig is the per-call VAD config: library defaults with
// vad.stop_ms applied (0 keeps the 200 ms default).
func (c *Config) VADProcessorConfig() vad.ProcessorConfig {
	return c.VADProcessorConfigForStopMS(0)
}

// vadStop is the call's VAD stop: the agent override (ms > 0), else vad.stop_ms, rounded up to
// whole 32 ms chunks like the VAD processor.
func (c *Config) vadStop(agentMS int) time.Duration {
	pc := c.VADProcessorConfigForStopMS(agentMS)
	return time.Duration(pc.StopChunks*vad.ChunkSamples*1000/vad.SampleRate) * time.Millisecond
}

// sttP99 resolves the primary STT provider's p99: agent override, server map, built-in table.
func (c *Config) sttP99(provider string, agentMS int) time.Duration {
	if agentMS > 0 {
		return time.Duration(agentMS) * time.Millisecond
	}
	if d, ok := c.Turn.STTP99[provider]; ok && d > 0 {
		return d
	}
	return coreturn.DefaultSTTP99(provider)
}

// VADProcessorConfigForStopMS returns the per-call VAD config applying an
// agent-level stop_ms override if positive, or the server-level vad.stop_ms otherwise.
func (c *Config) VADProcessorConfigForStopMS(overrideMS int) vad.ProcessorConfig {
	pc := vad.DefaultProcessorConfig()
	ms := c.VAD.StopMS
	if overrideMS > 0 {
		ms = overrideMS
	}
	if ms > 0 {
		chunkMS := vad.ChunkSamples * 1000 / vad.SampleRate
		pc.StopChunks = (ms + chunkMS - 1) / chunkMS
	}
	return pc
}

// CueConfig is one handoff cue sound: a PCM16 mono WAV, played once or looped
// until the new agent speaks. GainDB is -60..0 (default -20).
type CueConfig struct {
	File   string   `yaml:"file"`
	GainDB *float64 `yaml:"gain_db"`
	Loop   *bool    `yaml:"loop"` // default true
}
