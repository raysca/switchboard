package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"govoice/core"
	"govoice/core/resilience"
	coretools "govoice/core/tools"
	coretranscript "govoice/core/transcript"
	"govoice/providers/google"
	"govoice/providers/xai"
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
}

// CallSpec is everything a call needs, resolved from a pinned agent version.
type CallSpec struct {
	STT, LLM, TTS StageSpec
	MaxMessages   int
	MaxTokens     int
	// History, when set, sees every frame just before the transport (turn rows).
	History core.Processor
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
}

func factoryKey(provider, stage string) string { return provider + "." + stage }

func NewRegistry() *Registry {
	r := &Registry{
		factories:        make(map[string]ProcessorFactory),
		googleLLMBreaker: resilience.NewBreaker(resilience.DefaultConfig()),
		googleTTSBreaker: resilience.NewBreaker(resilience.DefaultConfig()),
		googleSTTBreaker: resilience.NewBreaker(resilience.DefaultConfig()),
		xaiBreaker:       resilience.NewBreaker(resilience.DefaultConfig()),
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
			llmCfg.ToolLoopConfig = coretools.DefaultToolLoopConfig()
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
		return google.NewTTSProcessor(ttsCfg)
	})

	r.Register("xai.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		key, err := xaiKey(spec.Secret)
		if err != nil {
			return nil, fmt.Errorf("xai.llm: %w", err)
		}
		xaiCfg := xai.Config{Model: spec.Model, Endpoint: cfg.Providers.XAILLM.Endpoint, SystemPrompt: spec.SystemPrompt, APIKey: key, CircuitBreaker: r.xaiBreaker}
		if len(spec.Tools) > 0 {
			loop, err := coretools.NewToolLoop(spec.Tools, spec.ToolExecutor, spec.History, coretools.DefaultToolLoopConfig())
			if err != nil {
				return nil, fmt.Errorf("xai.llm tools: %w", err)
			}
			xaiCfg.ToolDefinitions, xaiCfg.ToolLoop = spec.Tools, loop
		}
		return xai.NewProcessor(xaiCfg)
	})

	registerXAISpeech(r)

	return r
}

// xaiKey validates a stored xAI API key: a single non-empty token.
func xaiKey(secret []byte) (string, error) {
	key := strings.TrimSpace(string(secret))
	if key == "" || strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("stored xAI credential must be a single API key")
	}
	return key, nil
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
		return factory(cfg, spec)
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
	sttProc, err := build("stt", call.STT)
	if err != nil {
		return fail(fmt.Errorf("create stt processor: %w", err))
	}
	built = append(built, sttProc)
	llmSpec := call.LLM
	llmSpec.History = ctxPool
	llmProc, err := build("llm", llmSpec)
	if err != nil {
		return fail(fmt.Errorf("create llm processor: %w", err))
	}
	built = append(built, llmProc)
	ttsProc, err := build("tts", call.TTS)
	if err != nil {
		return fail(fmt.Errorf("create tts processor: %w", err))
	}
	built = append(built, ttsProc)

	transcriptProc, err := coretranscript.NewProcessor(coretranscript.DefaultConfig())
	if err != nil {
		return fail(fmt.Errorf("create transcript finalizer: %w", err))
	}
	userCtxProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn", RequireCommittedTranscript: true})
	asstCtxProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())
	if sink, ok := llmProc.(interface {
		SetContextProvider(func() []core.Message)
	}); ok {
		sink.SetContextProvider(func() []core.Message { return ctxPool.Messages() })
	}

	procs := []core.Processor{vadProc, sttProc, transcriptProc, userCtxProc, llmProc, asstCtxProc, ttsProc}
	if call.History != nil {
		procs = append(procs, call.History)
	}
	pipe, err := core.NewPipeline(append(procs, transportOutput)...)
	if err != nil {
		return fail(fmt.Errorf("assemble pipeline: %w", err))
	}
	return pipe, nil
}

// BindMetrics copies circuit transitions onto the process metrics used by /ready.
func (r *Registry) BindMetrics(m *Metrics) {
	if m == nil {
		return
	}
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
