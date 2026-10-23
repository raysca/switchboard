package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/vad"
)

// stepFakeLLM answers with its step's marker (the system prompt). "buy" makes
// it call the step's transfer tool through the spec's executor and say a
// farewell, the way a model would.
type stepFakeLLM struct {
	spec    StageSpec
	session core.SessionController
	index   int
	closed  *atomic.Int32
}

func (p *stepFakeLLM) Attach(s core.SessionController, i int) error {
	p.session, p.index = s, i
	return nil
}
func (p *stepFakeLLM) Close() error { p.closed.Add(1); return nil }

func (p *stepFakeLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	um, ok := f.(*core.UserMessageFrame)
	if !ok {
		return e.PushDirection(f, dir)
	}
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	gen := p.session.CurrentGeneration()
	return p.session.Go(func(ctx context.Context) {
		say := func(text string) {
			_ = p.session.Reenter(&core.LLMTextDeltaFrame{Delta: text, Turn: um.Turn, Gen: gen}, core.Downstream, p.index+1, gen)
			_ = p.session.Reenter(&core.LLMTextFrame{Text: text, Turn: um.Turn, Gen: gen}, core.Downstream, p.index+1, gen)
		}
		if strings.Contains(um.Content, "buy") && p.spec.ToolExecutor != nil {
			res, _ := p.spec.ToolExecutor.Execute(ctx, core.ToolCall{ID: "c-" + um.Turn, Name: "transfer_to_sales", Arguments: json.RawMessage(`{}`)})
			if res.Status != core.ToolSucceeded {
				say("Transfer failed.")
				return
			}
			say("Connecting you.")
			return
		}
		say(p.spec.SystemPrompt + " here.")
	})
}

// countedStage counts Close and forwards Attach to the inner processor.
type countedStage struct {
	core.Processor
	closed *atomic.Int32
}

func (c countedStage) Attach(s core.SessionController, i int) error {
	if a, ok := c.Processor.(core.Attacher); ok {
		return a.Attach(s, i)
	}
	return nil
}
func (c countedStage) Close() error {
	c.closed.Add(1)
	if cl, ok := c.Processor.(core.Closer); ok {
		return cl.Close()
	}
	return nil
}

type agentRig struct {
	sess                *core.Session
	spoken              func() string
	llmBuilt, ttsBuilt  atomic.Int32
	llmClosed, ttsClose map[string]*atomic.Int32 // by prompt / voice
	mu                  sync.Mutex
	voices              []string
	events              []core.HandoffEvent
}

func (r *agentRig) closedLLM(prompt string) int32 { return r.llmClosed[prompt].Load() }

func (r *agentRig) say(t *testing.T, turn, text string) {
	t.Helper()
	gen := r.sess.CurrentGeneration()
	idx := CallUserContextIndex
	for i, p := range r.sess.Processors() {
		if _, ok := p.(*core.UserContextProcessor); ok {
			idx = i
		}
	}
	if err := r.sess.Reenter(&core.FinalTranscriptionFrame{Text: text, Turn: turn, Gen: gen, Committed: true, Timestamp: time.Now()}, core.Downstream, idx, gen); err != nil {
		t.Fatal(err)
	}
}

func (r *agentRig) waitSpoken(t *testing.T, want string, n int) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); strings.Count(r.spoken(), want) < n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("spoken %q, want %d x %q", r.spoken(), n, want)
		}
	}
}

