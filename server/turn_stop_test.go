package server

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	coreturn "govoice/core/turn"
	"govoice/smartturn"
	"govoice/vad"
)

// TestMain releases the ORT environment after the in-process Smart-turn test,
// avoiding the exit-time abort.
func TestMain(m *testing.M) {
	code := m.Run()
	vad.ShutdownORT()
	os.Exit(code)
}

func TestTurnStopDetector(t *testing.T) {
	// An empty chain is the gated speech timeout (core/turn SpeechTimeout), not nil.
	if d, err := turnStopDetector(nil, coreturn.SpeechTimeoutConfig{}, 0, nil); d == nil || err != nil {
		t.Fatalf("default must be the gated speech timeout: %v %v", d, err)
	}
	if d, err := turnStopDetector(nil, coreturn.SpeechTimeoutConfig{UserSpeechTimeout: time.Second}, 0, nil); d == nil || err != nil {
		t.Fatalf("speech_timeout alone must apply: %v %v", d, err)
	}
	if d, err := turnStopDetector([]string{"eager", "speech-timeout"}, coreturn.SpeechTimeoutConfig{UserSpeechTimeout: time.Second}, 0, nil); d == nil || err != nil {
		t.Fatalf("chain: %v %v", d, err)
	}
	for _, bad := range [][]string{{"eager"}, {"speech-timeout", "turn-analyzer"}, {"speech-timeout", "external"},
		{"speech-timeout", "deferred:eager"}, {"speech-timeout", "nope"}} {
		if _, err := turnStopDetector(bad, coreturn.SpeechTimeoutConfig{}, 0, nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTurnStopNeedsManager(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Storage.Dialect = "sqlite"
	cfg.Turn.Manager = false // on by default since the turn-latency change
	cfg.Turn.Stop = []string{"speech-timeout"}
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "turn.manager") {
		t.Fatalf("turn.stop without turn.manager: %v", err)
	}
	cfg.Turn.Manager = true
	if err := ValidateConfig(cfg); err != nil && strings.Contains(err.Error(), "turn") {
		t.Fatal(err)
	}
}

type holdStrategy struct{}

func (holdStrategy) Detect(coreturn.DetectionInput) (coreturn.DetectionDecision, bool) {
	return coreturn.DetectionDecision{}, false
}

func TestTurnStopAnalyzerSubsumesSpeechTimeout(t *testing.T) {
	an := func(time.Duration) coreturn.UserTurnStopStrategy { return holdStrategy{} }
	in := coreturn.DetectionInput{SilenceFor: time.Second, SilenceAfter: 700 * time.Millisecond}
	d, err := turnStopDetector([]string{"turn-analyzer", "speech-timeout"}, coreturn.SpeechTimeoutConfig{UserSpeechTimeout: 700 * time.Millisecond}, 0, an)
	if err != nil {
		t.Fatal(err)
	}
	if _, done := d.Detect(in); done {
		t.Fatal("plain speech-timeout in the chain would end every turn the model holds")
	}
	if _, err := turnStopDetector([]string{"turn-analyzer"}, coreturn.SpeechTimeoutConfig{}, 0, an); err == nil {
		t.Fatal("turn-analyzer without speech-timeout accepted")
	}
	if _, err := turnStopDetector([]string{"turn-analyzer", "speech-timeout"}, coreturn.SpeechTimeoutConfig{}, 0, nil); err == nil {
		t.Fatal("turn-analyzer accepted without a loaded model")
	}
}

func TestTurnAnalyzerConfig(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Storage.Dialect = "sqlite"
	cfg.Turn.Manager = true
	cfg.Turn.Stop = []string{"turn-analyzer", "speech-timeout"}
	if err := ValidateConfig(cfg); err != nil && strings.Contains(err.Error(), "turn") {
		t.Fatal(err)
	}
	cfg.Turn.Analyzer.Mode = "nope"
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "turn.analyzer.mode") {
		t.Fatalf("bad mode: %v", err)
	}
	cfg.Turn.Analyzer.Mode = ""
	cfg.Turn.Manager = false
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "turn.manager") {
		t.Fatalf("turn-analyzer without turn.manager: %v", err)
	}
}

