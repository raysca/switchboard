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
	// OnFirstTokenTimeout is called when the first-token watchdog gives up on
	// this candidate (it hung without output), so its breaker can learn of it.
	// May be nil.
	OnFirstTokenTimeout func()
}

// ErrFirstTokenTimeout is the failure of a candidate that produced nothing
// within LLMFailoverConfig.FirstTokenTimeout. Classify should map it to the
// timeout class.
var ErrFirstTokenTimeout = errors.New("core: llm produced no output before the first-token deadline")

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
	// FirstTokenTimeout abandons a candidate that has produced no output (no
	// text, no tool call) that long after it started, and moves to the next
	// one. It never bounds a generation that has begun: a healthy long answer
	// is not cut. Zero disables it. Only armed while a next candidate exists.
	FirstTokenTimeout time.Duration
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

	mu     sync.Mutex
	cur    *failoverGen
	closed bool // set by Close: the watchdog never fails over again
}

// failoverGen is the state of the latest generation.
type failoverGen struct {
	gen       uint64
	ctx       context.Context // trace parentage: the user message's, for replays
	user      *UserMessageFrame
	order     []int // candidate indexes, breaker-open ones last
	pos       int   // position in order of the running candidate
	committed bool
	done      bool // interrupted or finished: no more failover
	start     time.Time
	timer     *time.Timer // first-token watchdog of the running candidate
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

func (w *LLMFailover) Attach(s SessionController, index int) error {
	w.session, w.index = s, index
	return attachCandidates(s, index, len(w.cfg.Candidates), func(i int) Processor { return w.cfg.Candidates[i].Proc }, w.fromCandidate)
}

func (w *LLMFailover) SetContextProvider(fn func() []Message) {
	for _, c := range w.cfg.Candidates {
		if s, ok := c.Proc.(interface{ SetContextProvider(func() []Message) }); ok {
			s.SetContextProvider(fn)
		}
	}
}

func (w *LLMFailover) Close() error {
	w.mu.Lock()
	w.closed = true
	if w.cur != nil {
		stopTimerLocked(w.cur)
	}
	w.mu.Unlock()
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
	case *llmExpire:
		w.firstTokenExpired(fr.g, fr.pos) // the watchdog fired: handled here, on the run loop
		return nil
	case *InterruptionFrame:
		w.mu.Lock()
		var active Processor
		if g := w.cur; g != nil {
			if !g.done {
				active = w.cfg.Candidates[g.order[g.pos]].Proc
			}
			g.done = true
			stopTimerLocked(g)
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
	w.armLocked(g)
	var stale Processor
	if prev != nil && !prev.done {
		prev.done = true
		stopTimerLocked(prev)
		if p := w.cfg.Candidates[prev.order[prev.pos]].Proc; p != w.cfg.Candidates[g.order[0]].Proc {
			stale = p // the new first candidate cancels its own previous work
		}
	}
	w.mu.Unlock()
	if stale != nil {
		_ = stale.ProcessFrame(ctx, &InterruptionFrame{}, Downstream, swallow{})
	}
	pe := &pushTracker{Emitter: e}
	err := w.cfg.Candidates[g.order[0]].Proc.ProcessFrame(ctx, um, dir, pe)
	if err == nil || pe.downstreamErr != nil {
		return err // started, or a later stage refused the user message: not this candidate's failure
	}
	return w.syncFailure(g, err, um, dir, e, pe.pushed)
}

// pushTracker records whether a candidate forwarded the user message and what
// the stages after this one answered.
type pushTracker struct {
	Emitter
	pushed        bool
	downstreamErr error
}

func (p *pushTracker) Push(f Frame) error { return p.note(f, p.Emitter.Push(f)) }

func (p *pushTracker) PushDirection(f Frame, d Direction) error {
	return p.note(f, p.Emitter.PushDirection(f, d))
}

func (p *pushTracker) note(f Frame, err error) error {
	if _, ok := f.(*UserMessageFrame); ok {
		p.pushed, p.downstreamErr = true, err
	}
	return err
}

// syncFailure handles the first candidate refusing the user message
// synchronously (its request could not start). Like an error frame before any
// output, it moves to the next candidate when mayFailOver allows: a provider
// failure does; the session's own limits do not (server llmClassify maps
// ErrTooManyTasks and ErrSessionClosed to ""), and then the error is the
// session's, as before. The user message is forwarded here when the failed
// candidate had not forwarded it.
func (w *LLMFailover) syncFailure(g *failoverGen, err error, um *UserMessageFrame, dir Direction, e Emitter, pushed bool) error {
	w.mu.Lock()
	if w.cur != g || g.done || g.pos != 0 || !w.mayFailOver(g, err) {
		if w.cur == g && !g.done {
			g.done = true
			stopTimerLocked(g)
		}
		w.mu.Unlock()
		return err
	}
	if ferr := w.failoverLocked(g, g.order[0], err, func() error { return err }); ferr != nil {
		return ferr // every candidate failed synchronously: the session's error, as before
	}
	if !pushed {
		return e.PushDirection(um, dir)
	}
	return nil
}

// fromCandidate sees every frame a candidate re-enters.
func (w *LLMFailover) fromCandidate(idx int, f Frame, dir Direction, resume int, gen uint64) error {
	if _, settles := f.(*ToolResultFrame); settles {
		// An announced call is settled whatever became of its attempt
		// (interrupted, abandoned).
		return w.session.Reenter(f, dir, resume, gen)
	}
	w.mu.Lock()
	g := w.cur
	switch {
	case g == nil:
		w.mu.Unlock()
		return w.session.Reenter(f, dir, resume, gen)
	case g.done || g.order[g.pos] != idx:
		// A finished, interrupted or abandoned attempt: drop, whatever its
		// generation (a replay can start under a newer one than g.gen), except
		// a tool call its loop already announced (it may be running).
		w.mu.Unlock()
		if _, announced := f.(*ToolCallFrame); announced {
			return w.session.Reenter(f, dir, resume, gen)
		}
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
			stopTimerLocked(g)
		}
		w.mu.Unlock()
		return w.session.Reenter(f, dir, resume, gen)
	case ef.Fatal || !w.mayFailOver(g, ef.Err):
		g.done = true
		stopTimerLocked(g)
		w.mu.Unlock()
		return w.session.Reenter(f, dir, resume, gen)
	}
	return w.failoverLocked(g, idx, ef.Err, func() error {
		return w.session.Reenter(f, dir, resume, gen) // chain exhausted: one clean error
	})
}

// failoverLocked moves the generation off candidate idx (failed with err) to
// the next one that starts. Called with w.mu held; unlocks it. exhausted runs
// when no candidate is left.
func (w *LLMFailover) failoverLocked(g *failoverGen, idx int, err error, exhausted func() error) error {
	from := w.cfg.Candidates[idx].Provider
	class := failoverClass(w.cfg.Classify, err)
	// Try the following candidates; a candidate that cannot even start counts as failed.
	for g.pos+1 < len(g.order) {
		g.pos++
		next := w.cfg.Candidates[g.order[g.pos]]
		w.mu.Unlock()
		noteFailover(w.session, w.cfg.OnFailover, DebugLLMFailover, from, next.Provider, class, TurnID(g.user.Turn), Generation(g.gen))
		err := next.Proc.ProcessFrame(g.ctx, g.user, Downstream, swallow{})
		if err == nil {
			w.mu.Lock()
			late := g.done // interrupted while starting: the cancel missed it
			w.armLocked(g)
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
	stopTimerLocked(g)
	w.mu.Unlock()
	return exhausted()
}

// armLocked starts the first-token watchdog for the running candidate (w.mu held).
func (w *LLMFailover) armLocked(g *failoverGen) {
	if w.cfg.FirstTokenTimeout <= 0 || w.closed || g.done || g.pos+1 >= len(g.order) {
		return
	}
	stopTimerLocked(g)
	pos := g.pos
	g.timer = time.AfterFunc(w.cfg.FirstTokenTimeout, func() { w.postExpiry(&llmExpire{g: g, pos: pos}) })
}

// llmExpire carries the first-token watchdog's expiry onto the session run loop, so the
// timer goroutine never drives candidates (a panic there would kill the process).
type llmExpire struct {
	g   *failoverGen
	pos int
}

func (*llmExpire) Type() FrameType { return FrameInternal }

// postExpiry queues e for the run loop; a full inbox refuses internal frames, so it retries
// until the session takes it, the generation is settled, or the session closes (ErrSessionClosed).
func (w *LLMFailover) postExpiry(e *llmExpire) {
	w.mu.Lock()
	stale := w.closed || e.g.done || e.g.committed || e.g.pos != e.pos
	w.mu.Unlock()
	if stale {
		return
	}
	if errors.Is(w.session.Reenter(e, Downstream, w.index, 0), ErrQueueFull) {
		time.AfterFunc(5*time.Millisecond, func() { w.postExpiry(e) })
	}
}

// stopTimerLocked disarms g's watchdog (w.mu held). A callback already running
// is neutralised by firstTokenExpired re-checking closed/done/committed under w.mu.
func stopTimerLocked(g *failoverGen) {
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

// firstTokenExpired abandons the candidate at pos if it is still silent: its
// breaker is told, the next candidate starts, and the silent one is cancelled
// (anything it emits later is dropped like any abandoned attempt).
func (w *LLMFailover) firstTokenExpired(g *failoverGen, pos int) {
	w.mu.Lock()
	if w.closed || w.cur != g || g.done || g.committed || g.pos != pos || !w.mayFailOver(g, ErrFirstTokenTimeout) {
		w.mu.Unlock()
		return
	}
	idx := g.order[pos]
	old := w.cfg.Candidates[idx]
	if old.OnFirstTokenTimeout != nil {
		old.OnFirstTokenTimeout()
	}
	_ = w.failoverLocked(g, idx, ErrFirstTokenTimeout, func() error {
		return w.session.Reenter(&ErrorFrame{Err: ErrFirstTokenTimeout}, Downstream, w.index, g.gen)
	})
	_ = old.Proc.ProcessFrame(g.ctx, &InterruptionFrame{}, Downstream, swallow{})
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
	class := failoverClass(w.cfg.Classify, err)
	return class != "" && (w.cfg.Allow == nil || w.cfg.Allow(class))
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
