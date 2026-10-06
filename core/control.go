package core

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
