package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/resilience"
	coretools "govoice/core/tools"
	coretranscript "govoice/core/transcript"
	coreturn "govoice/core/turn"
	"govoice/providers/cartesia"
	"govoice/providers/deepgram"
	"govoice/providers/elevenlabs"
	"govoice/providers/google"
	"govoice/providers/openai"
	"govoice/providers/xai"
	"govoice/server/agent"
	"govoice/smartturn"
	"govoice/vad"
)

// ProcessorFactory instantiates a core.Processor for a specific provider.
// StageSpec is one provider stage of a call, resolved from the stored agent.
// Secret is the credential assembled from the environment for this stage's
// provider only (server/catalog); the caller wipes it after the build.
type StageSpec struct {
	Provider string
	Model    string
	Language string
	Voice    string
	// VoiceSettings (ElevenLabs TTS only; nil = provider defaults).
	VoiceSettings *agent.ElevenLabsVoiceSettings
	SystemPrompt  string
	// Sampling (LLM only; zero = provider defaults). Copied to LLM fallbacks.
	Sampling agent.LLMSampling
	Secret   []byte

	// LLM stage only: the agent's captured MCP tools and the call history
	// the tool loop records into.
	Tools        []core.ToolDefinition
	ToolExecutor core.ToolExecutor
	History      *core.ConversationContext
	// AsyncSink receives finished ToolPolicy.Async calls and OnAsyncStart
	// their acknowledgement (see tools.ToolLoopConfig).
	AsyncSink    func(core.AsyncToolResult)
	OnAsyncStart func(core.ToolCall)

	// MaxParallel bounds concurrently executing calls within one round (1..16, 0 keeps default 1).
	MaxParallel int

	// Failover is set on a non-last candidate of a stage chain: its provider is
	// built with failoverPolicy (fewer, shorter attempts) so a fallback is
	// reached while the caller still waits.
	Failover bool

	// OnRetry, if set, is told before each retried request (LLM and TTS
	// factories copy it into their provider's retry policy).
	OnRetry func(retry int)
}

// sampling unpacks the agent's sampling into the provider configs' shape
// (providers cannot import server/agent): nil / 0 mean "provider default".
func (s StageSpec) sampling() (temperature, topP *float64, maxOutputTokens int) {
	if n := s.Sampling.MaxOutputTokens; n != nil {
		maxOutputTokens = *n
	}
	return s.Sampling.Temperature, s.Sampling.TopP, maxOutputTokens
}

// toolLoopConfig is the default loop config with the stage's async hooks.
func (s StageSpec) toolLoopConfig() coretools.ToolLoopConfig {
	cfg := coretools.DefaultToolLoopConfig()
	cfg.MaxParallel = s.MaxParallel
	cfg.AsyncSink, cfg.OnAsyncStart = s.AsyncSink, s.OnAsyncStart
	return cfg
}

// CallSpec is everything a call needs, resolved from a pinned agent version.
type CallSpec struct {
	STT, LLM, TTS StageSpec
	MaxMessages   int
	// AgentVersionID is the pinned agent version (trace attribute only).
	AgentVersionID string
	// StoredSessionID is the persisted session row (trace spans attach to it).
	StoredSessionID string
	// TurnLatencies collects the call's per-turn stage latencies for its turn rows.
	TurnLatencies *turnLatencies
	// MaxCallDuration is how long the call may last: the shorter of the agent's limit and the server's.
	MaxCallDuration time.Duration
	// Ender is set when some step has end_call; the builder wires it to the idle controller.
	Ender *callEnder
	// Agent is set for agents with more than one step: the call starts on the
	// start step and moves between steps by handoff.
	Agent *CallAgent
	// OnHandoff, if set, receives the handoff lifecycle (timeline, traces, metrics).
	OnHandoff func(core.HandoffEvent)
	// OnEndCall receives the step whose end_call ended the call (trace).
	OnEndCall func(stepID string)

	// OnProviderRetry, if set, is told of every retried LLM or TTS request (timeline).
	OnProviderRetry func(stage, provider string, retry int)
	// OnInjector, if set, receives the call's context injector as the pipeline is assembled
	// (before any stage runs); the server registers it only after a successful build.
	OnInjector func(ContextInjector)
	// Greeting is the start step's greeting ("" = none). OnGreeter, if set, receives the function that
	// speaks a fixed line as the assistant's turn (a failure goes to OnGreetFail), as the pipeline is assembled;
	// the server calls it once the session has started.
	Greeting  string
	OnGreeter func(speak func(string))
	// OnGreetFail is told when a greeting could not be queued (the server counts and logs it).
	OnGreetFail func(error)
	MaxTokens   int
	// Summary: summarize older turns with the primary LLM (off unless Enabled).
	Summary agent.SummarizationConfig
	// History, when set, sees every frame just before the transport (turn rows).
	History core.Processor
	// Recording: the agent opted in (and confirmed disclosure).
	Recording bool
	// LLMFallbacks are the LLM chain after the primary (credentials resolved);
	// Failover says which error classes may move a generation to the next one.
	LLMFallbacks []StageSpec
	// STTFallbacks are the STT chain after the primary (credentials resolved).
	STTFallbacks []StageSpec
	Failover     agent.FailoverPolicy
	// TTSFallbacks are the TTS chain after the primary (credentials resolved,
	// voice from each fallback's own spec).
	TTSFallbacks []StageSpec
	// DisableTextCleaning skips stripping markdown formatting and emojis before TTS.
	DisableTextCleaning bool
	// VADStopMS overrides server vad.stop_ms if > 0.
	VADStopMS int
	// STTP99MS overrides the STT p99 for this call if > 0.
	STTP99MS int
	// Idle, Audio and Turn are the agent's quiet-caller, audio and interruption settings (nil: the server's).
	Idle  *agent.IdleSettings
	Audio *agent.AudioSettings
	// toolHold plays the agent's hold sound while a slow synchronous tool runs (nil: none).
	toolHold *holdCue
	Turn     *agent.TurnSettings
	// PushToTalk: the client opted in at client-ready. Turns start and end
	// only on its push-to-talk messages (ExternalStart/ExternalStop).
	PushToTalk bool
}