// smartBackend answers every inference with p after delay; block never answers.
type smartBackend struct {
	p     float32
	block bool
}

func (b smartBackend) Infer(ctx context.Context, pcm []int16) (float32, error) {
	if b.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return b.p, nil
}
func (smartBackend) Ready() bool  { return true }
func (smartBackend) Close() error { return nil }

// End to end through BuildCallPipeline with turn.stop: [turn-analyzer,
// speech-timeout]: a "complete" verdict ends the turn before speech_timeout,
// "incomplete" holds it to stop_secs, and a model that never answers degrades
// to exactly speech_timeout. Real clock, so the bounds are generous.
func TestTurnAnalyzerThroughCallPipeline(t *testing.T) {
	const speechTimeout, stopSecs = 400 * time.Millisecond, 1500 * time.Millisecond
	run := func(t *testing.T, be smartturn.Backend, silence bool, budget time.Duration) (core.TurnReason, time.Duration) {
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Turn.Manager = true
		cfg.Turn.Stop = []string{"turn-analyzer", "speech-timeout"}
		cfg.Turn.SpeechTimeout = speechTimeout
		cfg.Turn.Analyzer.StopSecs = stopSecs
		cfg.Turn.Analyzer.Budget = budget
		reg := NewRegistry()
		llm := &coretest.PassthroughProcessor{}
		reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
		reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
		reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return llm, nil })
		reg.SetTurnBackend(be)
		vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
		pipe, err := reg.BuildCallPipeline(cfg, CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}}, vadProc, &fakeTransportSink{})
		if err != nil {
			t.Fatal(err)
		}
		procs := pipe.Processors()
		if _, ok := procs[1].(*coretest.PassthroughProcessor); ok || procs[0] != vadProc {
			t.Fatalf("audio tap must sit between VAD and STT: %T %T", procs[0], procs[1])
		}
		s, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		_ = s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
		pcm := make([]byte, 640)
		if !silence {
			for i := range pcm {
				pcm[i] = byte(i * 37)
			}
		}
		for i := 0; i < 25; i++ {
			_ = s.Post(&core.InputAudioFrame{Samples: pcm, Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1})
			time.Sleep(20 * time.Millisecond)
		}
		t0 := time.Now()
		_ = s.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
		for time.Since(t0) < 5*time.Second {
			for _, f := range llm.GetFrames() {
				if ev, ok := f.(*core.TurnEventFrame); ok && ev.Kind == core.TurnEventCompleted {
					return ev.Reason, time.Since(t0)
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("turn never completed")
		return core.TurnReasonNone, 0
	}
	t.Run("complete", func(t *testing.T) {
		r, d := run(t, smartBackend{p: 0.9}, false, 100*time.Millisecond)
		if r != core.TurnReasonSemantic || d >= speechTimeout {
			t.Fatalf("reason %v after %v", r, d)
		}
		t.Logf("ended after %v", d)
	})
	t.Run("incomplete holds to stop_secs", func(t *testing.T) {
		r, d := run(t, smartBackend{p: 0.1}, false, 100*time.Millisecond)
		if r != core.TurnReasonSilence || d < stopSecs-50*time.Millisecond || d > stopSecs+400*time.Millisecond {
			t.Fatalf("reason %v after %v", r, d)
		}
	})
	t.Run("degraded ends at VAD stop + speech_timeout", func(t *testing.T) {
		// The analyzer's fallback base is the gated speech timeout's resume window from onset.
		base := DefaultConfig(ProfileDevelopment).vadStop(0) + speechTimeout
		r, d := run(t, smartBackend{block: true}, false, 100*time.Millisecond)
		if r != core.TurnReasonSilence || d < base-50*time.Millisecond || d > base+400*time.Millisecond {
			t.Fatalf("reason %v after %v", r, d)
		}
	})
	t.Run("real model in process", func(t *testing.T) {
		be, err := smartturn.NewInProcess(smartturn.ModelPath())
		if err != nil {
			t.Skipf("model unavailable: %v", err)
		}
		defer be.Close()
		// Silence clip: the model says complete (p~0.99).
		// Generous budget: -race slows the Go features ~10x, so only the
		// reason is asserted (the model, not the fallback, ended the turn).
		r, d := run(t, be, true, time.Second)
		if r != core.TurnReasonSemantic {
			t.Fatalf("reason %v after %v", r, d)
		}
		t.Logf("real model ended the turn after %v", d)
	})
}

func TestTurnCompletionConfigValidation(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Storage.Dialect = "sqlite"
	cfg.Turn.Manager = true
	cfg.Turn.Stop = []string{"speech-timeout", "llm-completion"}
	if err := ValidateConfig(cfg); err != nil && strings.Contains(err.Error(), "turn") {
		t.Fatalf("valid chain rejected: %v", err)
	}
	cfg.Turn.Completion.LongWait = -time.Second
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "turn.completion") {
		t.Fatalf("negative wait: %v", err)
	}
	cfg.Turn.Completion = CompletionConfig{ShortWait: time.Second}
	cfg.Turn.Stop = []string{"speech-timeout"}
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "llm-completion") {
		t.Fatalf("completion settings without llm-completion: %v", err)
	}
	cfg.Turn.Manager, cfg.Turn.Stop = false, nil
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "turn.manager") {
		t.Fatalf("completion without manager: %v", err)
	}
	if _, err := turnStopDetector([]string{"llm-completion"}, coreturn.SpeechTimeoutConfig{}, 0, nil); err == nil {
		t.Fatal("llm-completion alone must still need speech-timeout")
	}
}

