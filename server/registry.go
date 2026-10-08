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
// Secret is the decrypted credential for this stage's provider only.
type StageSpec struct {
	Provider     string
	Model        string
	Language     string
	Voice        string
	SystemPrompt string
	Secret       []byte

	// LLM stage only: the agent's captured MCP tools and the call history
	// the tool loop records into.
	Tools        []core.ToolDefinition
	ToolExecutor core.ToolExecutor
	History      *core.ConversationContext
	// AsyncSink receives finished ToolPolicy.Async calls and OnAsyncStart
	// their acknowledgement (see tools.ToolLoopConfig).
	AsyncSink    func(core.AsyncToolResult)
	OnAsyncStart func(core.ToolCall)

	// Failover is set on a non-last candidate of a stage chain: its provider is
	// built with failoverPolicy (fewer, shorter attempts) so a fallback is
	// reached while the caller still waits.
	Failover bool
}

// toolLoopConfig is the default loop config with the stage's async hooks.
func (s StageSpec) toolLoopConfig() coretools.ToolLoopConfig {
	cfg := coretools.DefaultToolLoopConfig()
	cfg.AsyncSink, cfg.OnAsyncStart = s.AsyncSink, s.OnAsyncStart
	return cfg
}

// CallSpec is everything a call needs, resolved from a pinned agent version.
type CallSpec struct {
	STT, LLM, TTS StageSpec
	MaxMessages   int
	// AgentVersionID is the pinned agent version (trace attribute only).
	AgentVersionID string
	MaxTokens      int
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
}

// ProcessorFactory builds one stage processor. cfg supplies provider
// infrastructure settings only (project, region, endpoints, quotas); models,
// voices, prompts and credentials come from the agent via spec.
type ProcessorFactory func(cfg *Config, spec StageSpec) (core.Processor, error)

// Registry maps "<provider>.<stage>" to processor factories. Register only
// before serving calls; lookups are unsynchronised.
type Registry struct {
	factories map[string]ProcessorFactory

	googleLLMBreaker *resilience.Breaker
	googleTTSBreaker *resilience.Breaker
	googleSTTBreaker *resilience.Breaker
	xaiBreaker       *resilience.Breaker
	deepgramBreaker  *resilience.Breaker
	// One breaker per base URL: a tenant's broken vLLM must not open the
	// breaker for every other OpenAI-compatible endpoint.
	// ponytail: never evicted; bounded by the distinct base URLs stored.
	openaiBreakers       sync.Map // base URL -> *resilience.Breaker
	cartesiaBreaker      *resilience.Breaker
	cartesiaSTTBreaker   *resilience.Breaker
	elevenlabsBreaker    *resilience.Breaker
	elevenlabsSTTBreaker *resilience.Breaker

	nvidiaBreakers sync.Map // "llm|<base url>" / "stt|<endpoint>" -> *resilience.Breaker (nvidia.go)

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
}

func factoryKey(provider, stage string) string { return provider + "." + stage }

