package turn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
)

// Timer and Clock make the processor's tick driver deterministic in tests.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type realClock struct{}
type realTimer struct{ t *time.Timer }

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }
func (t realTimer) C() <-chan time.Time          { return t.t.C }
func (t realTimer) Stop() bool                   { return t.t.Stop() }

// ProcessorOptions tune the session driver; the zero value is production.
type ProcessorOptions struct {
	Detector TurnDetector  // nil: SilenceDetector
	Clock    Clock         // nil: wall clock
	Tick     time.Duration // poll interval while a turn is open (default 20ms); idle sessions do not poll
	// Mute strategies (see UserMuteStrategy); any muting strategy mutes the user.
	// MaxMute caps one strategy's continuous mute (default DefaultMaxMute).
	Mute       []UserMuteStrategy
	MaxMute    time.Duration
	OnMuteDrop func() // called per dropped input frame while muted
	// OnFinalWithoutInterims is called once per call when the first transcript
	// candidate is already final: the STT sends no interim results, so a
	// transcript-based start (min_words) cannot act before the final.
	OnFinalWithoutInterims func()
	// VADHangover is the estimated silence duration that has already elapsed when
	// UserStoppedSpeakingFrame is received.
	VADHangover time.Duration
}

// tickFrame re-enters the run loop so the manager advances on the same
// goroutine as observations. FrameInternal frames are shed when the queue is
// full, which is fine for a periodic tick.
type tickFrame struct{}

func (tickFrame) Type() core.FrameType { return core.FrameInternal }

// unstartedNoFinalExpiry: how long after the VAD stop an unstarted turn (min_words, wake
// phrase) still waits for its final transcript. A streaming STT without interim results
// (Google chirp_2) answers 1-2 s after the VAD stop; dropping the turn earlier lost the
// whole utterance. ponytail: fixed cap; make it config if a provider is slower.
const unstartedNoFinalExpiry = 2500 * time.Millisecond

type segment struct {
	turn  string
	text  string
	final bool
}

// Processor runs a Manager inside a live pipeline. Place it after STT and
// before the transcript processor: STT keeps opening its stream on the raw VAD
// frames (so a min-words strategy never stops transcription), while this
// processor feeds VAD, transcripts and bot-speaking state to the manager and
// emits core.TurnEventFrame. One managed turn may span several VAD segments
// (a short pause does not split it); their transcripts are joined and re-keyed
// to the managed turn so the transcript finalizer sees one turn.
//
// All state below is owned by the session run loop.
type Processor struct {
	mgr    *Manager
	clock  Clock
	tick   time.Duration
	expiry time.Duration
	// expiryNoFinal replaces expiry while the newest segment has no final transcript yet.
	expiryNoFinal time.Duration
	session       core.SessionController
	index         int
	wake          chan struct{}
	tickGen       atomic.Uint64 // generation of the queued tick; 0 = none queued
	busy          atomic.Bool
	extStop       bool // push-to-talk released while a turn is open; consumed by the next observation
	held          bool // push-to-talk button is down
	startOnce     sync.Once

	last        time.Time
	open        bool // a managed turn exists (first VAD start .. terminal event/expiry)
	started     bool // the manager emitted TurnEventStarted for it
	id          core.TurnID
	mgrGen      core.Generation
	segs        []segment
	closed      map[string]struct{} // recently retired segment ids; their late candidates are dropped
	vadSpeaking bool
	bot         bool
	lastStop    time.Time
	candGen     uint64 // generation of the last candidate forwarded for this turn

	mute         *muter
	onMuteDrop   func()
	onNoInterims func()
	vadHangover  time.Duration
	sawCandidate bool        // a transcript candidate arrived this call
	dropSeg      bool        // the current VAD segment's start was dropped; swallow its stop
	mgrMuted     bool        // the manager is paused for a not-yet-admitted turn
	mutedNow     atomic.Bool // mirrors mute.on for other goroutines (idle controller)
}

