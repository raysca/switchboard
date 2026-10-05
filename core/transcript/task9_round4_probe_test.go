package transcript

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"govoice/core"
)

func TestRound4CorrelatedObservableErrorPreservesOtherTurn(t *testing.T) {
	cfg := testConfig()
	cfg.GracePeriod = 0
	cfg.PolicyClock = &manualPolicyClock{now: time.Unix(300, 0)}
	p, _ := NewProcessor(cfg)
	emitter := &recordingEmitter{}
	send := func(frame core.Frame) {
		t.Helper()
		if err := p.ProcessFrame(context.Background(), frame, core.Downstream, emitter); err != nil {
			t.Fatal(err)
		}
	}
	send(&core.TranscriptCandidateFrame{Turn: "A", Gen: 1, Text: "partial A", Final: true, Timestamp: time.Unix(1, 0)})
	send(&core.TranscriptCandidateFrame{Turn: "B", Gen: 1, Text: "complete B", Final: true, Timestamp: time.Unix(2, 0)})
	send(&core.TranscriptInvalidationFrame{Turn: "A", Gen: 1, Err: core.ErrTranscriptInvalidated})
	send(&core.ErrorFrame{Err: fmt.Errorf("%w: capacity", core.ErrTranscriptInvalidated), Turn: "A", Gen: 1})
	send(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "A", Gen: 1})
	send(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "B", Gen: 1})
	var finals []*core.FinalTranscriptionFrame
	var observedError bool
	for _, frame := range emitter.frames {
		switch value := frame.(type) {
		case *core.ErrorFrame:
			observedError = errors.Is(value.Err, core.ErrTranscriptInvalidated)
		case *core.FinalTranscriptionFrame:
			finals = append(finals, value)
		}
	}
	if !observedError || len(finals) != 1 || finals[0].Turn != "B" || finals[0].Text != "complete B" {
		t.Fatalf("correlated error affected other turn: error=%v finals=%v", observedError, finals)
	}
}

func TestRound4FinalPrefixAllowsMutableTailCorrection(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	at := time.Unix(500, 0)
	for i, item := range []struct {
		text  string
		final bool
	}{
		{text: "hello", final: true},
		{text: "hello world"},
		{text: "hello earth"},
	} {
		if _, err := f.Observe(Candidate{Turn: "A", Generation: 1, Text: item.text, Final: item.final, At: at.Add(time.Duration(i) * time.Millisecond)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Complete("A", 1, at.Add(3*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	out, err := f.Advance(at.Add(time.Second))
	if err != nil || len(out) != 1 || out[0].Text != "hello earth" {
		t.Fatalf("mutable tail correction lost: %v %v", out, err)
	}
}

func TestRound4UtteranceFinalRejectsLaterInterimExtension(t *testing.T) {
	f, _ := NewFinalizer(testConfig())
	at := time.Unix(600, 0)
	if _, err := f.Observe(Candidate{Turn: "A", Generation: 1, Text: "hello", Final: true, EndOfUtterance: true, At: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Observe(Candidate{Turn: "A", Generation: 1, Text: "hello world", At: at.Add(time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	out, err := f.Advance(at.Add(time.Second))
	if err != nil || len(out) != 1 || out[0].Text != "hello" {
		t.Fatalf("interim replaced utterance final: %v %v", out, err)
	}
}
