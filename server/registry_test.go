package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	coretranscript "govoice/core/transcript"
	coreturn "govoice/core/turn"
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
	if _, err := reg.BuildCallPipeline(cfg, call, vadProc, sink); err == nil {
		t.Fatal("expected build to fail without provider credentials")
	}
	call.STT.Secret, call.LLM.Secret, call.TTS.Secret = []byte("$ENV_REF"), []byte("bad key with spaces"), nil
	if _, err := reg.BuildCallPipeline(cfg, call, vadProc, sink); err == nil {
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
	call.LLM.Tools = []core.ToolDefinition{{Name: "lookup"}}
	pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, sink)
	if err != nil || pipe == nil {
		t.Fatalf("build: pipe=%v err=%v", pipe, err)
	}
	if got["stt"].Model != "chirp_2" || got["stt"].Language != "en-US" || string(got["stt"].Secret) != "stt-key" {
		t.Fatalf("stt spec = %+v", got["stt"])
	}
	if got["llm"].Model != "grok-test" || got["llm"].SystemPrompt != "be brief" || string(got["llm"].Secret) != "llm-key" ||
		len(got["llm"].Tools) != 1 || got["llm"].History == nil {
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
	if _, err := reg.BuildCallPipeline(cfg, call, vadProc, sink); err == nil {
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
	if _, err := reg.BuildCallPipeline(DefaultConfig(ProfileDevelopment), call, vadProc, &fakeTransportSink{}); err == nil {
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
	pipe, err := reg.BuildCallPipeline(DefaultConfig(ProfileDevelopment), call, vadProc, &fakeTransportSink{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := pipe.Processors()[CallUserContextIndex].(*core.UserContextProcessor); !ok {
		t.Fatalf("processor %d is %T, want *core.UserContextProcessor", CallUserContextIndex, pipe.Processors()[CallUserContextIndex])
	}
}

func TestTurnManagerPipelineInstallsProcessorAfterSTT(t *testing.T) {
	reg := NewRegistry()
	for _, stage := range []string{"stt", "llm", "tts"} {
		reg.Register("google."+stage, func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })
	}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}}
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Turn.Manager, cfg.Turn.MinWords = true, 3
	pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, &fakeTransportSink{})
	if err != nil {
		t.Fatal(err)
	}
	procs := pipe.Processors()
	if _, ok := procs[2].(*coreturn.Processor); !ok {
		t.Fatalf("processor 2 is %T, want *turn.Processor right after STT", procs[2])
	}
	if _, ok := procs[4].(*core.UserContextProcessor); !ok {
		t.Fatalf("processor 4 is %T, want user context", procs[4])
	}
}

func TestDeepgramSTTFactoryRegisteredAndValidatesKey(t *testing.T) {
	reg := NewRegistry()
	if !reg.Supports("deepgram", "stt") || reg.Supports("deepgram", "llm") {
		t.Fatal("deepgram.stt only")
	}
	f := reg.factories["deepgram.stt"]
	if _, err := f(&Config{}, StageSpec{Secret: []byte("two words")}); err == nil {
		t.Fatal("malformed credential accepted")
	}
	p, err := f(&Config{}, StageSpec{Secret: []byte("dg-test-key-0123456789")})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.(interface{ Close() error }).Close()
}

