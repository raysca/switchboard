package core

import "time"

// ControlEnvelope carries typed correlation for future control adapters. Runtime
// settings application and queue priority remain outside this P0 contract.
type ControlEnvelope struct {
	Session    SessionID
	Turn       TurnID
	Generation Generation
	Frame      Frame
}

// RuntimeSettingsUpdate declares the revisioned P1 settings envelope. P0
// forwards it without validation or application.
type RuntimeSettingsUpdate struct {
	Revision            uint64
	InputMuted          *bool
	InterruptionEnabled *bool
	SilenceTimeout      *time.Duration
	TurnTimeout         *time.Duration
	Language            *string
}

func (*RuntimeSettingsUpdate) Type() FrameType { return FrameSettingsControl }

type InputMuteFrame struct{}

func (*InputMuteFrame) Type() FrameType { return FrameInputMute }

type InputUnmuteFrame struct{}

func (*InputUnmuteFrame) Type() FrameType { return FrameInputUnmute }

type InputMutedStateFrame struct{ Muted bool }

func (*InputMutedStateFrame) Type() FrameType { return FrameInputMutedState }

// OutputDisposition classifies media under an interruption. Zero-value media
// is normal assistant output and therefore interruptible.
type OutputDisposition uint8

const (
	OutputInterruptible OutputDisposition = iota
	OutputUninterruptible
)