// ProcessorFactory builds one stage processor. cfg supplies provider
// infrastructure settings only (project, region, endpoints, quotas); models,
// voices, prompts and credentials come from the agent via spec.
type ProcessorFactory func(cfg *Config, spec StageSpec) (core.Processor, error)

// Registry maps "<provider>.<stage>" to processor factories. Register only
// before serving calls; lookups are unsynchronised.
type Registry struct {
	factories map[string]ProcessorFactory

	turnWarned sync.Map // agent version ids already warned about turn settings with the manager off

	// breakers holds one breaker per circuit (Registry.breaker): an outage of one provider
	// stage, or of one tenant's openai base URL, never opens another's.
	// ponytail: never evicted; bounded by the provider stages and the distinct openai base URLs stored.
	breakers sync.Map                // circuitKey -> *resilience.Breaker
	metrics  atomic.Pointer[Metrics] // set by BindMetrics; observed breakers export their state through it

	turnBackend smartturn.Backend // smart-turn model; nil unless turn.stop has turn-analyzer

	onSTTFailover        func(from, to, class string)                     // set by BindMetrics
	onSTTFallbackSkipped func()                                           // set by BindMetrics
	onSTTReplayDropped   func(bytes int)                                  // set by BindMetrics
	onLLMFailover        func(from, to, class string)                     // set by BindMetrics
	onUserMuteDrop       func()                                           // set by BindMetrics
	onMinWordsNoInterims func()                                           // set by BindMetrics
	onAsyncTurn          func()                                           // set by BindMetrics
	onAsyncDrop          func()                                           // set by BindMetrics
	onSummary            func(outcome string, d time.Duration, saved int) // set by BindMetrics
	onCompletion         func(verdict string)                             // set by BindMetrics
	onFilter             func(string, time.Duration)                      // set by BindMetrics
	onLLMFallbackSkipped func()                                           // set by BindMetrics
	onTTSFailover        func(from, to, class string)                     // set by BindMetrics
	onTTSFallbackSkipped func()                                           // set by BindMetrics
	ttsUnitTimeout       time.Duration                                    // TTSFailover's stall watchdog; 0 = core's default (30 s); e2e tests shorten it
	// onCircuit receives every breaker state report (the server's call timelines);
	// set once by NewServer, read by breaker observers on any goroutine.
	onCircuit atomic.Pointer[func(stage, provider string, st resilience.State)]
}

// The Google stage configs, shared by the google and the echo factories: echo runs these same
// processors against simulated backends, so only the client, the breaker and the label differ.

func googleSTTConfig(cfg *Config, spec StageSpec, breaker *resilience.Breaker) google.STTConfig {
	c := google.DefaultSTTConfig()
	if spec.Model != "" {
		c.Model = spec.Model
	}
	if spec.Language != "" {
		c.LanguageCode = spec.Language
	}
	if t := cfg.Providers.GoogleSTT.RotationThreshold; t > 0 {
		c.StreamRotationThreshold = t
	}
	c.CircuitBreaker = breaker
	if spec.Failover {
		c.RetryPolicy = failoverPolicy("stt", c.RetryPolicy)
	}
	return c
}

func googleLLMConfig(spec StageSpec, breaker *resilience.Breaker) google.LLMConfig {
	c := google.DefaultLLMConfig()
	c.Model, c.SystemPrompt = spec.Model, spec.SystemPrompt
	c.Sampling.Temperature, c.Sampling.TopP, c.Sampling.MaxOutputTokens = spec.sampling()
	if len(spec.Tools) > 0 {
		c.ToolDefinitions, c.ToolExecutor, c.ToolHistory = spec.Tools, spec.ToolExecutor, spec.History
		c.ToolLoopConfig = spec.toolLoopConfig()
	}
	c.CircuitBreaker = breaker
	c.RetryPolicy.OnRetry = spec.OnRetry
	return c
}

func googleTTSConfig(cfg *Config, spec StageSpec, breaker *resilience.Breaker) google.TTSConfig {
	c := google.DefaultTTSConfig()
	if spec.Voice != "" {
		c.Voice = spec.Voice
	}
	if cfg.Providers.GoogleTTS.SampleRate > 0 {
		c.SampleRate = cfg.Providers.GoogleTTS.SampleRate
	}
	c.CircuitBreaker = breaker
	if spec.Failover {
		c.RetryPolicy = failoverPolicy("tts", c.RetryPolicy)
	}
	c.RetryPolicy.OnRetry = spec.OnRetry
	return c
}

// googleTTSStreams is whether voice is synthesized over a stream (Chirp 3 HD, unless disabled).
func googleTTSStreams(cfg *Config, voice string) bool {
	return google.IsChirp3HD(voice) && !cfg.Providers.GoogleTTS.DisableStreaming
}

