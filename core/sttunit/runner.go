// Package sttunit is the stream machinery shared by the streaming STT
// providers (Deepgram, Cartesia, ElevenLabs, Riva): one provider stream per
// utterance, opened off the session run loop with the audio that arrives
// meanwhile held, bounded, in a pre-roll; turn binding (an unbound stream is
// adopted by the next turn); the receive loop; finish and drain; the trace
// span and the error counter. A provider supplies only Dial and the Conn it
// returns (wire format and transcript assembly).
package sttunit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/resilience"
)

// DefaultMaxPreRollBytes bounds the audio a stream holds while it dials: 60 s
// of 16 kHz PCM16 mono, the server's largest STT failover replay (server
// maxSTTReplay), which lands in one burst while a replacement dials. Live
// audio fills it only for as long as the dial takes.
const DefaultMaxPreRollBytes = 60 * 16000 * 2

// ErrStreamEnded is what Conn.Recv returns when the provider ended the stream
// without a failure: a clean close, its idle timeout, or the end of results
// after Finish.
var ErrStreamEnded = errors.New("sttunit: stream ended by the provider")

var (
	errNilConn         = errors.New("sttunit: Dial returned a nil Conn without an error")
	errProviderFailure = errors.New("provider_failure")
	errCanceled        = errors.New("canceled")
)

// Conn is one open provider stream. The Runner calls Send and Finish from one
// goroutine at a time (never concurrently with each other), Recv from the
// stream's own task, and Close from any goroutine, possibly more than once.
type Conn interface {
	// Send writes one chunk of caller audio (16-bit mono PCM).
	Send(ctx context.Context, pcm []byte) error
	// Finish asks for the final results of the audio sent. An error closes
	// the stream without reporting it.
	Finish(ctx context.Context) error
	// Recv blocks for the next provider event and returns the stream's
	// cumulative transcript after it. It returns ErrStreamEnded when the
	// provider ended the stream cleanly; any other error is a failure.
	Recv(ctx context.Context) (Result, error)
	Close()
}

// Result is a stream's cumulative transcript after one provider event. Text is
// cumulative per Conn (the Conn keeps its own state, so a turn that adopts an
// unbound stream gets the text heard before it); an empty Text emits no
// candidate, and an empty-text Final settles the turn.
type Result struct {
	Text                  string
	Final, EndOfUtterance bool
	Stability             float64
	HasStability          bool
}

// Config wires a Runner to a provider.
type Config struct {
	Provider string // label on candidates and the trace span
	// Dial opens one stream on ctx (the stream lives on it). Retries and the
	// breaker's accounting of the open stay with the provider. It must return
	// a non-nil Conn when err is nil (nil, nil is treated as a failed dial);
	// a non-nil Conn returned with an error is closed by the Runner.
	Dial func(ctx context.Context) (Conn, error)
	// Breaker, with Classify, gets one report per mid-stream failure. May be nil.
	Breaker  *resilience.Breaker
	Classify resilience.Classify
	// DrainTimeout bounds the wait for the final results after Finish (default 2 s).
	DrainTimeout time.Duration
	// SendTimeout bounds one Send or Finish (default 500 ms). Live sends run on
	// the session run loop, so it must stay well under the 2 s frame watchdog.
	SendTimeout time.Duration
	// Now stamps candidates (default time.Now).
	Now func() time.Time
}

type stream struct {
	cancel context.CancelFunc // ends the dial, the reads and the drain

	mu       sync.Mutex
	conn     Conn     // nil while dialing and flushing the pre-roll
	preRoll  [][]byte // audio held while dialing, oldest first
	preBytes int
	turn     string // "" until a turn adopts the stream
	gen      uint64
	closing  bool // Finish is owed (still dialing) or sent
	stopped  bool
	ended    atomic.Bool // the provider ended the stream (Recv returned ErrStreamEnded)
	draining *time.Timer
	lastText string // last candidate emitted for the bound turn
	settled  bool   // that candidate was Final: all heard text is committed

	stopOnce, traceFinal, traceEnd, overflow sync.Once
	span                                     core.Span
	policy                                   core.TracePolicy
	traceSession                             core.SessionID
}

