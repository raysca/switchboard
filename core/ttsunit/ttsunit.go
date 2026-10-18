// Package ttsunit is the per-unit machinery shared by streaming TTS
// providers: in-order unit playback, cancel-all on interruption/Close, the
// trace span, 20 ms framing with a held-back final frame, and the usage metric.
// A provider supplies only Attempt (one synthesis try that calls Unit.Write).
package ttsunit

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/resilience"
)

const chunkDuration = 20 * time.Millisecond

// Config wires a Runner to a provider.
type Config struct {
	Provider, Model string
	SampleRate      int
	Breaker         *resilience.Breaker
	Policy          resilience.Policy
	Classify        resilience.Classify
	// Attempt performs one synthesis try, feeding audio to u.Write and finishing
	// with u.Finish. State from a failed try before commit is cleared by Reset.
	Attempt func(ctx context.Context, u *Unit, attempt int) error
	// EndReply, when set, is called on interruption/cancel/Close (under the
	// runner lock: it must not block long) and when a blank final unit ends a
	// reply without an Attempt, so a provider can release its reply session.
	EndReply func()
}

type cancelEntry struct {
	seq    uint64
	cancel context.CancelFunc
}

// Runner is the processor core: embed it in a provider's TTSProcessor.
type Runner struct {
	cfg     Config
	session core.SessionController
	index   int

	mu          sync.Mutex
	cond        *sync.Cond
	cancels     []cancelEntry
	nextSeq     uint64
	finishedSeq uint64
	closed      bool
	queue       core.SerialQueue // units wait here, not on a task slot each

	RequestsTotal atomic.Uint64
	RetriesTotal  atomic.Uint64
	AudioFrames   atomic.Uint64
	AudioBytes    atomic.Uint64
	ErrorsTotal   atomic.Uint64
	StaleTotal    atomic.Uint64
}

// TTSCounts implements core.TTSCounters.
func (r *Runner) TTSCounts() (req, errs, retries, stale uint64) {
	return r.RequestsTotal.Load(), r.ErrorsTotal.Load(), r.RetriesTotal.Load(), r.StaleTotal.Load()
}

func New(cfg Config) *Runner {
	r := &Runner{cfg: cfg}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *Runner) Attach(s core.SessionController, index int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.session, r.index = s, index
	return nil
}

// Close cancels in-flight synthesis; it is idempotent.
func (r *Runner) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.cancelAllLocked()
	return nil
}

func (r *Runner) cancelAllLocked() {
	if r.cfg.EndReply != nil {
		r.cfg.EndReply()
	}
	for _, c := range r.cancels {
		c.cancel()
	}
	r.cancels = nil
	r.finishedSeq = r.nextSeq
	r.cond.Broadcast()
}

func (r *Runner) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch fr := f.(type) {
	case *core.SynthesisUnitFrame:
		return r.synthesizeUnit(ctx, fr, e)
	case *core.InterruptionFrame:
		r.mu.Lock()
		r.cancelAllLocked()
		r.mu.Unlock()
		return e.Push(f)
	default:
		return e.PushDirection(f, dir)
	}
}