func googleTTSProcessor(cfg *Config, c google.TTSConfig) (core.Processor, error) {
	if googleTTSStreams(cfg, c.Voice) {
		return google.NewStreamingTTSProcessor(c)
	}
	return google.NewTTSProcessor(c)
}

func factoryKey(provider, stage string) string { return provider + "." + stage }

func NewRegistry() *Registry {
	r := &Registry{
		factories: make(map[string]ProcessorFactory),
	}

	r.Register("google.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		material, err := google.CredentialMaterialFromSecret(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("google.stt: %w", err)
		}
		sttCfg := googleSTTConfig(cfg, spec, r.breaker("stt", "google"))
		infra := cfg.Providers.GoogleSTT
		if infra.Location != "" {
			sttCfg.Location = infra.Location
		}
		sttCfg.ProjectID = infra.ProjectID
		if sttCfg.ProjectID == "" {
			sttCfg.ProjectID = material.ProjectID
		}
		sttCfg.CredentialsJSON, sttCfg.APIKey = material.ServiceAccountJSON, material.APIKey
		opener, err := google.NewRealGoogleSTTOpener(context.Background(), sttCfg)
		if err != nil {
			return nil, fmt.Errorf("google.stt: %w", err)
		}
		sttCfg.StreamOpener = opener
		return google.NewSTTProcessor(sttCfg)
	})

	r.Register("google.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		material, err := google.CredentialMaterialFromSecret(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("google.llm: %w", err)
		}
		cc := google.CredentialConfig{Mode: google.AuthModeGeminiAPIKey, APIKey: material.APIKey}
		if len(material.ServiceAccountJSON) > 0 {
			cc = google.CredentialConfig{Mode: google.AuthModeVertexADC, CredentialsJSON: material.ServiceAccountJSON, ProductionProfile: cfg.Profile == ProfileProduction}
		}
		creds, err := google.ResolveCredentials(cc)
		if err != nil {
			return nil, fmt.Errorf("google.llm: %w", err)
		}
		client, err := google.NewRealGoogleLLMClient(context.Background(), creds)
		if err != nil {
			return nil, fmt.Errorf("google.llm: %w", err)
		}
		llmCfg := googleLLMConfig(spec, r.breaker("llm", "google"))
		llmCfg.Client = client
		return google.NewLLMProcessor(llmCfg)
	})

	r.Register("google.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		material, err := google.CredentialMaterialFromSecret(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("google.tts: %w", err)
		}
		ttsCfg := googleTTSConfig(cfg, spec, r.breaker("tts", "google"))
		ttsCfg.CredentialsJSON, ttsCfg.APIKey = material.ServiceAccountJSON, material.APIKey
		client, err := google.NewRealGoogleTTSClient(context.Background(), ttsCfg)
		if err != nil {
			return nil, fmt.Errorf("google.tts: %w", err)
		}
		ttsCfg.Client = client
		return googleTTSProcessor(cfg, ttsCfg)
	})

	r.Register("xai.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("xai.llm: %w", err)
		}
		return xaiLLMProcessor(spec, cfg.Providers.XAILLM.Endpoint, key, r.breaker("llm", "xai"), "xai.llm")
	})

	// openai.llm covers OpenAI and any chat-completions server (Groq, Together,
	// vLLM). The credential is a bare API key (OpenAI) or JSON
	// {"api_key":"...","base_url":"https://api.groq.com/openai/v1"}.
	r.Register("openai.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, baseURL, err := openAICredential(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("openai.llm: %w", err)
		}
		oaCfg := openAILLMConfig(cfg, spec, key, baseURL, r.openaiBreaker(baseURL))
		if len(spec.Tools) > 0 {
			loop, err := coretools.NewToolLoop(spec.Tools, spec.ToolExecutor, spec.History, spec.toolLoopConfig())
			if err != nil {
				return nil, fmt.Errorf("openai.llm tools: %w", err)
			}
			oaCfg.ToolDefinitions, oaCfg.ToolLoop = spec.Tools, loop
		}
		return openai.NewProcessor(oaCfg)
	})

	r.Register("deepgram.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("deepgram.stt: %w", err)
		}
		sttCfg := deepgram.DefaultSTTConfig()
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.Language = spec.Language
		}
		sttCfg.APIKey, sttCfg.CircuitBreaker = key, r.breaker("stt", "deepgram")
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
		return deepgram.NewSTTProcessor(sttCfg)
	})

	r.Register("cartesia.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("cartesia.stt: %w", err)
		}
		sttCfg := cartesia.DefaultSTTConfig()
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.Language = spec.Language
		}
		sttCfg.APIKey, sttCfg.CircuitBreaker = key, r.breaker("stt", "cartesia")
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
		return cartesia.NewSTTProcessor(sttCfg)
	})

	r.Register("cartesia.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("cartesia.tts: %w", err)
		}
		ttsCfg := cartesia.DefaultTTSConfig()
		if spec.Model != "" {
			ttsCfg.Model = spec.Model
		}
		if spec.Voice != "" {
			ttsCfg.Voice = spec.Voice
		}
		if spec.Language != "" {
			ttsCfg.Language = spec.Language
		}
		// The pipeline's TTS output rate is configured once, under the google TTS settings.
		if cfg.Providers.GoogleTTS.SampleRate > 0 {
			ttsCfg.SampleRate = cfg.Providers.GoogleTTS.SampleRate
		}
		ttsCfg.APIKey, ttsCfg.CircuitBreaker = key, r.breaker("tts", "cartesia")
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		ttsCfg.RetryPolicy.OnRetry = spec.OnRetry
		return cartesia.NewTTSProcessor(ttsCfg)
	})

	r.Register("elevenlabs.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret) // same credential as elevenlabs.tts
		if err != nil {
			return nil, fmt.Errorf("elevenlabs.stt: %w", err)
		}
		sttCfg := elevenlabs.DefaultSTTConfig()
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.Language = spec.Language
		}
		sttCfg.APIKey, sttCfg.CircuitBreaker = key, r.breaker("stt", "elevenlabs")
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
		return elevenlabs.NewSTTProcessor(sttCfg)
	})

	r.Register("elevenlabs.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("elevenlabs.tts: %w", err)
		}
		ttsCfg := elevenlabsTTSConfig(cfg, spec)
		ttsCfg.APIKey, ttsCfg.CircuitBreaker = key, r.breaker("tts", "elevenlabs")
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		ttsCfg.RetryPolicy.OnRetry = spec.OnRetry
		return elevenlabs.NewTTSProcessor(ttsCfg)
	})

	registerXAISpeech(r)
	r.registerNvidia()
	registerEcho(r)
	registerSilence(r)
	registerOffline(r)

	return r
}

