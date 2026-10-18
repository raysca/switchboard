package smartturn

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/core/turn"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	when time.Time
	c    chan time.Time
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }
func (t *fakeTimer) Stop() bool          { return true }
func (c *fakeClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) NewTimer(d time.Duration) turn.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{when: c.now.Add(d), c: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	keep := c.timers[:0]
	for _, t := range c.timers {
		if !t.when.After(c.now) {
			t.c <- c.now
		} else {
			keep = append(keep, t)
		}
	}
	c.timers = keep
}

// fakeBackend answers with p (or err) once release is closed.
type fakeBackend struct {
	calls   atomic.Int32
	release chan struct{}
	p       float32
	err     error
	gotLen  atomic.Int32
	cancels atomic.Int32
}

func newFake(p float32, err error) *fakeBackend {
	return &fakeBackend{release: make(chan struct{}), p: p, err: err}
}

func (f *fakeBackend) Infer(ctx context.Context, pcm []int16) (float32, error) {
	f.calls.Add(1)
	f.gotLen.Store(int32(len(pcm)))
	select {
	case <-f.release:
		return f.p, f.err
	case <-ctx.Done():
		f.cancels.Add(1)
		return 0, ctx.Err()
	}
}
func (f *fakeBackend) Ready() bool  { return true }
func (f *fakeBackend) Close() error { return nil }

func newTestAnalyzer(be Backend, clk *fakeClock) *Analyzer {
	a := NewAnalyzer(be, Config{Now: clk.Now})
	a.Feed(make([]int16, 16000), clk.Now())
	return a
}

func input(clk *fakeClock, silence time.Duration, id core.TurnID) turn.DetectionInput {
	return turn.DetectionInput{At: clk.Now(), TurnStartedAt: clk.Now().Add(-2 * time.Second), TurnID: id, Generation: 1,
		SilenceFor: silence, SilenceAfter: 700 * time.Millisecond}
}

