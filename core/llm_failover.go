package core

import (
	"context"
	"errors"
	"sync"
	"time"
)

// DebugLLMFailover is emitted when a generation moves to the next candidate.
// Attributes (the trace allowlist): component (the one switched from), provider (the one switched
// to) and reason (failover class); all bounded.
const DebugLLMFailover = "llm.failover"

// LLMCandidate is one LLM stage processor of a failover chain.
type LLMCandidate struct {
	Provider string
	Proc     Processor
	// Open reports whether this candidate's circuit breaker would refuse now.
	// Open candidates are tried last. May be nil.
	Open func() bool
}

type LLMFailoverConfig struct {
	Candidates []LLMCandidate // ordered, primary first
	// Classify maps a candidate's failure to a failover class; "" never fails over.
	Classify func(error) string
	// Allow reports whether the class may fail over. Nil allows every class.
	Allow func(class string) bool
	// SideEffects is a monotonic count of tool executions. A generation whose
	// count moved has acted on the world and is never replayed. May be nil.
	SideEffects func() uint64
	// Budget bounds the time since the generation began within which a new
	// candidate may still be started. Zero means unbounded (chain length still bounds).
	Budget time.Duration
	// OnFailover is called for every switch (metrics). May be nil.
	OnFailover func(from, to, class string)
}

// LLMCounters is implemented by LLM processors (and the failover wrapper,
// which sums its candidates) so the server exports their lifetime counters.
type LLMCounters interface {
	LLMCounts() (requests, errors, retries, stale uint64)
}

// LLMCounts sums the counters of every candidate that reports them.
func (w *LLMFailover) LLMCounts() (req, errs, retries, stale uint64) {
	for _, c := range w.cfg.Candidates {
		if m, ok := c.Proc.(LLMCounters); ok {
			a, b, d, e := m.LLMCounts()
			req, errs, retries, stale = req+a, errs+b, retries+d, stale+e
		}
	}
	return
}

// LLMFailover is an LLM stage that runs a generation on the first candidate
// and, only if that fails with a failover-class error before anything reached
// the pipeline (no text, no tool call, no tool execution), replays the same
// request on the next. Once output was forwarded the failure is the turn's:
// the candidate's own error frame passes through and nothing is replayed.
//
// Candidates are attached through a proxy session that sees what they
// re-enter; provider processors keep their own generation fencing and
// cancellation, driven here by forwarding Interruption/Cancel to the candidate
// currently running.
type LLMFailover struct {
	cfg     LLMFailoverConfig
	session SessionController
	index   int

	mu  sync.Mutex
	cur *failoverGen
}

// failoverGen is the state of the latest generation.
type failoverGen struct {
	gen       uint64
	ctx       context.Context // the user message's (trace parentage for replays)
	user      *UserMessageFrame
	order     []int // candidate indexes, breaker-open ones last
	pos       int   // position in order of the running candidate
	committed bool
	done      bool // interrupted or finished: no more failover
	start     time.Time
	effects   uint64
}

func NewLLMFailover(cfg LLMFailoverConfig) (*LLMFailover, error) {
	if len(cfg.Candidates) == 0 {
		return nil, errors.New("core: llm failover needs at least one candidate")
	}
	for _, c := range cfg.Candidates {
		if c.Proc == nil {
			return nil, errors.New("core: llm failover candidate has no processor")
		}
	}
	return &LLMFailover{cfg: cfg}, nil
}

// proxySession shows a candidate the real session except for Reenter.
type proxySession struct {
	SessionController
	w   *LLMFailover
	idx int
}

func (p proxySession) Reenter(f Frame, dir Direction, resume int, gen uint64) error {
	return p.w.fromCandidate(p.idx, f, dir, resume, gen)
}