func NewRegistry() *Registry {
	r := &Registry{
		factories:            make(map[string]ProcessorFactory),
		googleLLMBreaker:     resilience.NewBreaker(resilience.DefaultConfig()),
		googleTTSBreaker:     resilience.NewBreaker(resilience.DefaultConfig()),
		googleSTTBreaker:     resilience.NewBreaker(resilience.DefaultConfig()),
		xaiBreaker:           resilience.NewBreaker(resilience.DefaultConfig()),
		deepgramBreaker:      resilience.NewBreaker(resilience.DefaultConfig()),
		cartesiaBreaker:      resilience.NewBreaker(resilience.DefaultConfig()),
		cartesiaSTTBreaker:   resilience.NewBreaker(resilience.DefaultConfig()),
		elevenlabsBreaker:    resilience.NewBreaker(resilience.DefaultConfig()),
		elevenlabsSTTBreaker: resilience.NewBreaker(resilience.DefaultConfig()),
	}

	r.Register("google.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		material, err := google.CredentialMaterialFromSecret(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("google.stt: %w", err)
		}
		sttCfg := google.DefaultSTTConfig()
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.LanguageCode = spec.Language
		}
		infra := cfg.Providers.GoogleSTT
		if infra.RotationThreshold > 0 {
			sttCfg.StreamRotationThreshold = infra.RotationThreshold
		}
		if infra.Location != "" {
			sttCfg.Location = infra.Location
		}
		sttCfg.ProjectID = infra.ProjectID
		if sttCfg.ProjectID == "" {
			sttCfg.ProjectID = material.ProjectID
		}
		sttCfg.CredentialsJSON, sttCfg.APIKey = material.ServiceAccountJSON, material.APIKey
		sttCfg.CircuitBreaker = r.googleSTTBreaker
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
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
		llmCfg := google.DefaultLLMConfig()
		llmCfg.Model, llmCfg.SystemPrompt = spec.Model, spec.SystemPrompt
		if len(spec.Tools) > 0 {
			llmCfg.ToolDefinitions, llmCfg.ToolExecutor, llmCfg.ToolHistory = spec.Tools, spec.ToolExecutor, spec.History
			llmCfg.ToolLoopConfig = spec.toolLoopConfig()
		}
		llmCfg.Client = client
		llmCfg.CircuitBreaker = r.googleLLMBreaker
		return google.NewLLMProcessor(llmCfg)
	})

	r.Register("google.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		material, err := google.CredentialMaterialFromSecret(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("google.tts: %w", err)
		}
		ttsCfg := google.DefaultTTSConfig()
		if spec.Voice != "" {
			ttsCfg.Voice = spec.Voice
		}
		if cfg.Providers.GoogleTTS.SampleRate > 0 {
			ttsCfg.SampleRate = cfg.Providers.GoogleTTS.SampleRate
		}
		ttsCfg.CredentialsJSON, ttsCfg.APIKey = material.ServiceAccountJSON, material.APIKey
		client, err := google.NewRealGoogleTTSClient(context.Background(), ttsCfg)
		if err != nil {
			return nil, fmt.Errorf("google.tts: %w", err)
		}
		ttsCfg.Client = client
		ttsCfg.CircuitBreaker = r.googleTTSBreaker
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		return google.NewTTSProcessor(ttsCfg)
	})

	r.Register("xai.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := apiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("xai.llm: %w", err)
		}
		xaiCfg := xai.Config{Model: spec.Model, Endpoint: cfg.Providers.XAILLM.Endpoint, SystemPrompt: spec.SystemPrompt, APIKey: key, CircuitBreaker: r.xaiBreaker}
		if len(spec.Tools) > 0 {
			loop, err := coretools.NewToolLoop(spec.Tools, spec.ToolExecutor, spec.History, spec.toolLoopConfig())
			if err != nil {
				return nil, fmt.Errorf("xai.llm tools: %w", err)
			}
			xaiCfg.ToolDefinitions, xaiCfg.ToolLoop = spec.Tools, loop
		}
		return xai.NewProcessor(xaiCfg)
	})

	// openai.llm covers OpenAI and any chat-completions server (Groq, Together,
	// vLLM). The stored credential is a bare API key (OpenAI) or JSON
	// {"api_key":"...","base_url":"https://api.groq.com/openai/v1"}.
	r.Register("openai.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, baseURL, err := openAICredential(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("openai.llm: %w", err)
		}
		br, _ := r.openaiBreakers.LoadOrStore(baseURL, resilience.NewBreaker(resilience.DefaultConfig()))
		oaCfg := openai.Config{Model: spec.Model, BaseURL: baseURL, SystemPrompt: spec.SystemPrompt, APIKey: key, CircuitBreaker: br.(*resilience.Breaker),
			AllowPrivate: cfg.Providers.OpenAILLM.AllowPrivateBaseURL}
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
		sttCfg.APIKey, sttCfg.CircuitBreaker = key, r.deepgramBreaker
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
		sttCfg.APIKey, sttCfg.CircuitBreaker = key, r.cartesiaSTTBreaker
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
		ttsCfg.APIKey, ttsCfg.CircuitBreaker = key, r.cartesiaBreaker
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
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
		sttCfg.APIKey, sttCfg.CircuitBreaker = key, r.elevenlabsSTTBreaker
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
		ttsCfg := elevenlabs.DefaultTTSConfig()
		if spec.Model != "" {
			ttsCfg.Model = spec.Model
		}
		if spec.Voice != "" {
			ttsCfg.Voice = spec.Voice
		}
		// The pipeline's TTS output rate is configured once, under the google TTS settings.
		if cfg.Providers.GoogleTTS.SampleRate > 0 {
			ttsCfg.SampleRate = cfg.Providers.GoogleTTS.SampleRate
		}
		ttsCfg.APIKey, ttsCfg.CircuitBreaker = key, r.elevenlabsBreaker
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		return elevenlabs.NewTTSProcessor(ttsCfg)
	})

	registerXAISpeech(r)
	r.registerNvidia()
	if registerFakeProviders != nil {
		registerFakeProviders(r) // -tags fakeproviders only (registry_fake.go)
	}

	return r
}

