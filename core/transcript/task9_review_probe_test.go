package transcript

import (
	"context"
	"errors"
	"testing"
	"time"

	"govoice/core"
)

func TestReviewFinalizerRejectedHigherGenerationPreservesState(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	t0 := time.Unix(100, 0)
	a := candidate("A", 1, "keep me", t0)
	a.Final = true
	f.Observe(a)
	f.Complete("A", 1, t0)
	b := candidate("B", 2, "invalid", t0.Add(-time.Second))
	if _, err := f.Observe(b); !errors.Is(err, ErrTimestampRegression) {
		t.Fatalf("err %v", err)
	}
	out, err := f.Advance(t0.Add(time.Second))
	if err != nil || len(out) != 1 || out[0].Text != "keep me" {
		t.Fatalf("rejected generation update destroyed A: out=%v err=%v", out, err)
	}
}

func TestReviewProcessorLateRetiredCandidateCannotStealStop(t *testing.T) {
	cfg := testConfig()
	cfg.GracePeriod = 0
	clock := &manualPolicyClock{now: time.Unix(100, 0)}
	cfg.PolicyClock = clock
	p, _ := NewProcessor(cfg)
	e := &recordingEmitter{}
	send := func(f core.Frame) {
		if err := p.ProcessFrame(context.Background(), f, core.Downstream, e); err != nil {
			t.Fatal(err)
		}
	}
	send(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	send(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "A", Final: true, EndOfUtterance: true})
	send(&core.UserStartedSpeakingFrame{Seq: 2, Gen: 1})
	send(&core.TranscriptCandidateFrame{Turn: "turn-1-2", Gen: 1, Text: "B", Final: true})
	send(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "late A", Final: true})
	send(&core.UserStoppedSpeakingFrame{Seq: 3, Gen: 1})
	clock.Set(time.Unix(110, 0))
	send(&core.TranscriptAdvanceFrame{})
	for _, fr := range e.frames {
		if f, ok := fr.(*core.FinalTranscriptionFrame); ok && f.Text == "B" {
			return
		}
	}
	t.Fatal("late retired candidate stole B's stop; B never committed")
}