// elevenlabsTTSConfig applies the stage spec (model, voice, voice settings) and
// the pipeline sample rate to the provider defaults.
func elevenlabsTTSConfig(cfg *Config, spec StageSpec) elevenlabs.TTSConfig {
	c := elevenlabs.DefaultTTSConfig()
	if spec.Model != "" {
		c.Model = spec.Model
	}
	if spec.Voice != "" {
		c.Voice = spec.Voice
	}
	// The pipeline's TTS output rate is configured once, under the google TTS settings.
	if cfg.Providers.GoogleTTS.SampleRate > 0 {
		c.SampleRate = cfg.Providers.GoogleTTS.SampleRate
	}
	if v := spec.VoiceSettings; v != nil {
		c.Stability, c.SimilarityBoost, c.Style, c.Speed = v.Stability, v.SimilarityBoost, v.Style, v.Speed
	}
	return c
}

// xaiLLMConfig is the xAI stage config, shared by xai.llm and offline.llm.
func xaiLLMConfig(spec StageSpec, endpoint, key string, br *resilience.Breaker) xai.Config {
	c := xai.Config{Model: spec.Model, Endpoint: endpoint, SystemPrompt: spec.SystemPrompt, APIKey: key, CircuitBreaker: br}
	c.RetryPolicy.OnRetry = spec.OnRetry
	c.Temperature, c.TopP, c.MaxOutputTokens = spec.sampling()
	return c
}

// xaiLLMProcessor is the xAI Responses-API stage, shared by xai.llm and offline.llm.
func xaiLLMProcessor(spec StageSpec, endpoint, key string, br *resilience.Breaker, who string) (core.Processor, error) {
	xaiCfg := xaiLLMConfig(spec, endpoint, key, br)
	if len(spec.Tools) > 0 {
		loop, err := coretools.NewToolLoop(spec.Tools, spec.ToolExecutor, spec.History, spec.toolLoopConfig())
		if err != nil {
			return nil, fmt.Errorf("%s tools: %w", who, err)
		}
		xaiCfg.ToolDefinitions, xaiCfg.ToolLoop = spec.Tools, loop
	}
	return xai.NewProcessor(xaiCfg)
}

func openAILLMConfig(cfg *Config, spec StageSpec, key, baseURL string, br *resilience.Breaker) openai.Config {
	c := openai.Config{Model: spec.Model, BaseURL: baseURL, SystemPrompt: spec.SystemPrompt, APIKey: key, CircuitBreaker: br,
		AllowPrivate: cfg.Providers.OpenAILLM.AllowPrivateBaseURL}
	c.RetryPolicy.OnRetry = spec.OnRetry
	c.Temperature, c.TopP, c.MaxOutputTokens = spec.sampling()
	return c
}

// apiKey validates an API key credential: a single non-empty token.
func apiKey(secret []byte) (string, error) {
	key := strings.TrimSpace(string(secret))
	if key == "" || strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("credential must be a single API key")
	}
	return key, nil
}

// openAICredential accepts a bare API key or {"api_key","base_url"} JSON. The
// base URL is chosen by the operator (OPENAI_BASE_URL in the environment).
func openAICredential(secret []byte) (key, baseURL string, err error) {
	trimmed := strings.TrimSpace(string(secret))
	if !strings.HasPrefix(trimmed, "{") {
		key, err = apiKey(secret)
		return key, "", err
	}
	var c struct {
		APIKey  string `json:"api_key"`
		BaseURL string `json:"base_url"`
	}
	if jerr := json.Unmarshal([]byte(trimmed), &c); jerr != nil {
		return "", "", errors.New("credential is not valid JSON")
	}
	if key, err = apiKey([]byte(c.APIKey)); err != nil {
		return "", "", err
	}
	return key, strings.TrimSpace(c.BaseURL), nil
}

// Register adds or overrides a provider stage factory ("<provider>.<stage>").
func (r *Registry) Register(name string, factory ProcessorFactory) {
	r.factories[name] = factory
}

// Supports reports whether a factory exists for provider and stage.
func (r *Registry) Supports(provider, stage string) bool {
	_, ok := r.factories[factoryKey(provider, stage)]
	return ok
}

// CallUserContextIndex is the position of the user context processor in the
// default call pipeline (turn processor installed); committed user turns
// (including client text) enter there. Callers look the processor up by type
// and use this only as the fallback.
const CallUserContextIndex = 4