func (st *stream) owner() (string, uint64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.turn, st.gen
}

func (st *stream) event(name string) {
	if st.span != nil {
		turn, gen := st.owner()
		st.span.AddEvent(core.NewTraceEvent(name, time.Now(), st.traceSession, core.TurnID(turn), core.Generation(gen), st.policy, nil))
	}
}

func (st *stream) endTrace(err error) {
	st.traceEnd.Do(func() {
		if st.span == nil {
			return
		}
		if err != nil {
			st.event("stt.failure")
		} else {
			st.event("stt.complete")
		}
		st.span.End(err)
	})
}

// Runner is the STT processor core: embed it in a provider's STTProcessor.
type Runner struct {
	cfg     Config
	session core.SessionController
	index   int
	// maxPreRoll bounds the audio held while a stream dials; beyond it the
	// oldest audio is dropped. DefaultMaxPreRollBytes; only tests change it.
	maxPreRoll int

	mu     sync.Mutex
	active *stream
	// all is the active stream plus finishing ones. Each holds one session
	// task, so the session's MaxAsyncTasks bounds it.
	all    map[*stream]struct{}
	closed bool
	turn   string
	gen    uint64
	noDial bool // a dial failed: no redial until the next turn or a barge-in

	errs atomic.Uint64
}

var _ core.STTCounters = (*Runner)(nil)

// New returns a Runner for cfg; cfg.Dial is required.
func New(cfg Config) *Runner {
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 2 * time.Second
	}
	if cfg.SendTimeout <= 0 {
		cfg.SendTimeout = 500 * time.Millisecond
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{cfg: cfg, maxPreRoll: DefaultMaxPreRollBytes, all: map[*stream]struct{}{}}
}

// STTCounts implements core.STTCounters (no rotation or dedup here).
func (r *Runner) STTCounts() (rot, rotFail, dedup, errs uint64) {
	return 0, 0, 0, r.errs.Load()
}

// Streams is the number of open provider streams (dialing, active or
// finishing). Test-only accessor: provider tests use it to observe teardown.
// ponytail: no server reader; a gauge can read it if operators ever need one.
func (r *Runner) Streams() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.all)
}

func (r *Runner) Attach(s core.SessionController, index int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session, r.index = s, index
	return nil
}

// Close tears down every stream, dials included; it is idempotent.
func (r *Runner) Close() error {
	r.mu.Lock()
	r.closed = true
	streams := r.detachAllLocked()
	r.mu.Unlock()
	for _, st := range streams {
		r.stop(st, nil)
	}
	return nil
}

func (r *Runner) detachAllLocked() []*stream {
	out := make([]*stream, 0, len(r.all))
	for st := range r.all {
		out = append(out, st)
	}
	r.all, r.active = map[*stream]struct{}{}, nil
	return out
}

func (r *Runner) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch fr := f.(type) {
	case *core.UserStartedSpeakingFrame:
		r.beginTurn(ctx, fmt.Sprintf("turn-%d-%d", fr.Gen, fr.Seq), fr.Gen)
		r.ensure(ctx)
		return e.Push(f)
	case *core.TurnEventFrame:
		switch fr.Kind {
		case core.TurnEventStarted:
			r.beginTurn(ctx, string(fr.Turn), uint64(fr.Gen))
		case core.TurnEventCompleted:
			r.finishActive(ctx, string(fr.Turn), uint64(fr.Gen))
		case core.TurnEventInterrupted:
			r.stopTurn(string(fr.Turn), uint64(fr.Gen))
		}
		return e.PushDirection(f, dir)
	case *core.InputAudioFrame:
		return r.handleAudio(ctx, fr, e)
	case *core.UserStoppedSpeakingFrame:
		r.finishActive(ctx, "", 0)
		return e.PushDirection(f, dir)
	case *core.InterruptionFrame, *core.CancelFrame:
		r.mu.Lock()
		if _, ok := f.(*core.InterruptionFrame); ok && r.session != nil && r.turn != "" {
			// Barge-in advanced the generation: the rest of this utterance opens
			// a new stream that must carry it, or the session fence drops it.
			r.gen = max(r.gen, r.session.CurrentGeneration())
		}
		r.noDial = false
		streams := r.detachAllLocked()
		r.mu.Unlock()
		for _, st := range streams {
			r.stop(st, nil)
		}
		return e.PushDirection(f, dir)
	default:
		return e.PushDirection(f, dir)
	}
}

