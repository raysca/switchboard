package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	coretools "govoice/core/tools"
	"govoice/vad"
)

// asyncFakeLLM is a tool-using LLM stage built the way the xai/openai stages
// are: one session-lived ToolLoop from the stage spec, a fence per generation.
//   - "run it"/"run both": starts one/two async calls, says "On it."
//   - an async-result turn: answers with the envelope the model sees
//   - anything else: "ok"
type asyncFakeLLM struct {
	spec          StageSpec
	loop          *coretools.ToolLoop
	session       core.SessionController
	index         int
	failAfterTool error
	started       atomic.Int32
}

func newAsyncFakeLLM(t *testing.T, spec StageSpec, failAfterTool error) *asyncFakeLLM {
	loop, err := coretools.NewToolLoop(spec.Tools, spec.ToolExecutor, spec.History, spec.toolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	return &asyncFakeLLM{spec: spec, loop: loop, failAfterTool: failAfterTool}
}

func (p *asyncFakeLLM) Attach(s core.SessionController, i int) error {
	p.session, p.index = s, i
	return nil
}
func (p *asyncFakeLLM) Close() error { p.loop.Close(); return nil }

func (p *asyncFakeLLM) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	um, ok := f.(*core.UserMessageFrame)
	if !ok {
		return e.PushDirection(f, dir)
	}
	p.started.Add(1)
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	gen := p.session.CurrentGeneration()
	return p.session.Go(func(taskCtx context.Context) {
		say := func(text string) {
			_ = p.session.Reenter(&core.LLMTextDeltaFrame{Delta: text, Turn: um.Turn, Gen: gen}, core.Downstream, p.index+1, gen)
			_ = p.session.Reenter(&core.LLMTextFrame{Text: text, Turn: um.Turn, Gen: gen}, core.Downstream, p.index+1, gen)
		}
		var calls []core.ToolCall
		mk := func(id, name string) core.ToolCall {
			return core.ToolCall{ID: id + "-" + um.Turn, Name: name, Arguments: json.RawMessage(`{}`), SessionID: "s", TurnID: core.TurnID(um.Turn), Generation: core.Generation(gen)}
		}
		switch {
		case um.Content == "run it":
			calls = append(calls, mk("a", "slow"))
		case um.Content == "run both":
			calls = append(calls, mk("a", "slow"), mk("b", "other"))
		case strings.HasPrefix(um.Content, "[async tool result"):
			var out []string
			for _, m := range p.spec.History.Messages() {
				if m.Metadata["async_tool_call_id"] != nil {
					out = append(out, m.Content)
				}
			}
			say("Result: " + strings.Join(out, " | "))
			return
		default:
			say("ok")
			return
		}
		loop := p.loop.WithCurrentFence(func(c core.ToolCall) bool { return p.session.CurrentGeneration() == uint64(c.Generation) })
		if _, err := loop.Run(taskCtx, calls, nil); err != nil {
			return
		}
		if p.failAfterTool != nil {
			_ = p.session.Reenter(&core.ErrorFrame{Err: p.failAfterTool}, core.Downstream, p.index, gen)
			return
		}
		say("On it.")
	})
}

type asyncRig struct {
	sess     *core.Session
	spoken   func() string
	llm      *asyncFakeLLM
	release  chan struct{}
	runs     atomic.Int32
	canceled atomic.Int32
}

func (r *asyncRig) userCtx() int {
	for i, p := range r.sess.Processors() {
		if _, ok := p.(*core.UserContextProcessor); ok {
			return i // shifts when the turn processor is installed
		}
	}
	return CallUserContextIndex
}

func (r *asyncRig) say(t *testing.T, turn, text string) {
	t.Helper()
	gen := r.sess.CurrentGeneration()
	if err := r.sess.Reenter(&core.FinalTranscriptionFrame{Text: text, Turn: turn, Gen: gen, Committed: true, Timestamp: time.Now()}, core.Downstream, r.userCtx(), gen); err != nil {
		t.Fatal(err)
	}
}

func (r *asyncRig) waitSpoken(t *testing.T, want string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !strings.Contains(r.spoken(), want); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("spoken %q, want it to contain %q", r.spoken(), want)
		}
	}
}

func newAsyncRig(t *testing.T, fallback bool, mods ...func(*Config)) *asyncRig {
	t.Helper()
	r := &asyncRig{release: make(chan struct{})}
	exec := core.ToolExecutorFunc(func(ctx context.Context, c core.ToolCall) (core.ToolResult, error) {
		r.runs.Add(1)
		select {
		case <-r.release:
			return core.ToolResult{Status: core.ToolSucceeded, Result: json.RawMessage(`{"answer":42}`)}, nil
		case <-ctx.Done():
			r.canceled.Add(1)
			return core.ToolResult{}, ctx.Err()
		}
	})
	defs := []core.ToolDefinition{
		{Name: "slow", InputSchema: json.RawMessage(`{"type":"object"}`), Policy: core.ToolPolicy{Async: true}},
		{Name: "other", InputSchema: json.RawMessage(`{"type":"object"}`), Policy: core.ToolPolicy{Async: true}},
	}
	reg := NewRegistry()
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return passSTTStage{}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) {
		return core.NewFakeTTSProcessor(core.FakeTTSConfig{FramesPerUnit: 2}), nil
	})
	reg.Register("google.llm", func(_ *Config, s StageSpec) (core.Processor, error) {
		var fail error
		if fallback {
			fail = errors.New("503 upstream")
		}
		r.llm = newAsyncFakeLLM(t, s, fail)
		return r.llm, nil
	})
	var fb *asyncFakeLLM
	reg.Register("xai.llm", func(_ *Config, s StageSpec) (core.Processor, error) {
		fb = newAsyncFakeLLM(t, s, nil)
		return fb, nil
	})
	call := CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, MaxMessages: 20, MaxTokens: 4000,
		LLM: StageSpec{Provider: "google", Tools: defs, ToolExecutor: exec}}
	if fallback {
		call.LLMFallbacks = []StageSpec{{Provider: "xai", Tools: defs, ToolExecutor: exec}}
	}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	cfg := DefaultConfig(ProfileDevelopment)
	for _, m := range mods {
		m(cfg)
	}
	pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, core.NewFakeTransportOutput(core.FakeTransportConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	r.sess, err = core.NewSession(7, pipe, core.DefaultResourceBudget())
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
	_ = fb
	return r
}