func NewProcessor(cfg Config, opts ProcessorOptions) (*Processor, error) {
	det := opts.Detector
	if det == nil {
		det = SilenceDetector{}
	}
	mgr, err := NewManager(cfg, det)
	if err != nil {
		return nil, err
	}
	p := &Processor{mgr: mgr, clock: opts.Clock, tick: opts.Tick, mute: newMuter(opts.Mute, opts.MaxMute), onMuteDrop: opts.OnMuteDrop, onNoInterims: opts.OnFinalWithoutInterims,
		vadHangover: opts.VADHangover, wake: make(chan struct{}, 1), closed: make(map[string]struct{})}
	if p.clock == nil {
		p.clock = realClock{}
	}
	if p.tick <= 0 {
		p.tick = 20 * time.Millisecond
	}
	// Long enough for a trailing final transcript before an unstarted turn is dropped.
	p.expiry = max(cfg.SilenceTimeout, time.Second)
	p.expiryNoFinal = max(p.expiry, unstartedNoFinalExpiry)
	if err := mgr.Start(p.now()); err != nil {
		return nil, err
	}
	return p, nil
}

// Attach starts the session-owned tick driver.
func (p *Processor) Attach(s core.SessionController, index int) error {
	p.session, p.index = s, index
	var err error
	p.startOnce.Do(func() { err = s.Go(p.run) })
	return err
}

// Close releases the manager; the driver exits with the session context.
func (p *Processor) Close() error { return p.mgr.Close() }

func (p *Processor) now() time.Time {
	n := p.clock.Now()
	if n.Before(p.last) {
		n = p.last
	}
	p.last = n
	return n
}

// run polls only while a turn is open or the user is speaking; an idle
// session blocks on wake. (The manager's initial no-speech event is trace-only
// and is not worth a per-session idle poll.)
func (p *Processor) run(ctx context.Context) {
	for {
		if !p.busy.Load() {
			select {
			case <-ctx.Done():
				return
			case <-p.wake:
			}
			continue
		}
		t := p.clock.NewTimer(p.tick)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-p.wake:
			t.Stop()
		case <-t.C():
			p.enqueueTick()
		}
	}
}

// enqueueTick keeps at most one tick queued. A queued tick is fenced (silently
// discarded) if the generation advances first, so a pending tick older than
// the current generation counts as lost and is replaced.
func (p *Processor) enqueueTick() {
	cur := p.session.CurrentGeneration()
	pending := p.tickGen.Load()
	if pending != 0 && pending >= cur {
		return
	}
	if !p.tickGen.CompareAndSwap(pending, cur) {
		return
	}
	if err := p.session.Reenter(tickFrame{}, core.Downstream, p.index, cur); err != nil {
		p.tickGen.CompareAndSwap(cur, 0)
	}
}