// beginTurn makes (turn, gen) current. An unbound stream (opened between
// turns) is adopted: it already carries the speech onset VAD reported late.
// The same turn at a newer generation (a turn manager's barge-in) keeps its
// stream, rebound. A stream owned by another turn is finished and drains alone.
func (r *Runner) beginTurn(ctx context.Context, turn string, gen uint64) {
	r.mu.Lock()
	r.turn, r.gen, r.noDial = turn, gen, false
	var old *stream
	if a := r.active; a != nil {
		a.mu.Lock()
		if a.turn == "" || (a.turn == turn && a.gen < gen) {
			a.turn, a.gen = turn, gen
		} else if a.turn != turn || a.gen != gen {
			old, r.active = a, nil
		}
		a.mu.Unlock()
	}
	r.mu.Unlock()
	if old != nil {
		r.finish(ctx, old)
	}
}

// ensure returns the stream audio goes to, starting one when there is none.
// The dial runs on the stream's session task, never here (A1). It returns nil
// when closed, after this turn's dial failed, or when the session has no task
// left (ErrTooManyTasks: reported like a failed dial, never returned, since a
// ProcessFrame error would fail the whole call).
func (r *Runner) ensure(ctx context.Context) *stream {
	r.mu.Lock()
	if r.active != nil || r.closed || r.noDial || r.session == nil {
		st := r.active
		r.mu.Unlock()
		return st
	}
	s, turn, gen := r.session, r.turn, r.gen
	r.mu.Unlock()

	traceSession := core.SessionID(strconv.FormatUint(s.ID(), 10))
	sctx, cancel := context.WithCancel(s.Context())
	st := &stream{
		cancel: cancel, turn: turn, gen: gen, traceSession: traceSession,
		span:   core.StartContextSpan(ctx, "stt.stream", traceSession, core.TurnID(turn), core.Generation(gen), map[string]any{"provider": r.cfg.Provider}),
		policy: core.TraceContextFromContext(ctx).Policy,
	}
	r.mu.Lock()
	if r.closed || r.active != nil { // Close ran meanwhile, or another caller won
		cur := r.active
		r.mu.Unlock()
		cancel()
		st.endTrace(errCanceled)
		return cur
	}
	r.active = st
	r.all[st] = struct{}{}
	r.mu.Unlock()
	if err := s.Go(func(context.Context) { r.run(sctx, st) }); err != nil {
		if errors.Is(err, core.ErrSessionClosed) {
			r.release(st)
			r.stop(st, errCanceled)
			return nil
		}
		r.dialFailed(st, err) // ErrTooManyTasks: lose this turn's transcript, not the call
		r.release(st)
		r.stop(st, errProviderFailure)
		return nil
	}
	return st
}

func (r *Runner) handleAudio(ctx context.Context, f *core.InputAudioFrame, e core.Emitter) error {
	st := r.ensure(ctx)
	if st == nil { // closed, no task, or this turn's dial failed (already reported)
		return e.Push(f)
	}
	conn := r.hold(st, f.Samples)
	if conn == nil { // held in the pre-roll until the dial completes
		return e.Push(f)
	}
	sctx, cancel := context.WithTimeout(ctx, r.cfg.SendTimeout)
	defer cancel()
	if err := conn.Send(sctx, f.Samples); err != nil {
		// A genuine write failure landing just before ErrStreamEnded is quiet too, by
		// design: the stream is already gone and the next audio opens a fresh one.
		if r.isStopped(st) || st.ended.Load() { // torn down, or closed by the provider first: not this Send's failure
			return e.Push(f)
		}
		r.errs.Add(1)
		r.release(st)
		r.stop(st, errProviderFailure)
		r.report(err)
		return err
	}
	return e.Push(f)
}

