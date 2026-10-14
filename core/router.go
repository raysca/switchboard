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
	ErrRoutePending = errors.New("core: a route is already in progress")
	ErrRouteLimit   = errors.New("core: route limit reached for this call")
	ErrRouteFlipped = errors.New("core: route already completed by the caller's turn")
	ErrRouteClosed  = errors.New("core: route closed with the session")
)

type RouteEventKind string

const (
	RouteCommitted RouteEventKind = "committed"
	RoutePreparing RouteEventKind = "preparing"
	RouteActive    RouteEventKind = "active"
	RouteFailed    RouteEventKind = "failed"
	RouteClosed    RouteEventKind = "closed"
)

type RouteEvent struct {
	Kind     RouteEventKind
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
	OnEvent       func(RouteEvent)
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
		return nil, errors.New("core: route needs LLM, TTS, Trigger, Build and Entry")
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
	h := &Router{cfg: cfg, active: cfg.Entry}
	cfg.LLM.SetBefore(h.beforeLLM)
	cfg.LLM.AddCloser(h.Close)
	return h, nil
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

// Close ends the route with the session: a step built but never installed
// is closed, and a build still running closes its stages when it finishes.
func (h *Router) Close() {
	h.mu.Lock()
	h.closed = true
	parked := h.ready
	h.pending, h.ready = "", nil
	h.mu.Unlock()
	if parked != nil {
		closeStages(*parked)
	}
}

func (h *Router) Active() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active
}

// CanTransfer reports whether another route may be committed.
func (h *Router) CanTransfer() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count < h.cfg.Max
}

func (h *Router) emit(e RouteEvent) {
	if h.cfg.OnEvent != nil {
		h.cfg.OnEvent(e)
	}
}

// Commit starts the route to target. It returns at once; the step is built
// in the background while the old step finishes its farewell.
func (h *Router) Commit(target string) error {
	sc := h.cfg.LLM.Session()
	if sc == nil {
		return ErrSwapNotAttached
	}
	h.mu.Lock()
	switch {
	case h.closed:
		h.mu.Unlock()
		return ErrRouteClosed
	case h.pending != "":
		h.mu.Unlock()
		return ErrRoutePending
	case h.count >= h.cfg.Max:
		h.mu.Unlock()
		return ErrRouteLimit
	}
	from := h.active
	h.pending, h.ready, h.started = target, nil, time.Now()
	h.count++
	h.mu.Unlock()
	h.emit(RouteEvent{Kind: RouteCommitted, From: from, To: target})
	h.emit(RouteEvent{Kind: RoutePreparing, From: from, To: target})
	if err := sc.Go(func(ctx context.Context) { h.build(ctx, from, target) }); err != nil {
		h.fail(from, target, fmt.Errorf("start build: %w", err))
		return err
	}
	return nil
}

func (h *Router) build(ctx context.Context, from, target string) {
	bctx, cancel := context.WithTimeout(ctx, h.cfg.BuildTimeout)
	defer cancel()
	stages, err := h.cfg.Build(bctx, target)
	if err == nil && (stages.LLM == nil || stages.TTS == nil) {
		err = errors.New("build returned no stages")
	}
	if err != nil {
		h.fail(from, target, err)
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		closeStages(stages) // the call ended while this step was being built
		return
	}
	h.ready = &stages
	h.mu.Unlock()
	h.cfg.Trigger.Deliver(AsyncToolResult{
		Call:   ToolCall{Name: "transfer_to_" + target},
		Inject: func() error { return h.flip(target, true) },
		Speaks: stages.Speaks,
	})
}

func (h *Router) fail(from, target string, err error) {
	h.mu.Lock()
	elapsed := time.Since(h.started)
	h.pending, h.ready = "", nil
	h.mu.Unlock()
	h.emit(RouteEvent{Kind: RouteFailed, From: from, To: target, Err: err, Elapsed: elapsed})
}

// beforeLLM runs ahead of every frame the LLM stage sees. A caller's own turn
// arriving while a built target waits flips first, so the new step answers.
func (h *Router) beforeLLM(f Frame, dir Direction) {
	um, ok := f.(*UserMessageFrame)
	if !ok || dir != Downstream || strings.HasPrefix(um.Turn, "async-") {
		return
	}
	h.mu.Lock()
	target, ready := h.pending, h.ready != nil
	h.mu.Unlock()
	if ready {
		_ = h.flip(target, false)
	}
}

// flip swaps the built target in. It runs at most once per commit. TTS is
// replaced first: if the LLM swap then fails, the call is left on the old
// model with the new voice (degraded but working) and RouteFailed says so.
func (h *Router) flip(target string, viaTrigger bool) error {
	h.mu.Lock()
	if h.pending != target || h.ready == nil {
		h.mu.Unlock()
		return ErrRouteFlipped
	}
	stages, from, started := *h.ready, h.active, h.started
	h.pending, h.ready, h.active = "", nil, target
	h.mu.Unlock()

	retireTTS, err := h.cfg.TTS.Replace(stages.TTS)
	var retireLLM func(time.Duration) error
	if err == nil {
		retireLLM, err = h.cfg.LLM.Replace(stages.LLM)
	}
	if err != nil {
		h.mu.Lock()
		h.active = from
		h.mu.Unlock()
		if retireTTS != nil { // the TTS swap succeeded: do not leak the stage it replaced
			h.retire(h.cfg.LLM.Session(), from, retireTTS)
			closeProc(stages.LLM) // never installed
		} else {
			closeStages(stages) // neither was installed
		}
		h.emit(RouteEvent{Kind: RouteFailed, From: from, To: target, Err: err, Elapsed: time.Since(started)})
		return err
	}
	sc := h.cfg.LLM.Session()
	if viaTrigger {
		// Idle flip: late frames of the old step are stale. A flip at the caller's
		// turn must not bump: that turn was admitted (and its interruption fenced
		// the old step) at the current generation, and the new step's reply has to
		// carry it or the assistant context discards the reply as stale.
		sc.IncrementGeneration()
	}
	h.retire(sc, from, retireLLM, retireTTS)
	h.emit(RouteEvent{Kind: RouteActive, From: from, To: target, Elapsed: time.Since(started)})
	if viaTrigger && h.cfg.Greet != nil {
		return h.cfg.Greet(target)
	}
	return nil
}

func (h *Router) retire(sc SessionController, from string, fns ...func(time.Duration) error) {
	_ = sc.Go(func(context.Context) {
		for _, fn := range fns {
			_ = fn(h.cfg.RetireTimeout)
		}
		h.emit(RouteEvent{Kind: RouteClosed, From: from})
	})
}
