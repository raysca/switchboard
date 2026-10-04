package core

import "time"

// TurnEventKind identifies a deterministic user-turn state transition.
type TurnEventKind uint8

const (
	TurnEventStarted TurnEventKind = iota + 1
	TurnEventStopped
	TurnEventCompleted
	TurnEventInterrupted
	TurnEventNoSpeechTimeout
)

// TurnReason explains why a turn manager emitted a terminal event.
type TurnReason uint8

const (
	TurnReasonNone TurnReason = iota
	TurnReasonSilence
	TurnReasonSemantic
	TurnReasonMaxDuration
	TurnReasonInitialNoSpeech
	TurnReasonInterrupted
)

// TurnEventFrame carries a provider-neutral, timestamped turn decision.
// It is an ordinary framework frame and has no implicit session side effect.
type TurnEventFrame struct {
	Kind          TurnEventKind
	Turn          TurnID
	Gen           Generation
	At            time.Time
	Reason        TurnReason
	Confidence    float64
	HasConfidence bool
}

func (*TurnEventFrame) Type() FrameType      { return FrameTurnEvent }
func (f *TurnEventFrame) Generation() uint64 { return uint64(f.Gen) }

var _ GenerationalFrame = (*TurnEventFrame)(nil)
