package server

import (
	"context"
	"testing"

	"govoice/core"
	coretranscript "govoice/core/transcript"
	"govoice/providers/google"
	"govoice/providers/xai"
	"govoice/vad"
)

// fakeTransportSink implements core.Processor for testing pipeline building.
type fakeTransportSink struct{}

func (f *fakeTransportSink) ProcessFrame(ctx context.Context, frame core.Frame, dir core.Direction, e core.Emitter) error {
	return nil
}
func (f *fakeTransportSink) Attach(s core.SessionController, index int) error { return nil }

type contextSinkProc struct {
	provider func() []core.Message
}

func (p *contextSinkProc) ProcessFrame(ctx context.Context, frame core.Frame, dir core.Direction, e core.Emitter) error {
	return nil
}
func (p *contextSinkProc) SetContextProvider(fn func() []core.Message) { p.provider = fn }

func TestRegistry_BuildPipelineGoogleAndMixed(t *testing.T) {
	cfg, err := LoadConfig("../config/development.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	reg := NewRegistry()
	mockExec := &mockVADExecutor{ready: true, available: 4, total: 4}
	vadProc := vad.NewProcessor(mockExec, vad.DefaultProcessorConfig())
	sink := &fakeTransportSink{}

	// Missing credentials must fail closed rather than returning a nil-backed processor.
	if _, _, err := reg.BuildPipeline(cfg, "default", vadProc, sink); err == nil {
		t.Fatal("expected default pipeline to fail without provider credentials")
	}

	stub := func(c *Config) (core.Processor, error) { return &fakeTransportSink{}, nil }
	reg.Register("google.stt", stub)
	reg.Register("google.llm", func(c *Config) (core.Processor, error) { return &contextSinkProc{}, nil })
	reg.Register("google.tts", stub)
	reg.Register("xai.llm", func(c *Config) (core.Processor, error) { return &contextSinkProc{}, nil })

	// 1. Build default (Google STT -> Google LLM -> Google TTS) pipeline
	pipeDefault, ctxPoolDefault, err := reg.BuildPipeline(cfg, "default", vadProc, sink)
	if err != nil {
		t.Fatalf("build default pipeline: %v", err)
	}
	if pipeDefault == nil || ctxPoolDefault == nil {
		t.Fatal("expected non-nil pipeline and conversation context")
	}

	// 2. Build mixed (Google STT -> xAI LLM -> Google TTS) pipeline
	pipeXAI, ctxPoolXAI, err := reg.BuildPipeline(cfg, "xai", vadProc, sink)
	if err != nil {
		t.Fatalf("build xai pipeline: %v", err)
	}
	if pipeXAI == nil || ctxPoolXAI == nil {
		t.Fatal("expected non-nil xai pipeline and conversation context")
	}

	// 3. Programmatic construction equivalence
	// Programmatic core API directly constructs processors:
	sttProc, err := google.NewSTTProcessor(google.DefaultSTTConfig())
	if err != nil {
		t.Fatalf("programmatic stt: %v", err)
	}
	progCtx := core.NewConversationContext(core.ContextConfig{MaxMessages: 20, MaxTokens: 4000})
	transcriptCfg := coretranscript.DefaultConfig()
	transcriptCfg.GracePeriod = 0
	transcriptProc, _ := coretranscript.NewProcessor(transcriptCfg)
	userCtx := core.NewUserContextProcessor(progCtx, core.UserContextConfig{TurnPrefix: "turn", RequireCommittedTranscript: true})
	llmProc, err := google.NewLLMProcessor(google.DefaultLLMConfig())
	if err != nil {
		t.Fatalf("programmatic llm: %v", err)
	}
	asstCtx := core.NewAssistantContextProcessor(progCtx, core.DefaultAssistantContextConfig())
	ttsProc, err := google.NewTTSProcessor(google.DefaultTTSConfig())
	if err != nil {
		t.Fatalf("programmatic tts: %v", err)
	}

	progPipe, err := core.NewPipeline(vadProc, sttProc, transcriptProc, userCtx, llmProc, asstCtx, ttsProc, sink)
	if err != nil {
		t.Fatalf("programmatic pipeline: %v", err)
	}

	if progPipe == nil {
		t.Fatal("expected non-nil programmatic pipeline")
	}

	// 4. Test error handling for nonexistent pipeline
	_, _, err = reg.BuildPipeline(cfg, "nonexistent", vadProc, sink)
	if err == nil {
		t.Fatal("expected error for nonexistent pipeline")
	}

	// 5. Test error handling for missing provider factory
	cfg.Pipelines["broken"] = PipelineConfig{
		STT:         "unknown.stt",
		LLM:         "google.llm",
		TTS:         "google.tts",
		MaxMessages: 10,
		MaxTokens:   1000,
	}
	_, _, err = reg.BuildPipeline(cfg, "broken", vadProc, sink)
	if err == nil {
		t.Fatal("expected error for missing provider factory")
	}

	// 6. Test custom factory registration
	reg.Register("custom.llm", func(c *Config) (core.Processor, error) {
		return xai.NewProcessor(xai.Config{Model: "custom-model", Endpoint: "http://localhost"})
	})
	cfg.Pipelines["custom"] = PipelineConfig{
		STT:         "google.stt",
		LLM:         "custom.llm",
		TTS:         "google.tts",
		MaxMessages: 10,
		MaxTokens:   1000,
	}
	pipeCustom, _, err := reg.BuildPipeline(cfg, "custom", vadProc, sink)
	if err != nil {
		t.Fatalf("build custom pipeline: %v", err)
	}
	if pipeCustom == nil {
		t.Fatal("expected non-nil custom pipeline")
	}
}