// at moves the clock to silenceStart+sil and returns that tick's input.
func at(clk *fakeClock, silenceStart time.Time, sil time.Duration, id core.TurnID) turn.DetectionInput {
	clk.mu.Lock()
	clk.now = silenceStart.Add(sil)
	clk.mu.Unlock()
	return input(clk, sil, id)
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

// The session loop never blocks on the model, and one (turn, silence) costs
// exactly one inference no matter how many 20 ms ticks ask.
func TestAnalyzerAsyncAndCached(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	be := newFake(0.9, nil)
	a := newTestAnalyzer(be, clk)
	s := Stop(a, 700*time.Millisecond, 3*time.Second)

	t0 := clk.Now()
	if _, done := s.Detect(at(clk, t0, 120*time.Millisecond, "t1")); done {
		t.Fatal("verdict must not be ready on the first call")
	}
	close(be.release)
	var d turn.DetectionDecision
	eventually(t, func() bool {
		var ok bool
		d, ok = s.Detect(at(clk, t0, 125*time.Millisecond, "t1"))
		return ok
	})
	if d.Reason != core.TurnReasonSemantic || !d.HasConfidence || d.Confidence < 0.89 {
		t.Fatalf("decision %+v", d)
	}
	for i := 0; i < 20; i++ {
		s.Detect(at(clk, t0, 130*time.Millisecond, "t1"))
	}
	if n := be.calls.Load(); n != 1 {
		t.Fatalf("%d inferences for one silence, want 1", n)
	}
}

func TestAnalyzerIncompleteHoldsUntilHold(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	be := newFake(0.1, nil)
	close(be.release)
	a := newTestAnalyzer(be, clk)
	s := Stop(a, 700*time.Millisecond, 3*time.Second)
	// fixed silence start: At advances with SilenceFor
	start := clk.Now()
	for _, sil := range []time.Duration{120, 400, 800, 1500, 2900} {
		clk.now = start.Add(sil * time.Millisecond)
		in := input(clk, sil*time.Millisecond, "t1")
		if _, done := s.Detect(in); done {
			t.Fatalf("incomplete verdict ended the turn at %dms", sil)
		}
		time.Sleep(10 * time.Millisecond)
	}
	clk.now = start.Add(3 * time.Second)
	d, done := s.Detect(input(clk, 3*time.Second, "t1"))
	if !done || d.Reason != core.TurnReasonSilence {
		t.Fatalf("hold expiry: %+v %v", d, done)
	}
	if be.calls.Load() != 1 {
		t.Fatalf("calls %d", be.calls.Load())
	}
}

func TestAnalyzerNewSilenceReinfers(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	be := newFake(0.1, nil)
	close(be.release)
	a := newTestAnalyzer(be, clk)
	a.verdict(input(clk, 100*time.Millisecond, "t1"))
	clk.Advance(2 * time.Second) // user spoke again, then went quiet: different silence start
	a.verdict(input(clk, 100*time.Millisecond, "t1"))
	eventually(t, func() bool { return be.calls.Load() == 2 })
}

// A backend that never answers: after Budget the verdict is degraded and the
// strategy ends the turn at speech-timeout, cancelling the stuck call.
func TestAnalyzerTimeoutFallsBackToSpeechTimeout(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	be := newFake(0, nil) // never released
	a := newTestAnalyzer(be, clk)
	s := Stop(a, 700*time.Millisecond, 3*time.Second)
	before := Fallbacks(ReasonTimeout)
	start := clk.Now()
	var endedAt time.Duration = -1
	for el := 120 * time.Millisecond; el <= 2*time.Second; el += 20 * time.Millisecond {
		clk.now = start.Add(el)
		if d, ok := s.Detect(input(clk, el, "t1")); ok {
			if d.Reason != core.TurnReasonSilence {
				t.Fatalf("reason %v", d.Reason)
			}
			endedAt = el
			break
		}
	}
	if endedAt != 700*time.Millisecond {
		t.Fatalf("degraded turn ended at %v, want the 700ms speech timeout", endedAt)
	}
	if Fallbacks(ReasonTimeout) != before+1 {
		t.Fatal("timeout fallback not counted")
	}
	eventually(t, func() bool { return be.cancels.Load() == 1 })
}

func TestAnalyzerFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{{ErrBusy, ReasonOverload}, {ErrUnavailable, ReasonUnavailable}, {errors.New("boom"), ReasonError}} {
		clk := &fakeClock{now: time.Unix(1000, 0)}
		be := newFake(0, tc.err)
		close(be.release)
		a := newTestAnalyzer(be, clk)
		s := Stop(a, 700*time.Millisecond, 3*time.Second)
		before := Fallbacks(tc.reason)
		t0 := clk.Now()
		s.Detect(at(clk, t0, 120*time.Millisecond, "t1"))
		eventually(t, func() bool { return Fallbacks(tc.reason) == before+1 })
		if _, done := s.Detect(at(clk, t0, 300*time.Millisecond, "t1")); done {
			t.Fatalf("%v: degraded turn ended before speech-timeout", tc.err)
		}
		d, done := s.Detect(at(clk, t0, 700*time.Millisecond, "t1"))
		if !done || d.Reason != core.TurnReasonSilence {
			t.Fatalf("%v: %+v %v", tc.err, d, done)
		}
	}
}

func TestAnalyzerNoAudioAndPanic(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	a := NewAnalyzer(newFake(0.9, nil), Config{Now: clk.Now}) // nothing fed
	before := Fallbacks(ReasonNoAudio)
	a.verdict(input(clk, 120*time.Millisecond, "t1"))
	if Fallbacks(ReasonNoAudio) != before+1 {
		t.Fatal("no_audio not counted")
	}
	pb := &panicBackend{}
	a = newTestAnalyzer(pb, clk)
	b := Fallbacks(ReasonError)
	a.verdict(input(clk, 120*time.Millisecond, "t2"))
	eventually(t, func() bool { return Fallbacks(ReasonError) == b+1 })
}

type panicBackend struct{}

func (panicBackend) Infer(context.Context, []int16) (float32, error) { panic("model exploded") }
func (panicBackend) Ready() bool                                     { return true }
func (panicBackend) Close() error                                    { return nil }

