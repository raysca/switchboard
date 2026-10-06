package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"govoice/core"
	coretranscript "govoice/core/transcript"
	"govoice/vad"
)

// fakeTransportSink implements core.Processor for testing pipeline building.
type fakeTransportSink struct{}

func (f *fakeTransportSink) ProcessFrame(ctx context.Context, frame core.Frame, dir core.Direction, e core.Emitter) error {
	return nil
}

func TestTranscriptDeadlinePipelineDelayedFinalInterimAndEmptyTurns(t *testing.T) {
	for _, tc := range []struct {
		name string
		post func(*core.Session)
		want string
	}{
		{"delayed-final", func(s *core.Session) {
			_ = s.Post(&core.TranscriptCandidateFrame{Text: "interim", Turn: "A", Gen: 1, Timestamp: time.Unix(1, 0)})
			_ = s.Post(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "A", Gen: 1, At: time.Unix(2, 0)})
			time.Sleep(3 * time.Millisecond)
			_ = s.Post(&core.TranscriptCandidateFrame{Text: "better final", Turn: "A", Gen: 1, Timestamp: time.Unix(3, 0), Final: true})
		}, "better final"},
		{"interim-only", func(s *core.Session) {
			_ = s.Post(&core.TranscriptCandidateFrame{Text: "fallback", Turn: "B", Gen: 1, Timestamp: time.Unix(4, 0)})
			_ = s.Post(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "B", Gen: 1, At: time.Unix(5, 0)})
		}, "fallback"},
		{"empty", func(s *core.Session) {
			_ = s.Post(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "C", Gen: 1, At: time.Unix(6, 0)})
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := coretranscript.DefaultConfig()
			cfg.GracePeriod = 5 * time.Millisecond
			cfg.FinalizationTimeout = 15 * time.Millisecond
			finalizer, _ := coretranscript.NewProcessor(cfg)
			conversation := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
			user := core.NewUserContextProcessor(conversation, core.UserContextConfig{RequireCommittedTranscript: true})
			sink := &core.PassthroughProcessor{}
			pipe, _ := core.NewPipeline(finalizer, user, sink)
			session, _ := core.NewSession(920, pipe, core.DefaultResourceBudget())
			var mu sync.Mutex
			var messages []string
			session.OnOutput = func(frame core.Frame) {
				if msg, ok := frame.(*core.UserMessageFrame); ok {
					mu.Lock()
					messages = append(messages, msg.Content)
					mu.Unlock()
				}
			}
			_ = session.Start()
			defer session.Close()
			tc.post(session)
			time.Sleep(50 * time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			if tc.want == "" && len(messages) != 0 {
				t.Fatalf("empty turn emitted user message: %v", messages)
			}
			if tc.want != "" && (len(messages) != 1 || messages[0] != tc.want) {
				t.Fatalf("messages=%v want one %q", messages, tc.want)
			}
		})
	}
}
func (f *fakeTransportSink) Attach(s core.SessionController, index int) error { return nil }

type contextSinkProc struct {
	provider func() []core.Message
}

func (p *contextSinkProc) ProcessFrame(ctx context.Context, frame core.Frame, dir core.Direction, e core.Emitter) error {
	return nil
}
func (p *contextSinkProc) SetContextProvider(fn func() []core.Message) { p.provider = fn }