// callStop is the call's end-of-turn detector: the server's chain with the agent's choices laid over it
// (speech timeout, detector, smart hold). turnTap is the smart-turn analyzer's audio tap, nil without one.
func (r *Registry) callStop(cfg *Config, call CallSpec) (stop coreturn.TurnDetector, turnTap core.Processor, err error) {
	tc := cfg.Turn.forCall(call.Turn)
	var analyzer func(time.Duration) coreturn.UserTurnStopStrategy
	if slices.Contains(tc.Stop, "turn-analyzer") && r.turnBackend != nil {
		a := smartturn.NewAnalyzer(r.turnBackend, smartturn.Config{Budget: tc.Analyzer.Budget, Hold: tc.Analyzer.Hold})
		turnTap = a.Tap()
		analyzer = func(base time.Duration) coreturn.UserTurnStopStrategy {
			return smartturn.Stop(a, base, tc.Analyzer.Hold)
		}
	}
	st := coreturn.SpeechTimeoutConfig{
		VADStop: cfg.vadStop(call.VADStopMS), UserSpeechTimeout: tc.SpeechTimeout,
		STTP99: cfg.sttP99(call.STT.Provider, call.STTP99MS), NoTranscriptTimeout: tc.NoTranscriptTimeout,
	}
	stop, err = turnStopDetector(tc.Stop, st, tc.EagerGrace, analyzer)
	return stop, turnTap, err
}

