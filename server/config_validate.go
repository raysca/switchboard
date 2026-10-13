package server

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"govoice/providers/echo"

	coreturn "govoice/core/turn"
)

// ValidateConfig enforces production safety and constraint validation.
func ValidateConfig(cfg *Config) error {
	if err := validateProfileAndStorage(cfg); err != nil {
		return err
	}
	if err := validateListeners(cfg); err != nil {
		return err
	}
	if err := validateProductionSecurity(cfg); err != nil {
		return err
	}
	if err := validateLimits(cfg); err != nil {
		return err
	}
	if err := validateTurn(cfg); err != nil {
		return err
	}
	if err := validateAudioAndTTS(cfg); err != nil {
		return err
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
	return validateRetentionAndRecording(cfg)
}

func validateProfileAndStorage(cfg *Config) error {
	if cfg.Profile != ProfileDevelopment && cfg.Profile != ProfileProduction {
		return fmt.Errorf("invalid profile %q: must be 'development' or 'production'", cfg.Profile)
	}
	if cfg.Token.MaxLifetime != 0 && cfg.Token.MaxLifetime < 2*time.Second {
		return errors.New("token.max_lifetime must be at least 2 seconds when configured")
	}
	if a := cfg.Demo.AutoInstall; a != "" && a != "dental" {
		return errors.New(`demo.auto_install must be empty or "dental"`)
	}
	switch cfg.Storage.Dialect {
	case "", "sqlite", "postgres": // empty is reported below as required
	default:
		return fmt.Errorf("invalid storage.dialect %q: must be 'sqlite' or 'postgres'", cfg.Storage.Dialect)
	}
	// Calls run stored flows only: the flow store is mandatory.
	if cfg.Storage.Dialect == "" {
		return errors.New("storage.dialect is required: calls run stored flows (set sqlite or postgres)")
	}
	return nil
}

func validateListeners(cfg *Config) error {
	if cfg.PublicListenAddr == "" {
		return errors.New("public_listen_addr must be specified")
	}
	if cfg.AdminListenAddr == "" {
		return errors.New("admin_listen_addr must be specified")
	}
	if cfg.PublicListenAddr == cfg.AdminListenAddr && !strings.HasSuffix(cfg.PublicListenAddr, ":0") { // ":0" = two ephemeral ports (tests)
		return fmt.Errorf("public_listen_addr (%s) and admin_listen_addr (%s) must be separate listeners",
			cfg.PublicListenAddr, cfg.AdminListenAddr)
	}
	if cfg.ProbeListenAddr != "" && (cfg.ProbeListenAddr == cfg.PublicListenAddr || cfg.ProbeListenAddr == cfg.AdminListenAddr) {
		return fmt.Errorf("probe_listen_addr (%s) must differ from the public and admin listeners", cfg.ProbeListenAddr)
	}
	return nil
}

func validateProductionSecurity(cfg *Config) error {
	if cfg.Profile != ProfileProduction {
		return nil
	}
	if cfg.MCP.AllowLocalHTTP {
		return errors.New("mcp.allow_local_http is for development and tests")
	}
	hasTLS := cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != ""
	hasProxy := cfg.TLS.TrustedProxyMode && len(cfg.TLS.TrustedProxies) > 0
	if cfg.TLS.TrustedProxyMode && len(cfg.TLS.TrustedProxies) == 0 {
		return errors.New("production trusted_proxy_mode requires tls.trusted_proxies")
	}
	if !hasTLS && !hasProxy && !cfg.InsecureOverrides.AllowPlainWSInProduction {
		return errors.New("production profile requires TLS cert/key or trusted_proxy_mode enabled (or explicit insecure override allow_plain_ws_in_production)")
	}
	if cfg.OriginAllowSameHost && !cfg.TLS.TrustedProxyMode {
		return errors.New("origin_allow_same_host in production needs tls.trusted_proxy_mode (the Host header must come from your proxy)")
	}
	if cfg.PublicAdmin.Enabled && !cfg.TLS.TrustedProxyMode {
		return errors.New("production public_admin needs tls.trusted_proxy_mode: the rate limits and handshake limiter key on the proxy-trusted client address")
	}
	if _, err := ResolveTokenSecret(cfg); err != nil {
		return err
	}
	if cfg.VAD.Mode == "inprocess" {
		return errors.New("production profile requires VAD mode 'process' (worker isolation)")
	}
	return nil
}

func validateLimits(cfg *Config) error {
	if cfg.Limits.MaxPendingHandshakes <= 0 {
		return errors.New("max_pending_handshakes must be > 0")
	}
	if cfg.Limits.MaxSessions <= 0 {
		return errors.New("max_sessions must be > 0")
	}
	if cfg.Limits.MaxSessionsPerPrincipal <= 0 {
		return errors.New("max_sessions_per_principal must be > 0")
	}
	if cfg.Limits.MaxSessionsPerFlow < 0 || cfg.Limits.StartRatePerPrincipal < 0 {
		return errors.New("max_sessions_per_flow and start_rate_per_principal must be >= 0")
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
	if cfg.Limits.PingInterval < 0 || (cfg.Limits.PingInterval > 0 && (cfg.Limits.PingTimeout <= 0 || cfg.Limits.PingTimeout >= cfg.Limits.PingInterval)) {
		return errors.New("limits: ping_interval must be >= 0 (0 = off) and, when set, 0 < ping_timeout < ping_interval")
	}
	if cfg.STT.ReplayBuffer < 0 || cfg.STT.ReplayBuffer > maxSTTReplay {
		return fmt.Errorf("stt.replay_buffer must be between 0 (default 10s) and %v", maxSTTReplay)
	}
	if err := validateLLMFailoverTiming(cfg.Failover.LLMBudget, cfg.Failover.LLMFirstToken); err != nil {
		return err
	}
	if cfg.Tools.MaxParallel < 0 || cfg.Tools.MaxParallel > 16 {
		return errors.New("tools.max_parallel must be between 0 and 16")
	}
	return nil
}

func validateTurn(cfg *Config) error {
	if _, err := turnStopDetector(cfg.Turn.Stop, coreturn.SpeechTimeoutConfig{UserSpeechTimeout: cfg.Turn.SpeechTimeout}, cfg.Turn.EagerGrace, func(time.Duration) coreturn.UserTurnStopStrategy { return nil }); err != nil {
		return err
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
	if cfg.Turn.SpeechTimeout < 0 || cfg.Turn.EagerGrace < 0 || cfg.Turn.NoTranscriptTimeout < 0 {
		return errors.New("turn.speech_timeout, turn.eager_grace and turn.no_transcript_timeout must be >= 0")
	}
	for p, d := range cfg.Turn.STTP99 {
		if d < 100*time.Millisecond || d > 10*time.Second {
			return fmt.Errorf("turn.stt_p99.%s must be between 100ms and 10s", p)
		}
	}
	if _, err := coreturn.ParseMuteStrategies(cfg.Turn.Mute, nil); err != nil {
		return err
	}
	if k := cfg.Turn.Completion; k.ShortWait < 0 || k.LongWait < 0 {
		return errors.New("turn.completion.short_wait and long_wait must be >= 0")
	}
	if ms := cfg.VAD.StopMS; ms != 0 && (ms < 200 || ms > 2000) {
		return errors.New("vad.stop_ms must be between 200 and 2000")
	}
	if cfg.Turn.MaxMute < 0 {
		return errors.New("turn.max_mute must be >= 0")
	}
	if !cfg.Turn.Manager && (len(cfg.Turn.Mute) > 0 || cfg.Turn.MaxMute > 0) {
		return errors.New("turn.mute and turn.max_mute need turn.manager: true")
	}
	if !cfg.Turn.Manager && (len(cfg.Turn.Stop) > 0 || cfg.Turn.SpeechTimeout > 0 || cfg.Turn.EagerGrace > 0 || cfg.Turn.MinWords > 0 || cfg.Turn.Completion != (CompletionConfig{}) || cfg.Turn.NoTranscriptTimeout > 0 || len(cfg.Turn.STTP99) > 0) {
		return errors.New("turn.stop, turn.speech_timeout, turn.eager_grace, turn.min_words, turn.no_transcript_timeout and turn.stt_p99 need turn.manager: true")
	}
	if cfg.Turn.Completion != (CompletionConfig{}) && !slices.Contains(cfg.Turn.Stop, "llm-completion") {
		return errors.New("turn.completion needs llm-completion in turn.stop")
	}
	return nil
}

func validateAudioAndTTS(cfg *Config) error {
	// Every TTS provider (and so every failover candidate) is built at this one
	// rate; these are the pcm rates Cartesia and ElevenLabs both offer.
	if r := cfg.Providers.GoogleTTS.SampleRate; r != 0 && !slices.Contains([]int{8000, 16000, 22050, 24000, 44100, 48000}, r) {
		return fmt.Errorf("providers.google_tts.sample_rate %d must be one of 8000, 16000, 22050, 24000, 44100, 48000 (the rates every TTS provider can output)", r)
	}
	if m := cfg.Providers.Echo.Mimic; !echo.Mimic(m).Valid() {
		return fmt.Errorf("providers.echo.mimic: %q must be one of google, instant", m)
	}
	if m := cfg.Providers.Echo.STT; !slices.Contains([]string{"text", "audio"}, m) {
		return fmt.Errorf("providers.echo.stt: %q must be one of text, audio", m)
	}
	if cfg.Providers.Echo.LLMExtraDelay < 0 {
		return errors.New("providers.echo.llm_extra_delay must not be negative")
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
	if cfg.TTSMerge.Margin < 0 || cfg.TTSMerge.Cap < 0 {
		return errors.New("tts_merge.margin and tts_merge.cap must be >= 0")
	}
	return nil
}

func validateRetentionAndRecording(cfg *Config) error {
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

// lowVADStopWithoutManager reports a config where turns end on a short raw VAD stop: with the
// turn manager off nothing waits for the transcript, so callers get cut off mid-sentence.
func lowVADStopWithoutManager(c *Config) bool {
	ms := c.VAD.StopMS
	if ms == 0 {
		ms = 200
	}
	return !c.Turn.Manager && ms < 500
}
