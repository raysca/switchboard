package core_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// llmSink records the user turns that reach the LLM position.
type llmSink struct {
	mu    sync.Mutex
	turns []*core.UserMessageFrame
}

func (l *llmSink) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if um, ok := f.(*core.UserMessageFrame); ok {
		l.mu.Lock()
		l.turns = append(l.turns, um)
		l.mu.Unlock()
	}
	return e.PushDirection(f, dir)
}

func (l *llmSink) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.turns)
}

type triggerRig struct {
	trig     *core.AsyncTrigger
	llm      *llmSink
	sess     *core.Session
	injected atomic.Int32
	busy     atomic.Bool
}

func newTriggerRig(t *testing.T, mod func(*core.AsyncTriggerConfig)) *triggerRig {
	t.Helper()
	r := &triggerRig{llm: &llmSink{}}
	cfg := core.AsyncTriggerConfig{Busy: r.busy.Load, Settle: 20 * time.Millisecond, Poll: 5 * time.Millisecond}
	if mod != nil {
		mod(&cfg)
	}
	r.trig = core.NewAsyncTrigger(cfg)
	pipe, err := core.NewPipeline(r.trig, r.llm, &coretest.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	r.sess, err = core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.sess.Close() })
	return r
}

func (r *triggerRig) deliver(name string) {
	r.trig.Deliver(core.AsyncToolResult{
		Call:   core.ToolCall{ID: name, Name: name},
		Inject: func() error { r.injected.Add(1); return nil },
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func never(t *testing.T, what string, cond func() bool) {
	t.Helper()
	time.Sleep(120 * time.Millisecond) // several settle windows
	if cond() {
		t.Fatalf("unexpected: %s", what)
	}
}

func TestAsyncTriggerIdleInjectsOnceAndStartsATurn(t *testing.T) {
	r := newTriggerRig(t, nil)
	r.deliver("lookup")
	waitFor(t, "turn", func() bool { return r.llm.count() == 1 })
	never(t, "second turn", func() bool { return r.llm.count() != 1 })
	if r.injected.Load() != 1 || r.trig.TriggeredCount.Load() != 1 {
		t.Fatalf("injected=%d triggered=%d", r.injected.Load(), r.trig.TriggeredCount.Load())
	}
}

func TestAsyncTriggerWaitsForUserTurn(t *testing.T) {
	r := newTriggerRig(t, nil)
	if err := r.sess.Post(&core.UserStartedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // let the run loop observe it
	r.deliver("lookup")
	never(t, "turn while the user speaks", func() bool { return r.llm.count() != 0 || r.injected.Load() != 0 })
	if err := r.sess.Post(&core.UserStoppedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn after the user stopped", func() bool { return r.llm.count() == 1 })
}

func TestAsyncTriggerQueuesBehindBotSpeech(t *testing.T) {
	r := newTriggerRig(t, nil)
	if err := r.sess.Reenter(&core.SpeakingStateFrame{State: core.SpeakingStarted}, core.Upstream, r.sess.TailIndex(), 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	r.deliver("lookup")
	never(t, "turn while the bot speaks", func() bool { return r.llm.count() != 0 })
	if err := r.sess.Reenter(&core.SpeakingStateFrame{State: core.SpeakingStopped}, core.Upstream, r.sess.TailIndex(), 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "turn after the bot stopped", func() bool { return r.llm.count() == 1 })
}

func TestAsyncTriggerWaitsForActiveReply(t *testing.T) {
	r := newTriggerRig(t, nil)
	if err := r.sess.Post(&core.UserMessageFrame{Turn: "t1", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "user turn", func() bool { return r.llm.count() == 1 })
	r.busy.Store(true)
	r.deliver("lookup")
	never(t, "turn during a reply", func() bool { return r.llm.count() != 1 })
	r.busy.Store(false)
	waitFor(t, "turn after the reply", func() bool { return r.llm.count() == 2 })
}

func TestAsyncTriggerCoalescesAndInjectsOverflowEarly(t *testing.T) {
	r := newTriggerRig(t, func(c *core.AsyncTriggerConfig) { c.MaxQueue = 2 })
	r.busy.Store(true)
	if err := r.sess.Post(&core.UserMessageFrame{Turn: "t1"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "user turn", func() bool { return r.llm.count() == 1 })
	for _, n := range []string{"a", "b", "c"} {
		r.deliver(n)
	}
	r.busy.Store(false)
	waitFor(t, "one turn for the batch", func() bool { return r.llm.count() == 2 })
	never(t, "extra turn", func() bool { return r.llm.count() != 2 })
	if r.injected.Load() != 3 || r.trig.DroppedCount.Load() != 0 { // "a" injected on overflow, never lost
		t.Fatalf("injected=%d dropped=%d", r.injected.Load(), r.trig.DroppedCount.Load())
	}
}

func TestAsyncTriggerSessionCloseDropsQueueWithoutLeak(t *testing.T) {
	r := newTriggerRig(t, nil)
	if err := r.sess.Post(&core.UserStartedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	r.deliver("lookup")
	if err := r.sess.Close(); err != nil {
		t.Fatalf("close: %v", err) // a leaked worker would be reported here
	}
	if r.injected.Load() != 0 || r.llm.count() != 0 {
		t.Fatal("delivered after close")
	}
	r.deliver("late") // after close: ignored, must not panic or block
}

// A reply with nothing to speak (empty final text) must not leave Active stuck,
// or async results would wait for MaxWait.
func TestAssistantEmptyReplyClearsActive(t *testing.T) {
	asst := core.NewAssistantContextProcessor(core.NewConversationContext(core.ContextConfig{}), core.DefaultAssistantContextConfig())
	pipe, err := core.NewPipeline(asst, &coretest.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	gen := sess.CurrentGeneration()
	if err := sess.Post(&core.UserMessageFrame{Turn: "t1", Gen: gen}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "active", asst.Active)
	if err := sess.Post(&core.LLMTextFrame{Turn: "t1", Gen: gen}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "inactive", func() bool { return !asst.Active() })
}

// A speaking item (a greeting): the other results wait until the call is idle again, or their model
// turn would cut the line off before it is committed.
func TestAsyncTriggerLetsASpeakingItemSpeakBeforeTheOthers(t *testing.T) {
	r := newTriggerRig(t, nil)
	r.busy.Store(true)
	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	r.trig.Deliver(core.AsyncToolResult{
		Call:   core.ToolCall{ID: "flip", Name: "flip"},
		Speaks: true,
		Inject: func() error { note("flip"); r.busy.Store(true); return nil }, // the greeting starts speaking
	})
	r.trig.Deliver(core.AsyncToolResult{
		Call:   core.ToolCall{ID: "tool", Name: "tool"},
		Inject: func() error { note("tool"); return nil },
	})
	r.busy.Store(false)
	waitFor(t, "the no-turn item", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 1 })
	never(t, "the tool result or a model turn while the greeting speaks", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) != 1 || r.llm.count() != 0
	})
	r.busy.Store(false) // the greeting finished
	waitFor(t, "the tool result and its turn", func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 2 && r.llm.count() == 1 })
	if order[0] != "flip" || order[1] != "tool" {
		t.Fatalf("order %v", order)
	}
}
