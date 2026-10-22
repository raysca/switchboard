package smartturn

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"time"

	"govoice/core"
	"govoice/core/turn"
)

// Backend runs the model. Infer must honour ctx and may be called from any
// goroutine; ErrBusy/ErrUnavailable report overload and a dead worker.
type Backend interface {
	Infer(ctx context.Context, pcm []int16) (float32, error)
	Ready() bool
	Close() error
}

var (
	ErrBusy        = errors.New("smartturn: all workers busy")
	ErrUnavailable = errors.New("smartturn: no worker available")
)

// Config tunes one per-call Analyzer. Zero values are the defaults.
type Config struct {
	Budget    time.Duration    // verdict deadline; later = fall back (default 150ms)
	PreSpeech time.Duration    // audio kept before the speech start (default 500ms, Pipecat pre_speech_ms)
	Hold      time.Duration    // silence after which the next VAD start begins a new window (default 3s, Pipecat stop_secs)
	Now       func() time.Time // default time.Now; must be the turn processor's clock
}

// Analyzer is the per-call smart-turn model client. It never runs inference on the
// caller: verdict starts one async job per (turn, silence start), returns
// "incomplete" while it runs, and serves the cached verdict afterwards. A job
// that is not done inside Budget, or that fails, is a degraded verdict, which
// Stop turns into the plain speech-timeout.
type Analyzer struct {
	be  Backend
	cfg Config

	mu     sync.Mutex
	chunks []chunk // last ~8.5 s of audio, oldest first
	total  int     // samples in chunks
	job    *job

	// Pipecat's _speech_start_time: the first VAD start since the window was
	// last cleared (complete verdict, turn ended here, or Hold of silence).
	// The manager's TurnStartedAt can be much later (min_words, transcript
	// start), which would leave the window holding only trailing silence.
	speechStart time.Time
	speaking    bool
	lastStop    time.Time
}

type chunk struct {
	at  time.Time
	pcm []int16
}

type jobKey struct {
	turn core.TurnID
	gen  core.Generation
	from time.Time // silence start = At - SilenceFor
}

// same compares silence starts with Equal: At-SilenceFor keeps the monotonic
// reading, so it is exact tick to tick, while its wall clock (UnixNano)
// drifts by the wall/monotonic skew and would restart the job every tick.
func (k jobKey) same(o jobKey) bool {
	return k.turn == o.turn && k.gen == o.gen && k.from.Equal(o.from)
}

type job struct {
	key     jobKey
	started time.Time
	cancel  context.CancelFunc
	done    bool
	res     turn.AnalyzerResult
	reason  string // non-empty: degraded
}