func (w *LLMFailover) Attach(s SessionController, index int) error {
	w.session, w.index = s, index
	for i, c := range w.cfg.Candidates {
		if a, ok := c.Proc.(Attacher); ok {
			if err := a.Attach(proxySession{SessionController: s, w: w, idx: i}, index); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *LLMFailover) SetContextProvider(fn func() []Message) {
	for _, c := range w.cfg.Candidates {
		if s, ok := c.Proc.(interface{ SetContextProvider(func() []Message) }); ok {
			s.SetContextProvider(fn)
		}
	}
}

func (w *LLMFailover) Close() error {
	var errs []error
	for _, c := range w.cfg.Candidates {
		if cl, ok := c.Proc.(Closer); ok {
			errs = append(errs, cl.Close())
		}
	}
	return errors.Join(errs...)
}

// swallow drops what a replayed candidate pushes synchronously: the user
// message was already forwarded by the first attempt.
type swallow struct{}

func (swallow) Push(Frame) error                     { return nil }
func (swallow) PushDirection(Frame, Direction) error { return nil }

func (w *LLMFailover) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	switch fr := f.(type) {
	case *UserMessageFrame:
		return w.begin(ctx, fr, dir, e)
	case *InterruptionFrame, *CancelFrame:
		w.mu.Lock()
		var active Processor
		if g := w.cur; g != nil {
			if !g.done {
				active = w.cfg.Candidates[g.order[g.pos]].Proc
			}
			g.done = true
		}
		w.mu.Unlock()
		if active != nil {
			return active.ProcessFrame(ctx, f, dir, e) // cancels, then pushes f downstream
		}
	}
	return e.PushDirection(f, dir)
}

func (w *LLMFailover) begin(ctx context.Context, um *UserMessageFrame, dir Direction, e Emitter) error {
	if w.session == nil {
		return errors.New("core: llm failover not attached to a session")
	}
	g := &failoverGen{gen: w.session.CurrentGeneration(), ctx: ctx, user: um, start: time.Now()}
	if um.Gen != 0 {
		g.gen = um.Gen
	}
	if w.cfg.SideEffects != nil {
		g.effects = w.cfg.SideEffects()
	}
	var open []int
	for i, c := range w.cfg.Candidates {
		if c.Open != nil && c.Open() {
			open = append(open, i)
		} else {
			g.order = append(g.order, i)
		}
	}
	g.order = append(g.order, open...)

	w.mu.Lock()
	prev := w.cur
	w.cur = g
	var stale Processor
	if prev != nil && !prev.done {
		prev.done = true
		if p := w.cfg.Candidates[prev.order[prev.pos]].Proc; p != w.cfg.Candidates[g.order[0]].Proc {
			stale = p // the new first candidate cancels its own previous work
		}
	}
	w.mu.Unlock()
	if stale != nil {
		_ = stale.ProcessFrame(ctx, &InterruptionFrame{}, Downstream, swallow{})
	}
	return w.cfg.Candidates[g.order[0]].Proc.ProcessFrame(ctx, um, dir, e)
}

// fromCandidate sees every frame a candidate re-enters.
func (w *LLMFailover) fromCandidate(idx int, f Frame, dir Direction, resume int, gen uint64) error {
	w.mu.Lock()
	g := w.cur
	switch {
	case g == nil:
		w.mu.Unlock()
		return w.session.Reenter(f, dir, resume, gen)
	case g.done || g.order[g.pos] != idx:
		// A finished, interrupted or abandoned attempt: drop, whatever its
		// generation (a replay can start under a newer one than g.gen).
		w.mu.Unlock()
		return nil
	case gen != 0 && gen != g.gen:
		w.mu.Unlock()
		return w.session.Reenter(f, dir, resume, gen) // not ours to judge: session fencing decides
	}
	ef, isErr := f.(*ErrorFrame)
	switch {
	case !isErr:
		if _, usage := f.(*UsageMetricsFrame); !usage {
			g.committed = true
		}
		w.mu.Unlock()
		return w.session.Reenter(f, dir, resume, gen)
	case ef.Fatal || !w.mayFailOver(g, ef.Err):
		g.done = true
		w.mu.Unlock()
		return w.session.Reenter(f, dir, resume, gen)
	}
	from := w.cfg.Candidates[idx].Provider
	class := w.classOf(ef.Err)
	// Try the following candidates; a candidate that cannot even start counts as failed.
	for g.pos+1 < len(g.order) {
		g.pos++
		next := w.cfg.Candidates[g.order[g.pos]]
		w.mu.Unlock()
		w.noteFailover(g, from, next.Provider, class)
		err := next.Proc.ProcessFrame(g.ctx, g.user, Downstream, swallow{})
		if err == nil {
			w.mu.Lock()
			late := g.done // interrupted while starting: the cancel missed it
			w.mu.Unlock()
			if late {
				_ = next.Proc.ProcessFrame(g.ctx, &InterruptionFrame{}, Downstream, swallow{})
			}
			return nil
		}
		from, class = next.Provider, "server_error"
		w.mu.Lock()
		if g.done {
			w.mu.Unlock()
			return nil
		}
	}
	g.done = true
	w.mu.Unlock()
	return w.session.Reenter(f, dir, resume, gen) // chain exhausted: one clean error
}

// mayFailOver is called with w.mu held.
func (w *LLMFailover) mayFailOver(g *failoverGen, err error) bool {
	if g.committed || g.pos+1 >= len(g.order) || w.session.CurrentGeneration() != g.gen {
		return false
	}
	if w.cfg.SideEffects != nil && w.cfg.SideEffects() != g.effects {
		return false
	}
	if w.cfg.Budget > 0 && time.Since(g.start) > w.cfg.Budget {
		return false
	}
	class := w.classOf(err)
	return class != "" && (w.cfg.Allow == nil || w.cfg.Allow(class))
}

func (w *LLMFailover) noteFailover(g *failoverGen, from, to, class string) {
	if w.cfg.OnFailover != nil {
		w.cfg.OnFailover(from, to, class)
	}
	if em, ok := w.session.(interface{ EmitDebug(DebugEvent) }); ok {
		em.EmitDebug(NewDebugEvent(DebugLLMFailover, time.Now(), "", TurnID(g.user.Turn), Generation(g.gen),
			map[string]any{"component": from, "provider": to, "reason": class}))
	}
}

func (w *LLMFailover) classOf(err error) string {
	if w.cfg.Classify == nil {
		return ""
	}
	return w.cfg.Classify(err)
}

// SummarizeText asks the primary candidate (fallbacks never summarize) for one
// plain completion; it errors when the primary has no such capability.
func (w *LLMFailover) SummarizeText(ctx context.Context, system, user string) (string, error) {
	if s, ok := w.cfg.Candidates[0].Proc.(interface {
		SummarizeText(context.Context, string, string) (string, error)
	}); ok {
		return s.SummarizeText(ctx, system, user)
	}
	return "", errors.New("core: primary llm cannot summarize")
}
