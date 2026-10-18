package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"govoice/core/resilience"
)

// DebugTTSFailover is emitted when synthesis moves to the next candidate.
// Attributes (the trace allowlist): component (the one switched from),
// provider (the one switched to) and reason (failover class); all bounded.
const DebugTTSFailover = "tts.failover"

// TTSCandidate is one TTS stage processor of a failover chain.
type TTSCandidate struct {
	Provider string
	Proc     Processor
	// Open reports whether this candidate's circuit breaker would refuse now.
	// Open candidates are tried last. May be nil.
	Open func() bool
	// Rate is the output sample rate the candidate is configured for (0 =
	// unknown). expire closes a stalled unit with a frame at this rate, so
	// known rates must all be equal.
	Rate int
}

// TTSCounters is implemented by TTS processors (and the failover wrapper, which
// sums its candidates) so the server can export their lifetime counters
// without type-switching on every provider.
type TTSCounters interface {
	TTSCounts() (requests, errors, retries, stale uint64)
}

// TTSCounts sums the counters of every candidate that reports them.
func (w *TTSFailover) TTSCounts() (req, errs, retries, stale uint64) {
	for _, c := range w.cfg.Candidates {
		if m, ok := c.Proc.(TTSCounters); ok {
			a, b, d, e := m.TTSCounts()
			req, errs, retries, stale = req+a, errs+b, retries+d, stale+e
		}
	}
	return
}

type TTSFailoverConfig struct {
	Candidates []TTSCandidate // ordered, primary first
	// Classify maps a candidate's failure to a failover class; "" never fails over.
	Classify func(error) string
	// Allow reports whether the class may fail over. Nil allows every class.
	Allow func(class string) bool
	// OnFailover is called for every switch (metrics). May be nil.
	OnFailover func(from, to, class string)
	// UnitTimeout bounds how long the candidate running a unit may go without
	// producing a frame of it (plus 20 ms per character of the unit's text)
	// before it is abandoned as wedged. 0 = 30 s.
	UnitTimeout time.Duration
}

// TTSFailover is a TTS stage that runs synthesis units one at a time (providers
// already synthesize strictly in order, so this costs no latency) and moves a
// unit to the next candidate when the current one fails with a failover-class
// error before any of that unit's output was released downstream.
//
// A candidate that fails after audio of a unit started is not replayed (the
// caller would hear it twice): the provider has already closed the unit out and
// the failure passes through as that unit's one error. Either way the failed
// candidate is demoted for the rest of the turn, so following units go to the
// next candidate; the next turn starts from the configured order again.
//
// Candidates see a proxy session; only frames from the candidate that owns the
// unit in flight (or the one that just finished it) reach the pipeline, so late
// output from an abandoned candidate is dropped whatever its generation.
type TTSFailover struct {
	cfg     TTSFailoverConfig
	session SessionController
	index   int

	mu    sync.Mutex
	queue []*ttsUnit
	cur   *ttsUnit   // in flight
	done  []*ttsUnit // recently finished (newest last): their tail frames (stopped, usage, tail error) still arrive
	turn  string
	bad   map[int]string // candidates demoted this turn
	carry *failNote      // a tail failure to report when the next unit actually moves
}

type ttsUnit struct {
	su        *SynthesisUnitFrame
	ctx       context.Context // the unit's (trace parentage for replays)
	order     []int           // candidate indexes
	pos       int             // position in order of the running candidate
	owner     int             // candidate index running it
	committed bool            // a frame of this unit went downstream
	attempt   int             // bumped per candidate start; fences the watchdog
	watchdog  *time.Timer
}

type failNote struct{ from, to, class string }

func NewTTSFailover(cfg TTSFailoverConfig) (*TTSFailover, error) {
	if len(cfg.Candidates) == 0 {
		return nil, errors.New("core: tts failover needs at least one candidate")
	}
	rate := 0
	for _, c := range cfg.Candidates {
		if c.Proc == nil {
			return nil, errors.New("core: tts failover candidate has no processor")
		}
		if c.Rate > 0 && rate > 0 && c.Rate != rate {
			return nil, errors.New("core: tts failover candidates must output the same sample rate")
		}
		if c.Rate > 0 {
			rate = c.Rate
		}
	}
	return &TTSFailover{cfg: cfg, bad: map[int]string{}}, nil
}

type ttsProxySession struct {
	SessionController
	w   *TTSFailover
	idx int
}

func (p ttsProxySession) Reenter(f Frame, dir Direction, resume int, gen uint64) error {
	return p.w.fromCandidate(p.idx, f, dir, resume, gen)
}

