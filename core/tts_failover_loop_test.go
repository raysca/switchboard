package core_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// loopTTS is a TTS candidate that counts the calls it gets while the session's run loop is parked (held):
// such a call can only come from another goroutine, which is what the "callers serialize" contract of
// ttsunit.Runner and core.SerialQueue forbids. stall makes it swallow the next unit, as a wedged provider
// does; any other unit is answered at once from a session task, as a provider's synthesis task does.
type loopTTS struct {
	session core.SessionController
	index   int
	held    *atomic.Bool
	offLoop atomic.Int32
	units   atomic.Int32 // synthesis units received
	stall   atomic.Bool
}

func (p *loopTTS) Attach(s core.SessionController, index int) error {
	p.session, p.index = s, index
	return nil
}

func (p *loopTTS) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if p.held.Load() {
		p.offLoop.Add(1)
	}
	su, ok := f.(*core.SynthesisUnitFrame)
	if !ok {
		return e.PushDirection(f, dir)
	}
	p.units.Add(1)
	if p.stall.CompareAndSwap(true, false) {
		return nil // wedged: this unit is never answered
	}
	return p.session.Go(func(context.Context) {
		_ = p.session.Reenter(&core.TTSStartedFrame{Turn: su.Turn, Gen: su.Gen, UnitID: su.UnitID}, core.Downstream, p.index+1, su.Gen)
		_ = p.session.Reenter(&core.OutputAudioFrame{Samples: make([]byte, 960), Encoding: core.EncodingPCM16LE, SampleRate: 24000,
			Channels: 1, Duration: 20 * time.Millisecond, Turn: su.Turn, Gen: su.Gen, UnitID: su.UnitID, Seq: 1, IsFinal: true,
			TurnFinal: su.IsFinal}, core.Downstream, p.index+1, su.Gen)
	})
}

// newLoopFailover is TTSFailover(primary, fallback) -> gateStage -> recorder on a started session. The gate
// parks the run loop (held = true) on a gateFrame until release is closed.
func newLoopFailover(t *testing.T) (sess *core.Session, primary, fallback *loopTTS, rec *coretest.PassthroughProcessor, entered, release chan struct{}) {
	t.Helper()
	var held atomic.Bool
	primary, fallback = &loopTTS{held: &held}, &loopTTS{held: &held}
	w, err := core.NewTTSFailover(core.TTSFailoverConfig{UnitTimeout: 30 * time.Millisecond,
		Candidates: []core.TTSCandidate{{Provider: "a", Proc: primary}, {Provider: "b", Proc: fallback}}})
	if err != nil {
		t.Fatal(err)
	}
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	gate := &gateStage{hold: func() {
		held.Store(true)
		once.Do(func() { close(entered) })
		<-release
		held.Store(false)
	}}
	rec = &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(w, gate, rec)
	if err != nil {
		t.Fatal(err)
	}
	sess, err = core.NewSession(950, pipe, core.DefaultResourceBudget())
	if err != nil || sess.Start() != nil {
		t.Fatal(err)
	}
	return sess, primary, fallback, rec, entered, release
}

func audioFor(rec *coretest.PassthroughProcessor, unit uint64) bool {
	for _, f := range rec.GetFrames() {
		if a, ok := f.(*core.OutputAudioFrame); ok && a.UnitID == unit {
			return true
		}
	}
	return false
}

// A unit's watchdog expires while the run loop is busy elsewhere (here: parked in the gate). The expiry
// must wait for the run loop instead of cancelling the wedged candidate and starting the fallback from the
// timer goroutine; afterwards the unit is spoken by the fallback, and so is the next one.
func TestTTSWatchdogExpiryDrivesCandidatesOnlyFromTheRunLoop(t *testing.T) {
	sess, primary, fallback, rec, entered, release := newLoopFailover(t)
	defer func() {
		_ = sess.Close()
		if n := sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	}()
	gen := sess.CurrentGeneration()
	primary.stall.Store(true)
	if err := sess.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: gen, UnitID: 1, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Post(&gateFrame{}); err != nil { // queued behind the unit: the loop parks after starting it
		t.Fatal(err)
	}
	<-entered
	// The watchdog (30 ms + 20 ms per character) fires several times over while the loop is parked.
	time.Sleep(300 * time.Millisecond) // a bounded wait for something that must not happen
	if n := primary.offLoop.Load() + fallback.offLoop.Load(); n != 0 {
		close(release)
		t.Fatalf("%d candidate calls came from outside the run loop (the watchdog's timer goroutine)", n)
	}
	close(release)
	if err := sess.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: gen, UnitID: 2, Text: "b", IsFinal: true}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(2 * time.Second); !(audioFor(rec, 1) && audioFor(rec, 2)); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("units after the stall were not spoken; frames=%d", rec.Count())
		}
	}
}

// A watchdog that fires after the session closed finds no run loop: the expiry is dropped, nothing leaks.
func TestTTSWatchdogExpiryAfterCloseIsDropped(t *testing.T) {
	sess, primary, _, _, _, release := newLoopFailover(t)
	close(release)
	primary.stall.Store(true)
	if err := sess.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: sess.CurrentGeneration(), UnitID: 1, Text: "a"}); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(time.Second); primary.units.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the unit never reached the primary")
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // past the 50 ms watchdog: its expiry meets a closed session
	if n := sess.LeakedTasks(); n != 0 {
		t.Fatalf("leaked %d tasks", n)
	}
}