func NewAnalyzer(be Backend, cfg Config) *Analyzer {
	if cfg.Budget <= 0 {
		cfg.Budget = 150 * time.Millisecond
	}
	if cfg.PreSpeech <= 0 {
		cfg.PreSpeech = 500 * time.Millisecond
	}
	if cfg.Hold <= 0 {
		cfg.Hold = 3 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Analyzer{be: be, cfg: cfg}
}

// Feed appends a copy of 16 kHz mono PCM to the bounded ring (last 8.5 s).
func (a *Analyzer) Feed(pcm []int16, at time.Time) { a.feed(append([]int16(nil), pcm...), at) }

// feed takes ownership of pcm: the tap decodes each frame once, into the ring.
func (a *Analyzer) feed(pcm []int16, at time.Time) {
	if len(pcm) == 0 {
		return
	}
	c := chunk{at, pcm}
	a.mu.Lock()
	a.chunks = append(a.chunks, c)
	a.total += len(c.pcm)
	maxN := WindowLen + int(a.cfg.PreSpeech*SampleRate/time.Second)
	drop := 0
	for a.total-len(a.chunks[drop].pcm) >= maxN {
		a.total -= len(a.chunks[drop].pcm)
		drop++
	}
	if drop > 0 {
		a.chunks = append(a.chunks[:0], a.chunks[drop:]...)
	}
	a.mu.Unlock()
}

// vad records a VAD transition seen by the tap (Pipecat append_audio's is_speech).
func (a *Analyzer) vad(speaking bool, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if speaking && !a.speaking && (a.speechStart.IsZero() || at.Sub(a.lastStop) >= a.cfg.Hold) {
		a.speechStart = at
	}
	if !speaking && a.speaking {
		a.lastStop = at
	}
	a.speaking = speaking
}

// clearLocked starts the next window at the next VAD start (Pipecat _clear).
func (a *Analyzer) clearLocked() {
	a.speechStart = time.Time{}
	if a.speaking {
		a.speechStart = a.cfg.Now()
	}
}

// Close cancels any in-flight job.
func (a *Analyzer) Close() {
	a.mu.Lock()
	if a.job != nil {
		a.abandonLocked(a.job)
	}
	a.mu.Unlock()
}

// abandonLocked drops a job nobody waits for any more (superseded or closed): its late result,
// including the context.Canceled its own cancel causes, is ignored and counted nowhere.
func (a *Analyzer) abandonLocked(j *job) {
	j.done = true
	j.cancel()
}

// verdict returns the cached/async result; degraded is true when the model
// could not judge this silence (timeout, crash, overload).
func (a *Analyzer) verdict(in turn.DetectionInput) (turn.AnalyzerResult, bool) {
	key := jobKey{in.TurnID, in.Generation, in.At.Add(-in.SilenceFor)}
	now := a.cfg.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.job == nil || !a.job.key.same(key) {
		a.startLocked(key, in.TurnStartedAt, now)
	}
	j := a.job
	if !j.done && now.Sub(j.started) >= a.cfg.Budget {
		a.failLocked(j, ReasonTimeout)
	}
	return j.res, j.reason != ""
}

func (a *Analyzer) startLocked(key jobKey, turnStart, now time.Time) {
	if a.job != nil {
		a.abandonLocked(a.job)
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{key: key, started: now, cancel: cancel}
	a.job = j
	pcm := a.segmentLocked(turnStart)
	if len(pcm) == 0 {
		a.failLocked(j, ReasonNoAudio)
		return
	}
	go func() {
		defer func() {
			if recover() != nil {
				a.finish(j, 0, errors.New("panic"))
			}
		}()
		t0 := time.Now()
		p, err := a.be.Infer(ctx, pcm)
		if err == nil {
			observeLatency(float64(time.Since(t0).Microseconds()) / 1000)
		}
		a.finish(j, p, err)
	}()
}

func (a *Analyzer) finish(j *job, p float32, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if j.done {
		return
	}
	if err != nil {
		reason := ReasonError
		switch {
		case errors.Is(err, ErrBusy):
			reason = ReasonOverload
		case errors.Is(err, ErrUnavailable):
			reason = ReasonUnavailable
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			reason = ReasonTimeout
		}
		a.failLocked(j, reason)
		return
	}
	j.done = true
	j.res = turn.AnalyzerResult{Complete: p > 0.5, Confidence: float64(p)}
	if j.res.Complete {
		a.clearLocked()
		stats.complete.Add(1)
	} else {
		stats.incompl.Add(1)
	}
}

func (a *Analyzer) failLocked(j *job, reason string) {
	j.done, j.reason = true, reason
	j.cancel()
	countFallback(reason)
}

// segmentLocked is the audio from the speech start (or the turn start, if
// earlier or no VAD start was seen) minus PreSpeech to now, last 8 s.
func (a *Analyzer) segmentLocked(turnStart time.Time) []int16 {
	if !a.speechStart.IsZero() && a.speechStart.Before(turnStart) {
		turnStart = a.speechStart
	}
	from := turnStart.Add(-a.cfg.PreSpeech)
	i := 0
	for i < len(a.chunks) && a.chunks[i].at.Before(from) {
		i++
	}
	n := 0
	for _, c := range a.chunks[i:] {
		n += len(c.pcm)
	}
	out := make([]int16, 0, min(n, WindowLen))
	for _, c := range a.chunks[i:] {
		out = append(out, c.pcm...)
	}
	if len(out) > WindowLen {
		out = out[len(out)-WindowLen:]
	}
	return out
}

// Stop is the user-turn stop strategy for the model. While the verdict is
// pending or "incomplete" the turn is held until hold of silence (Pipecat's
// stop_secs); when the model cannot judge (degraded) it behaves exactly like
// speech-timeout and ends at base. base <= 0 uses the manager's SilenceTimeout.
func Stop(a *Analyzer, base, hold time.Duration) turn.UserTurnStopStrategy {
	return stop{a, base, max(hold, base)}
}

type stop struct {
	a          *Analyzer
	base, hold time.Duration
}

func (s stop) Detect(in turn.DetectionInput) (turn.DetectionDecision, bool) {
	base := s.base
	if base <= 0 {
		base = in.SilenceAfter
	}
	if base <= 0 || in.SilenceFor <= 0 {
		return turn.DetectionDecision{}, false
	}
	if in.SilenceFor >= max(s.hold, base) {
		return s.end(turn.DetectionDecision{Reason: core.TurnReasonSilence})
	}
	r, degraded := s.a.verdict(in)
	switch {
	case r.Complete:
		return turn.DetectionDecision{Reason: core.TurnReasonSemantic, Confidence: r.Confidence, HasConfidence: true}, true
	case degraded && in.SilenceFor >= base:
		return s.end(turn.DetectionDecision{Reason: core.TurnReasonSilence})
	}
	return turn.DetectionDecision{}, false
}

// end clears the window when this strategy ends the turn (Pipecat clears on
// turn stop). ponytail: a turn ended by another strategy (eager, external)
// keeps the window until a complete verdict or Hold of silence clears it.
func (s stop) end(d turn.DetectionDecision) (turn.DetectionDecision, bool) {
	s.a.mu.Lock()
	s.a.clearLocked()
	s.a.mu.Unlock()
	return d, true
}

// Tap returns a pass-through processor that feeds the analyzer with inbound
// audio. Place it after VAD and before STT: STT consumes audio frames, so
// nothing after it (the turn processor included) ever sees them.
func (a *Analyzer) Tap() core.Processor { return tap{a} }

type tap struct{ a *Analyzer }

// Close cancels the analyzer's in-flight job at session teardown.
func (t tap) Close() error { t.a.Close(); return nil }

func (t tap) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if af, ok := f.(*core.InputAudioFrame); ok && dir == core.Downstream &&
		af.SampleRate == SampleRate && (af.Encoding == "" || af.Encoding == core.EncodingPCM16LE) && af.Channels <= 1 {
		pcm := make([]int16, len(af.Samples)/2)
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(af.Samples[2*i:]))
		}
		t.a.feed(pcm, t.a.cfg.Now())
	}
	switch f.(type) {
	case *core.UserStartedSpeakingFrame:
		t.a.vad(true, t.a.cfg.Now())
	case *core.UserStoppedSpeakingFrame:
		t.a.vad(false, t.a.cfg.Now())
	}
	return e.PushDirection(f, dir)
}