func newAgentRig(t *testing.T) *agentRig {
	t.Helper()
	r := &agentRig{
		llmClosed: map[string]*atomic.Int32{"FRONT": {}, "SALES": {}},
		ttsClose:  map[string]*atomic.Int32{"front-voice": {}, "sales-voice": {}},
	}
	g := twoStepAgent(t)
	graph := &CallAgent{Start: "front"}
	graph.Resolve = func(_ context.Context, id string) (StepSpec, error) {
		return StepSpec{Name: "Sales",
			LLM: StageSpec{Provider: "google", SystemPrompt: "SALES"},
			TTS: StageSpec{Provider: "google", Voice: "sales-voice"}}, nil
	}
	exec := stepExecutor(nil, nil, transferTargets(g, "front"), func(target, _ string) error { return graph.Commit(target) }, nil)

	reg := NewRegistry()
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return passSTTStage{}, nil })
	reg.Register("google.llm", func(_ *Config, s StageSpec) (core.Processor, error) {
		r.llmBuilt.Add(1)
		return &stepFakeLLM{spec: s, closed: r.llmClosed[s.SystemPrompt]}, nil
	})
	reg.Register("google.tts", func(_ *Config, s StageSpec) (core.Processor, error) {
		r.ttsBuilt.Add(1)
		r.mu.Lock()
		r.voices = append(r.voices, s.Voice)
		r.mu.Unlock()
		return countedStage{coretest.NewTTSProcessor(coretest.TTSConfig{FramesPerUnit: 2}), r.ttsClose[s.Voice]}, nil
	})
	call := CallSpec{
		STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google", Voice: "front-voice"},
		LLM:         StageSpec{Provider: "google", SystemPrompt: "FRONT", Tools: transferTools(g, "front"), ToolExecutor: exec},
		MaxMessages: 20, MaxTokens: 4000, Agent: graph,
		OnHandoff: func(e core.HandoffEvent) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() },
	}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(DefaultConfig(ProfileDevelopment), call, vadProc, coretest.NewTransportOutput(coretest.TransportConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	r.sess, err = core.NewSession(9, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var spoken strings.Builder
	r.sess.OnFrame = func(f core.Frame, _ core.Direction) {
		if su, ok := f.(*core.SynthesisUnitFrame); ok {
			mu.Lock()
			spoken.WriteString(su.Text + "\n")
			mu.Unlock()
		}
	}
	r.spoken = func() string { mu.Lock(); defer mu.Unlock(); return spoken.String() }
	if err := r.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.sess.Close()
		if n := r.sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	})
	return r
}

func TestAgentHandoffMovesTheCallToTheSalesStep(t *testing.T) {
	r := newAgentRig(t)
	if r.llmBuilt.Load() != 1 || r.ttsBuilt.Load() != 1 {
		t.Fatalf("only the entry step may be built at start: llm=%d tts=%d", r.llmBuilt.Load(), r.ttsBuilt.Load())
	}
	r.say(t, "t1", "hello")
	r.waitSpoken(t, "FRONT here.", 1)

	r.say(t, "t2", "I want to buy")
	r.waitSpoken(t, "Connecting you.", 1)
	r.waitSpoken(t, "SALES here.", 1) // the idle flip started the sales step's turn

	if r.llmBuilt.Load() != 2 || r.ttsBuilt.Load() != 2 {
		t.Fatalf("sales built once: llm=%d tts=%d", r.llmBuilt.Load(), r.ttsBuilt.Load())
	}
	for end := time.Now().Add(3 * time.Second); (r.closedLLM("FRONT") != 1 || r.ttsClose["front-voice"].Load() != 1) && time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
	}
	if r.closedLLM("FRONT") != 1 || r.ttsClose["front-voice"].Load() != 1 {
		t.Fatalf("the left step must be closed once: llm=%d tts=%d", r.closedLLM("FRONT"), r.ttsClose["front-voice"].Load())
	}
	if r.closedLLM("SALES") != 0 || r.ttsClose["sales-voice"].Load() != 0 {
		t.Fatal("the active step must stay open")
	}

	r.say(t, "t3", "what are your prices")
	r.waitSpoken(t, "SALES here.", 2)
	if strings.Count(r.spoken(), "FRONT here.") != 1 {
		t.Fatalf("the front step must not answer after the handoff: %q", r.spoken())
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	kinds := map[core.HandoffEventKind]bool{}
	for _, e := range r.events {
		kinds[e.Kind] = true
	}
	for _, k := range []core.HandoffEventKind{core.HandoffCommitted, core.HandoffPreparing, core.HandoffActive} {
		if !kinds[k] {
			t.Fatalf("missing handoff event %q in %+v", k, r.events)
		}
	}
	if len(r.voices) != 2 || r.voices[1] != "sales-voice" {
		t.Fatalf("voices built = %v", r.voices)
	}
}