// BuildCallPipeline builds VAD -> STT -> transcript -> user context -> LLM ->
// assistant context -> TTS -> transport for one call of a stored agent.
func (r *Registry) BuildCallPipeline(cfg *Config, call CallSpec, vadProc *vad.Processor, transportOutput core.Processor) (*core.Pipeline, error) {
	build := func(stage string, spec StageSpec) (core.Processor, error) {
		if report := call.OnProviderRetry; report != nil {
			provider := spec.Provider
			spec.OnRetry = func(retry int) { report(stage, provider, retry) }
		}
		proc, err := r.BuildStage(cfg, stage, spec)
		if err == nil {
			r.warm(cfg, proc) // every candidate, primary and fallbacks
		}
		return proc, err
	}
	ctxPool := core.NewConversationContext(core.ContextConfig{MaxMessages: call.MaxMessages, MaxTokens: call.MaxTokens})
	// Processors hold provider clients; close those already built if a later
	// stage fails, or a bad credential would leak a client per connect attempt.
	var built []core.Processor
	fail := func(err error) (*core.Pipeline, error) {
		for _, p := range built {
			if c, ok := p.(interface{ Close() error }); ok {
				_ = c.Close()
			}
		}
		return nil, err
	}
	sttProc, err := r.buildSTT(cfg, call, build)
	if err != nil {
		return fail(fmt.Errorf("create stt processor: %w", err))
	}
	built = append(built, sttProc)
	tc := cfg.Turn.forCall(call.Turn)
	if call.Turn != nil && !tc.Manager {
		// Once per agent version (a handful of short strings at most), not on every call.
		if _, seen := r.turnWarned.LoadOrStore(call.AgentVersionID, struct{}{}); !seen {
			slog.Warn("the agent sets turn settings but turn.manager is off on this server: ignored, except the mute list and the longest mute for push-to-talk calls", slog.String("agent_version", call.AgentVersionID))
		}
	}
	idleCfg := cfg.Idle.forCall(call.Idle)
	if call.Idle == nil && call.STT.Provider == silenceProvider {
		idleCfg.UserTimeout = 0 // a caller who types is not silent: only an explicit agent setting turns the nudges on
	}
	if err := idleCfg.validate(cfg.DTMF.Aggregate, cfg.DTMF.AggregateTimeout); err != nil {
		return fail(fmt.Errorf("agent idle settings: %w", err)) // the server's DTMF rules changed since the agent was published
	}
	dt, err := buildDTMF(cfg)
	if err != nil {
		return fail(fmt.Errorf("create dtmf processors: %w", err))
	}
	inFilter, err := buildInputFilter(cfg, call.Audio, r.onFilter)
	if err != nil {
		return fail(fmt.Errorf("create input filter: %w", err))
	}
	if inFilter != nil {
		built = append(built, inFilter)
	}
	// LLM turn-completion protocol (turn.stop: llm-completion): every candidate's
	// system prompt carries the marker instructions.
	completion := completionConfig(tc.Stop, tc.Completion, r.onCompletion)
	if call.PushToTalk {
		completion = nil // a release is a definite end of turn: no "incomplete" holds
	}
	// function_call mute: every LLM stage's tool executor reports in-flight calls.
	var muteTools *coreturn.ToolActivity
	if ((tc.Manager || call.PushToTalk) && slices.Contains(tc.Mute, coreturn.MuteFunctionCall)) || idleCfg.enabled() { // idle also pauses while a tool runs
		muteTools = coreturn.NewToolActivity()
	}
	// The trigger is on every call: finished async tools, handoff flips and operator notes
	// all wait here for the call to be idle, then inject and start one model turn.
	var asstActive func() bool
	asyncTrigger := core.NewAsyncTrigger(core.AsyncTriggerConfig{Busy: func() bool { return asstActive != nil && asstActive() },
		OnTriggered: r.onAsyncTurn, OnDropped: r.onAsyncDrop})
	if call.OnInjector != nil {
		call.OnInjector(newCallInjector(asyncTrigger, ctxPool))
	}
	// prepLLM applies the call-wide LLM stage setup to one step's specs; the
	// start step uses it here and every later step when it is activated.
	prepLLM := func(spec StageSpec, fallbacks []StageSpec) (StageSpec, []StageSpec) {
		spec.History = ctxPool
		fallbacks = slices.Clone(fallbacks)
		if dt.tones != nil { // send_dtmf is a call tool: every step's stages have it, before the tool-activity wrap
			dt.addTool(&spec)
			for i := range fallbacks {
				dt.addTool(&fallbacks[i])
			}
		}
		if completion != nil {
			spec.SystemPrompt = completionPrompt(spec.SystemPrompt, tc.Completion)
			for i := range fallbacks {
				fallbacks[i].SystemPrompt = completionPrompt(fallbacks[i].SystemPrompt, tc.Completion)
			}
		}
		if muteTools != nil {
			spec.ToolExecutor = muteTools.Wrap(spec.ToolExecutor, spec.Tools)
			for i := range fallbacks {
				fallbacks[i].ToolExecutor = muteTools.Wrap(fallbacks[i].ToolExecutor, fallbacks[i].Tools)
			}
		}
		spec.ToolExecutor = holdTools(spec.ToolExecutor, spec.Tools, call.toolHold)
		for i := range fallbacks {
			fallbacks[i].ToolExecutor = holdTools(fallbacks[i].ToolExecutor, fallbacks[i].Tools, call.toolHold)
		}
		spec.AsyncSink = asyncTrigger.Deliver
		for i := range fallbacks {
			fallbacks[i].AsyncSink = asyncTrigger.Deliver
		}
		return spec, fallbacks
	}
	llmSpec, llmFallbacks := prepLLM(call.LLM, call.LLMFallbacks)
	call.LLMFallbacks = llmFallbacks
	llmProc, err := r.buildLLM(cfg, call, llmSpec, build)
	if err != nil {
		return fail(fmt.Errorf("create llm processor: %w", err))
	}
	built = append(built, llmProc)
	ttsProc, err := r.buildTTS(cfg, call, build)
	if err != nil {
		return fail(fmt.Errorf("create tts processor: %w", err))
	}
	built = append(built, ttsProc)
	var speak func(string) // says a fixed line as the assistant; set once the assistant processor exists, before the session starts
	if call.Agent != nil {
		// The start step is live; every other step is built when the call moves
		// to it and closed when it leaves. The swaps are the pipeline stages.
		var lastStep, lastGreeting atomic.Pointer[string]
		buildStep := func(ctx context.Context, id string) (core.StepStages, error) {
			ns, err := call.Agent.Resolve(ctx, id)
			if err != nil {
				return core.StepStages{}, fmt.Errorf("resolve step %q: %w", id, err)
			}
			defer ns.wipe()
			spec, fallbacks := prepLLM(ns.LLM, ns.LLMFallbacks)
			stepCall := call
			stepCall.LLMFallbacks = fallbacks
			stepCall.TTS, stepCall.TTSFallbacks = ns.TTS, ns.TTSFallbacks
			llm, err := r.buildLLM(cfg, stepCall, spec, build)
			if err != nil {
				return core.StepStages{}, fmt.Errorf("step %q llm: %w", id, err)
			}
			tts, err := r.buildTTS(cfg, stepCall, build)
			if err != nil {
				if c, ok := llm.(interface{ Close() error }); ok {
					_ = c.Close()
				}
				return core.StepStages{}, fmt.Errorf("step %q tts: %w", id, err)
			}
			lastStep.Store(&ns.Name)
			lastGreeting.Store(&ns.Greeting)
			return core.StepStages{LLM: llm, TTS: tts, Speaks: saysOwnGreeting(id, call.Agent.Start, ns.Greeting)}, nil
		}
		llmSwap, ttsSwap := core.NewStageSwap(llmProc), core.NewStageSwap(ttsProc)
		router, err := core.NewRouter(core.RouterConfig{
			Entry: call.Agent.Start, LLM: llmSwap, TTS: ttsSwap, Trigger: asyncTrigger,
			Build:   buildStep,
			OnEvent: call.OnHandoff,
			Greet: func(stepID string) error {
				name, greeting := "another agent", ""
				if n := lastStep.Load(); n != nil {
					name = *n
				}
				if g := lastGreeting.Load(); g != nil {
					greeting = *g
				}
				return call.Agent.greetStep(stepID, name, greeting, ctxPool.AddMessage, func(text string) { speak(text) })
			},
		})
		if err != nil {
			return fail(fmt.Errorf("create router: %w", err))
		}
		call.Agent.Commit = router.Commit
		llmProc, ttsProc = llmSwap, ttsSwap
	}

	transcriptProc, err := coretranscript.NewProcessor(coretranscript.DefaultConfig())
	if err != nil {
		return fail(fmt.Errorf("create transcript finalizer: %w", err))
	}
	userCtxProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn", RequireCommittedTranscript: true, Sanitize: neutralizeNoteMarker})
	asstCfg := core.DefaultAssistantContextConfig()
	asstCfg.DisableTextCleaning = call.DisableTextCleaning
	asstCfg.FirstClauseMinLength = cfg.FirstClauseChars
	if m := cfg.TTSMerge; m.Enabled {
		if sc, ok := core.SlackDefaults(call.TTS.Provider); ok {
			if m.Margin > 0 {
				sc.Margin = m.Margin
			}
			if m.Cap > 0 {
				sc.Cap = m.Cap
			}
			if asstCfg.FirstClauseMinLength == 0 {
				asstCfg.FirstClauseMinLength = 40
			}
			asstCfg.SlackMerge = &sc
		}
	}
	asstCfg.Completion = completion
	if call.Summary.Enabled {
		if c, ok := llmProc.(textCompleter); ok {
			asstCfg.Summary = summaryConfig(call.Summary, call.MaxMessages, call.MaxTokens, c, r.onSummary)
		}
	}
	var turnProc, turnTap core.Processor
	var turnTyped *coreturn.Processor
	if tc.Manager || call.PushToTalk {
		mc := coreturn.DefaultConfig()
		var stop coreturn.TurnDetector
		if call.PushToTalk {
			// The button replaces voice detection for starting and ending turns,
			// so the smart-turn model, the stop chain and min_words are unused.
			// turn.mute strategies still apply: they are the operator's call.
			// MaxTurnDuration still caps a held button.
			mc.StartStrategies = []coreturn.UserTurnStartStrategy{coreturn.ExternalStart()}
			stop = coreturn.StopChain(coreturn.ExternalStop())
		} else {
			if tc.MinWords > 0 {
				mc.StartStrategies = []coreturn.UserTurnStartStrategy{coreturn.MinWordsStart(tc.MinWords, true)}
			}
			var err error
			if stop, turnTap, err = r.callStop(cfg, call); err != nil {
				return fail(err)
			}
		}
		mute, err := coreturn.ParseMuteStrategies(tc.Mute, muteTools)
		if err != nil {
			return fail(err)
		}
		opts := coreturn.ProcessorOptions{Detector: stop, Mute: mute, MaxMute: tc.MaxMute, OnMuteDrop: r.onUserMuteDrop}
		if tc.MinWords > 0 && !call.PushToTalk {
			stt, count := call.STT.Provider, r.onMinWordsNoInterims
			opts.OnFinalWithoutInterims = func() {
				slog.Warn("turn.min_words is on but the STT sent no interim results: turns start (and barge-in happens) only when the final transcript arrives; use an STT with interim results or disable turn.min_words",
					slog.String("stt", stt), slog.Int("min_words", tc.MinWords))
				if count != nil {
					count()
				}
			}
		}
		tp, err := coreturn.NewProcessor(mc, opts)
		if err != nil {
			return fail(fmt.Errorf("create turn processor: %w", err))
		}
		transcriptProc.ManageTurns()
		asstCfg.BargeInOnTurnEvent = true
		turnProc = tp
		turnTyped = tp
	}
	asstCtxProc := core.NewAssistantContextProcessor(ctxPool, asstCfg)
	speak = func(text string) {
		if err := asstCtxProc.Say(text); err != nil && call.OnGreetFail != nil {
			call.OnGreetFail(err) // a greeting never fails a call: the server counts and logs it
		}
	}
	if call.OnGreeter != nil {
		call.OnGreeter(speak)
	}
	asstActive = asstCtxProc.Active
	// An incomplete user turn waiting for its continuation is not idle time.
	held := func() bool { return false }
	if completion != nil {
		held = asstCtxProc.Held
	}
	if sink, ok := llmProc.(interface {
		SetContextProvider(func() []core.Message)
	}); ok {
		sink.SetContextProvider(func() []core.Message { return ctxPool.Messages() })
	}

	procs := []core.Processor{vadProc, sttProc, transcriptProc, userCtxProc, llmProc, asstCtxProc, ttsProc, &core.UnitFailureReporter{}}
	idleProc, err := idleCfg.stage(ctxPool, muteTools, turnTyped, asstCtxProc.Active, asstCtxProc.Say, held, call.Ender != nil)
	if err != nil {
		return fail(fmt.Errorf("create idle controller: %w", err))
	}
	if idleProc != nil { // before the async trigger and the model; zero cost when off
		procs = slices.Insert(procs, slices.Index(procs, core.Processor(llmProc)), core.Processor(idleProc))
	}
	if call.Ender != nil && idleProc != nil {
		call.Ender.End = func(stepID string) error {
			if !idleProc.RequestEnd(EndReasonAgent, endCallWait) {
				return errCallEnding
			}
			if call.OnEndCall != nil {
				call.OnEndCall(stepID)
			}
			return nil
		}
	}
	procs = slices.Insert(procs, slices.Index(procs, core.Processor(llmProc)), core.Processor(asyncTrigger))
	if dt.agg != nil {
		procs = slices.Insert(procs, 3, dt.agg) // directly before the user context
	}
	if turnProc != nil {
		// After STT (it must see transcripts) and before the finalizer.
		procs = slices.Insert(procs, 2, turnProc)
	}
	if turnTap != nil {
		// STT consumes audio frames, so the model's audio tap must precede it.
		procs = slices.Insert(procs, 1, turnTap)
	}
	if call.History != nil {
		procs = append(procs, call.History)
	}
	if dt.out != nil {
		procs = append(procs, dt.out)
	}
	if inFilter != nil {
		procs = slices.Insert(procs, 0, inFilter) // before VAD, after DTMF (inserted next)
	}
	if dt.in != nil {
		procs = slices.Insert(procs, 0, dt.in) // before VAD
	}
	if g, err := buildOutputGain(cfg, call.Audio); err != nil {
		return fail(fmt.Errorf("create output gain: %w", err))
	} else if g != nil {
		procs = append(procs, g) // last stage: scales speech and DTMF tones
	}
	pipe, err := core.NewPipeline(append(procs, transportOutput)...)
	if err != nil {
		return fail(fmt.Errorf("assemble pipeline: %w", err))
	}
	return pipe, nil
}

