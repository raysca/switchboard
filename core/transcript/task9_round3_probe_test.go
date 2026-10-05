package transcript

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

func TestRound3ExpiredObserveCannotResurrectTurn(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(100, 0)
	a := candidate("A", 1, "first", t0)
	a.Final = true
	a.EndOfUtterance = true
	f.Observe(a)
	late := candidate("A", 1, "late", t0.Add(300*time.Millisecond))
	late.Final = true
	late.EndOfUtterance = true
	first, err := f.Observe(late)
	if err != nil || len(first) != 1 {
		t.Fatalf("first expiry: %v %v", first, err)
	}
	second, err := f.Advance(t0.Add(time.Second))
	if err != nil || len(second) != 0 {
		t.Fatalf("expired turn committed again: first=%v second=%v err=%v", first, second, err)
	}
}

func TestRound3ExpiredCompleteCannotResurrectTurn(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(200, 0)
	a := candidate("A", 1, "first", t0)
	a.Final, a.EndOfUtterance = true, true
	_, _ = f.Observe(a)
	first, err := f.Complete("A", 1, t0.Add(300*time.Millisecond))
	if err != nil || len(first) != 1 || first[0].Text != "first" {
		t.Fatalf("completion expiry mismatch: out=%v err=%v", first, err)
	}
	second, err := f.Advance(t0.Add(time.Second))
	if err != nil || len(second) != 0 {
		t.Fatalf("completion resurrected expired turn: %v %v", second, err)
	}
}

func TestRound3CorrelatedInvalidationDoesNotRetireUnrelatedTurn(t *testing.T) {
	cfg := testConfig()
	cfg.GracePeriod = 0
	clock := &manualPolicyClock{now: time.Unix(300, 0)}
	cfg.PolicyClock = clock
	p, _ := NewProcessor(cfg)
	emitter := &recordingEmitter{}
	send := func(frame core.Frame) {
		if err := p.ProcessFrame(context.Background(), frame, core.Downstream, emitter); err != nil {
			t.Fatal(err)
		}
	}
	send(&core.TranscriptCandidateFrame{Turn: "A", Gen: 1, Text: "partial A", Final: true, Timestamp: time.Unix(1, 0)})
	send(&core.TranscriptCandidateFrame{Turn: "B", Gen: 1, Text: "complete B", Final: true, Timestamp: time.Unix(2, 0)})
	send(&core.TranscriptInvalidationFrame{Turn: "A", Gen: 1, Err: core.ErrTranscriptInvalidated})
	send(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "A", Gen: 1})
	send(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "B", Gen: 1})
	var finals []*core.FinalTranscriptionFrame
	for _, frame := range emitter.frames {
		if final, ok := frame.(*core.FinalTranscriptionFrame); ok {
			finals = append(finals, final)
		}
	}
	if len(finals) != 1 || finals[0].Turn != "B" || finals[0].Text != "complete B" {
		t.Fatalf("correlated invalidation affected unrelated turn: %#v", finals)
	}
}