func (r *Runner) synthesizeUnit(ctx context.Context, unit *core.SynthesisUnitFrame, e core.Emitter) error {
	r.mu.Lock()
	if r.closed { // the session is already ending: drop, do not fail it
		r.mu.Unlock()
		return nil
	}
	s := r.session
	if s == nil {
		r.mu.Unlock()
		return errors.New(r.cfg.Provider + " tts not attached to session")
	}
	if unit.Gen != 0 && unit.Gen < s.CurrentGeneration() {
		r.StaleTotal.Add(1)
		r.mu.Unlock()
		return nil
	}
	synthCtx, cancel := context.WithCancel(s.Context()) // barge-in; the deadline is per attempt
	seq := r.nextSeq + 1
	r.nextSeq = seq
	r.cancels = append(r.cancels, cancelEntry{seq, cancel})
	r.mu.Unlock()

	if err := e.Push(unit); err != nil { // assistant context tracking
		cancel()
		r.abandon(seq)
		return err
	}
	r.RequestsTotal.Add(1)
	if err := r.queue.Go(s, func(sessionCtx context.Context) {
		traceSession := core.SessionID(strconv.FormatUint(s.ID(), 10))
		traceTurn, traceGen := core.TurnID(unit.Turn), core.Generation(unit.Gen)
		span := core.StartContextSpan(ctx, "tts.request", traceSession, traceTurn, traceGen, map[string]any{"provider": r.cfg.Provider})
		policy := core.TraceContextFromContext(ctx).Policy
		traceFailure := error(errors.New("canceled"))
		defer func() {
			if v := recover(); v != nil {
				traceFailure = errors.New("provider_failure")
				span.AddEvent(core.NewTraceEvent("tts.failure", time.Now(), traceSession, traceTurn, traceGen, policy, nil))
				span.End(traceFailure)
				panic(v)
			}
			name := "tts.complete"
			if traceFailure != nil {
				name = "tts.failure"
			}
			span.AddEvent(core.NewTraceEvent(name, time.Now(), traceSession, traceTurn, traceGen, policy, nil))
			span.End(traceFailure)
		}()
		defer cancel()
		defer func() {
			r.mu.Lock()
			kept := r.cancels[:0:0]
			for _, c := range r.cancels {
				if c.seq != seq {
					kept = append(kept, c)
				}
			}
			r.cancels = kept
			if r.finishedSeq+1 == seq {
				r.finishedSeq = seq
			}
			r.cond.Broadcast()
			r.mu.Unlock()
		}()

		// Units are emitted strictly in arrival order.
		r.mu.Lock()
		for r.finishedSeq+1 != seq && synthCtx.Err() == nil && sessionCtx.Err() == nil && !r.closed {
			r.cond.Wait()
		}
		stale := r.closed || synthCtx.Err() != nil || sessionCtx.Err() != nil || (unit.Gen != 0 && unit.Gen < s.CurrentGeneration())
		r.mu.Unlock()
		if stale {
			r.StaleTotal.Add(1)
			return
		}

		u := &Unit{Frame: unit, r: r, s: s, span: span, session: traceSession, turn: traceTurn, gen: traceGen, policy: policy}
		var err error
		if strings.TrimSpace(unit.Text) == "" { // nothing to say; keep the drain boundary
			if unit.IsFinal && r.cfg.EndReply != nil {
				r.cfg.EndReply()
			}
			err = u.Finish()
		} else {
			err = resilience.Execute(synthCtx, r.cfg.Breaker, r.cfg.Policy, r.cfg.Classify, func() bool { return u.committed }, func(ctx context.Context, attempt int) error {
				if attempt > 0 {
					r.RetriesTotal.Add(1)
				}
				u.buf, u.Timings = nil, nil // nothing was emitted, so a retry starts clean
				return r.cfg.Attempt(ctx, u, attempt)
			})
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || synthCtx.Err() != nil {
				return
			}
			traceFailure = errors.New("provider_failure")
			r.ErrorsTotal.Add(1)
			// The error goes first so a failover stage has demoted this provider
			// before the closing frame lets it start the next unit.
			_ = s.Reenter(&core.ErrorFrame{Err: err, Turn: traceTurn, Gen: traceGen, UnitID: unit.UnitID}, core.Downstream, r.index+1, unit.Gen)
			if u.committed { // close out what was heard so playback can drain
				_ = u.Finish()
			}
			return
		}
		traceFailure = nil
	}); err != nil {
		// The task was refused, so nothing will ever complete this slot: give
		// it back so later units are not wedged behind it.
		cancel()
		r.abandon(seq)
		if errors.Is(err, core.ErrSessionClosed) {
			return nil
		}
		if errors.Is(err, core.ErrTooManyTasks) { // lose this unit, not the call
			// Re-entered, not pushed: under TTSFailover e swallows, and the
			// wrapper would wait for this unit until its watchdog fired.
			_ = s.Reenter(&core.ErrorFrame{Err: err, Turn: core.TurnID(unit.Turn), Gen: core.Generation(unit.Gen), UnitID: unit.UnitID}, core.Downstream, r.index+1, unit.Gen)
			return nil
		}
		return err
	}
	return nil
}

// abandon releases seq when its task was never started. Calls are serialized
// with synthesizeUnit, so seq is still the newest slot.
func (r *Runner) abandon(seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.cancels[:0:0]
	for _, c := range r.cancels {
		if c.seq != seq {
			kept = append(kept, c)
		}
	}
	r.cancels = kept
	if r.nextSeq == seq {
		r.nextSeq--
		if r.finishedSeq > r.nextSeq {
			r.finishedSeq = r.nextSeq
		}
	}
	r.cond.Broadcast()
}