func (p *Processor) setBusy() {
	was := p.busy.Swap(p.open || p.vadSpeaking)
	if !was && (p.open || p.vadSpeaking) {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

func (p *Processor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	defer p.setBusy()
	if err := p.checkMute(e); err != nil {
		return err
	}
	if dir == core.Upstream {
		switch v := f.(type) {
		case *core.SpeakingStateFrame:
			p.bot = v.State == core.SpeakingStarted
			p.mute.onBot(p.bot, v.Uninterruptible)
			if err := p.checkMute(e); err != nil {
				return err
			}
		case *core.InterruptionFrame:
			// While a user turn is open the barge-in is that user speaking: STT
			// must not cancel their stream, and the stream must move to the new
			// generation or the session fence drops its transcripts. STT
			// providers adopt a same-turn TurnEventStarted with a newer gen.
			if p.open && p.session != nil {
				return e.PushDirection(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: core.TurnID(p.segs[len(p.segs)-1].turn),
					Gen: core.Generation(p.session.CurrentGeneration()), At: p.now()}, core.Upstream)
			}
		}
		return e.PushDirection(f, dir)
	}
	switch v := f.(type) {
	case tickFrame:
		p.tickGen.Store(0)
		return p.advance(e)
	case *core.PushToTalkFrame:
		return p.onPushToTalk(v.Pressed, e)
	case *core.UserStartedSpeakingFrame:
		if p.muted() {
			p.dropSeg = true
			p.markDropped(fmt.Sprintf("turn-%d-%d", v.Gen, v.Seq))
			return nil
		}
		p.dropSeg = false
		if err := e.Push(f); err != nil {
			return err
		}
		return p.onVADStart(v, e)
	case *core.UserStoppedSpeakingFrame:
		if p.dropSeg {
			p.dropSeg = false
			p.drop()
			return nil
		}
		if err := e.Push(f); err != nil {
			return err
		}
		if !p.open {
			return nil
		}
		p.vadSpeaking = false
		p.lastStop = p.now()
		silenceSince := time.Time{}
		if v.SilenceDuration > 0 {
			silenceSince = p.lastStop.Add(-v.SilenceDuration)
		} else if p.vadHangover > 0 {
			silenceSince = p.lastStop.Add(-p.vadHangover)
		}
		return p.observeSilence(e, "", false, silenceSince)
	case *core.TranscriptCandidateFrame:
		return p.onCandidate(v, e)
	}
	return e.PushDirection(f, dir)
}

func (p *Processor) onVADStart(v *core.UserStartedSpeakingFrame, e core.Emitter) error {
	seg := fmt.Sprintf("turn-%d-%d", v.Gen, v.Seq) // same identity STT derives
	if !p.open {
		// The last turn ended with no transcript at all while its final is still
		// due (a stop faster than a non-streaming STT, e.g. Smart Turn + chirp_2):
		// the user is resuming that utterance, so its late final joins this turn.
		n := len(p.segs)
		carry := n > 0 && !p.segs[n-1].final && p.joined() == "" && p.now().Sub(p.lastStop) < p.expiryNoFinal
		segs := p.segs
		if !carry {
			p.retireSegments()
			segs = nil
		}
		p.extStop = false // a stale signal must not end the next turn
		gen := core.Generation(v.Gen)
		if gen == 0 && p.session != nil {
			gen = core.Generation(p.session.CurrentGeneration())
		}
		p.open, p.started, p.id, p.mgrGen, p.segs, p.candGen = true, false, core.TurnID(seg), gen, segs, 0
	}
	p.segs = append(p.segs, segment{turn: seg})
	p.vadSpeaking = true
	return p.observe(e, "", false)
}

// onPushToTalk applies a push-to-talk press or release and consumes the frame.
// A press is ExternalStart evidence for the current or next VAD segment, so
// pressing just before or while speaking both work; a release ends the open
// turn through ExternalStop. A release with no open turn clears the press, so
// it cannot start a later turn.
func (p *Processor) onPushToTalk(pressed bool, e core.Emitter) error {
	p.held = pressed
	if !pressed && p.open {
		p.extStop = true
	}
	return p.observe(e, "", false) // no-op when no turn is open
}

func (p *Processor) onCandidate(c *core.TranscriptCandidateFrame, e core.Emitter) error {
	if !p.sawCandidate {
		p.sawCandidate = true
		if (c.Final || c.EndOfUtterance) && p.onNoInterims != nil {
			p.onNoInterims()
		}
	}
	idx := -1
	for i := range p.segs {
		if p.segs[i].turn == string(c.Turn) {
			idx = i
		}
	}
	if idx < 0 {
		if _, dead := p.closed[string(c.Turn)]; dead || p.muted() {
			if p.muted() {
				p.drop()
			}
			return nil
		}
		return e.Push(c)
	}
	p.segs[idx].text, p.segs[idx].final = c.Text, c.Final || c.EndOfUtterance
	out := *c
	out.Turn, out.Gen, out.Text = p.id, p.wireGen(), p.joined()
	// The manager owns the turn boundary: an STT end-of-utterance would let the finalizer commit
	// the turn 200 ms later, before resumed speech can join it. TurnEventCompleted commits it.
	out.Final, out.EndOfUtterance = c.Final || c.EndOfUtterance, false
	if idx != len(p.segs)-1 {
		out.Final = false
	}
	p.candGen = uint64(out.Gen)
	if err := e.Push(&out); err != nil {
		return err
	}
	return p.observe(e, out.Text, out.Final)
}

func (p *Processor) joined() string {
	var parts []string
	for _, s := range p.segs {
		if t := strings.TrimSpace(s.text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

func (p *Processor) wireGen() core.Generation {
	if p.session != nil {
		return core.Generation(p.session.CurrentGeneration())
	}
	return p.mgrGen
}

func (p *Processor) observe(e core.Emitter, text string, final bool) error {
	return p.observeSilence(e, text, final, time.Time{})
}

func (p *Processor) observeSilence(e core.Emitter, text string, final bool, silenceSince time.Time) error {
	if !p.open {
		return nil
	}
	at := p.now()
	stop := p.extStop
	p.extStop = false
	obs := Observation{ExternalStart: p.held, ExternalStop: stop, At: at, SilenceSince: silenceSince, TurnID: p.id, Generation: p.mgrGen, Speaking: p.vadSpeaking, Activity: p.vadSpeaking,
		BotSpeaking: p.bot, Transcript: text, TranscriptFinal: final}
	frames, err := p.mgr.Observe(obs)
	if errors.Is(err, ErrRetiredTurnLimit) {
		// Long calls retire one id per turn; start a fresh epoch rather than go deaf.
		if p.mgr.Reset(at) == nil {
			frames, err = p.mgr.Observe(obs)
		}
	}
	if err != nil {
		return nil // ponytail: a rejected observation is dropped; the silence fallback still ends the turn
	}
	return p.emit(e, frames)
}

func (p *Processor) advance(e core.Emitter) error {
	at := p.now()
	frames, err := p.mgr.Advance(at)
	if err == nil {
		if err := p.emit(e, frames); err != nil {
			return err
		}
	}
	limit := p.expiry
	if n := len(p.segs); n > 0 && !p.segs[n-1].final {
		limit = p.expiryNoFinal
	}
	if p.open && !p.started && !p.vadSpeaking && at.Sub(p.lastStop) >= limit {
		// Never admitted (backchannel, wake phrase idle): retire its transcripts
		// uncommitted so they cannot leak into the next turn.
		gen := core.Generation(p.candGen)
		id := p.id
		p.finish()
		if gen != 0 {
			return e.Push(&core.TranscriptInvalidationFrame{Turn: id, Gen: gen})
		}
	}
	return nil
}

// finish ends the managed turn but keeps its segments so late finals inside the
// finalizer's grace period are still re-keyed and delivered.
func (p *Processor) finish() {
	p.open, p.started = false, false
}

func (p *Processor) retireSegments() {
	if len(p.closed) > 64 {
		p.closed = make(map[string]struct{})
	}
	for _, s := range p.segs {
		p.closed[s.turn] = struct{}{}
	}
	p.segs = nil
}

func (p *Processor) emit(e core.Emitter, frames []core.Frame) error {
	for _, f := range frames {
		ev, ok := f.(*core.TurnEventFrame)
		if !ok {
			if err := e.Push(f); err != nil {
				return err
			}
			continue
		}
		out := *ev
		if out.Turn != "" {
			out.Gen = p.wireGen()
		}
		switch ev.Kind {
		case core.TurnEventStarted:
			p.started = true
		case core.TurnEventCompleted, core.TurnEventInterrupted:
			// Barge-in bumped the generation after the last candidate; re-key the
			// snapshot so the finalizer completes the turn it actually holds.
			if text := p.joined(); text != "" && p.candGen != uint64(out.Gen) {
				if err := e.Push(&core.TranscriptCandidateFrame{Text: text, Timestamp: out.At, Turn: p.id, Gen: out.Gen,
					Final: p.segs[len(p.segs)-1].final}); err != nil {
					return err
				}
			}
		}
		if err := e.Push(&out); err != nil {
			return err
		}
		if ev.Kind == core.TurnEventCompleted || ev.Kind == core.TurnEventInterrupted {
			p.finish()
		}
	}
	return nil
}
