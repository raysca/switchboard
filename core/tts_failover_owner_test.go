package core_test

import (
	"context"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

type ownerGateFrame struct{}

func (ownerGateFrame) Type() core.FrameType { return core.FrameInternal }

// ownerGateSink records frames like PassthroughProcessor and parks the run loop on a ownerGateFrame.
type ownerGateSink struct {
	coretest.PassthroughProcessor
	entered chan struct{}
	release chan struct{}
}

func (g *ownerGateSink) ProcessFrame(ctx context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	if _, ok := f.(ownerGateFrame); ok {
		close(g.entered)
		<-g.release
		return nil
	}
	return g.PassthroughProcessor.ProcessFrame(ctx, f, d, e)
}

// errThenLateTTS fails the unit twice and then re-enters a final audio frame, all once armed is closed.
type errThenLateTTS struct {
	s     core.SessionController
	idx   int
	armed chan struct{}
}

func (p *errThenLateTTS) Attach(s core.SessionController, i int) error { p.s, p.idx = s, i; return nil }
func (p *errThenLateTTS) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	su, ok := f.(*core.SynthesisUnitFrame)
	if !ok {
		return e.PushDirection(f, d)
	}
	return p.s.Go(func(context.Context) {
		<-p.armed
		for i := 0; i < 2; i++ {
			_ = p.s.Reenter(&core.ErrorFrame{Err: errTTS, UnitID: su.UnitID}, core.Downstream, p.idx+1, su.Gen)
		}
		_ = p.s.Reenter(&core.OutputAudioFrame{Samples: []byte{9, 9}, UnitID: su.UnitID, Gen: su.Gen, Turn: su.Turn, IsFinal: true}, core.Downstream, p.idx+1, su.Gen)
	})
}

// After a candidate's error moves the unit on, the failed candidate no longer owns it, even while the run
// loop has not yet started the next candidate: its further errors do not skip a candidate and its late
// frames neither reach the pipeline nor finish the unit.
func TestTTSFailedCandidateLosesTheUnitBeforeTheNextOneStarts(t *testing.T) {
	var switches []string
	a := &errThenLateTTS{armed: make(chan struct{})}
	w, err := core.NewTTSFailover(core.TTSFailoverConfig{
		Candidates: []core.TTSCandidate{{Provider: "a", Proc: a}, {Provider: "b", Proc: fakeTTS(0, nil)}, {Provider: "c", Proc: fakeTTS(0, errTTS)}},
		Classify:   func(error) string { return "server_error" },
		OnFailover: func(from, to, class string) { switches = append(switches, from+">"+to) },
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &ownerGateSink{entered: make(chan struct{}), release: make(chan struct{})}
	pipe, err := core.NewPipeline(w, sink)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(902, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: sess.CurrentGeneration(), UnitID: 1, Text: "hi", IsFinal: true}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Post(ownerGateFrame{}); err != nil { // parks the run loop: the retry waits behind a's frames
		t.Fatal(err)
	}
	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("run loop never parked")
	}
	close(a.armed)
	time.Sleep(100 * time.Millisecond) // a's task re-enters its two errors and the late frame
	close(sink.release)
	for end := time.Now().Add(2 * time.Second); sink.Count() < 2 && time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
	}
	time.Sleep(100 * time.Millisecond)
	for _, f := range sink.GetFrames() {
		if o, ok := f.(*core.OutputAudioFrame); ok && len(o.Samples) == 2 && o.Samples[0] == 9 {
			t.Fatal("late frame of the failed candidate reached the pipeline")
		}
	}
	if len(switches) != 1 || switches[0] != "a>b" {
		t.Fatalf("switches = %v, want [a>b]: a second error skipped a candidate", switches)
	}
}