// SetTurnBackend installs the smart-turn model used by turn.stop: turn-analyzer.
func (r *Registry) SetTurnBackend(b smartturn.Backend) { r.turnBackend = b }

// BindMetrics copies circuit transitions onto the process metrics used by /ready.
func (r *Registry) BindMetrics(m *Metrics) {
	if m == nil {
		return
	}
	r.metrics.Store(m)
	r.onSTTFailover = m.stt.failovers.add
	r.onSTTFallbackSkipped = func() { m.stt.FallbacksSkippedTotal.Add(1) }
	r.onSTTReplayDropped = func(n int) { m.stt.ReplayDroppedBytesTotal.Add(uint64(n)) }
	r.onLLMFailover = m.llmFailovers.add
	r.onLLMFallbackSkipped = func() { m.LLMFallbacksSkippedTotal.Add(1) }
	r.onTTSFailover = m.ttsFailovers.add
	r.onTTSFallbackSkipped = func() { m.TTSFallbacksSkippedTotal.Add(1) }
	r.onUserMuteDrop = func() { m.UserMuteDroppedTotal.Add(1) }
	r.onMinWordsNoInterims = func() { m.MinWordsNoInterimsTotal.Add(1) }
	r.onAsyncTurn = func() { m.AsyncToolTurnsTotal.Add(1) }
	r.onAsyncDrop = func() { m.AsyncToolResultsDroppedTotal.Add(1) }
	r.onSummary = m.summary.observe
	r.onCompletion = m.completionVerdict
	r.onFilter = m.filters.observe
	// The fixed circuits show at 0 from the start; the others (nvidia) from first use.
	for _, p := range []string{"google", "xai", "echo", silenceProvider, "offline"} {
		for _, stage := range breakerSpecs[p].stages {
			if p != "offline" || r.Supports(p, stage) { // the local stack only for the stages this build has
				r.breaker(stage, p)
				m.circuits.ensure(circuitOf(stage, p))
			}
		}
	}
}

