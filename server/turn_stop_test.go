package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"govoice/core"
	coreturn "govoice/core/turn"
	"govoice/smartturn"
	"govoice/vad"
)

func TestTurnStopDetector(t *testing.T) {
	if d, err := turnStopDetector(nil, 0, 0, nil); d != nil || err != nil {
		t.Fatalf("default must be the built-in fallback: %v %v", d, err)
	}
	if d, err := turnStopDetector(nil, time.Second, 0, nil); d == nil || err != nil {
		t.Fatalf("speech_timeout alone must apply: %v %v", d, err)
	}
	if d, err := turnStopDetector([]string{"eager", "speech-timeout"}, time.Second, 0, nil); d == nil || err != nil {
		t.Fatalf("chain: %v %v", d, err)
	}
	for _, bad := range [][]string{{"eager"}, {"speech-timeout", "turn-analyzer"}, {"speech-timeout", "external"},
		{"speech-timeout", "deferred:eager"}, {"speech-timeout", "nope"}} {
		if _, err := turnStopDetector(bad, 0, 0, nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTurnStopNeedsManager(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Storage.Dialect = "sqlite"
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
	d, err := turnStopDetector([]string{"turn-analyzer", "speech-timeout"}, 700*time.Millisecond, 0, an)
	if err != nil {
		t.Fatal(err)
	}
	if _, done := d.Detect(in); done {
		t.Fatal("plain speech-timeout in the chain would end every turn the model holds")
	}
	if _, err := turnStopDetector([]string{"turn-analyzer"}, 0, 0, an); err == nil {
		t.Fatal("turn-analyzer without speech-timeout accepted")
	}
	if _, err := turnStopDetector([]string{"turn-analyzer", "speech-timeout"}, 0, 0, nil); err == nil {
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
		llm := &core.PassthroughProcessor{}
		reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })
		reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })
		reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return llm, nil })
		reg.SetTurnBackend(be)
		vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
		pipe, err := reg.BuildCallPipeline(cfg, CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}}, vadProc, &fakeTransportSink{})
		if err != nil {
			t.Fatal(err)
		}
		procs := pipe.Processors()
		if _, ok := procs[1].(*core.PassthroughProcessor); ok || procs[0] != vadProc {
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
	t.Run("degraded ends at speech_timeout", func(t *testing.T) {
		r, d := run(t, smartBackend{block: true}, false, 100*time.Millisecond)
		if r != core.TurnReasonSilence || d < speechTimeout-50*time.Millisecond || d > speechTimeout+400*time.Millisecond {
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
	if _, err := turnStopDetector([]string{"llm-completion"}, 0, 0, nil); err == nil {
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
		tts := &core.PassthroughProcessor{}
		llm := &scriptedLLM{reply: tc.reply}
		reg := NewRegistry()
		reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })
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