// Unit is the state of one unit's synthesis across attempts.
type Unit struct {
	Frame *core.SynthesisUnitFrame
	// Timings accumulate until the next frame is emitted and ride on it.
	Timings []core.WordTiming

	r       *Runner
	s       core.SessionController
	span    core.Span
	session core.SessionID
	turn    core.TurnID
	gen     core.Generation
	policy  core.TracePolicy

	committed bool   // audio has been emitted: no retry
	buf       []byte // audio not yet emitted; the last chunk is held to mark IsFinal
	sent      int    // bytes emitted so far
	seq       uint64
	stopped   bool
}

func (u *Unit) reenter(f core.Frame) error {
	return u.s.Reenter(f, core.Downstream, u.r.index+1, u.Frame.Gen)
}

func (u *Unit) stale() bool {
	return u.Frame.Gen != 0 && u.Frame.Gen < u.s.CurrentGeneration()
}

// Write buffers pcm and emits every full 20 ms chunk, holding back the
// remainder so the last frame of the unit can carry IsFinal.
func (u *Unit) Write(pcm []byte) error {
	u.buf = append(u.buf, pcm...)
	n := u.r.cfg.SampleRate * 2 * int(chunkDuration/time.Millisecond) / 1000
	for len(u.buf) > n {
		if err := u.emit(u.buf[:n], false); err != nil {
			return err
		}
		u.buf = u.buf[n:]
	}
	return nil
}

func (u *Unit) start() error {
	if u.committed {
		return nil
	}
	if u.stale() {
		return context.Canceled
	}
	u.committed = true
	return u.reenter(&core.TTSStartedFrame{Turn: u.Frame.Turn, Gen: u.Frame.Gen, UnitID: u.Frame.UnitID})
}

func (u *Unit) emit(pcm []byte, final bool) error {
	if err := u.start(); err != nil {
		return err
	}
	if u.stale() {
		return context.Canceled
	}
	pcm = append([]byte(nil), pcm...)
	if len(pcm)%2 != 0 { // keep sample alignment; a lone byte is dropped
		pcm = pcm[:len(pcm)-1]
	}
	rate := u.r.cfg.SampleRate
	u.seq++
	frame := &core.OutputAudioFrame{
		Samples: pcm, Encoding: core.EncodingPCM16LE, SampleRate: rate, Channels: 1,
		Duration:  time.Duration(len(pcm)/2) * time.Second / time.Duration(rate),
		Timestamp: time.Now(), Turn: u.Frame.Turn, Gen: u.Frame.Gen, UnitID: u.Frame.UnitID, Seq: u.seq,
		IsFinal: final, TurnFinal: final && u.Frame.IsFinal, WordTimings: u.Timings,
	}
	u.Timings = nil
	u.r.AudioFrames.Add(1)
	u.r.AudioBytes.Add(uint64(len(pcm)))
	if err := u.reenter(frame); err != nil {
		return err
	}
	if u.sent == 0 {
		u.span.AddEvent(core.NewTraceEvent("tts.first_audio", time.Now(), u.session, u.turn, u.gen, u.policy, nil))
	}
	u.sent += len(pcm)
	return nil
}

// Finish emits the held tail as the final frame (or an empty final marker),
// TTSStopped and usage. It is safe to call more than once.
func (u *Unit) Finish() error {
	if u.stopped {
		return nil
	}
	u.stopped = true
	if err := u.emit(u.buf, true); err != nil {
		return err
	}
	u.buf = nil
	if err := u.reenter(&core.TTSStoppedFrame{Turn: u.Frame.Turn, Gen: u.Frame.Gen, UnitID: u.Frame.UnitID}); err != nil {
		return err
	}
	rate := u.r.cfg.SampleRate
	_ = u.reenter(&core.UsageMetricsFrame{
		SessionID: u.session, Turn: u.Frame.Turn, Gen: u.Frame.Gen, Provider: u.r.cfg.Provider, Model: u.r.cfg.Model,
		Service: "tts", Timestamp: time.Now(),
		Characters:      core.Int64Ptr(int64(len([]rune(u.Frame.Text)))),
		AudioDurationMs: core.Int64Ptr(int64(u.sent / 2 * 1000 / rate)),
		Requests:        core.Int64Ptr(1),
	})
	return nil
}

var _ core.TTSCounters = (*Runner)(nil)