// hold returns st's conn, or keeps pcm in the pre-roll while st dials and
// returns nil. Past maxPreRoll the oldest audio is dropped; the span
// says so once.
func (r *Runner) hold(st *stream, pcm []byte) Conn {
	st.mu.Lock()
	if st.conn != nil {
		conn := st.conn
		st.mu.Unlock()
		return conn
	}
	// ponytail: frames are immutable once posted (core/stt_failover.go journals them the same way), so the pre-roll keeps the slice, not a copy.
	st.preRoll = append(st.preRoll, pcm)
	st.preBytes += len(pcm)
	dropped := false
	// ponytail: preRoll[1:] keeps the dropped prefix's array alive until the next append grows it (at most 2x capacity).
	for st.preBytes > r.maxPreRoll && len(st.preRoll) > 1 {
		st.preBytes -= len(st.preRoll[0])
		st.preRoll[0] = nil
		st.preRoll = st.preRoll[1:]
		dropped = true
	}
	st.mu.Unlock()
	if dropped {
		st.overflow.Do(func() { st.event("stt.preroll_dropped") })
	}
	return nil
}

// run is a stream's session task: dial, flush the pre-roll in order, then read
// results until the stream ends or is stopped.
func (r *Runner) run(ctx context.Context, st *stream) {
	var traceErr error
	var conn Conn
	defer func() {
		// Stopped before the conn closes: a Send racing the provider's close
		// then sees a dead stream, not a provider failure.
		r.release(st)
		r.stop(st, traceErr)
		if conn != nil {
			conn.Close() // idempotent (Conn contract)
		}
	}()
	var err error
	conn, err = r.cfg.Dial(ctx)
	if err == nil && conn == nil {
		err = errNilConn
	}
	if err != nil {
		if ctx.Err() == nil { // not our own teardown
			traceErr = errProviderFailure
			r.dialFailed(st, err)
		}
		return
	}
	st.event("stt.start")
	if err := r.flush(ctx, st, conn); err != nil {
		if ctx.Err() == nil && !r.isStopped(st) {
			// The stream died taking its held audio: a failed open, with the breaker told.
			traceErr = errProviderFailure
			r.report(err)
			if turn, _ := st.owner(); turn != "" {
				r.dropTurn(st, err) // the held audio of a bound turn is gone (A8)
			} else {
				r.dialFailed(st, err)
			}
		}
		return
	}
	traceErr = r.receive(ctx, st, conn)
}

// dialFailed reports a failed open: an ErrorFrame for the stream's turn (what
// an STT failover moves on), counted unless the breaker refused the dial.
// Nothing redials until the next turn or a barge-in, so an outage costs one
// error per turn, not one per audio frame. An unbound stream that is no longer
// active (STTFailover's "recovered" re-evaluation abandoned it) owes nobody a
// transcript: no frame, which would only reach the client as a spurious
// RTVI provider_error with no turn.
func (r *Runner) dialFailed(st *stream, err error) {
	r.mu.Lock()
	active := r.active == st
	if active {
		r.noDial = true
	}
	r.mu.Unlock()
	// An open breaker and the session's own task budget are not provider errors.
	if !errors.Is(err, resilience.ErrCircuitOpen) && !errors.Is(err, core.ErrTooManyTasks) {
		r.errs.Add(1)
	}
	if turn, _ := st.owner(); turn == "" && !active {
		return
	}
	r.reportError(st, err)
}

