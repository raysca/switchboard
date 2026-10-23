package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrHandoffPending = errors.New("core: a handoff is already in progress")
	ErrHandoffLimit   = errors.New("core: handoff limit reached for this call")
	ErrHandoffFlipped = errors.New("core: handoff already completed by the caller's turn")
	ErrHandoffClosed  = errors.New("core: handoff closed with the session")
)

type HandoffEventKind string

const (
	HandoffCommitted HandoffEventKind = "committed"
	HandoffPreparing HandoffEventKind = "preparing"
	HandoffActive    HandoffEventKind = "active"
	HandoffFailed    HandoffEventKind = "failed"
	HandoffClosed    HandoffEventKind = "closed"
)

type HandoffEvent struct {
	Kind     HandoffEventKind
	From, To string
	Err      error
	Elapsed  time.Duration // commit to active/failed, monotonic
}

// StepStages are the processors of one step, built when it is activated.
type StepStages struct {
	LLM, TTS Processor
	// Speaks: the step says its own line from Greet, so the idle flip starts no model turn for it.
	Speaks bool
}

type BuildStepFunc func(ctx context.Context, stepID string) (StepStages, error)

type RouterConfig struct {
	Entry    string
	LLM, TTS *StageSwap
	// Trigger is the call's AsyncTrigger: a built target is delivered to it so
	// the flip happens when the call is idle.
	Trigger *AsyncTrigger
	Build   BuildStepFunc
	// Greet adds the "caller handed to you" message to history; it runs only
	// when the flip is idle-driven (the caller has not spoken yet).
	Greet         func(stepID string) error
	OnEvent       func(HandoffEvent)
	Max           int           // transfers per call (default 10)
	BuildTimeout  time.Duration // default 10s
	RetireTimeout time.Duration // default 2s
}

// Router moves the call between steps: exactly one step's LLM and TTS are in
// use at a time (plus the incoming step while it is being built).
type Router struct {
	cfg RouterConfig

	mu      sync.Mutex
	active  string
	pending string      // committed target, "" when idle
	ready   *StepStages // built target waiting for the flip
	started time.Time
	count   int
	closed  bool
}

func NewRouter(cfg RouterConfig) (*Router, error) {
	if cfg.LLM == nil || cfg.TTS == nil || cfg.Trigger == nil || cfg.Build == nil || cfg.Entry == "" {
		return nil, errors.New("core: handoff needs LLM, TTS, Trigger, Build and Entry")
	}
	if cfg.Max <= 0 {
		cfg.Max = 10
	}
	if cfg.BuildTimeout <= 0 {
		cfg.BuildTimeout = 10 * time.Second
	}
	if cfg.RetireTimeout <= 0 {
		cfg.RetireTimeout = 2 * time.Second
	}
	rt := &Router{cfg: cfg, active: cfg.Entry}
	cfg.LLM.SetBefore(rt.beforeLLM)
	cfg.LLM.AddCloser(rt.Close)
	return rt, nil
}

func closeProc(p Processor) {
	if c, ok := p.(Closer); ok {
		_ = c.Close()
	}
}

func closeStages(s StepStages) {
	closeProc(s.LLM)
	closeProc(s.TTS)
}

// Close ends the handoff with the session: a step built but never installed
// is closed, and a build still running closes its stages when it finishes.
func (rt *Router) Close() {
	rt.mu.Lock()
	rt.closed = true
	parked := rt.ready
	rt.pending, rt.ready = "", nil
	rt.mu.Unlock()
	if parked != nil {
		closeStages(*parked)
	}
}

func (rt *Router) emit(e HandoffEvent) {
	if rt.cfg.OnEvent != nil {
		rt.cfg.OnEvent(e)
	}
}

// Commit starts the handoff to target. It returns at once; the step is built
// in the background while the old step finishes its farewell.
func (rt *Router) Commit(target string) error {
	sc := rt.cfg.LLM.Session()
	if sc == nil {
		return ErrSwapNotAttached
	}
	rt.mu.Lock()
	switch {
	case rt.closed:
		rt.mu.Unlock()
		return ErrHandoffClosed
	case rt.pending != "":
		rt.mu.Unlock()
		return ErrHandoffPending
	case rt.count >= rt.cfg.Max:
		rt.mu.Unlock()
		return ErrHandoffLimit
	}
	from := rt.active
	rt.pending, rt.ready, rt.started = target, nil, time.Now()
	rt.count++
	rt.mu.Unlock()
	rt.emit(HandoffEvent{Kind: HandoffCommitted, From: from, To: target})
	rt.emit(HandoffEvent{Kind: HandoffPreparing, From: from, To: target})
	if err := sc.Go(func(ctx context.Context) { rt.build(ctx, from, target) }); err != nil {
		rt.fail(from, target, fmt.Errorf("start build: %w", err))
		return err
	}
	return nil
}

