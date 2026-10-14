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

type routerSession struct {
	ctx      context.Context
	gens     atomic.Uint64
	reenters atomic.Int64
}

func (s *routerSession) ID() uint64                  { return 1 }
func (s *routerSession) Context() context.Context    { return s.ctx }
func (s *routerSession) CurrentGeneration() uint64   { return s.gens.Load() }
func (s *routerSession) IncrementGeneration() uint64 { return s.gens.Add(1) }
func (s *routerSession) Go(fn func(context.Context)) error {
	go fn(s.ctx)
	return nil
}
func (s *routerSession) Reenter(Frame, Direction, int, uint64) error {
	s.reenters.Add(1)
	return nil
}

type nopEmitter struct{}

func (nopEmitter) Push(Frame) error                     { return nil }
func (nopEmitter) PushDirection(Frame, Direction) error { return nil }

type routerEnv struct {
	llm, tts                       *StageSwap
	oldLLM, oldTTS, newLLM, newTTS *probeStage
	trig                           *AsyncTrigger
	sess                           *routerSession
	greeted                        atomic.Int64
	events                         chan RouteEvent
	mu                             sync.Mutex
	seen                           []RouteEvent
}

func newTrigger() *AsyncTrigger {
	return NewAsyncTrigger(AsyncTriggerConfig{Settle: time.Millisecond, Poll: time.Millisecond})
}

// busy makes a trigger report the caller as speaking, so it never fires on its own.
func busy(t *AsyncTrigger) *AsyncTrigger {
	_ = t.ProcessFrame(context.Background(), &UserStartedSpeakingFrame{}, Downstream, nopEmitter{})
	return t
}

func newRouterEnv(t *testing.T, mod func(*RouterConfig)) (*Router, *routerEnv) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	env := &routerEnv{
		oldLLM: &probeStage{}, oldTTS: &probeStage{}, newLLM: &probeStage{}, newTTS: &probeStage{},
		sess: &routerSession{ctx: ctx}, events: make(chan RouteEvent, 32),
	}
	env.llm, env.tts = NewStageSwap(env.oldLLM), NewStageSwap(env.oldTTS)
	cfg := RouterConfig{
		Entry: "front", LLM: env.llm, TTS: env.tts, Trigger: newTrigger(),
		Build: func(context.Context, string) (StepStages, error) {
			return StepStages{LLM: env.newLLM, TTS: env.newTTS}, nil
		},
		Greet: func(string) error { env.greeted.Add(1); return nil },
		OnEvent: func(e RouteEvent) {
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
	h, err := NewRouter(cfg)
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

func (e *routerEnv) waitEvent(t *testing.T, kind RouteEventKind) {
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

func (e *routerEnv) waitReady(t *testing.T, h *Router) {
	t.Helper()
	eventually(t, "built target", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.ready != nil
	})
}

func TestRouterFlipsAtIdleAndClosesOldNode(t *testing.T) {
	h, env := newRouterEnv(t, nil)
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
		t.Fatal("the active step must stay open")
	}
	eventually(t, "trigger started the new step's turn", func() bool { return env.sess.reenters.Load() == 1 })
}

func TestRouterUserTurnFlipsOnce(t *testing.T) {
	h, env := newRouterEnv(t, func(c *RouterConfig) { c.Trigger = busy(newTrigger()) })
	_ = h.Commit("sales")
	env.waitReady(t, h) // built, not flipped: the trigger never goes idle
	if h.Active() != "front" {
		t.Fatal("must not flip while the caller is mid-speech")
	}
	_ = env.llm.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "t1"}, Downstream, nopEmitter{})
	if h.Active() != "sales" || env.newLLM.frames.Load() != 1 || env.oldLLM.frames.Load() != 0 {
		t.Fatalf("the caller's turn must go to the new step: active=%s new=%d old=%d", h.Active(), env.newLLM.frames.Load(), env.oldLLM.frames.Load())
	}
	if env.greeted.Load() != 0 {
		t.Fatal("no greeting when the caller spoke first")
	}
	// The trigger's queued result fires once the caller stops: it must not start a second turn.
	_ = env.trig.ProcessFrame(context.Background(), &UserStoppedSpeakingFrame{}, Downstream, nopEmitter{})
	eventually(t, "trigger dropped the stale result", func() bool { return env.trig.DroppedCount.Load() == 1 })
	if env.sess.reenters.Load() != 0 {
		t.Fatal("a flipped route must not start an extra model turn")
	}
}

