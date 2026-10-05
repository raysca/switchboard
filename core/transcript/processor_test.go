package transcript

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

type recordingEmitter struct{ frames []core.Frame }

func (e *recordingEmitter) Push(f core.Frame) error {
	e.frames = append(e.frames, f)
	return nil
}

func (e *recordingEmitter) PushDirection(f core.Frame, _ core.Direction) error {
	e.frames = append(e.frames, f)
	return nil
}

func TestProcessorSeparatesRawCandidatesFromCommittedFinals(t *testing.T) {
	cfg := testConfig()
	cfg.GracePeriod = 0
	p, err := NewProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	emitter := &recordingEmitter{}
	at := time.Unix(10, 0)
	err = p.ProcessFrame(context.Background(), &core.TranscriptCandidateFrame{
		Text:           " committed once ",
		Turn:           "A",
		Gen:            1,
		Timestamp:      at,
		Final:          true,
		EndOfUtterance: true,
		Provider:       "test",
	}, core.Downstream, emitter)
	if err != nil {
		t.Fatal(err)
	}
	if len(emitter.frames) != 1 {
		t.Fatalf("expected one output, got %d: %T", len(emitter.frames), emitter.frames)
	}
	final, ok := emitter.frames[0].(*core.FinalTranscriptionFrame)
	if !ok || !final.Committed || final.Text != "committed once" || final.Turn != "A" || final.Timestamp != at {
		t.Fatalf("raw candidate did not become one marked commit: %#v", emitter.frames[0])
	}

	// A committed output is ordinary downstream data. The processor must never
	// reinterpret it as a raw candidate and recursively commit it.
	if err := p.ProcessFrame(context.Background(), final, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	if len(emitter.frames) != 2 || emitter.frames[1] != final {
		t.Fatalf("committed output was not forwarded exactly once: %#v", emitter.frames)
	}
}

func TestProcessorConsumesTurnDecisionAndExplicitAdvance(t *testing.T) {
	p, _ := NewProcessor(testConfig())
	emitter := &recordingEmitter{}
	t0 := time.Unix(11, 0)
	if err := p.ProcessFrame(context.Background(), &core.TranscriptCandidateFrame{
		Text: "fallback", Turn: "A", Gen: 1, Timestamp: t0,
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessFrame(context.Background(), &core.TurnEventFrame{
		Kind: core.TurnEventCompleted, Turn: "A", Gen: 1, At: t0.Add(time.Millisecond),
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessFrame(context.Background(), &core.TranscriptAdvanceFrame{
		At: t0.Add(time.Second + time.Millisecond),
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	var commits int
	for _, frame := range emitter.frames {
		if final, ok := frame.(*core.FinalTranscriptionFrame); ok && final.Committed {
			commits++
		}
	}
	if commits != 1 {
		t.Fatalf("expected one timeout commit, got %d in %#v", commits, emitter.frames)
	}
}

func TestProcessorDropsInterruptedTurn(t *testing.T) {
	p, _ := NewProcessor(testConfig())
	emitter := &recordingEmitter{}
	t0 := time.Unix(12, 0)
	_ = p.ProcessFrame(context.Background(), &core.TranscriptCandidateFrame{
		Text: "discard me", Turn: "A", Gen: 1, Timestamp: t0,
	}, core.Downstream, emitter)
	if err := p.ProcessFrame(context.Background(), &core.TurnEventFrame{
		Kind: core.TurnEventInterrupted, Turn: "A", Gen: 1, At: t0.Add(time.Millisecond),
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	if err := p.ProcessFrame(context.Background(), &core.TranscriptAdvanceFrame{
		At: t0.Add(2 * time.Second),
	}, core.Downstream, emitter); err != nil {
		t.Fatal(err)
	}
	for _, frame := range emitter.frames {
		if final, ok := frame.(*core.FinalTranscriptionFrame); ok && final.Committed {
			t.Fatalf("interrupted turn committed: %#v", final)
		}
	}
}