// flush sends the pre-roll in order, then attaches conn so the run loop sends
// live audio straight to it. Audio arriving during a pass joins the pre-roll
// and goes in the next one, so no lock is held across a Send. A Finish owed by
// a turn that ended while dialing goes after the held audio.
func (r *Runner) flush(ctx context.Context, st *stream, conn Conn) error {
	for {
		st.mu.Lock()
		if st.stopped {
			st.mu.Unlock()
			return errCanceled
		}
		batch := st.preRoll
		st.preRoll, st.preBytes = nil, 0
		if len(batch) == 0 {
			st.conn = conn
			owed := st.closing
			st.mu.Unlock()
			if owed {
				r.sendFinish(ctx, st, conn)
			}
			return nil
		}
		st.mu.Unlock()
		for _, pcm := range batch {
			sctx, cancel := context.WithTimeout(ctx, r.cfg.SendTimeout)
			err := conn.Send(sctx, pcm)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

// finish asks for st's final results. While st dials, the Finish is owed and
// its task sends it after the pre-roll. Safe to call more than once.
//
// Invariant closing => !active: finishActive and beginTurn take st out of
// r.active before calling finish, so the run loop never sends live audio to a
// finishing stream and Send and Finish never overlap on one Conn.
func (r *Runner) finish(ctx context.Context, st *stream) {
	st.mu.Lock()
	if st.closing {
		st.mu.Unlock()
		return
	}
	st.closing = true
	conn := st.conn
	st.mu.Unlock()
	if conn != nil {
		r.sendFinish(ctx, st, conn)
	}
}

// sendFinish sends Finish and force-closes st after DrainTimeout.
func (r *Runner) sendFinish(ctx context.Context, st *stream, conn Conn) {
	fctx, cancel := context.WithTimeout(ctx, r.cfg.SendTimeout)
	defer cancel()
	if err := conn.Finish(fctx); err != nil {
		r.release(st)
		r.stop(st, nil)
		return
	}
	st.mu.Lock()
	if !st.stopped {
		st.draining = time.AfterFunc(r.cfg.DrainTimeout, func() {
			defer func() { // a provider's Close must not kill the daemon from a timer goroutine
				if rec := recover(); rec != nil {
					turn, _ := st.owner()
					slog.Error("sttunit: recovered provider panic on drain timeout", "provider", r.cfg.Provider, "turn", turn, "panic", fmt.Sprint(rec))
				}
			}()
			r.release(st)
			r.stop(st, nil)
		})
	}
	st.mu.Unlock()
}

// finishActive finishes the active stream (turn "" = whichever is active, i.e.
// the user stopped speaking). The turn is released so audio after it opens an
// unbound stream for the next utterance.
func (r *Runner) finishActive(ctx context.Context, turn string, gen uint64) {
	r.mu.Lock()
	st := r.active
	if st == nil {
		r.mu.Unlock()
		return
	}
	t, g := st.owner()
	if turn != "" && (t != turn || g != gen) {
		r.mu.Unlock()
		return
	}
	r.active = nil // late audio opens a new stream rather than a finishing one
	if r.turn == t && r.gen == g {
		r.turn, r.gen = "", 0
	}
	r.mu.Unlock()
	r.finish(ctx, st)
}

func (r *Runner) stopTurn(turn string, gen uint64) {
	r.mu.Lock()
	var victims []*stream
	for st := range r.all {
		if t, g := st.owner(); t == turn && g == gen {
			victims = append(victims, st)
			delete(r.all, st)
			if r.active == st {
				r.active = nil
			}
		}
	}
	r.mu.Unlock()
	for _, st := range victims {
		r.stop(st, nil)
	}
}

func (r *Runner) release(st *stream) {
	r.mu.Lock()
	delete(r.all, st)
	if r.active == st {
		r.active = nil
	}
	r.mu.Unlock()
}

// stop cancels st (its dial, reads and drain) and closes its conn exactly
// once, then ends the trace with err.
func (r *Runner) stop(st *stream, err error) {
	defer st.endTrace(err) // even if a provider's Close panics
	st.mu.Lock()
	st.stopped = true
	if st.draining != nil {
		st.draining.Stop()
	}
	conn := st.conn
	st.mu.Unlock()
	st.stopOnce.Do(func() {
		st.cancel()
		if conn != nil {
			conn.Close()
		}
	})
}

func (r *Runner) isStopped(st *stream) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.stopped
}

// receive reads results until the stream ends, and returns the trace error.
func (r *Runner) receive(ctx context.Context, st *stream, conn Conn) error {
	for {
		res, err := conn.Recv(ctx)
		switch {
		case err == nil:
			r.emit(st, res)
		case ctx.Err() != nil:
			return nil // stopped: barge-in, turn interrupted, drain timeout or Close
		case errors.Is(err, ErrStreamEnded):
			st.ended.Store(true)
			if !r.lost(st) {
				return nil
			}
			r.dropTurn(st, err)
			return errProviderFailure
		default:
			r.errs.Add(1)
			r.report(err)
			r.reportError(st, err)
			return errProviderFailure
		}
	}
}

// lost reports whether the provider ended st while its bound turn still owed
// a transcript: before Finish, or after it with words heard but not committed
// by a Final candidate.
func (r *Runner) lost(st *stream) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.turn != "" && (!st.closing || (st.lastText != "" && !st.settled))
}

// dropTurn ends a bound stream that lost its turn's audio. Nothing redials
// for the rest of the turn (the turn is invalidated and, under failover,
// replayed elsewhere); then the turn is invalidated.
func (r *Runner) dropTurn(st *stream, cause error) {
	r.mu.Lock()
	if r.active == st {
		r.noDial = true
	}
	r.mu.Unlock()
	r.errs.Add(1)
	r.invalidate(st, cause)
}

// invalidate retires the bound turn's partial transcript, then raises the
// error an STT failover replays the turn on (the xai M3-09 shape). A clean
// end is not an outage, so the breaker is not told.
func (r *Runner) invalidate(st *stream, cause error) {
	turn, gen := st.owner()
	gen = r.frameGen(gen)
	if err := r.session.Reenter(&core.TranscriptInvalidationFrame{Turn: core.TurnID(turn), Gen: core.Generation(gen), Err: cause}, core.Downstream, r.index+1, gen); err != nil {
		return // the session is closing
	}
	_ = r.session.Reenter(&core.ErrorFrame{
		Err:  fmt.Errorf("%w: %s stream ended mid-utterance: %w", core.ErrTranscriptInvalidated, r.cfg.Provider, cause),
		Turn: core.TurnID(turn), Gen: core.Generation(gen),
	}, core.Downstream, r.index+1, gen)
}

// emit turns a result into a candidate for the stream's turn.
func (r *Runner) emit(st *stream, res Result) {
	if res.Text == "" {
		if res.Final { // an empty Final retracts what was heard: nothing is left uncommitted
			st.mu.Lock()
			if st.turn != "" {
				st.lastText, st.settled = "", true
			}
			st.mu.Unlock()
		}
		return
	}
	st.mu.Lock()
	turn, gen := st.turn, st.gen
	if turn != "" {
		// Final, not Final && EndOfUtterance: a closing response need not carry
		// an EOU final (Riva may answer CloseSend with nothing more).
		st.lastText, st.settled = res.Text, res.Final
	}
	st.mu.Unlock()
	if turn == "" { // unbound (between turns): the Conn keeps the text for the adopting turn
		return
	}
	gen = r.frameGen(gen)
	frame := &core.TranscriptCandidateFrame{
		Text: res.Text, Turn: core.TurnID(turn), Gen: core.Generation(gen), Timestamp: r.cfg.Now(),
		Final: res.Final, EndOfUtterance: res.EndOfUtterance,
		Stability: res.Stability, HasStability: res.HasStability, Provider: r.cfg.Provider,
	}
	if r.session.Reenter(frame, core.Downstream, r.index+1, gen) == nil && res.Final {
		st.traceFinal.Do(func() { st.event("stt.first_final") })
	}
}

func (r *Runner) reportError(st *stream, err error) {
	turn, gen := st.owner()
	gen = r.frameGen(gen)
	_ = r.session.Reenter(&core.ErrorFrame{Err: err, Turn: core.TurnID(turn), Gen: core.Generation(gen)}, core.Downstream, r.index+1, gen)
}

func (r *Runner) frameGen(gen uint64) uint64 {
	if gen == 0 {
		return r.session.CurrentGeneration()
	}
	return gen
}

func (r *Runner) report(err error) {
	if r.cfg.Breaker != nil && r.cfg.Classify != nil {
		r.cfg.Breaker.Report(err, r.cfg.Classify)
	}
}