func TestElevenLabsSTTFactoryRegisteredAndValidatesKey(t *testing.T) {
	reg := NewRegistry()
	if !reg.Supports("elevenlabs", "stt") || !reg.Supports("elevenlabs", "tts") || reg.Supports("elevenlabs", "llm") {
		t.Fatal("elevenlabs offers stt and tts")
	}
	if reg.sttBreaker("elevenlabs") == nil || reg.sttBreaker("elevenlabs") == reg.elevenlabsBreaker {
		t.Fatal("elevenlabs STT needs its own breaker")
	}
	f := reg.factories["elevenlabs.stt"]
	if _, err := f(&Config{}, StageSpec{Secret: []byte("two words")}); err == nil {
		t.Fatal("malformed credential accepted")
	}
	p, err := f(&Config{}, StageSpec{Secret: []byte("xi-test-key-0123456789"), Failover: true, Language: "en-US"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(core.STTCounters); !ok {
		t.Fatal("elevenlabs STT must export counters")
	}
	_ = p.(interface{ Close() error }).Close()
}

func TestElevenLabsTTSFactoryRegisteredAndValidatesKey(t *testing.T) {
	reg := NewRegistry()
	if !reg.Supports("elevenlabs", "tts") || reg.Supports("elevenlabs", "llm") {
		t.Fatal("elevenlabs.tts only")
	}
	f := reg.factories["elevenlabs.tts"]
	if _, err := f(&Config{}, StageSpec{Secret: []byte("two words")}); err == nil {
		t.Fatal("malformed credential accepted")
	}
	bad := &Config{}
	bad.Providers.GoogleTTS.SampleRate = 12345
	if _, err := f(bad, StageSpec{Secret: []byte("xi-test-key-0123456789")}); err == nil {
		t.Fatal("unsupported sample rate accepted")
	}
	p, err := f(&Config{}, StageSpec{Secret: []byte("xi-test-key-0123456789")})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.(interface{ Close() error }).Close()
}

func TestCartesiaTTSFactoryRegisteredAndValidatesKey(t *testing.T) {
	reg := NewRegistry()
	if !reg.Supports("cartesia", "tts") || reg.Supports("cartesia", "llm") {
		t.Fatal("cartesia.tts only")
	}
	f := reg.factories["cartesia.tts"]
	if _, err := f(&Config{}, StageSpec{Secret: []byte("two words")}); err == nil {
		t.Fatal("malformed credential accepted")
	}
	p, err := f(&Config{}, StageSpec{Secret: []byte("cartesia-test-key-0123456789")})
	if err != nil {
		t.Fatal(err)
	}
	_ = p.(interface{ Close() error }).Close()
}

func TestCartesiaSTTFactoryRegisteredAndValidatesKey(t *testing.T) {
	reg := NewRegistry()
	if !reg.Supports("cartesia", "stt") || reg.Supports("cartesia", "llm") {
		t.Fatal("cartesia must support stt")
	}
	f := reg.factories["cartesia.stt"]
	if _, err := f(&Config{}, StageSpec{Secret: []byte("two words")}); err == nil {
		t.Fatal("malformed credential accepted")
	}
	p, err := f(&Config{}, StageSpec{Secret: []byte("cartesia-test-key-0123456789")})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(core.STTCounters); !ok {
		t.Fatal("cartesia stt must report STT counters")
	}
	if reg.sttBreaker("cartesia") != reg.cartesiaSTTBreaker || reg.cartesiaSTTBreaker == reg.cartesiaBreaker {
		t.Fatal("cartesia stt needs its own breaker")
	}
	_ = p.(interface{ Close() error }).Close()
}

func TestOpenAILLMFactoryAndCredential(t *testing.T) {
	reg := NewRegistry()
	if !reg.Supports("openai", "llm") || reg.Supports("openai", "tts") {
		t.Fatal("openai.llm only")
	}
	key, base, err := openAICredential([]byte(`{"api_key":"sk-abc123","base_url":" http://localhost:8000/v1 "}`))
	if err != nil || key != "sk-abc123" || base != "http://localhost:8000/v1" {
		t.Fatalf("json credential: %q %q %v", key, base, err)
	}
	if key, base, err = openAICredential([]byte("sk-plain\n")); err != nil || key != "sk-plain" || base != "" {
		t.Fatalf("bare credential: %q %q %v", key, base, err)
	}
	if _, _, err = openAICredential([]byte(`{"base_url":"http://x"}`)); err == nil {
		t.Fatal("missing api_key accepted")
	}
	p, err := reg.factories["openai.llm"](&Config{}, StageSpec{Provider: "openai", Model: "llama-3", Secret: []byte(`{"api_key":"k","base_url":"https://api.groq.com/openai/v1"}`)})
	if err != nil || p == nil {
		t.Fatalf("factory: %v", err)
	}
	if _, err := reg.factories["openai.llm"](&Config{}, StageSpec{Secret: []byte(`{"api_key":"k","base_url":"file:///x"}`)}); err == nil {
		t.Fatal("non-http base_url accepted")
	}
	// Plain http (self-hosted vLLM) needs the operator opt-in.
	vllm := StageSpec{Secret: []byte(`{"api_key":"k","base_url":"http://10.0.0.5:8000/v1"}`)}
	if _, err := reg.factories["openai.llm"](&Config{}, vllm); err == nil {
		t.Fatal("http base_url accepted without allow_private_base_url")
	}
	var opt Config
	opt.Providers.OpenAILLM.AllowPrivateBaseURL = true
	if _, err := reg.factories["openai.llm"](&opt, vllm); err != nil {
		t.Fatal(err)
	}
	// Breakers are per base URL.
	a, _ := reg.openaiBreakers.Load("https://api.groq.com/openai/v1")
	b, _ := reg.openaiBreakers.Load("http://10.0.0.5:8000/v1")
	if a == nil || b == nil || a == b {
		t.Fatal("openai breakers must be scoped per base URL")
	}
}

// scriptSTT stands in for a streaming STT provider with the real ownership
// semantics: the stream takes its generation from VAD start, a same-turn
// TurnEventStarted with a newer generation adopts it, and an InterruptionFrame
// cuts it. Transcripts re-enter after STT carrying the stream's generation.
type scriptSTT struct {
	s    core.SessionController
	idx  int
	mu   sync.Mutex
	turn core.TurnID
	gen  uint64
}

func (p *scriptSTT) Attach(s core.SessionController, i int) error { p.s, p.idx = s, i; return nil }
func (p *scriptSTT) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	p.mu.Lock()
	switch v := f.(type) {
	case *core.UserStartedSpeakingFrame:
		p.turn, p.gen = core.TurnID(fmt.Sprintf("turn-%d-%d", v.Gen, v.Seq)), v.Gen
	case *core.TurnEventFrame:
		if v.Kind == core.TurnEventStarted && v.Turn == p.turn && uint64(v.Gen) > p.gen {
			p.gen = uint64(v.Gen)
		}
	case *core.InterruptionFrame:
		p.turn = ""
	}
	p.mu.Unlock()
	return e.PushDirection(f, d)
}
func (p *scriptSTT) say(text string, final bool) {
	p.mu.Lock()
	turn, gen := p.turn, p.gen
	p.mu.Unlock()
	if turn != "" {
		_ = p.s.Reenter(&core.TranscriptCandidateFrame{Turn: turn, Gen: core.Generation(gen), Text: text, Final: final, Timestamp: time.Now()}, core.Downstream, p.idx+1, gen)
	}
}

// End to end through the production call pipeline with turn.manager on: a
// normal turn is answered, a backchannel during bot speech is not, and a real
// barge-in flushes bot audio and its whole phrase reaches the LLM.
func TestTurnManagerCallPipelineEndToEnd(t *testing.T) {
	stt := &scriptSTT{}
	reg := NewRegistry()
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return stt, nil })
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) {
		return core.NewFakeLLMProcessor(core.FakeLLMConfig{ResponseChunks: []string{"You said: "}, EchoUser: true}), nil
	})
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) {
		return core.NewFakeTTSProcessor(core.FakeTTSConfig{FramesPerUnit: 2}), nil
	})
	out := core.NewFakeTransportOutput(core.FakeTransportConfig{WriteDelay: time.Minute}) // bot audio stays "playing"
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Turn.Manager, cfg.Turn.MinWords = true, 3
	call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, out)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var asked []string
	seen := map[string]bool{}
	var botAudio atomic.Int32
	sess.OnFrame = func(f core.Frame, _ core.Direction) { // once per hop
		switch v := f.(type) {
		case *core.UserMessageFrame:
			mu.Lock()
			if !seen[v.Turn] {
				seen[v.Turn] = true
				asked = append(asked, v.Content)
			}
			mu.Unlock()
		case *core.OutputAudioFrame:
			botAudio.Add(1)
		}
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	llmAsked := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), asked...) }
	waitFor := func(what string, d time.Duration, cond func() bool) {
		t.Helper()
		for end := time.Now().Add(d); !cond(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out: %s (llm saw %q)", what, llmAsked())
			}
		}
	}
	var seq uint64
	vadStart := func() {
		seq++
		_ = sess.Reenter(&core.UserStartedSpeakingFrame{Seq: seq, Gen: sess.CurrentGeneration()}, core.Downstream, 1, 0)
		time.Sleep(20 * time.Millisecond)
	}
	vadStop := func() {
		_ = sess.Reenter(&core.UserStoppedSpeakingFrame{Seq: seq, Gen: sess.CurrentGeneration()}, core.Downstream, 1, 0)
	}

	// 1. A normal turn is answered.
	vadStart()
	stt.say("what time is it", true)
	vadStop()
	waitFor("first turn answered", 3*time.Second, func() bool { return len(llmAsked()) == 1 && botAudio.Load() > 0 })
	if got := llmAsked()[0]; got != "what time is it" {
		t.Fatalf("llm saw %q", got)
	}
	_ = sess.Reenter(&core.SpeakingStateFrame{State: core.SpeakingStarted}, core.Upstream, sess.TailIndex(), 0)

	// 2. A backchannel while the bot speaks neither barges in nor reaches the LLM.
	vadStart()
	stt.say("uh-huh", false)
	vadStop()
	time.Sleep(1500 * time.Millisecond)
	stt.say("uh-huh", true) // a late final for the retired backchannel
	time.Sleep(50 * time.Millisecond)
	if out.FlushCount.Load() != 0 || len(llmAsked()) != 1 {
		t.Fatalf("backchannel: flushes=%d llm=%q", out.FlushCount.Load(), llmAsked())
	}

	// 3. A real barge-in flushes bot audio; the rest of the phrase, transcribed
	// after the interruption, still reaches the LLM.
	vadStart()
	stt.say("wait stop that", false)
	waitFor("barge-in flush", 2*time.Second, func() bool { return out.FlushCount.Load() == 1 })
	stt.say("wait stop that and tell me the date", true)
	vadStop()
	waitFor("barge-in answered", 3*time.Second, func() bool { return len(llmAsked()) == 2 })
	if got := llmAsked()[1]; got != "wait stop that and tell me the date" {
		t.Fatalf("llm saw %q", got)
	}
}