// scriptedLLM answers every user message with one fixed reply.
type scriptedLLM struct {
	sess  core.SessionController
	index int
	reply string
}

func (l *scriptedLLM) Attach(s core.SessionController, i int) error {
	l.sess, l.index = s, i
	return nil
}
func (l *scriptedLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	um, ok := f.(*core.UserMessageFrame)
	if !ok {
		return e.PushDirection(f, dir)
	}
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	gen := l.sess.CurrentGeneration()
	_ = l.sess.Reenter(&core.LLMTextDeltaFrame{Delta: l.reply, Turn: um.Turn, Gen: gen}, core.Downstream, l.index+1, gen)
	return l.sess.Reenter(&core.LLMTextFrame{Text: l.reply, Turn: um.Turn, Gen: gen}, core.Downstream, l.index+1, gen)
}

// TestTurnCompletionEndToEnd builds a real call pipeline with llm-completion:
// the protocol instructions reach the primary and fallback system prompts, a
// complete reply is spoken without its marker and an incomplete one is not
// spoken at all.
func TestTurnCompletionEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		reply string
		want  string // "" = nothing spoken
	}{{"● Hi there, friend.", "Hi there, friend."}, {"◐", ""}} {
		var prompts []string
		tts := &coretest.PassthroughProcessor{}
		llm := &scriptedLLM{reply: tc.reply}
		reg := NewRegistry()
		reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
		reg.Register("google.llm", func(_ *Config, s StageSpec) (core.Processor, error) {
			prompts = append(prompts, s.SystemPrompt)
			return llm, nil
		})
		reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return tts, nil })
		var metrics Metrics
		reg.onCompletion = metrics.completionVerdict
		vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
		call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google", SystemPrompt: "Be brief."}, TTS: StageSpec{Provider: "google"}}
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Turn.Manager = true
		cfg.Turn.Stop = []string{"speech-timeout", "llm-completion"}
		pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, &fakeTransportSink{})
		if err != nil {
			t.Fatal(err)
		}
		if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "Be brief.") || !strings.Contains(prompts[0], "TURN COMPLETION PROTOCOL") {
			t.Fatalf("system prompt = %q", prompts)
		}
		sess, err := core.NewSession(930, pipe, core.DefaultResourceBudget())
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Start(); err != nil {
			t.Fatal(err)
		}
		llmIdx := slices.Index(pipe.Processors(), core.Processor(llm))
		_ = sess.Reenter(&core.UserMessageFrame{Turn: "turn-1", Content: "hello"}, core.Downstream, llmIdx, 0)
		time.Sleep(100 * time.Millisecond)
		var spoken []string
		for _, f := range tts.GetFrames() {
			if u, ok := f.(*core.SynthesisUnitFrame); ok {
				spoken = append(spoken, u.Text)
			}
		}
		_ = sess.Close()
		if tc.want == "" && len(spoken) != 0 || tc.want != "" && (len(spoken) != 1 || spoken[0] != tc.want) {
			t.Fatalf("reply %q spoke %q, want %q", tc.reply, spoken, tc.want)
		}
		if metrics.completion[0].Load()+metrics.completion[1].Load() != 1 {
			t.Fatalf("verdict not counted: %d/%d", metrics.completion[0].Load(), metrics.completion[1].Load())
		}
	}
}

