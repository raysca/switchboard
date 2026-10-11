package core

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type handoffSession struct {
	ctx      context.Context
	gens     atomic.Uint64
	reenters atomic.Int64
}

func (s *handoffSession) ID() uint64                  { return 1 }
func (s *handoffSession) Context() context.Context    { return s.ctx }
func (s *handoffSession) CurrentGeneration() uint64   { return s.gens.Load() }
func (s *handoffSession) IncrementGeneration() uint64 { return s.gens.Add(1) }
func (s *handoffSession) Go(fn func(context.Context)) error {
	go fn(s.ctx)
	return nil
}
func (s *handoffSession) Reenter(Frame, Direction, int, uint64) error {
	s.reenters.Add(1)
	return nil
}

type nopEmitter struct{}

func (nopEmitter) Push(Frame) error                     { return nil }
func (nopEmitter) PushDirection(Frame, Direction) error { return nil }

type handoffEnv struct {
	llm, tts                       *StageSwap
	oldLLM, oldTTS, newLLM, newTTS *probeStage
	trig                           *AsyncTrigger
	sess                           *handoffSession
	greeted                        atomic.Int64
	events                         chan HandoffEvent
	mu                             sync.Mutex
	seen                           []HandoffEvent
}

func newTrigger() *AsyncTrigger {
	return NewAsyncTrigger(AsyncTriggerConfig{Settle: time.Millisecond, Poll: time.Millisecond})
}

// busy makes a trigger report the caller as speaking, so it never fires on its own.
func busy(t *AsyncTrigger) *AsyncTrigger {
	_ = t.ProcessFrame(context.Background(), &UserStartedSpeakingFrame{}, Downstream, nopEmitter{})
	return t
}

func newHandoffEnv(t *testing.T, mod func(*HandoffConfig)) (*Handoff, *handoffEnv) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	env := &handoffEnv{
		oldLLM: &probeStage{}, oldTTS: &probeStage{}, newLLM: &probeStage{}, newTTS: &probeStage{},
		sess: &handoffSession{ctx: ctx}, events: make(chan HandoffEvent, 32),
	}
	env.llm, env.tts = NewStageSwap(env.oldLLM), NewStageSwap(env.oldTTS)
	cfg := HandoffConfig{
		Entry: "front", LLM: env.llm, TTS: env.tts, Trigger: newTrigger(),
		Build: func(context.Context, string) (NodeStages, error) {
			return NodeStages{LLM: env.newLLM, TTS: env.newTTS}, nil
		},
		Greet: func(string) error { env.greeted.Add(1); return nil },
		OnEvent: func(e HandoffEvent) {
			env.mu.Lock()
			env.seen = append(env.seen, e)
			env.mu.Unlock()
			env.events <- e
		},
		RetireTimeout: time.Second,
	}
	if mod != nil {
		mod(&cfg)
	}
	env.trig = cfg.Trigger
	if err := env.trig.Attach(env.sess, 0); err != nil {
		t.Fatal(err)
	}
	if err := env.llm.Attach(env.sess, 1); err != nil {
		t.Fatal(err)
	}
	if err := env.tts.Attach(env.sess, 2); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandoff(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h, env
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (e *handoffEnv) waitEvent(t *testing.T, kind HandoffEventKind) {
	t.Helper()
	eventually(t, "event "+string(kind), func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, s := range e.seen {
			if s.Kind == kind {
				return true
			}
		}
		return false
	})
}

func (e *handoffEnv) waitReady(t *testing.T, h *Handoff) {
	t.Helper()
	eventually(t, "built target", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.ready != nil
	})
}

func TestHandoffFlipsAtIdleAndClosesOldNode(t *testing.T) {
	h, env := newHandoffEnv(t, nil)
	if err := h.Commit("sales"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "sales active", func() bool { return h.Active() == "sales" })
	eventually(t, "old stages closed", func() bool { return env.oldLLM.closed.Load() == 1 && env.oldTTS.closed.Load() == 1 })
	time.Sleep(30 * time.Millisecond)
	if env.oldLLM.closed.Load() != 1 || env.oldTTS.closed.Load() != 1 {
		t.Fatal("each left stage must be closed exactly once")
	}
	if env.greeted.Load() != 1 || env.sess.gens.Load() != 1 {
		t.Fatalf("greet=%d gen=%d", env.greeted.Load(), env.sess.gens.Load())
	}
	if env.newLLM.closed.Load() != 0 || env.newTTS.closed.Load() != 0 {
		t.Fatal("the active node must stay open")
	}
	eventually(t, "trigger started the new node's turn", func() bool { return env.sess.reenters.Load() == 1 })
}

