package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	mu         sync.RWMutex
	factories  map[string]ProcessorFactory
	live       map[string]bool
	assumeLive bool

	googleLLMBreaker *google.CircuitBreaker
	googleTTSBreaker *google.CircuitBreaker
	googleSTTBreaker *google.CircuitBreaker
	xaiBreaker       *xai.CircuitBreaker
}

// NewRegistry initializes the provider registry with default factories.
func NewRegistry() *Registry {
	r := &Registry{
		factories:        make(map[string]ProcessorFactory),
		live:             make(map[string]bool),
		googleLLMBreaker: google.NewCircuitBreaker(google.DefaultCircuitConfig()),
		googleTTSBreaker: google.NewCircuitBreaker(google.DefaultCircuitConfig()),
		googleSTTBreaker: google.NewCircuitBreaker(google.DefaultCircuitConfig()),
		xaiBreaker:       xai.NewCircuitBreaker(xai.DefaultCircuitConfig()),
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
		if cfg.Providers.GoogleSTT.CredentialsFile != "" {
			sttCfg.CredentialsFile = cfg.Providers.GoogleSTT.CredentialsFile
		}
		if cfg.Providers.GoogleSTT.ProjectID != "" {
			sttCfg.ProjectID = cfg.Providers.GoogleSTT.ProjectID
		}
		if cfg.Providers.GoogleSTT.Location != "" {
			sttCfg.Location = cfg.Providers.GoogleSTT.Location
		}
		if sttCfg.CredentialsFile == "" {
			if env := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); env != "" {
				sttCfg.CredentialsFile = env
			}
		}
		if sttCfg.ProjectID == "" && sttCfg.CredentialsFile != "" {
			if project := projectIDFromCredentials(sttCfg.CredentialsFile); project != "" {
				sttCfg.ProjectID = project
			}
		}
		if apiKey := os.Getenv("GOOGLE_API_KEY"); apiKey != "" {
			sttCfg.APIKey = apiKey
		}
		if sttCfg.CredentialsFile == "" && sttCfg.APIKey == "" {
			return nil, fmt.Errorf("google.stt: no credentials")
		}
		sttCfg.CircuitBreaker = r.googleSTTBreaker
		opener, err := google.NewRealGoogleSTTOpener(context.Background(), sttCfg)
		if err != nil {
			return nil, fmt.Errorf("google.stt: %w", err)
		}
		sttCfg.StreamOpener = opener
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
		apiKeyVar := cfg.Providers.GoogleLLM.APIKeyEnv
		if apiKeyVar == "" {
			apiKeyVar = "GOOGLE_API_KEY"
		}
		var creds *google.ResolvedCredentials
		var credErr error
		if apiKey := os.Getenv(apiKeyVar); apiKey != "" {
			creds, credErr = google.ResolveCredentials(google.CredentialConfig{
				Mode:   google.AuthModeGeminiAPIKey,
				APIKey: apiKey,
			})
		} else if credFile := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); credFile != "" {
			creds, credErr = google.ResolveCredentials(google.CredentialConfig{
				Mode:                   google.AuthModeVertexADC,
				CredentialsFile:        credFile,
				AllowInsecureFilePerms: cfg.InsecureOverrides.SkipCredentialPermsCheck,
				ProductionProfile:      cfg.Profile == ProfileProduction,
			})
		} else {
			return nil, fmt.Errorf("google.llm: no credentials (set %s or GOOGLE_APPLICATION_CREDENTIALS)", apiKeyVar)
		}
		if credErr != nil {
			return nil, fmt.Errorf("google.llm: %w", credErr)
		}
		client, err := google.NewRealGoogleLLMClient(context.Background(), creds)
		if err != nil {
			return nil, fmt.Errorf("google.llm: %w", err)
		}
		llmCfg.Client = client
		llmCfg.CircuitBreaker = r.googleLLMBreaker
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
		if cfg.Providers.GoogleTTS.CredentialsFile != "" {
			ttsCfg.CredentialsFile = cfg.Providers.GoogleTTS.CredentialsFile
		} else if env := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); env != "" {
			ttsCfg.CredentialsFile = env
		}
		if apiKey := os.Getenv("GOOGLE_API_KEY"); apiKey != "" {
			ttsCfg.APIKey = apiKey
		}
		if ttsCfg.CredentialsFile == "" && ttsCfg.APIKey == "" {
			return nil, fmt.Errorf("google.tts: no credentials")
		}
		client, err := google.NewRealGoogleTTSClient(context.Background(), ttsCfg)
		if err != nil {
			return nil, fmt.Errorf("google.tts: %w", err)
		}
		ttsCfg.Client = client
		ttsCfg.CircuitBreaker = r.googleTTSBreaker
		return google.NewTTSProcessor(ttsCfg)
	})

	r.Register("xai.llm", func(cfg *Config) (core.Processor, error) {
		model := cfg.Providers.XAILLM.Model
		if envModel := os.Getenv("XAI_MODEL"); envModel != "" {
			model = envModel
		}
		endpoint := cfg.Providers.XAILLM.Endpoint
		if envEndpoint := os.Getenv("XAI_ENDPOINT"); envEndpoint != "" {
			endpoint = envEndpoint
		}
		xaiCfg := xai.Config{
			Model:        model,
			Endpoint:     endpoint,
			SystemPrompt: cfg.Providers.XAILLM.SystemPrompt,
		}
		apiKeyVar := cfg.Providers.XAILLM.APIKeyEnv
		if apiKeyVar == "" {
			apiKeyVar = "XAI_API_KEY"
		}
		xaiCfg.APIKey = os.Getenv(apiKeyVar)
		if xaiCfg.APIKey == "" {
			return nil, fmt.Errorf("xai.llm: %s is empty", apiKeyVar)
		}
		xaiCfg.CircuitBreaker = r.xaiBreaker
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
	if sink, ok := llmProc.(interface {
		SetContextProvider(func() []core.Message)
	}); ok {
		sink.SetContextProvider(func() []core.Message { return ctxPool.Messages() })
	}

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

// Prepare constructs every configured provider once. Production startup must
// fail if this returns an error. A successful provider is marked live.
func (r *Registry) Prepare(cfg *Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	seen := map[string]struct{}{}
	var errs []error
	for _, p := range cfg.Pipelines {
		for _, name := range []string{p.STT, p.LLM, p.TTS} {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			factory, ok := r.factories[name]
			if !ok {
				r.live[name] = false
				errs = append(errs, fmt.Errorf("provider %q is not registered", name))
				continue
			}
			proc, err := factory(cfg)
			if err != nil || proc == nil {
				r.live[name] = false
				if err == nil {
					err = fmt.Errorf("provider %q returned nil", name)
				}
				errs = append(errs, err)
				continue
			}
			r.live[name] = true
			if closer, ok := proc.(interface{ Close() error }); ok {
				_ = closer.Close()
			}
		}
	}
	if r.pipelinesLiveLocked(cfg) {
		return nil
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// BindMetrics copies circuit transitions onto the process metrics used by /ready.
func (r *Registry) BindMetrics(m *Metrics) {
	if m == nil {
		return
	}
	r.googleSTTBreaker.SetObserver(func(st google.CircuitState) {
		m.STTCircuitStateGoogle.Store(circuitCode(string(st)))
	})
	r.googleLLMBreaker.SetObserver(func(st google.CircuitState) {
		m.LLMCircuitStateGoogle.Store(circuitCode(string(st)))
	})
	r.googleTTSBreaker.SetObserver(func(st google.CircuitState) {
		m.TTSCircuitStateGoogle.Store(circuitCode(string(st)))
	})
	r.xaiBreaker.SetObserver(func(st xai.CircuitState) {
		m.LLMCircuitStateXAI.Store(circuitCode(string(st)))
	})
}

func (r *Registry) pipelinesLiveLocked(cfg *Config) bool {
	if r.assumeLive {
		return true
	}
	for _, p := range cfg.Pipelines {
		if r.live[p.STT] && r.live[p.LLM] && r.live[p.TTS] {
			return true
		}
	}
	return false
}

// PipelinesLive reports whether every provider in every pipeline initialized.
func (r *Registry) PipelinesLive(cfg *Config) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pipelinesLiveLocked(cfg)
}

func (r *Registry) pipelineProvidersLive(p PipelineConfig) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.assumeLive {
		return true
	}
	return r.live[p.STT] && r.live[p.LLM] && r.live[p.TTS]
}

func projectIDFromCredentials(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return ""
	}
	return doc.ProjectID
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
