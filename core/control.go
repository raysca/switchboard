package core

type InputMuteFrame struct{}

func (*InputMuteFrame) Type() FrameType { return FrameInputMute }

type InputUnmuteFrame struct{}

func (*InputUnmuteFrame) Type() FrameType { return FrameInputUnmute }

type InputMutedStateFrame struct{ Muted bool }

func (*InputMutedStateFrame) Type() FrameType { return FrameInputMutedState }

// PushToTalkFrame reports the client's push-to-talk button. With the
// ExternalStart/ExternalStop strategies, pressed opens the next user turn and
// released ends it.
type PushToTalkFrame struct{ Pressed bool }

func (*PushToTalkFrame) Type() FrameType { return FramePushToTalk }

// SurvivesGenerationFence: the barge-in a press causes bumps the generation;
// the matching release must still reach the turn processor.
func (*PushToTalkFrame) SurvivesGenerationFence() {}

// OutputDisposition classifies media under an interruption. Zero-value media
// is normal assistant output and therefore interruptible.
type OutputDisposition uint8

const (
	OutputInterruptible OutputDisposition = iota
	OutputUninterruptible
)