// Audio handed to the model: from turn start minus pre-speech, last 8 s only.
func TestAnalyzerSegment(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	a := NewAnalyzer(newFake(0.5, nil), Config{Now: clk.Now})
	for i := 0; i < 1000; i++ { // 20 s of 20 ms frames
		a.Feed(make([]int16, 320), clk.now.Add(time.Duration(i)*20*time.Millisecond))
	}
	if a.total > WindowLen+SampleRate/2+320 {
		t.Fatalf("ring unbounded: %d samples", a.total)
	}
	turnStart := clk.now.Add(19 * time.Second)
	a.mu.Lock()
	seg := a.segmentLocked(turnStart)
	a.mu.Unlock()
	if want := (500 + 1000) * 16; len(seg) < want-640 || len(seg) > want+640 { // 0.5 s pre-speech + 1 s of turn
		t.Fatalf("segment %d samples, want ~%d", len(seg), want)
	}
	a.mu.Lock()
	seg = a.segmentLocked(clk.now) // turn older than the ring
	a.mu.Unlock()
	if len(seg) > WindowLen {
		t.Fatalf("segment %d > window", len(seg))
	}
}

// Through a real session: tap -> (STT stand-in) -> turn processor with the
// model strategy, on a fake clock and a scripted verdict stream.
func TestSessionVerdictStream(t *testing.T) {
	run := func(t *testing.T, be Backend) (reason core.TurnReason, silence time.Duration) {
		clk := &fakeClock{now: time.Unix(1000, 0)}
		a := NewAnalyzer(be, Config{Now: clk.Now})
		cfg := turn.DefaultConfig()
		proc, err := turn.NewProcessor(cfg, turn.ProcessorOptions{Clock: clk, Detector: Stop(a, cfg.SilenceTimeout, 3*time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		sink := &coretest.PassthroughProcessor{}
		pipe, err := core.NewPipeline(a.Tap(), &coretest.PassthroughProcessor{}, proc, sink)
		if err != nil {
			t.Fatal(err)
		}
		sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.Start(); err != nil {
			t.Fatal(err)
		}
		defer sess.Close()
		defer a.Close()
		pcm := make([]byte, 640)
		_ = sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
		for i := 0; i < 20; i++ {
			_ = sess.Post(&core.InputAudioFrame{Samples: pcm, Encoding: core.EncodingPCM16LE, SampleRate: SampleRate, Channels: 1})
			clk.Advance(20 * time.Millisecond)
			time.Sleep(2 * time.Millisecond)
		}
		_ = sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
		time.Sleep(30 * time.Millisecond)
		start := clk.Now()
		for el := time.Duration(0); el < 5*time.Second; el += 20 * time.Millisecond {
			clk.Advance(20 * time.Millisecond)
			time.Sleep(3 * time.Millisecond)
			for _, f := range sink.GetFrames() {
				if ev, ok := f.(*core.TurnEventFrame); ok && ev.Kind == core.TurnEventCompleted {
					return ev.Reason, clk.Now().Sub(start)
				}
			}
		}
		t.Fatal("turn never completed")
		return
	}
	t.Run("complete verdict ends the turn early", func(t *testing.T) {
		be := newFake(0.95, nil)
		close(be.release)
		r, d := run(t, be)
		if r != core.TurnReasonSemantic || d >= 700*time.Millisecond {
			t.Fatalf("reason %v after %v", r, d)
		}
	})
	t.Run("incomplete verdict holds to stop_secs", func(t *testing.T) {
		be := newFake(0.05, nil)
		close(be.release)
		r, d := run(t, be)
		if r != core.TurnReasonSilence || d < 2500*time.Millisecond {
			t.Fatalf("reason %v after %v", r, d)
		}
	})
	t.Run("crashed model falls back to speech-timeout and the call continues", func(t *testing.T) {
		be := newFake(0, ErrUnavailable)
		close(be.release)
		r, d := run(t, be)
		if r != core.TurnReasonSilence || d > 900*time.Millisecond {
			t.Fatalf("reason %v after %v", r, d)
		}
	})
}

// capBackend answers p and keeps the last window.
type capBackend struct {
	mu  sync.Mutex
	pcm []int16
	p   float32
}

func (b *capBackend) Infer(_ context.Context, pcm []int16) (float32, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pcm = append([]int16(nil), pcm...)
	return b.p, nil
}
func (*capBackend) Ready() bool  { return true }
func (*capBackend) Close() error { return nil }
func (b *capBackend) count(v int16) (n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, x := range b.pcm {
		if x == v {
			n++
		}
	}
	return n
}

// feedFor feeds d of 20 ms frames (value v) through the tap's path, advancing clk.
func feedFor(a *Analyzer, clk *fakeClock, d time.Duration, v int16) {
	for el := time.Duration(0); el < d; el += 20 * time.Millisecond {
		pcm := make([]int16, 320)
		for i := range pcm {
			pcm[i] = v
		}
		a.feed(pcm, clk.Now())
		clk.Advance(20 * time.Millisecond)
	}
}

// Pipecat's window starts at the VAD speech start (minus pre-speech), not at
// the late turn start the manager reports with min_words; it spans pauses
// while the verdict is incomplete, and a complete verdict or Hold of silence
// starts the next window at the next speech.
func TestAnalyzerWindowFollowsSpeechStart(t *testing.T) {
	clk := &fakeClock{now: time.Unix(1000, 0)}
	be := &capBackend{p: 0.9}
	a := NewAnalyzer(be, Config{Now: clk.Now})
	s := Stop(a, 700*time.Millisecond, 3*time.Second)
	utter := func(d time.Duration, v int16, gap time.Duration) {
		a.vad(true, clk.Now())
		feedFor(a, clk, d, v)
		a.vad(false, clk.Now())
		feedFor(a, clk, gap, 0)
	}
	analyze := func(id core.TurnID, turnStart time.Time, v int16, want int) {
		t.Helper()
		in := turn.DetectionInput{At: clk.Now(), TurnStartedAt: turnStart, TurnID: id, Generation: 1, SilenceFor: 100 * time.Millisecond, SilenceAfter: 700 * time.Millisecond}
		s.Detect(in)
		eventually(t, func() bool { return a.jobDone() })
		if n := be.count(v); n != want {
			t.Fatalf("%s: window holds %d samples of value %d, want %d", id, n, v, want)
		}
	}

	feedFor(a, clk, 2*time.Second, 0)
	utter(time.Second, 1000, 1500*time.Millisecond)
	analyze("t1", clk.Now().Add(-300*time.Millisecond), 1000, SampleRate) // admitted by a late final

	// Complete cleared it: the next utterance (inside Hold) is a window of its own.
	feedFor(a, clk, time.Second, 0)
	utter(500*time.Millisecond, 2000, 300*time.Millisecond)
	analyze("t2", clk.Now(), 2000, SampleRate/2)
	if n := be.count(1000); n != 0 {
		t.Fatalf("t2 window still holds %d samples of t1", n)
	}

	// Incomplete keeps the window across a pause ...
	be.mu.Lock()
	be.p = 0.1
	be.mu.Unlock()
	feedFor(a, clk, time.Second, 0)
	utter(500*time.Millisecond, 3000, 400*time.Millisecond)
	analyze("t3", clk.Now(), 3000, SampleRate/2)
	utter(500*time.Millisecond, 3000, 100*time.Millisecond)
	analyze("t3", clk.Now(), 3000, SampleRate)
	// ... until Hold of silence: the next speech opens a new window.
	feedFor(a, clk, 3100*time.Millisecond, 0)
	utter(200*time.Millisecond, 4000, 100*time.Millisecond)
	analyze("t4", clk.Now(), 4000, SampleRate/5)
	if n := be.count(3000); n != 0 {
		t.Fatalf("t4 window still holds %d samples of t3", n)
	}
}

func (a *Analyzer) jobDone() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.job != nil && a.job.done
}
