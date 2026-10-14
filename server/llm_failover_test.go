package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/resilience"
	"govoice/server/flow"
	"govoice/vad"
)

type clientErr struct{}

func (clientErr) Error() string     { return "401 bad key" }
func (clientErr) ClientError() bool { return true }

func TestLLMClassify(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{resilience.ErrTimeout, flow.ClassTimeout},
		{fmt.Errorf("x: %w", resilience.ErrCircuitOpen), flow.ClassCircuit},
		{clientErr{}, flow.ClassClient},
		{errors.New("503"), flow.ClassServer},
	} {
		if got := llmClassify(tc.err); got != tc.want {
			t.Errorf("llmClassify(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func failoverRegistry(primaryErr error, order *[]string) *Registry {
	reg := NewRegistry()
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"x"}, FailWith: primaryErr}), nil
	})
	reg.Register("xai.llm", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"from ", "xai"}}), nil
	})
	return reg
}

func runGeneration(t *testing.T, llm core.Processor) (text string, errs int) {
	t.Helper()
	rec := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(llm, rec)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(77, pipe, core.DefaultResourceBudget())
	if err != nil || sess.Start() != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sess.Close()
		if sess.LeakedTasks() != 0 {
			t.Error("leaked tasks")
		}
	}()
	if err := sess.Post(&core.UserMessageFrame{Turn: "t", Content: "hi", Gen: sess.CurrentGeneration()}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		text, errs = "", 0
		done := false
		for _, f := range rec.GetFrames() {
			switch v := f.(type) {
			case *core.LLMTextDeltaFrame:
				text += v.Delta
			case *core.LLMTextFrame:
				done = true
			case *core.ErrorFrame:
				errs++
				done = true
			}
		}
		if done {
			time.Sleep(30 * time.Millisecond)
			return text, errs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("generation did not finish")
	return
}

func TestBuildLLMFailsOverAndCountsIt(t *testing.T) {
	reg := failoverRegistry(resilience.ErrTimeout, nil)
	m := NewMetrics()
	reg.BindMetrics(m)
	build := func(stage string, sp StageSpec) (core.Processor, error) {
		return reg.factories[factoryKey(sp.Provider, stage)](DefaultConfig(ProfileDevelopment), sp)
	}
	call := CallSpec{LLM: StageSpec{Provider: "google"}, LLMFallbacks: []StageSpec{{Provider: "xai"}, {Provider: "nope"}}}
	llm, err := reg.buildLLM(nil, call, call.LLM, func(stage string, sp StageSpec) (core.Processor, error) {
		if sp.Provider == "nope" {
			return nil, errors.New("unbuildable fallback")
		}
		return build(stage, sp)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := llm.(*core.LLMFailover); !ok {
		t.Fatalf("got %T", llm)
	}
	if text, errs := runGeneration(t, llm); text != "from xai" || errs != 0 {
		t.Fatalf("text=%q errs=%d", text, errs)
	}
	var out strings.Builder
	m.WritePrometheus(&out)
	if !strings.Contains(out.String(), `voice_llm_failover_total{from="google",to="xai",class="timeout"} 1`) ||
		!strings.Contains(out.String(), "voice_llm_fallbacks_skipped_total 1") {
		t.Fatalf("metric missing:\n%s", out.String())
	}
}

func TestBuildLLMPolicyOptOutAndNoFallbacks(t *testing.T) {
	reg := failoverRegistry(resilience.ErrTimeout, nil)
	build := func(stage string, sp StageSpec) (core.Processor, error) {
		return reg.factories[factoryKey(sp.Provider, stage)](DefaultConfig(ProfileDevelopment), sp)
	}
	// Only server_error may fail over; a timeout is the turn's failure.
	call := CallSpec{LLM: StageSpec{Provider: "google"}, LLMFallbacks: []StageSpec{{Provider: "xai"}},
		Failover: flow.FailoverPolicy{On: []string{flow.ClassServer}}}
	llm, err := reg.buildLLM(nil, call, call.LLM, build)
	if err != nil {
		t.Fatal(err)
	}
	if text, errs := runGeneration(t, llm); text != "" || errs != 1 {
		t.Fatalf("text=%q errs=%d", text, errs)
	}
	// No fallbacks: the plain primary, as before.
	plain, err := reg.buildLLM(nil, CallSpec{LLM: StageSpec{Provider: "google"}}, StageSpec{Provider: "google"}, build)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.(*core.LLMFailover); ok {
		t.Fatal("flow without fallbacks must build the old pipeline")
	}
}

func TestBuildCallPipelineWrapsLLMOnlyWithFallbacks(t *testing.T) {
	reg := failoverRegistry(nil, nil)
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &fakeTransportSink{}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return &fakeTransportSink{}, nil })
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}}
	for _, withFallback := range []bool{false, true} {
		call.LLMFallbacks = nil
		if withFallback {
			call.LLMFallbacks = []StageSpec{{Provider: "xai"}}
		}
		pipe, err := reg.BuildCallPipeline(DefaultConfig(ProfileDevelopment), call, vadProc, &fakeTransportSink{})
		if err != nil {
			t.Fatal(err)
		}
		wrapped := false
		for _, p := range pipe.Processors() {
			_, ok := p.(*core.LLMFailover)
			wrapped = wrapped || ok
		}
		if wrapped != withFallback {
			t.Fatalf("withFallback=%v wrapped=%v", withFallback, wrapped)
		}
	}
}

// End to end through the production call pipeline: the primary LLM fails
// before its first token, the fallback answers, and TTS speaks that answer once.
func TestCallPipelineFailoverSpeaksOneAnswer(t *testing.T) {
	reg := NewRegistry()
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return passSTTStage{}, nil })
	reg.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"never"}, FailWith: errors.New("503 upstream")}), nil
	})
	reg.Register("xai.llm", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewLLMProcessor(coretest.LLMConfig{ResponseChunks: []string{"Fallback says: "}, EchoUser: true}), nil
	})
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) {
		return coretest.NewTTSProcessor(coretest.TTSConfig{FramesPerUnit: 2}), nil
	})
	call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"},
		LLMFallbacks: []StageSpec{{Provider: "xai"}}, MaxMessages: 10, MaxTokens: 1000}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(DefaultConfig(ProfileDevelopment), call, vadProc, coretest.NewTransportOutput(coretest.TransportConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(5, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var spoken strings.Builder
	errs := 0
	sess.OnFrame = func(f core.Frame, _ core.Direction) {
		mu.Lock()
		defer mu.Unlock()
		switch v := f.(type) {
		case *core.SynthesisUnitFrame:
			spoken.WriteString(v.Text)
		case *core.ErrorFrame:
			errs++
		}
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sess.Close()
		if n := sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	}()
	gen := sess.CurrentGeneration()
	if err := sess.Reenter(&core.FinalTranscriptionFrame{Text: "hello there", Turn: "text-1", Gen: gen, Committed: true, Timestamp: time.Now()},
		core.Downstream, CallUserContextIndex, gen); err != nil {
		t.Fatal(err)
	}
	const want = "Fallback says: hello there"
	got := func() (string, int) { mu.Lock(); defer mu.Unlock(); return spoken.String(), errs }
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if s, _ := got(); strings.Contains(s, want) {
			break
		}
		if time.Now().After(end) {
			s, _ := got()
			t.Fatalf("spoken %q, want %q", s, want)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if s, e := got(); strings.TrimSpace(s) != want || e != 0 {
		t.Fatalf("spoken %q (want exactly one answer %q), errors %d", s, want, e)
	}
}

type passSTTStage struct{}

func (passSTTStage) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	return e.PushDirection(f, d)
}