func TestRegistry_BuildCallPipelineFromAgentSpec(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	reg := NewRegistry()
	mockExec := &mockVADExecutor{ready: true, available: 4, total: 4}
	vadProc := vad.NewProcessor(mockExec, vad.DefaultProcessorConfig())
	sink := &fakeTransportSink{}
	call := CallSpec{
		STT:         StageSpec{Provider: "google", Model: "chirp_2", Language: "en-US"},
		LLM:         StageSpec{Provider: "xai", Model: "grok-test", SystemPrompt: "be brief"},
		TTS:         StageSpec{Provider: "google", Voice: "en-US-Journey-F"},
		MaxMessages: 10, MaxTokens: 1000,
	}

	// Real factories fail closed without a stored credential.
	if _, _, err := reg.BuildCallPipeline(cfg, call, vadProc, sink); err == nil {
		t.Fatal("expected build to fail without provider credentials")
	}
	call.STT.Secret, call.LLM.Secret, call.TTS.Secret = []byte("$ENV_REF"), []byte("bad key with spaces"), nil
	if _, _, err := reg.BuildCallPipeline(cfg, call, vadProc, sink); err == nil {
		t.Fatal("expected build to reject unusable secrets")
	}

	// Factories receive exactly the agent's stage settings and credential.
	got := map[string]StageSpec{}
	stub := func(stage string, proc core.Processor) ProcessorFactory {
		return func(c *Config, spec StageSpec) (core.Processor, error) { got[stage] = spec; return proc, nil }
	}
	llm := &contextSinkProc{}
	reg.Register("google.stt", stub("stt", &fakeTransportSink{}))
	reg.Register("xai.llm", stub("llm", llm))
	reg.Register("google.tts", stub("tts", &fakeTransportSink{}))
	call.STT.Secret, call.LLM.Secret, call.TTS.Secret = []byte("stt-key"), []byte("llm-key"), []byte("tts-key")
	call.ToolDefinitions = []core.ToolDefinition{{Name: "lookup"}}
	pipe, ctxPool, err := reg.BuildCallPipeline(cfg, call, vadProc, sink)
	if err != nil || pipe == nil || ctxPool == nil {
		t.Fatalf("build: pipe=%v ctx=%v err=%v", pipe, ctxPool, err)
	}
	if got["stt"].Model != "chirp_2" || got["stt"].Language != "en-US" || string(got["stt"].Secret) != "stt-key" {
		t.Fatalf("stt spec = %+v", got["stt"])
	}
	if got["llm"].Model != "grok-test" || got["llm"].SystemPrompt != "be brief" || string(got["llm"].Secret) != "llm-key" ||
		len(got["llm"].Tools) != 1 || got["llm"].History != ctxPool {
		t.Fatalf("llm spec = %+v", got["llm"])
	}
	if got["tts"].Voice != "en-US-Journey-F" || string(got["tts"].Secret) != "tts-key" {
		t.Fatalf("tts spec = %+v", got["tts"])
	}
	if llm.provider == nil {
		t.Fatal("LLM did not receive the conversation context provider")
	}

	// A provider with no factory for a stage is rejected.
	call.TTS.Provider = "acme"
	if _, _, err := reg.BuildCallPipeline(cfg, call, vadProc, sink); err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if reg.Supports("xai", "tts") != xaiSpeechEnabled {
		t.Fatalf("xAI TTS availability must follow the xaispeech build tag (%v)", xaiSpeechEnabled)
	}
}

type closeCounter struct {
	fakeTransportSink
	closed *int
}

func (c closeCounter) Close() error { *c.closed++; return nil }

// A later stage failing must close the provider clients already built.
func TestBuildCallPipelineClosesBuiltStagesOnFailure(t *testing.T) {
	reg := NewRegistry()
	closed := 0
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &closeCounter{closed: &closed}, nil })
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return &closeCounter{closed: &closed}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return nil, errors.New("bad credential") })
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}}
	if _, _, err := reg.BuildCallPipeline(DefaultConfig(ProfileDevelopment), call, vadProc, &fakeTransportSink{}); err == nil {
		t.Fatal("expected build failure")
	}
	if closed != 2 {
		t.Fatalf("closed %d built stages, want 2", closed)
	}
}

func TestCallUserContextIndexMatchesPipeline(t *testing.T) {
	reg := NewRegistry()
	for _, stage := range []string{"stt", "llm", "tts"} {
		reg.Register("google."+stage, func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })
	}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}}
	pipe, _, err := reg.BuildCallPipeline(DefaultConfig(ProfileDevelopment), call, vadProc, &fakeTransportSink{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pipe.Processors()[CallUserContextIndex].(*core.UserContextProcessor); !ok {
		t.Fatalf("processor %d is %T, want *core.UserContextProcessor", CallUserContextIndex, pipe.Processors()[CallUserContextIndex])
	}
}