// An async tool acks, the user keeps talking, the results land after the
// generation ended: both envelopes are injected once and one assistant turn
// (whose model sees them) tells the user.
func TestCallPipelineAsyncToolsResultTriggersAssistantTurn(t *testing.T) {
	r := newAsyncRig(t, false)
	r.say(t, "t1", "run both")
	r.waitSpoken(t, "On it.")
	r.say(t, "t2", "how are you")
	r.waitSpoken(t, "ok")
	close(r.release)
	r.waitSpoken(t, "Result: ")
	// The announcement streams as synthesis units: wait for both envelopes.
	r.waitSpoken(t, `"tool":"slow"`)
	r.waitSpoken(t, `"tool":"other"`)
	out := r.spoken()
	if strings.Count(out, `"tool":"slow"`) != 1 || strings.Count(out, `"tool":"other"`) != 1 || !strings.Contains(out, `"answer":42`) {
		t.Fatalf("spoken = %q", out)
	}
	time.Sleep(700 * time.Millisecond) // past the settle window: no second announcement
	if strings.Count(r.spoken(), "Result: ") != 1 || r.runs.Load() != 2 {
		t.Fatalf("spoken = %q runs=%d", r.spoken(), r.runs.Load())
	}
}

func TestCallPipelineAsyncResultDeferredWhileUserSpeaks(t *testing.T) {
	r := newAsyncRig(t, false)
	r.say(t, "t1", "run it")
	r.waitSpoken(t, "On it.")
	if err := r.sess.Reenter(&core.UserStartedSpeakingFrame{}, core.Downstream, CallUserContextIndex, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	close(r.release)
	time.Sleep(800 * time.Millisecond)
	if strings.Contains(r.spoken(), "Result: ") {
		t.Fatalf("assistant barged in on the user: %q", r.spoken())
	}
	if err := r.sess.Reenter(&core.UserStoppedSpeakingFrame{}, core.Downstream, CallUserContextIndex, 0); err != nil {
		t.Fatal(err)
	}
	r.waitSpoken(t, "Result: ")
}

func TestCallPipelineAsyncCallsCancelledOnSessionClose(t *testing.T) {
	r := newAsyncRig(t, false)
	r.say(t, "t1", "run both")
	r.waitSpoken(t, "On it.")
	for end := time.Now().Add(2 * time.Second); r.runs.Load() < 2 && time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
	}
	if err := r.sess.Close(); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(2 * time.Second); r.canceled.Load() < 2 && time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
	}
	if r.canceled.Load() != 2 {
		t.Fatalf("cancelled %d of 2 in-flight async calls", r.canceled.Load())
	}
}

// The primary acknowledges an async call, then fails with a failover-class
// error before any text. The call already acted, so the generation is not
// replayed on the fallback (which would run the tool again).
func TestCallPipelineAsyncCallBlocksFailoverReplay(t *testing.T) {
	r := newAsyncRig(t, true)
	r.say(t, "t1", "run it")
	time.Sleep(600 * time.Millisecond)
	if n := r.runs.Load(); n != 1 {
		t.Fatalf("tool ran %d times, want 1", n)
	}
	if strings.Contains(r.spoken(), "On it.") {
		t.Fatalf("fallback answered: %q", r.spoken())
	}
}

// With the turn manager on, an open user turn (turn events, not VAD) defers
// the async-result turn; the synthetic "async-N" turn then plays and commits
// even though the turn manager never started it.
func TestCallPipelineAsyncResultWithTurnManager(t *testing.T) {
	r := newAsyncRig(t, false, func(c *Config) { c.Turn.Manager = true })
	r.say(t, "t1", "run it")
	r.waitSpoken(t, "On it.")
	if err := r.sess.Reenter(&core.TurnEventFrame{Kind: core.TurnEventStarted}, core.Downstream, r.userCtx(), 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	close(r.release)
	time.Sleep(800 * time.Millisecond)
	if strings.Contains(r.spoken(), "Result: ") {
		t.Fatalf("assistant talked over an open user turn: %q", r.spoken())
	}
	r.say(t, "t2", "how are you") // the user turn commits
	r.waitSpoken(t, "ok")
	r.waitSpoken(t, "Result: ")
}