// recordingBackend answers p and keeps the last window it was given.
type recordingBackend struct {
	mu  sync.Mutex
	pcm []int16
	p   float32
}

func (b *recordingBackend) Infer(_ context.Context, pcm []int16) (float32, error) {
	b.mu.Lock()
	b.pcm = append([]int16(nil), pcm...)
	b.mu.Unlock()
	return b.p, nil
}
func (*recordingBackend) Ready() bool  { return true }
func (*recordingBackend) Close() error { return nil }

// turn.manager + min_words + turn-analyzer: the turn starts only when the
// transcript arrives (after the VAD stop), yet the model must still be given
// the speech, not the trailing silence after the late turn start.
func TestTurnAnalyzerWindowWithMinWords(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Turn.Manager = true
	cfg.Turn.MinWords = 3
	cfg.Turn.Stop = []string{"turn-analyzer", "speech-timeout"}
	cfg.Turn.Analyzer.Budget = 500 * time.Millisecond
	reg := NewRegistry()
	llm := &coretest.PassthroughProcessor{}
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return llm, nil })
	be := &recordingBackend{p: 0.9}
	reg.SetTurnBackend(be)
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(cfg, CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}}, vadProc, &fakeTransportSink{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	speech := make([]byte, 640)
	for i := range speech {
		speech[i] = 0x11 // every sample 0x1111: never zero
	}
	_ = s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	const frames = 25
	for i := 0; i < frames; i++ {
		_ = s.Post(&core.InputAudioFrame{Samples: speech, Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1})
		time.Sleep(20 * time.Millisecond)
	}
	_ = s.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	for i := 0; i < 30; i++ { // trailing silence until the late final (no interims)
		_ = s.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1})
		time.Sleep(20 * time.Millisecond)
	}
	_ = s.Post(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "what is two plus two", Final: true})
	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(llm.GetFrames(), func(f core.Frame) bool {
		ev, ok := f.(*core.TurnEventFrame)
		return ok && ev.Kind == core.TurnEventCompleted
	}) {
		if time.Now().After(deadline) {
			t.Fatal("turn never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	n := 0
	for _, v := range be.pcm {
		if v == 0x1111 {
			n++
		}
	}
	if want := frames * 320 * 9 / 10; n < want {
		t.Fatalf("model window held %d speech samples of %d (window %d samples): it starts at the late turn start", n, frames*320, len(be.pcm))
	}
}

// Tests the server-level integration of turn taking: barge-in over speaking TTS
// advances generation and triggers turn start, followed by silence end-of-turn completion.
func TestTurnManager_BargeInOverSpeakingTTSAndSilenceCompletion(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Turn.Manager = true
	cfg.Turn.SpeechTimeout = 200 * time.Millisecond

	reg := NewRegistry()
	llm := &coretest.PassthroughProcessor{}
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return llm, nil })

	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(cfg, CallSpec{
		STT: StageSpec{Provider: "google"},
		TTS: StageSpec{Provider: "google"},
		LLM: StageSpec{Provider: "google"},
	}, vadProc, &fakeTransportSink{})
	if err != nil {
		t.Fatal(err)
	}

	s, err := core.NewSession(2, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 1. Bot is actively speaking TTS (speaking state frame flowing upstream)
	_ = s.Reenter(&core.SpeakingStateFrame{State: core.SpeakingStarted}, core.Upstream, s.TailIndex(), s.CurrentGeneration())
	time.Sleep(20 * time.Millisecond)

	// 2. User starts speaking (barge-in interruption)
	startGen := s.CurrentGeneration()
	_ = s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: startGen})

	// Wait for turn event started
	deadline := time.Now().Add(2 * time.Second)
	for !slices.ContainsFunc(llm.GetFrames(), func(f core.Frame) bool {
		ev, ok := f.(*core.TurnEventFrame)
		return ok && ev.Kind == core.TurnEventStarted
	}) {
		if time.Now().After(deadline) {
			t.Fatal("turn event started was not emitted on barge-in")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 3. User finishes speaking after audio frames
	speech := make([]byte, 640)
	_ = s.Post(&core.InputAudioFrame{Samples: speech, Encoding: core.EncodingPCM16LE, SampleRate: 16000, Channels: 1})
	// The STT keys the transcript by the VAD segment (turn-<gen at start>-<seq>); the turn
	// processor re-keys it. The gated speech timeout needs it to end the turn.
	_ = s.Post(&core.TranscriptCandidateFrame{Turn: core.TurnID(fmt.Sprintf("turn-%d-1", startGen)), Gen: core.Generation(s.CurrentGeneration()), Text: "stop talking now", Final: true})
	_ = s.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: s.CurrentGeneration(), SilenceDuration: 200 * time.Millisecond})

	// 4. Silence ends the turn (TurnEventCompleted)
	deadline = time.Now().Add(2 * time.Second)
	for !slices.ContainsFunc(llm.GetFrames(), func(f core.Frame) bool {
		ev, ok := f.(*core.TurnEventFrame)
		return ok && ev.Kind == core.TurnEventCompleted
	}) {
		if time.Now().After(deadline) {
			t.Fatal("turn event completed was not emitted after silence")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSTTP99Resolution(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	if got := cfg.sttP99("google", 0); got != 1570*time.Millisecond {
		t.Fatalf("table: %v", got)
	}
	cfg.Turn.STTP99 = map[string]time.Duration{"google": 900 * time.Millisecond}
	if got := cfg.sttP99("google", 0); got != 900*time.Millisecond {
		t.Fatalf("server override: %v", got)
	}
	if got := cfg.sttP99("google", 1200); got != 1200*time.Millisecond {
		t.Fatalf("agent override: %v", got)
	}
	if got := cfg.sttP99("someone-new", 0); got != time.Second {
		t.Fatalf("unknown provider: %v", got)
	}
}

func TestVADStopResolution(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.VAD.StopMS = 800
	if got := cfg.vadStop(0); got != 800*time.Millisecond {
		t.Fatalf("server: %v", got)
	}
	if got := cfg.vadStop(300); got != 320*time.Millisecond {
		t.Fatalf("agent, rounded to 32 ms chunks: %v", got)
	}
}

// An empty turn.stop still builds the gated speech timeout (not the bare silence detector).
func TestEmptyTurnStopIsGatedSpeechTimeout(t *testing.T) {
	d, err := turnStopDetector(nil, coreturn.SpeechTimeoutConfig{VADStop: 200 * time.Millisecond, STTP99: time.Second}, 0, nil)
	if err != nil || d == nil {
		t.Fatalf("d=%v err=%v", d, err)
	}
	if _, ok := d.Detect(coreturn.DetectionInput{SilenceFor: 2 * time.Second}); ok {
		t.Fatal("ended a transcript-less turn before the no-transcript fallback")
	}
	if _, ok := d.Detect(coreturn.DetectionInput{SilenceFor: 800 * time.Millisecond, Text: "hi", TextFinal: true}); !ok {
		t.Fatal("did not end a transcribed turn at the resume window")
	}
}