// breakerSpec is how a provider's breakers are keyed and reported.
type breakerSpec struct {
	stages   []string // the stages that have a breaker
	endpoint string   // the endpoint label of voice_<stage>_circuit_state; "" for none
	observed bool     // exports the gauge and reports transitions to call timelines (/ready, probes, timelines)
}

// breakerSpecs lists every provider stage with a breaker; openai has one per base URL (openaiBreaker),
// nvidia one per hosted endpoint. deepgram, cartesia and elevenlabs are not observed.
var breakerSpecs = map[string]breakerSpec{
	"google":        {[]string{"stt", "llm", "tts"}, "", true},
	"xai":           {[]string{"stt", "llm", "tts"}, "", true},
	"echo":          {[]string{"stt", "llm", "tts"}, "simulated", true},
	silenceProvider: {[]string{"stt", "tts"}, "text", true},
	"offline":       {[]string{"stt", "llm", "tts"}, "local", true},
	"nvidia":        {stages: []string{"stt", "llm", "tts"}, observed: true}, // endpoint: nvidiaEndpoint
	"deepgram":      {[]string{"stt"}, "", false},
	"cartesia":      {[]string{"stt", "tts"}, "", false},
	"elevenlabs":    {[]string{"stt", "tts"}, "", false},
}

// circuitOf is the circuit of a provider stage.
func circuitOf(stage, provider string) circuitKey {
	if provider == "nvidia" {
		return circuitKey{stage, provider, nvidiaEndpoint(stage)}
	}
	return circuitKey{stage, provider, breakerSpecs[provider].endpoint}
}

// breaker is the breaker of a provider stage (nil when it has none). Factories and failover
// both take it from here, so failover checks the breaker the processor records on.
func (r *Registry) breaker(stage, provider string) *resilience.Breaker {
	sp, ok := breakerSpecs[provider]
	if !ok || !slices.Contains(sp.stages, stage) {
		return nil
	}
	return r.keyed(circuitOf(stage, provider), sp.observed)
}

// openaiBreaker is the breaker of one openai-compatible base URL. It is not observed: the base
// URL is a tenant's and must not become a metric label, and call timelines key by provider only,
// so one tenant's outage would show on every openai call.
func (r *Registry) openaiBreaker(baseURL string) *resilience.Breaker {
	return r.keyed(circuitKey{"llm", "openai", baseURL}, false)
}

func (r *Registry) keyed(k circuitKey, observed bool) *resilience.Breaker {
	if b, ok := r.breakers.Load(k); ok {
		return b.(*resilience.Breaker)
	}
	b := resilience.NewBreaker(resilience.DefaultConfig())
	if observed { // before the store: no transition is missed, and a live state is never reset
		if m := r.metrics.Load(); m != nil {
			m.circuits.ensure(k)
		}
		b.SetObserver(func(st resilience.State) {
			if m := r.metrics.Load(); m != nil {
				m.circuits.set(k.stage, k.provider, k.endpoint, circuitCode(string(st)))
			}
			r.circuit(k.stage, k.provider, st)
		})
	}
	got, _ := r.breakers.LoadOrStore(k, b)
	return got.(*resilience.Breaker)
}

// circuit passes a breaker state report on to the server's call timelines.
func (r *Registry) circuit(stage, provider string, st resilience.State) {
	if h := r.onCircuit.Load(); h != nil {
		(*h)(stage, provider, st)
	}
}

func circuitCode(state string) int64 {
	switch state {
	case "OPEN":
		return 1
	case "HALF_OPEN":
		return 2
	default:
		return 0
	}
}