func TestHandoffUserTurnFlipsOnce(t *testing.T) {
	h, env := newHandoffEnv(t, func(c *HandoffConfig) { c.Trigger = busy(newTrigger()) })
	_ = h.Commit("sales")
	env.waitReady(t, h) // built, not flipped: the trigger never goes idle
	if h.Active() != "front" {
		t.Fatal("must not flip while the caller is mid-speech")
	}
	_ = env.llm.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "t1"}, Downstream, nopEmitter{})
	if h.Active() != "sales" || env.newLLM.frames.Load() != 1 || env.oldLLM.frames.Load() != 0 {
		t.Fatalf("the caller's turn must go to the new node: active=%s new=%d old=%d", h.Active(), env.newLLM.frames.Load(), env.oldLLM.frames.Load())
	}
	if env.greeted.Load() != 0 {
		t.Fatal("no greeting when the caller spoke first")
	}
	// The trigger's queued result fires once the caller stops: it must not start a second turn.
	_ = env.trig.ProcessFrame(context.Background(), &UserStoppedSpeakingFrame{}, Downstream, nopEmitter{})
	eventually(t, "trigger dropped the stale result", func() bool { return env.trig.DroppedCount.Load() == 1 })
	if env.sess.reenters.Load() != 0 {
		t.Fatal("a flipped handoff must not start an extra model turn")
	}
}

func TestHandoffAsyncMessageDoesNotFlip(t *testing.T) {
	h, env := newHandoffEnv(t, func(c *HandoffConfig) { c.Trigger = busy(newTrigger()) })
	_ = h.Commit("sales")
	env.waitReady(t, h)
	_ = env.llm.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "async-1"}, Downstream, nopEmitter{})
	if h.Active() != "front" {
		t.Fatal("an async-tool trigger turn must not flip the handoff")
	}
}

func TestHandoffBuildFailureKeepsOldNode(t *testing.T) {
	h, env := newHandoffEnv(t, func(c *HandoffConfig) {
		c.Build = func(context.Context, string) (NodeStages, error) { return NodeStages{}, errors.New("provider down") }
	})
	_ = h.Commit("sales")
	env.waitEvent(t, HandoffFailed)
	if h.Active() != "front" || env.oldLLM.closed.Load() != 0 || env.oldTTS.closed.Load() != 0 {
		t.Fatal("a failed build must leave the old node active and open")
	}
	if err := h.Commit("sales"); err != nil { // pending cleared: a later transfer may try again
		t.Fatalf("commit after failure: %v", err)
	}
}

func TestHandoffPendingAndLimit(t *testing.T) {
	h, env := newHandoffEnv(t, func(c *HandoffConfig) { c.Max = 1; c.Trigger = busy(newTrigger()) })
	_ = h.Commit("sales")
	if err := h.Commit("support"); !errors.Is(err, ErrHandoffPending) {
		t.Fatalf("want ErrHandoffPending, got %v", err)
	}
	env.waitReady(t, h)
	_ = env.llm.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "t1"}, Downstream, nopEmitter{})
	if err := h.Commit("front"); !errors.Is(err, ErrHandoffLimit) || h.CanTransfer() {
		t.Fatalf("limit not enforced: %v can=%v", err, h.CanTransfer())
	}
}

func TestHandoffBuildTimeout(t *testing.T) {
	h, env := newHandoffEnv(t, func(c *HandoffConfig) {
		c.BuildTimeout = 20 * time.Millisecond
		c.Build = func(ctx context.Context, _ string) (NodeStages, error) {
			<-ctx.Done() // a hung provider: only the timeout ends this
			return NodeStages{}, ctx.Err()
		}
	})
	before := runtime.NumGoroutine()
	_ = h.Commit("sales")
	env.waitEvent(t, HandoffFailed)
	time.Sleep(50 * time.Millisecond)
	if h.Active() != "front" || runtime.NumGoroutine() > before+1 {
		t.Fatalf("active=%s goroutines before=%d after=%d", h.Active(), before, runtime.NumGoroutine())
	}
}

func TestNewHandoffRequiresParts(t *testing.T) {
	if _, err := NewHandoff(HandoffConfig{}); err == nil {
		t.Fatal("an empty config must be rejected")
	}
}

type failAttach struct{ probeStage }

func (*failAttach) Attach(SessionController, int) error { return errors.New("attach failed") }

func TestHandoffPartialFlipStillClosesReplacedTTS(t *testing.T) {
	h, env := newHandoffEnv(t, func(c *HandoffConfig) {
		c.Build = func(context.Context, string) (NodeStages, error) {
			return NodeStages{LLM: &failAttach{}, TTS: &probeStage{}}, nil
		}
	})
	_ = h.Commit("sales")
	env.waitEvent(t, HandoffFailed)
	eventually(t, "replaced tts closed", func() bool { return env.oldTTS.closed.Load() == 1 })
	if h.Active() != "front" || env.oldLLM.closed.Load() != 0 {
		t.Fatalf("the old model stays active and open: active=%s llmClosed=%d", h.Active(), env.oldLLM.closed.Load())
	}
}
