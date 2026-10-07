package core_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
)

var errTTS = errors.New("tts 503")

type ttsRig struct {
	sess     *core.Session
	rec      *core.PassthroughProcessor
	mu       sync.Mutex
	switches []string
	events   chan core.DebugEvent
	cands    []core.TTSCandidate
}

func newTTSRig(t *testing.T, cfg core.TTSFailoverConfig) *ttsRig {
	t.Helper()
	r := &ttsRig{rec: &core.PassthroughProcessor{}, events: make(chan core.DebugEvent, 16)}
	cfg.OnFailover = func(from, to, class string) {
		r.mu.Lock()
		r.switches = append(r.switches, from+">"+to+":"+class)
		r.mu.Unlock()
	}
	if cfg.Classify == nil {
		cfg.Classify = func(error) string { return "server_error" }
	}
	w, err := core.NewTTSFailover(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := core.NewPipeline(w, r.rec)
	if err != nil {
		t.Fatal(err)
	}
	r.sess, err = core.NewSession(901, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Start(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.sess.Subscribe(16, func(e core.DebugEvent) {
		if e.Name == core.DebugTTSFailover {
			r.events <- e
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.sess.Close()
		if n := r.sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	})
	return r
}

func (r *ttsRig) unit(t *testing.T, turn string, id uint64, final bool) {
	t.Helper()
	if err := r.sess.Post(&core.SynthesisUnitFrame{Turn: turn, Gen: r.sess.CurrentGeneration(), UnitID: id, Text: "hi", IsFinal: final}); err != nil {
		t.Fatal(err)
	}
}

func (r *ttsRig) switched() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.switches...)
}

// audio returns the unit ids of audio frames in arrival order, and error count.
func (r *ttsRig) audio() (ids []uint64, finals, errs int) {
	for _, f := range r.rec.GetFrames() {
		switch v := f.(type) {
		case *core.OutputAudioFrame:
			ids = append(ids, v.UnitID)
			if v.IsFinal {
				finals++
			}
		case *core.ErrorFrame:
			errs++
		}
	}
	return
}

func (r *ttsRig) waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met; frames=%d %v", r.rec.Count(), func() (o []string) {
				for _, f := range r.rec.GetFrames() {
					o = append(o, fmt.Sprintf("%T%+v", f, f))
				}
				return
			}())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func fakeTTS(failAfter int, err error) *core.FakeTTSProcessor {
	return core.NewFakeTTSProcessor(core.FakeTTSConfig{FramesPerUnit: 2, FailWith: err, FailAfterAudio: failAfter})
}

