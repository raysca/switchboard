package server

import (
	"fmt"
	"sync"

	"govoice/core"
	"govoice/providers/google"
	"govoice/providers/xai"
	"govoice/vad"
)

// ProcessorFactory instantiates a core.Processor for a specific provider.
type ProcessorFactory func(cfg *Config) (core.Processor, error)

// Registry manages available provider factories and builds pipelines.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]ProcessorFactory
}

// NewRegistry initializes the provider registry with default factories.
func NewRegistry() *Registry {
	r := &Registry{
		factories: make(map[string]ProcessorFactory),
	}

	// Register default production providers
	r.Register("google.stt", func(cfg *Config) (core.Processor, error) {
		sttCfg := google.DefaultSTTConfig()
		if cfg.Providers.GoogleSTT.Model != "" {
			sttCfg.Model = cfg.Providers.GoogleSTT.Model
		}
		if cfg.Providers.GoogleSTT.LanguageCode != "" {
			sttCfg.LanguageCode = cfg.Providers.GoogleSTT.LanguageCode
		}
		if cfg.Providers.GoogleSTT.RotationThreshold > 0 {
			sttCfg.StreamRotationThreshold = cfg.Providers.GoogleSTT.RotationThreshold
		}
		return google.NewSTTProcessor(sttCfg)
	})

	r.Register("google.llm", func(cfg *Config) (core.Processor, error) {
		llmCfg := google.DefaultLLMConfig()
		if cfg.Providers.GoogleLLM.Model != "" {
			llmCfg.Model = cfg.Providers.GoogleLLM.Model
		}
		if cfg.Providers.GoogleLLM.SystemPrompt != "" {
			llmCfg.SystemPrompt = cfg.Providers.GoogleLLM.SystemPrompt
		}
		return google.NewLLMProcessor(llmCfg)
	})

	r.Register("google.tts", func(cfg *Config) (core.Processor, error) {
		ttsCfg := google.DefaultTTSConfig()
		if cfg.Providers.GoogleTTS.Voice != "" {
			ttsCfg.Voice = cfg.Providers.GoogleTTS.Voice
		}
		if cfg.Providers.GoogleTTS.SampleRate > 0 {
			ttsCfg.SampleRate = cfg.Providers.GoogleTTS.SampleRate
		}
		return google.NewTTSProcessor(ttsCfg)
	})

	r.Register("xai.llm", func(cfg *Config) (core.Processor, error) {
		xaiCfg := xai.Config{
			Model:        cfg.Providers.XAILLM.Model,
			Endpoint:     cfg.Providers.XAILLM.Endpoint,
			SystemPrompt: cfg.Providers.XAILLM.SystemPrompt,
		}
		return xai.NewProcessor(xaiCfg)
	})

	return r
}

// Register adds or overrides a provider processor factory.
func (r *Registry) Register(name string, factory ProcessorFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
}

// BuildPipeline constructs the programmatic pipeline for a named pipeline configuration.
func (r *Registry) BuildPipeline(
	cfg *Config,
	pipelineName string,
	vadProc *vad.Processor,
	transportOutput core.Processor,
) (*core.Pipeline, *core.ConversationContext, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	pCfg, ok := cfg.Pipelines[pipelineName]
	if !ok {
		return nil, nil, fmt.Errorf("pipeline configuration %q not found", pipelineName)
	}

	sttFactory, ok := r.factories[pCfg.STT]
	if !ok {
		return nil, nil, fmt.Errorf("stt provider factory %q not registered", pCfg.STT)
	}
	llmFactory, ok := r.factories[pCfg.LLM]
	if !ok {
		return nil, nil, fmt.Errorf("llm provider factory %q not registered", pCfg.LLM)
	}
	ttsFactory, ok := r.factories[pCfg.TTS]
	if !ok {
		return nil, nil, fmt.Errorf("tts provider factory %q not registered", pCfg.TTS)
	}

	sttProc, err := sttFactory(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create stt processor: %w", err)
	}
	llmProc, err := llmFactory(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create llm processor: %w", err)
	}
	ttsProc, err := ttsFactory(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create tts processor: %w", err)
	}

	// Build context and bounded context processors
	ctxPool := core.NewConversationContext(core.ContextConfig{
		MaxMessages: pCfg.MaxMessages,
		MaxTokens:   pCfg.MaxTokens,
	})
	userCtxProc := core.NewUserContextProcessor(ctxPool, core.UserContextConfig{TurnPrefix: "turn"})
	asstCtxProc := core.NewAssistantContextProcessor(ctxPool, core.DefaultAssistantContextConfig())

	// Assemble pipeline: VAD -> STT -> UserContext -> LLM -> AssistantContext -> TTS -> TransportOutput
	pipe, err := core.NewPipeline(
		vadProc,
		sttProc,
		userCtxProc,
		llmProc,
		asstCtxProc,
		ttsProc,
		transportOutput,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("assemble pipeline: %w", err)
	}

	return pipe, ctxPool, nil
}