func (rt *Router) build(ctx context.Context, from, target string) {
	bctx, cancel := context.WithTimeout(ctx, rt.cfg.BuildTimeout)
	defer cancel()
	stages, err := rt.cfg.Build(bctx, target)
	if err == nil && (stages.LLM == nil || stages.TTS == nil) {
		err = errors.New("build returned no stages")
	}
	if err != nil {
		rt.fail(from, target, err)
		return
	}
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		closeStages(stages) // the call ended while this step was being built
		return
	}
	rt.ready = &stages
	rt.mu.Unlock()
	rt.cfg.Trigger.Deliver(AsyncToolResult{
		Call:   ToolCall{Name: "transfer_to_" + target},
		Inject: func() error { return rt.flip(target, true) },
		Speaks: stages.Speaks,
	})
}

func (rt *Router) fail(from, target string, err error) {
	rt.mu.Lock()
	elapsed := time.Since(rt.started)
	rt.pending, rt.ready = "", nil
	rt.mu.Unlock()
	rt.emit(HandoffEvent{Kind: HandoffFailed, From: from, To: target, Err: err, Elapsed: elapsed})
}

// beforeLLM runs ahead of every frame the LLM stage sees. A caller's own turn
// arriving while a built target waits flips first, so the new step answers.
func (rt *Router) beforeLLM(f Frame, dir Direction) {
	um, ok := f.(*UserMessageFrame)
	if !ok || dir != Downstream || strings.HasPrefix(um.Turn, "async-") {
		return
	}
	rt.mu.Lock()
	target, ready := rt.pending, rt.ready != nil
	rt.mu.Unlock()
	if ready {
		_ = rt.flip(target, false)
	}
}

// flip swaps the built target in. It runs at most once per commit. TTS is
// replaced first: if the LLM swap then fails, the call is left on the old
// model with the new voice (degraded but working) and HandoffFailed says so.
func (rt *Router) flip(target string, viaTrigger bool) error {
	rt.mu.Lock()
	if rt.pending != target || rt.ready == nil {
		rt.mu.Unlock()
		return ErrHandoffFlipped
	}
	stages, from, started := *rt.ready, rt.active, rt.started
	rt.pending, rt.ready, rt.active = "", nil, target
	rt.mu.Unlock()

	retireTTS, err := rt.cfg.TTS.Replace(stages.TTS)
	var retireLLM func(time.Duration) error
	if err == nil {
		retireLLM, err = rt.cfg.LLM.Replace(stages.LLM)
	}
	if err != nil {
		rt.mu.Lock()
		rt.active = from
		rt.mu.Unlock()
		if retireTTS != nil { // the TTS swap succeeded: do not leak the stage it replaced
			rt.retire(rt.cfg.LLM.Session(), from, retireTTS)
			closeProc(stages.LLM) // never installed
		} else {
			closeStages(stages) // neither was installed
		}
		rt.emit(HandoffEvent{Kind: HandoffFailed, From: from, To: target, Err: err, Elapsed: time.Since(started)})
		return err
	}
	sc := rt.cfg.LLM.Session()
	if viaTrigger {
		// Idle flip: late frames of the old step are stale. A flip at the caller's
		// turn must not bump: that turn was admitted (and its interruption fenced
		// the old step) at the current generation, and the new step's reply has to
		// carry it or the assistant context discards the reply as stale.
		sc.IncrementGeneration()
	}
	rt.retire(sc, from, retireLLM, retireTTS)
	rt.emit(HandoffEvent{Kind: HandoffActive, From: from, To: target, Elapsed: time.Since(started)})
	if viaTrigger && rt.cfg.Greet != nil {
		return rt.cfg.Greet(target)
	}
	return nil
}

func (rt *Router) retire(sc SessionController, from string, fns ...func(time.Duration) error) {
	_ = sc.Go(func(context.Context) {
		for _, fn := range fns {
			_ = fn(rt.cfg.RetireTimeout)
		}
		rt.emit(HandoffEvent{Kind: HandoffClosed, From: from})
	})
}
