package transcript

import (
	"context"
	"fmt"

	"govoice/core"
)

// Processor adapts typed pipeline frames to Finalizer operations. It consumes
// raw candidates/advance frames and emits only stable interims or marked
// committed finals; already-committed frames are forwarded unchanged.
type Processor struct {
	finalizer  *Finalizer
	legacyTurn core.TurnID
	legacyGen  core.Generation
}

func NewProcessor(cfg Config) (*Processor, error) {
	finalizer, err := NewFinalizer(cfg)
	if err != nil {
		return nil, err
	}
	return &Processor{finalizer: finalizer}, nil
}

func (p *Processor) ProcessFrame(_ context.Context, frame core.Frame, dir core.Direction, emitter core.Emitter) error {
	if dir != core.Downstream {
		return emitter.PushDirection(frame, dir)
	}
	var (
		outputs []Output
		err     error
		forward = true
	)
	switch value := frame.(type) {
	case *core.TranscriptCandidateFrame:
		forward = false
		p.legacyTurn, p.legacyGen = value.Turn, value.Gen
		outputs, err = p.finalizer.Observe(Candidate{
			Turn: value.Turn, Generation: value.Gen, Text: value.Text, At: value.Timestamp,
			Final: value.Final, EndOfUtterance: value.EndOfUtterance,
			Stability: value.Stability, HasStability: value.HasStability,
		})
	case *core.TranscriptAdvanceFrame:
		forward = false
		outputs, err = p.finalizer.Advance(value.At)
	case *core.TurnEventFrame:
		switch value.Kind {
		case core.TurnEventStarted:
			p.legacyTurn, p.legacyGen = value.Turn, value.Gen
		case core.TurnEventCompleted:
			outputs, err = p.finalizer.Complete(value.Turn, value.Gen, value.At)
		case core.TurnEventInterrupted:
			err = p.finalizer.Interrupt(value.Turn, value.Gen, value.At)
		}
	case *core.UserStartedSpeakingFrame:
		// Explicit V1 adapter: legacy speech frames predate typed TurnID, and
		// both shipped STT providers historically derive this exact identity.
		p.legacyTurn = core.TurnID(fmt.Sprintf("turn-%d-%d", value.Gen, value.Seq))
		p.legacyGen = core.Generation(value.Gen)
	case *core.UserStoppedSpeakingFrame:
		if p.legacyTurn != "" && p.legacyGen == core.Generation(value.Gen) && !value.Timestamp.IsZero() {
			outputs, err = p.finalizer.Complete(p.legacyTurn, p.legacyGen, value.Timestamp)
		}
	}
	if err != nil {
		return err
	}
	if forward {
		if err := emitter.PushDirection(frame, dir); err != nil {
			return err
		}
	}
	for _, output := range outputs {
		if output.Kind == Interim {
			if err := emitter.Push(&core.InterimTranscriptionFrame{
				Text: output.Text, Turn: string(output.Turn), Gen: uint64(output.Generation), Timestamp: output.At,
			}); err != nil {
				return err
			}
			continue
		}
		if err := emitter.Push(&core.FinalTranscriptionFrame{
			Text: output.Text, Turn: string(output.Turn), Gen: uint64(output.Generation), Timestamp: output.At, Committed: true,
		}); err != nil {
			return err
		}
	}
	return nil
}