// registerFakeProviders is set only by the fakeproviders build (registry_fake.go).
var registerFakeProviders func(*Registry)

// apiKey validates a stored API key credential: a single non-empty token.
func apiKey(secret []byte) (string, error) {
	key := strings.TrimSpace(string(secret))
	if key == "" || strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("stored credential must be a single API key")
	}
	return key, nil
}

// openAICredential accepts a bare API key or {"api_key","base_url"} JSON. The
// base URL is chosen by whoever stored the credential (the agent owner).
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
		return "", "", errors.New("stored credential is not valid JSON")
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

// CallUserContextIndex is the position of the user context processor in a
// call pipeline; committed user turns (including client text) enter there.
const CallUserContextIndex = 3

// BuildCallPipeline builds VAD -> STT -> transcript -> user context -> LLM ->
// assistant context -> TTS -> transport for one call of a stored agent.
func (r *Registry) BuildCallPipeline(cfg *Config, call CallSpec, vadProc *vad.Processor, transportOutput core.Processor) (*core.Pipeline, error) {
	build := func(stage string, spec StageSpec) (core.Processor, error) {
		factory, ok := r.factories[factoryKey(spec.Provider, stage)]
		if !ok {
			return nil, fmt.Errorf("%s provider %q is not available", stage, spec.Provider)
		}
		proc, err := factory(cfg, spec)
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
	dt, err := buildDTMF(cfg, &call)
	if err != nil {
		return fail(fmt.Errorf("create dtmf processors: %w", err))
	}
	inFilter, err := buildInputFilter(cfg, r.onFilter)
	if err != nil {
		return fail(fmt.Errorf("create input filter: %w", err))
	}
	if inFilter != nil {
		built = append(built, inFilter)
	}
	llmSpec := call.LLM
	llmSpec.History = ctxPool
	// LLM turn-completion protocol (turn.stop: llm-completion): every candidate's
	// system prompt carries the marker instructions.
	completion := completionConfig(cfg.Turn.Stop, cfg.Turn.Completion, r.onCompletion)
	if completion != nil {
		llmSpec.SystemPrompt = completionPrompt(llmSpec.SystemPrompt, cfg.Turn.Completion)
		call.LLMFallbacks = slices.Clone(call.LLMFallbacks)
		for i := range call.LLMFallbacks {
			call.LLMFallbacks[i].SystemPrompt = completionPrompt(call.LLMFallbacks[i].SystemPrompt, cfg.Turn.Completion)
		}
	}
	// function_call mute: every LLM stage's tool executor reports in-flight calls.
	var muteTools *coreturn.ToolActivity
	if (cfg.Turn.Manager && slices.Contains(cfg.Turn.Mute, coreturn.MuteFunctionCall)) || cfg.Idle.enabled() { // idle also pauses while a tool runs
		muteTools = coreturn.NewToolActivity()
		llmSpec.ToolExecutor = muteTools.Wrap(llmSpec.ToolExecutor, llmSpec.Tools)
		call.LLMFallbacks = slices.Clone(call.LLMFallbacks)
		for i := range call.LLMFallbacks {
			call.LLMFallbacks[i].ToolExecutor = muteTools.Wrap(call.LLMFallbacks[i].ToolExecutor, call.LLMFallbacks[i].Tools)
		}
	}
	// Async tools: finished results go through a trigger stage before the LLM
	// that injects them and starts a model turn once the call is idle.
	var asyncTrigger *core.AsyncTrigger
	var asstActive func() bool
	if slices.ContainsFunc(llmSpec.Tools, func(d core.ToolDefinition) bool { return d.Policy.Async }) {
		asyncTrigger = core.NewAsyncTrigger(core.AsyncTriggerConfig{Busy: func() bool { return asstActive != nil && asstActive() },
			OnTriggered: r.onAsyncTurn, OnDropped: r.onAsyncDrop})
		llmSpec.AsyncSink = asyncTrigger.Deliver
		call.LLMFallbacks = slices.Clone(call.LLMFallbacks)
		for i := range call.LLMFallbacks {
			call.LLMFallbacks[i].AsyncSink = asyncTrigger.Deliver
		}
	}
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

	transcriptProc, err := coretranscript.NewProcessor(coretranscript.DefaultConfig())
	if err != nil {
		return fail(fmt.Errorf("create transcript finalizer: %w", err))
	}
	userCtxProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn", RequireCommittedTranscript: true})
	asstCfg := core.DefaultAssistantContextConfig()
	asstCfg.FirstClauseMinLength = cfg.FirstClauseChars
	asstCfg.Completion = completion
	if call.Summary.Enabled {
		if c, ok := llmProc.(textCompleter); ok {
			asstCfg.Summary = summaryConfig(call.Summary, call.MaxMessages, call.MaxTokens, c, r.onSummary)
		}
	}
	var turnProc, turnTap core.Processor
	var turnTyped *coreturn.Processor
	if cfg.Turn.Manager {
		mc := coreturn.DefaultConfig()
		if cfg.Turn.MinWords > 0 {
			mc.StartStrategies = []coreturn.UserTurnStartStrategy{coreturn.MinWordsStart(cfg.Turn.MinWords, true)}
		}
		var analyzer func(time.Duration) coreturn.UserTurnStopStrategy
		if slices.Contains(cfg.Turn.Stop, "turn-analyzer") && r.turnBackend != nil {
			a := smartturn.NewAnalyzer(r.turnBackend, smartturn.Config{Budget: cfg.Turn.Analyzer.Budget, Hold: cfg.Turn.Analyzer.StopSecs})
			turnTap = a.Tap()
			analyzer = func(base time.Duration) coreturn.UserTurnStopStrategy {
				return smartturn.Stop(a, base, cfg.Turn.Analyzer.StopSecs)
			}
		}
		stop, err := turnStopDetector(cfg.Turn.Stop, cfg.Turn.SpeechTimeout, cfg.Turn.EagerGrace, analyzer)
		if err != nil {
			return fail(err)
		}
		mute, err := coreturn.ParseMuteStrategies(cfg.Turn.Mute, muteTools)
		if err != nil {
			return fail(err)
		}
		opts := coreturn.ProcessorOptions{Detector: stop, Mute: mute, MaxMute: cfg.Turn.MaxMute, OnMuteDrop: r.onUserMuteDrop}
		if cfg.Turn.MinWords > 0 {
			stt, count := call.STT.Provider, r.onMinWordsNoInterims
			opts.OnFinalWithoutInterims = func() {
				slog.Warn("turn.min_words is on but the STT sent no interim results: turns start (and barge-in happens) only when the final transcript arrives; use an STT with interim results or disable turn.min_words",
					slog.String("stt", stt), slog.Int("min_words", cfg.Turn.MinWords))
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

	procs := []core.Processor{vadProc, sttProc, transcriptProc, userCtxProc, llmProc, asstCtxProc, ttsProc}
	idleProc, err := cfg.Idle.stage(ctxPool, muteTools, turnTyped, asstCtxProc.Active, held)
	if err != nil {
		return fail(fmt.Errorf("create idle controller: %w", err))
	}
	if idleProc != nil { // before the async trigger and the model; zero cost when off
		procs = slices.Insert(procs, slices.Index(procs, core.Processor(llmProc)), core.Processor(idleProc))
	}
	if asyncTrigger != nil {
		procs = slices.Insert(procs, slices.Index(procs, core.Processor(llmProc)), core.Processor(asyncTrigger))
	}
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
	if idleProc != nil {
		idleProc.SetSpeakIndex(slices.Index(procs, ttsProc)) // after every insertion above
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
	if g, err := buildOutputGain(cfg); err != nil {
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
	r.googleSTTBreaker.SetObserver(func(st resilience.State) {
		m.STTCircuitStateGoogle.Store(circuitCode(string(st)))
	})
	r.googleLLMBreaker.SetObserver(func(st resilience.State) {
		m.LLMCircuitStateGoogle.Store(circuitCode(string(st)))
	})
	r.googleTTSBreaker.SetObserver(func(st resilience.State) {
		m.TTSCircuitStateGoogle.Store(circuitCode(string(st)))
	})
	r.xaiBreaker.SetObserver(func(st resilience.State) {
		// One xAI breaker serves every xAI stage.
		m.LLMCircuitStateXAI.Store(circuitCode(string(st)))
		m.STTCircuitStateXAI.Store(circuitCode(string(st)))
		m.TTSCircuitStateXAI.Store(circuitCode(string(st)))
	})
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