func TestRouterAsyncMessageDoesNotFlip(t *testing.T) {
	h, env := newRouterEnv(t, func(c *RouterConfig) { c.Trigger = busy(newTrigger()) })
	_ = h.Commit("sales")
	env.waitReady(t, h)
	_ = env.llm.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "async-1"}, Downstream, nopEmitter{})
	if h.Active() != "front" {
		t.Fatal("an async-tool trigger turn must not flip the route")
	}
}

func TestRouterBuildFailureKeepsOldNode(t *testing.T) {
	h, env := newRouterEnv(t, func(c *RouterConfig) {
		c.Build = func(context.Context, string) (StepStages, error) { return StepStages{}, errors.New("provider down") }
	})
	_ = h.Commit("sales")
	env.waitEvent(t, RouteFailed)
	if h.Active() != "front" || env.oldLLM.closed.Load() != 0 || env.oldTTS.closed.Load() != 0 {
		t.Fatal("a failed build must leave the old step active and open")
	}
	if err := h.Commit("sales"); err != nil { // pending cleared: a later transfer may try again
		t.Fatalf("commit after failure: %v", err)
	}
}

func TestRouterPendingAndLimit(t *testing.T) {
	h, env := newRouterEnv(t, func(c *RouterConfig) { c.Max = 1; c.Trigger = busy(newTrigger()) })
	_ = h.Commit("sales")
	if err := h.Commit("support"); !errors.Is(err, ErrRoutePending) {
		t.Fatalf("want ErrRoutePending, got %v", err)
	}
	env.waitReady(t, h)
	_ = env.llm.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "t1"}, Downstream, nopEmitter{})
	if err := h.Commit("front"); !errors.Is(err, ErrRouteLimit) || h.CanTransfer() {
		t.Fatalf("limit not enforced: %v can=%v", err, h.CanTransfer())
	}
}

func TestRouterBuildTimeout(t *testing.T) {
	h, env := newRouterEnv(t, func(c *RouterConfig) {
		c.BuildTimeout = 20 * time.Millisecond
		c.Build = func(ctx context.Context, _ string) (StepStages, error) {
			<-ctx.Done() // a hung provider: only the timeout ends this
			return StepStages{}, ctx.Err()
		}
	})
	before := runtime.NumGoroutine()
	_ = h.Commit("sales")
	env.waitEvent(t, RouteFailed)
	time.Sleep(50 * time.Millisecond)
	if h.Active() != "front" || runtime.NumGoroutine() > before+1 {
		t.Fatalf("active=%s goroutines before=%d after=%d", h.Active(), before, runtime.NumGoroutine())
	}
}

func TestNewRouterRequiresParts(t *testing.T) {
	if _, err := NewRouter(RouterConfig{}); err == nil {
		t.Fatal("an empty config must be rejected")
	}
}

type failAttach struct{ probeStage }

func (*failAttach) Attach(SessionController, int) error { return errors.New("attach failed") }

func TestRouterPartialFlipStillClosesReplacedTTS(t *testing.T) {
	h, env := newRouterEnv(t, func(c *RouterConfig) {
		c.Build = func(context.Context, string) (StepStages, error) {
			return StepStages{LLM: &failAttach{}, TTS: &probeStage{}}, nil
		}
	})
	_ = h.Commit("sales")
	env.waitEvent(t, RouteFailed)
	eventually(t, "replaced tts closed", func() bool { return env.oldTTS.closed.Load() == 1 })
	if h.Active() != "front" || env.oldLLM.closed.Load() != 0 {
		t.Fatalf("the old model stays active and open: active=%s llmClosed=%d", h.Active(), env.oldLLM.closed.Load())
	}
}