func (w *TTSFailover) Attach(s SessionController, index int) error {
	w.session, w.index = s, index
	for i, c := range w.cfg.Candidates {
		if a, ok := c.Proc.(Attacher); ok {
			if err := a.Attach(ttsProxySession{SessionController: s, w: w, idx: i}, index); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *TTSFailover) Close() error {
	w.mu.Lock()
	w.cur.stop()
	w.mu.Unlock()
	var errs []error
	for _, c := range w.cfg.Candidates {
		if cl, ok := c.Proc.(Closer); ok {
			errs = append(errs, cl.Close())
		}
	}
	return errors.Join(errs...)
}

func (w *TTSFailover) ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error {
	switch fr := f.(type) {
	case *SynthesisUnitFrame:
		if w.session == nil {
			return errors.New("core: tts failover not attached to a session")
		}
		if fr.Gen != 0 && fr.Gen < w.session.CurrentGeneration() {
			return nil // stale, as the providers drop it
		}
		// Downstream tracking sees each unit once, however many candidates try it.
		if err := e.Push(fr); err != nil {
			return err
		}
		w.mu.Lock()
		if fr.Turn != w.turn {
			w.turn, w.bad = fr.Turn, map[int]string{}
		}
		if w.cur != nil && w.cur.su.Gen != 0 && w.cur.su.Gen < w.session.CurrentGeneration() {
			w.cur.stop() // fenced unit whose candidate went quiet: never queue behind it
			w.cur = nil
		}
		w.queue = append(w.queue, &ttsUnit{su: fr, ctx: ctx})
		var start *ttsUnit
		var note *failNote
		if w.cur == nil {
			start, note = w.next()
		}
		w.mu.Unlock()
		w.noteFailover(note, fr)
		if start != nil {
			w.run(start)
		}
		return nil
	case *ttsExpire:
		w.expire(fr.u, fr.attempt) // a watchdog fired: handled here, on the run loop
		return nil
	case *InterruptionFrame, *CancelFrame:
		w.mu.Lock()
		w.cur.stop()
		w.queue, w.cur, w.done, w.carry = nil, nil, nil, nil
		w.mu.Unlock()
		var errs []error
		for _, c := range w.cfg.Candidates { // all of them: no candidate keeps synthesizing
			errs = append(errs, c.Proc.ProcessFrame(ctx, f, dir, swallow{}))
		}
		return errors.Join(append(errs, e.PushDirection(f, dir))...)
	}
	return e.PushDirection(f, dir)
}

// next makes the first non-stale queued unit current. Call with w.mu held.
func (w *TTSFailover) next() (*ttsUnit, *failNote) {
	w.cur.stop()
	for len(w.queue) > 0 {
		u := w.queue[0]
		w.queue = w.queue[1:]
		if u.su.Gen != 0 && u.su.Gen < w.session.CurrentGeneration() {
			continue
		}
		var healthy, rest []int
		for i, c := range w.cfg.Candidates {
			if _, bad := w.bad[i]; !bad && (c.Open == nil || !c.Open()) {
				healthy = append(healthy, i)
			} else {
				rest = append(rest, i)
			}
		}
		u.order = append(healthy, rest...)
		w.cur = u
		var note *failNote
		if c := w.carry; c != nil {
			w.carry = nil
			if to := w.cfg.Candidates[u.order[0]].Provider; to != c.from {
				note = &failNote{c.from, to, c.class}
			}
		}
		return u, note
	}
	w.cur = nil
	return nil, nil
}

// run starts u on its current candidate; a candidate that cannot even start
// counts as failed. After the chain is exhausted u's error is reported and the
// next unit starts.
func (w *TTSFailover) run(u *ttsUnit) {
	for u != nil {
		w.mu.Lock()
		if w.cur != u {
			w.mu.Unlock()
			return
		}
		u.owner = u.order[u.pos]
		c := w.cfg.Candidates[u.owner]
		w.mu.Unlock()
		err := c.Proc.ProcessFrame(u.ctx, u.su, Downstream, swallow{})
		if err == nil {
			w.mu.Lock()
			if w.cur == u {
				u.stop()
				u.attempt++
				attempt := u.attempt
				u.watchdog = time.AfterFunc(w.unitTimeout(u), func() { w.postExpire(&ttsExpire{u: u, attempt: attempt}) })
			}
			w.mu.Unlock()
			return
		}
		w.mu.Lock()
		if w.cur != u {
			w.mu.Unlock()
			return
		}
		from, class := c.Provider, "server_error"
		w.bad[u.owner] = class
		if u.pos+1 < len(u.order) {
			u.pos++
			to := w.cfg.Candidates[u.order[u.pos]].Provider
			w.mu.Unlock()
			w.noteFailover(&failNote{from, to, class}, u.su)
			continue
		}
		failed := u
		w.finish(failed)
		w.carry = nil
		var note *failNote
		u, note = w.next()
		w.mu.Unlock()
		w.noteFailover(note, nil)
		_ = w.session.Reenter(&ErrorFrame{Err: err, Turn: TurnID(failed.su.Turn), Gen: Generation(failed.su.Gen), UnitID: failed.su.UnitID}, Downstream, w.index+1, failed.su.Gen)
	}
}

// owned returns the unit a candidate's frame belongs to (the one in flight,
// else the one just finished), or nil to drop the frame. Call with w.mu held.
func (w *TTSFailover) owned(idx int, f Frame, gen uint64) *ttsUnit {
	var unit uint64
	switch fr := f.(type) {
	case *OutputAudioFrame:
		unit = fr.UnitID
	case *TTSStartedFrame:
		unit = fr.UnitID
	case *TTSStoppedFrame:
		unit = fr.UnitID
	case *ErrorFrame:
		unit = fr.UnitID
	}
	// Keyed by (candidate, unit): another candidate's frame for the same unit
	// id (one it was abandoned on) never matches.
	match := func(u *ttsUnit) bool {
		return u != nil && u.owner == idx && (unit == 0 || unit == u.su.UnitID) && (gen == 0 || gen == u.su.Gen)
	}
	if match(w.cur) { // the hot path (audio of the unit in flight): no allocation
		return w.cur
	}
	for i := len(w.done) - 1; i >= 0; i-- {
		if match(w.done[i]) {
			return w.done[i]
		}
	}
	return nil
}

// fromCandidate sees every frame a candidate re-enters.
func (w *TTSFailover) fromCandidate(idx int, f Frame, dir Direction, resume int, gen uint64) error {
	w.mu.Lock()
	u := w.owned(idx, f, gen)
	if u == nil { // abandoned, finished or interrupted: drop, whatever its generation
		w.mu.Unlock()
		return nil
	}
	if u == w.cur && u.watchdog != nil {
		u.watchdog.Reset(w.unitTimeout(u)) // progress
	}
	var started *ttsUnit
	var note *failNote
	switch fr := f.(type) {
	case *UsageMetricsFrame:
	case *ErrorFrame:
		class := w.classOf(fr.Err)
		allowed := !fr.Fatal && class != "" && (w.cfg.Allow == nil || w.cfg.Allow(class))
		if allowed && u == w.cur && !u.committed && u.pos+1 < len(u.order) && !(u.su.Gen != 0 && u.su.Gen < w.session.CurrentGeneration()) {
			from := w.cfg.Candidates[idx].Provider
			w.bad[idx] = class
			u.pos++
			to := w.cfg.Candidates[u.order[u.pos]].Provider
			w.mu.Unlock()
			w.noteFailover(&failNote{from, to, class}, u.su)
			w.run(u)
			return nil
		}
		if allowed {
			// Output may have started (the provider closed the unit out): no replay.
			w.bad[idx] = class
			w.carry = &failNote{from: w.cfg.Candidates[idx].Provider, class: class}
		}
		if u == w.cur && !u.committed { // else its closing frame ends the unit
			w.finish(u)
			started, note = w.next()
		}
	case *OutputAudioFrame:
		u.committed = true
		if fr.IsFinal && u == w.cur {
			w.finish(u)
			started, note = w.next()
		}
	default:
		u.committed = true
	}
	w.mu.Unlock()
	err := w.session.Reenter(f, dir, resume, gen)
	w.noteFailover(note, nil)
	if started != nil {
		w.run(started)
	}
	return err
}

// finish remembers u as finished. A later unit can start (and fail) inside u's
// final-frame re-entry, before u's stopped/usage frames, so more than the last
// one is kept; the few newest suffice. Each extra entry needs another unit to
// finish inside that re-entry, so 4 covers a cascade of 3; deeper cascades only
// lose the oldest unit's tail (stopped, usage, tail error). Call with w.mu held.
func (w *TTSFailover) finish(u *ttsUnit) {
	if len(w.done) >= 4 {
		w.done = w.done[1:]
	}
	w.done = append(w.done, u)
}

func (u *ttsUnit) stop() {
	if u != nil && u.watchdog != nil {
		u.watchdog.Stop()
	}
}

func (w *TTSFailover) unitTimeout(u *ttsUnit) time.Duration {
	d := w.cfg.UnitTimeout
	if d <= 0 {
		d = 30 * time.Second
	}
	return d + time.Duration(utf8.RuneCountInString(u.su.Text))*20*time.Millisecond
}

// ttsExpire carries a unit watchdog's expiry onto the session run loop, so a
// candidate is never driven from the timer goroutine while the run loop drives
// it too (ttsunit.Runner and core.SerialQueue need their callers serialized).
type ttsExpire struct {
	u       *ttsUnit
	attempt int
}

func (*ttsExpire) Type() FrameType { return FrameInternal }

// postExpire queues e for the run loop behind the frames already admitted. A
// full inbox refuses internal frames, so it retries until the session takes it;
// a closed session (ErrSessionClosed) ends it.
func (w *TTSFailover) postExpire(e *ttsExpire) {
	if errors.Is(w.session.Reenter(e, Downstream, w.index, 0), ErrQueueFull) {
		time.AfterFunc(5*time.Millisecond, func() { w.postExpire(e) })
	}
}

// expire abandons a candidate that went quiet on u (wedged socket, a provider
// that ignores cancellation or never closes the unit): it is cancelled and
// demoted, and u moves to the next candidate if nothing was released yet;
// otherwise u ends with one error (plus a closing final frame if audio went
// out, so playback drains) and the next unit starts.
func (w *TTSFailover) expire(u *ttsUnit, attempt int) {
	w.mu.Lock()
	if w.cur != u || u.attempt != attempt {
		w.mu.Unlock()
		return
	}
	c := w.cfg.Candidates[u.owner]
	err := fmt.Errorf("core: tts %s stalled on unit %d: %w", c.Provider, u.su.UnitID, resilience.ErrTimeout)
	class := w.classOf(err)
	if class == "" {
		class = "timeout"
	}
	w.bad[u.owner] = class
	cancel := func() {
		_ = c.Proc.ProcessFrame(u.ctx, &InterruptionFrame{Turn: u.su.Turn, Gen: u.su.Gen}, Downstream, swallow{})
	}
	if !u.committed && u.pos+1 < len(u.order) && (w.cfg.Allow == nil || w.cfg.Allow(class)) {
		u.pos++
		to := w.cfg.Candidates[u.order[u.pos]].Provider
		w.mu.Unlock()
		cancel()
		w.noteFailover(&failNote{c.Provider, to, class}, u.su)
		w.run(u)
		return
	}
	committed := u.committed
	kept := w.done[:0] // the wedged candidate's late frames are dropped; other candidates' tails still pass
	for _, d := range w.done {
		if d.owner != u.owner {
			kept = append(kept, d)
		}
	}
	w.done = kept
	w.carry = &failNote{from: c.Provider, class: class}
	started, note := w.next()
	w.mu.Unlock()
	cancel()
	_ = w.session.Reenter(&ErrorFrame{Err: err, Turn: TurnID(u.su.Turn), Gen: Generation(u.su.Gen), UnitID: u.su.UnitID}, Downstream, w.index+1, u.su.Gen)
	if committed { // close out what was heard so playback drains
		rate := c.Rate
		if rate == 0 {
			rate = 24000
		}
		_ = w.session.Reenter(&OutputAudioFrame{Encoding: EncodingPCM16LE, SampleRate: rate, Channels: 1, Turn: u.su.Turn, Gen: u.su.Gen,
			UnitID: u.su.UnitID, IsFinal: true, TurnFinal: u.su.IsFinal}, Downstream, w.index+1, u.su.Gen)
	}
	w.noteFailover(note, nil)
	w.run(started)
}

func (w *TTSFailover) noteFailover(n *failNote, su *SynthesisUnitFrame) {
	if n == nil {
		return
	}
	if w.cfg.OnFailover != nil {
		w.cfg.OnFailover(n.from, n.to, n.class)
	}
	if em, ok := w.session.(interface{ EmitDebug(DebugEvent) }); ok {
		var turn TurnID
		var gen Generation
		if su != nil {
			turn, gen = TurnID(su.Turn), Generation(su.Gen)
		}
		em.EmitDebug(NewDebugEvent(DebugTTSFailover, time.Now(), "", turn, gen,
			map[string]any{"component": n.from, "provider": n.to, "reason": n.class}))
	}
}

func (w *TTSFailover) classOf(err error) string {
	if w.cfg.Classify == nil {
		return ""
	}
	return w.cfg.Classify(err)
}