func TestTTSFailoverBeforeAudioReplaysUnitOnce(t *testing.T) {
	a, b := fakeTTS(0, errTTS), fakeTTS(0, nil)
	r := newTTSRig(t, core.TTSFailoverConfig{Candidates: []core.TTSCandidate{{Provider: "a", Proc: a}, {Provider: "b", Proc: b}}})
	r.unit(t, "t1", 1, true)
	r.waitFor(t, func() bool { _, f, _ := r.audio(); return f == 1 })
	time.Sleep(30 * time.Millisecond)
	ids, finals, errs := r.audio()
	if len(ids) != 2 || finals != 1 || errs != 0 || b.Units.Load() != 1 {
		t.Fatalf("ids=%v finals=%d errs=%d", ids, finals, errs)
	}
	if got := r.switched(); len(got) != 1 || got[0] != "a>b:server_error" {
		t.Fatalf("switches %v", got)
	}
	select {
	case e := <-r.events:
		if a := e.Attributes(); a["component"] != "a" || a["provider"] != "b" || a["reason"] != "server_error" {
			t.Fatalf("attrs %v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("no tts.failover event")
	}
	su := 0
	for _, f := range r.rec.GetFrames() {
		if _, ok := f.(*core.SynthesisUnitFrame); ok {
			su++
		}
	}
	if su != 1 {
		t.Fatalf("unit frame went downstream %d times", su)
	}
}

func TestTTSFailAfterAudioDoesNotReplayAndNextUnitUsesFallback(t *testing.T) {
	a, b := fakeTTS(1, errTTS), fakeTTS(0, nil)
	r := newTTSRig(t, core.TTSFailoverConfig{Candidates: []core.TTSCandidate{{Provider: "a", Proc: a}, {Provider: "b", Proc: b}}})
	r.unit(t, "t1", 1, false)
	r.unit(t, "t1", 2, true)
	r.waitFor(t, func() bool { ids, f, e := r.audio(); return f == 2 && e == 1 && len(ids) == 4 })
	time.Sleep(30 * time.Millisecond)
	ids, _, errs := r.audio()
	// unit 1: one audio frame + closing final frame from a; unit 2: two frames from b.
	if errs != 1 || len(ids) != 4 || ids[0] != 1 || ids[1] != 1 || ids[2] != 2 || ids[3] != 2 {
		t.Fatalf("ids=%v errs=%d", ids, errs)
	}
	if a.Units.Load() != 1 || b.Units.Load() != 1 {
		t.Fatalf("a=%d b=%d units", a.Units.Load(), b.Units.Load())
	}
	if got := r.switched(); len(got) != 1 || got[0] != "a>b:server_error" {
		t.Fatalf("switches %v", got)
	}
}

func TestTTSNewTurnReevaluatesFromThePrimary(t *testing.T) {
	a := core.NewFakeTTSProcessor(core.FakeTTSConfig{FramesPerUnit: 1, FailWith: errTTS})
	b := fakeTTS(0, nil)
	r := newTTSRig(t, core.TTSFailoverConfig{Candidates: []core.TTSCandidate{{Provider: "a", Proc: a}, {Provider: "b", Proc: b}}})
	r.unit(t, "t1", 1, false)
	r.unit(t, "t1", 2, true)
	r.waitFor(t, func() bool { _, f, _ := r.audio(); return f == 2 })
	if a.Units.Load() != 1 { // demoted for the rest of turn 1
		t.Fatalf("a tried %d units in turn 1", a.Units.Load())
	}
	r.unit(t, "t2", 1, true)
	r.waitFor(t, func() bool { _, f, _ := r.audio(); return f == 3 })
	if a.Units.Load() != 2 {
		t.Fatalf("primary not retried on the next turn: %d", a.Units.Load())
	}
}

func TestTTSAllCandidatesFailGivesOneError(t *testing.T) {
	r := newTTSRig(t, core.TTSFailoverConfig{Candidates: []core.TTSCandidate{
		{Provider: "a", Proc: fakeTTS(0, errTTS)}, {Provider: "b", Proc: fakeTTS(0, errTTS)}}})
	r.unit(t, "t1", 1, true)
	r.waitFor(t, func() bool { _, _, e := r.audio(); return e >= 1 })
	time.Sleep(40 * time.Millisecond)
	if ids, _, errs := r.audio(); len(ids) != 0 || errs != 1 {
		t.Fatalf("ids=%v errs=%d", ids, errs)
	}
}

func TestTTSDisallowedClassPassesThrough(t *testing.T) {
	b := fakeTTS(0, nil)
	r := newTTSRig(t, core.TTSFailoverConfig{
		Classify: func(error) string { return "client_error" }, Allow: func(c string) bool { return c != "client_error" },
		Candidates: []core.TTSCandidate{{Provider: "a", Proc: fakeTTS(0, errTTS)}, {Provider: "b", Proc: b}}})
	r.unit(t, "t1", 1, true)
	r.waitFor(t, func() bool { _, _, e := r.audio(); return e == 1 })
	if b.Units.Load() != 0 {
		t.Fatal("failed over on a disallowed class")
	}
}

func TestTTSOpenBreakerCandidateIsTriedLast(t *testing.T) {
	a, b := fakeTTS(0, nil), fakeTTS(0, nil)
	r := newTTSRig(t, core.TTSFailoverConfig{Candidates: []core.TTSCandidate{
		{Provider: "a", Proc: a, Open: func() bool { return true }}, {Provider: "b", Proc: b}}})
	r.unit(t, "t1", 1, true)
	r.waitFor(t, func() bool { _, f, _ := r.audio(); return f == 1 })
	if a.Units.Load() != 0 || b.Units.Load() != 1 {
		t.Fatalf("a=%d b=%d", a.Units.Load(), b.Units.Load())
	}
}

func TestTTSRejectsMixedSampleRates(t *testing.T) {
	_, err := core.NewTTSFailover(core.TTSFailoverConfig{Candidates: []core.TTSCandidate{
		{Provider: "a", Proc: fakeTTS(0, nil), Rate: 24000}, {Provider: "b", Proc: fakeTTS(0, nil), Rate: 16000}}})
	if err == nil {
		t.Fatal("mixed rates accepted")
	}
}

// lateTTS fails the unit, then (after the failover) re-enters audio for the
// same unit under the same and a newer generation.
type lateTTS struct {
	s   core.SessionController
	idx int
}

func (p *lateTTS) Attach(s core.SessionController, i int) error { p.s, p.idx = s, i; return nil }
func (p *lateTTS) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	su, ok := f.(*core.SynthesisUnitFrame)
	if !ok {
		return e.PushDirection(f, d)
	}
	return p.s.Go(func(context.Context) {
		_ = p.s.Reenter(&core.ErrorFrame{Err: errTTS, UnitID: su.UnitID}, core.Downstream, p.idx+1, su.Gen)
		time.Sleep(30 * time.Millisecond)
		for _, g := range []uint64{su.Gen, su.Gen + 1} {
			_ = p.s.Reenter(&core.OutputAudioFrame{Samples: []byte{1, 2}, UnitID: su.UnitID, Gen: g, Turn: su.Turn, IsFinal: true}, core.Downstream, p.idx+1, g)
		}
	})
}

func TestTTSAbandonedCandidateLateOutputIsDropped(t *testing.T) {
	r := newTTSRig(t, core.TTSFailoverConfig{Candidates: []core.TTSCandidate{{Provider: "a", Proc: &lateTTS{}}, {Provider: "b", Proc: fakeTTS(0, nil)}}})
	r.unit(t, "t1", 1, true)
	r.waitFor(t, func() bool { _, f, _ := r.audio(); return f == 1 })
	time.Sleep(100 * time.Millisecond)
	ids, finals, errs := r.audio()
	if len(ids) != 2 || finals != 1 || errs != 0 {
		t.Fatalf("ids=%v finals=%d errs=%d", ids, finals, errs)
	}
	for _, f := range r.rec.GetFrames() {
		if o, ok := f.(*core.OutputAudioFrame); ok && len(o.Samples) == 2 {
			t.Fatal("late frame of the abandoned candidate reached the pipeline")
		}
	}
}

func TestTTSOrderingAcrossSwitch(t *testing.T) {
	a, b := fakeTTS(0, errTTS), fakeTTS(0, nil)
	r := newTTSRig(t, core.TTSFailoverConfig{Candidates: []core.TTSCandidate{{Provider: "a", Proc: a}, {Provider: "b", Proc: b}}})
	for i := uint64(1); i <= 4; i++ {
		r.unit(t, "t1", i, i == 4)
	}
	r.waitFor(t, func() bool { _, f, _ := r.audio(); return f == 4 })
	ids, _, errs := r.audio()
	for i, id := range ids {
		if id != uint64(i/2+1) {
			t.Fatalf("out of order: %v", ids)
		}
	}
	if errs != 0 || len(ids) != 8 {
		t.Fatalf("ids=%v errs=%d", ids, errs)
	}
}

func TestTTSBargeInCancelsEveryCandidate(t *testing.T) {
	a := core.NewFakeTTSProcessor(core.FakeTTSConfig{FramesPerUnit: 50, FrameDelay: 10 * time.Millisecond})
	b := core.NewFakeTTSProcessor(core.FakeTTSConfig{FramesPerUnit: 50, FrameDelay: 10 * time.Millisecond})
	r := newTTSRig(t, core.TTSFailoverConfig{Candidates: []core.TTSCandidate{{Provider: "a", Proc: a}, {Provider: "b", Proc: b}}})
	r.unit(t, "t1", 1, false)
	r.unit(t, "t1", 2, true)
	r.waitFor(t, func() bool { ids, _, _ := r.audio(); return len(ids) > 2 })
	if err := r.sess.Post(&core.InterruptionFrame{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	n, _, _ := r.audio()
	time.Sleep(300 * time.Millisecond)
	if m, _, _ := r.audio(); len(m) != len(n) {
		t.Fatalf("audio after barge-in: %d -> %d", len(n), len(m))
	}
}

// hangTTS emits `frames` audio frames of a unit and then goes quiet forever
// (wedged socket); it records the cancellations it receives.
type hangTTS struct {
	s        core.SessionController
	idx      int
	frames   int
	canceled chan struct{}
}

func (p *hangTTS) Attach(s core.SessionController, i int) error { p.s, p.idx = s, i; return nil }
func (p *hangTTS) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	switch fr := f.(type) {
	case *core.SynthesisUnitFrame:
		for i := 0; i < p.frames; i++ {
			_ = p.s.Reenter(&core.OutputAudioFrame{Samples: make([]byte, 640), SampleRate: 24000, Channels: 1, UnitID: fr.UnitID, Gen: fr.Gen, Turn: fr.Turn}, core.Downstream, p.idx+1, fr.Gen)
		}
		return nil
	case *core.InterruptionFrame:
		select {
		case p.canceled <- struct{}{}:
		default:
		}
	}
	return e.PushDirection(f, d)
}

func TestTTSWatchdogFailsOverWedgedCandidate(t *testing.T) {
	a, b := &hangTTS{canceled: make(chan struct{}, 1)}, fakeTTS(0, nil)
	r := newTTSRig(t, core.TTSFailoverConfig{UnitTimeout: 50 * time.Millisecond,
		Candidates: []core.TTSCandidate{{Provider: "a", Proc: a}, {Provider: "b", Proc: b}}})
	r.unit(t, "t1", 1, false)
	r.unit(t, "t1", 2, true)
	r.waitFor(t, func() bool { _, f, _ := r.audio(); return f == 2 })
	if ids, _, errs := r.audio(); errs != 0 || len(ids) != 4 || b.Units.Load() != 2 {
		t.Fatalf("ids=%v errs=%d b=%d", ids, errs, b.Units.Load())
	}
	select {
	case <-a.canceled:
	default:
		t.Fatal("wedged candidate was not cancelled")
	}
	if got := r.switched(); len(got) != 1 || got[0] != "a>b:server_error" { // the rig classifies everything as server_error
		t.Fatalf("switches %v", got)
	}
}

func TestTTSWatchdogClosesOutCommittedUnit(t *testing.T) {
	a := &hangTTS{frames: 1, canceled: make(chan struct{}, 1)}
	b := fakeTTS(0, nil)
	r := newTTSRig(t, core.TTSFailoverConfig{UnitTimeout: 50 * time.Millisecond,
		Candidates: []core.TTSCandidate{{Provider: "a", Proc: a}, {Provider: "b", Proc: b}}})
	r.unit(t, "t1", 1, false)
	r.unit(t, "t1", 2, true)
	r.waitFor(t, func() bool { _, f, _ := r.audio(); return f == 2 })
	ids, _, errs := r.audio()
	// unit 1: a's frame + the synthesized closing final (no replay); unit 2 on b.
	if errs != 1 || len(ids) != 4 || ids[0] != 1 || ids[1] != 1 || ids[2] != 2 || b.Units.Load() != 1 {
		t.Fatalf("ids=%v errs=%d b=%d", ids, errs, b.Units.Load())
	}
}

// tailTTS finishes unit 1 with a final frame and only then its stopped frame,
// like a streaming provider; unit 2 fails to start.
type tailTTS struct {
	sess core.SessionController
	idx  int
}

func (p *tailTTS) Attach(s core.SessionController, i int) error { p.sess, p.idx = s, i; return nil }
func (p *tailTTS) ProcessFrame(ctx context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	su, ok := f.(*core.SynthesisUnitFrame)
	if !ok {
		return nil
	}
	if su.UnitID == 2 {
		return errTTS // fails synchronously
	}
	go func() {
		_ = p.sess.Reenter(&core.OutputAudioFrame{Encoding: core.EncodingPCM16LE, SampleRate: 24000, Channels: 1, Gen: su.Gen, Turn: su.Turn, UnitID: su.UnitID, IsFinal: true}, core.Downstream, p.idx+1, su.Gen)
		_ = p.sess.Reenter(&core.TTSStoppedFrame{Turn: su.Turn, Gen: su.Gen, UnitID: su.UnitID}, core.Downstream, p.idx+1, su.Gen)
	}()
	return nil
}

// Unit 2 failing synchronously (inside unit 1's final-frame re-entry) must not
// make the failover forget unit 1: its stopped frame still has to arrive.
func TestTTSNextUnitSyncFailureKeepsPreviousUnitTail(t *testing.T) {
	a := &tailTTS{}
	r := newTTSRig(t, core.TTSFailoverConfig{Candidates: []core.TTSCandidate{{Provider: "a", Proc: a}}})
	r.unit(t, "t1", 1, false)
	r.unit(t, "t1", 2, true)
	r.waitFor(t, func() bool {
		for _, f := range r.rec.GetFrames() {
			if s, ok := f.(*core.TTSStoppedFrame); ok && s.UnitID == 1 {
				return true
			}
		}
		return false
	})
}

// abandonTTS fails every unit before audio and keeps it, so the test can
// re-enter late output for it after another candidate finished the unit.
type abandonTTS struct {
	s   core.SessionController
	idx int
	su  chan *core.SynthesisUnitFrame
}

func (p *abandonTTS) Attach(s core.SessionController, i int) error { p.s, p.idx = s, i; return nil }
func (p *abandonTTS) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	su, ok := f.(*core.SynthesisUnitFrame)
	if !ok {
		return e.PushDirection(f, d)
	}
	p.su <- su
	return p.s.Go(func(context.Context) {
		_ = p.s.Reenter(&core.ErrorFrame{Err: errTTS, UnitID: su.UnitID}, core.Downstream, p.idx+1, su.Gen)
	})
}

// Ownership is per (candidate, unit): once b finished unit 1 (it sits in the
// finished list), a's late frames for the same unit id are still dropped.
func TestTTSAbandonedCandidateLateFrameForFinishedUnitIsDropped(t *testing.T) {
	a := &abandonTTS{su: make(chan *core.SynthesisUnitFrame, 1)}
	r := newTTSRig(t, core.TTSFailoverConfig{Candidates: []core.TTSCandidate{{Provider: "a", Proc: a}, {Provider: "b", Proc: fakeTTS(0, nil)}}})
	r.unit(t, "t1", 1, true)
	r.waitFor(t, func() bool { _, f, _ := r.audio(); return f == 1 })
	time.Sleep(30 * time.Millisecond)
	before := r.rec.Count()
	su := <-a.su
	_ = a.s.Reenter(&core.OutputAudioFrame{Samples: []byte{9}, Turn: su.Turn, Gen: su.Gen, UnitID: su.UnitID, IsFinal: true}, core.Downstream, a.idx+1, su.Gen)
	_ = a.s.Reenter(&core.TTSStoppedFrame{Turn: su.Turn, Gen: su.Gen, UnitID: su.UnitID}, core.Downstream, a.idx+1, su.Gen)
	_ = a.s.Reenter(&core.ErrorFrame{Err: errTTS, Turn: core.TurnID(su.Turn), Gen: core.Generation(su.Gen), UnitID: su.UnitID}, core.Downstream, a.idx+1, su.Gen)
	time.Sleep(50 * time.Millisecond)
	if n := r.rec.Count(); n != before {
		t.Fatalf("abandoned candidate's late frames reached the pipeline: %d -> %d", before, n)
	}
}

// slowTailTTS finishes a unit at once but re-enters its stopped frame late.
type slowTailTTS struct {
	s   core.SessionController
	idx int
}

func (p *slowTailTTS) Attach(s core.SessionController, i int) error { p.s, p.idx = s, i; return nil }
func (p *slowTailTTS) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	su, ok := f.(*core.SynthesisUnitFrame)
	if !ok {
		return e.PushDirection(f, d)
	}
	return p.s.Go(func(context.Context) {
		_ = p.s.Reenter(&core.OutputAudioFrame{Encoding: core.EncodingPCM16LE, SampleRate: 24000, Channels: 1, Samples: make([]byte, 640), Turn: su.Turn, Gen: su.Gen, UnitID: su.UnitID, IsFinal: true}, core.Downstream, p.idx+1, su.Gen)
		time.Sleep(200 * time.Millisecond)
		_ = p.s.Reenter(&core.TTSStoppedFrame{Turn: su.Turn, Gen: su.Gen, UnitID: su.UnitID}, core.Downstream, p.idx+1, su.Gen)
	})
}

// A wedged candidate's expiry drops only its own units' tails: unit 1's late
// stopped frame from the healthy candidate still arrives.
func TestTTSWatchdogKeepsOtherCandidatesTails(t *testing.T) {
	var aOpen atomic.Bool
	aOpen.Store(true) // unit 1 goes to b
	a := &hangTTS{frames: 1, canceled: make(chan struct{}, 1)}
	r := newTTSRig(t, core.TTSFailoverConfig{UnitTimeout: 30 * time.Millisecond, Candidates: []core.TTSCandidate{
		{Provider: "a", Proc: a, Open: aOpen.Load}, {Provider: "b", Proc: &slowTailTTS{}}}})
	r.unit(t, "t1", 1, false)
	r.waitFor(t, func() bool { _, f, _ := r.audio(); return f == 1 })
	aOpen.Store(false) // unit 2 goes to a, which wedges after one frame
	r.unit(t, "t1", 2, true)
	r.waitFor(t, func() bool { _, _, e := r.audio(); return e == 1 }) // expired
	r.waitFor(t, func() bool {
		for _, f := range r.rec.GetFrames() {
			if s, ok := f.(*core.TTSStoppedFrame); ok && s.UnitID == 1 {
				return true
			}
		}
		return false
	})
}