func TestRouterCloseReleasesBuiltButUnactivatedNode(t *testing.T) {
	h, env := newRouterEnv(t, func(c *RouterConfig) { c.Trigger = busy(newTrigger()) })
	_ = h.Commit("sales")
	env.waitReady(t, h) // built, parked: the caller is mid-speech, so no flip yet
	if err := env.llm.Close(); err != nil {
		t.Fatal(err)
	}
	if env.newLLM.closed.Load() != 1 || env.newTTS.closed.Load() != 1 {
		t.Fatalf("a parked step must be closed with the session: llm=%d tts=%d", env.newLLM.closed.Load(), env.newTTS.closed.Load())
	}
}

func TestRouterBuildFinishingAfterCloseClosesItsStages(t *testing.T) {
	release := make(chan struct{})
	h, env := newRouterEnv(t, func(c *RouterConfig) {
		c.Build = func(context.Context, string) (StepStages, error) {
			<-release
			return StepStages{LLM: nil, TTS: nil}, nil // replaced below
		}
	})
	late := StepStages{LLM: &probeStage{}, TTS: &probeStage{}}
	h.cfg.Build = func(context.Context, string) (StepStages, error) { <-release; return late, nil }
	_ = h.Commit("sales")
	_ = env.llm.Close() // the call ends while the step is still being built
	close(release)
	eventually(t, "late stages closed", func() bool {
		return late.LLM.(*probeStage).closed.Load() == 1 && late.TTS.(*probeStage).closed.Load() == 1
	})
	if h.Active() != "front" {
		t.Fatal("a closed route must not flip")
	}
}

func TestRouterFailedFlipClosesEveryStageItDidNotInstall(t *testing.T) {
	llm, tts := &failAttach{}, &probeStage{}
	h, env := newRouterEnv(t, func(c *RouterConfig) {
		c.Build = func(context.Context, string) (StepStages, error) { return StepStages{LLM: llm, TTS: tts}, nil }
	})
	_ = h.Commit("sales")
	env.waitEvent(t, RouteFailed)
	eventually(t, "uninstalled llm closed", func() bool { return llm.closed.Load() == 1 })
	if tts.closed.Load() != 0 {
		t.Fatal("the TTS that was installed is in use and must stay open")
	}

	// TTS cannot be attached: neither new stage is installed, both must be closed.
	llm2, tts2 := &probeStage{}, &failAttach{}
	h2, env2 := newRouterEnv(t, func(c *RouterConfig) {
		c.Build = func(context.Context, string) (StepStages, error) { return StepStages{LLM: llm2, TTS: tts2}, nil }
	})
	_ = h2.Commit("sales")
	env2.waitEvent(t, RouteFailed)
	eventually(t, "both uninstalled stages closed", func() bool { return llm2.closed.Load() == 1 && tts2.closed.Load() == 1 })
}

func TestRouterUserTurnFlipKeepsTheCallersGeneration(t *testing.T) {
	h, env := newRouterEnv(t, func(c *RouterConfig) { c.Trigger = busy(newTrigger()) })
	_ = h.Commit("sales")
	env.waitReady(t, h)
	_ = env.llm.ProcessFrame(context.Background(), &UserMessageFrame{Turn: "t1", Gen: 0}, Downstream, nopEmitter{})
	if env.sess.gens.Load() != 0 {
		t.Fatalf("the caller's committed turn already fenced the old step; bumping again makes the new step's reply stale (gen=%d)", env.sess.gens.Load())
	}
}

// A step that says its own fixed line needs no model turn: the flip still runs Greet, but the trigger
// starts no turn for it.
func TestRouterSpeakingNodeFlipsAndGreetsWithoutStartingAModelTurn(t *testing.T) {
	h, env := newRouterEnv(t, func(c *RouterConfig) {
		build := c.Build
		c.Build = func(ctx context.Context, id string) (StepStages, error) {
			ns, err := build(ctx, id)
			ns.Speaks = id == "sales"
			return ns, err
		}
	})
	if err := h.Commit("sales"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Greet to run", func() bool { return env.greeted.Load() == 1 })
	if h.Active() != "sales" {
		t.Fatal("sales must be active once Greet ran")
	}
	time.Sleep(100 * time.Millisecond)
	if n := env.sess.reenters.Load(); n != 0 {
		t.Fatalf("a model turn was started (%d reenters)", n)
	}
}
