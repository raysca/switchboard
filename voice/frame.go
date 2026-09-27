package voice

import (
	"context"
	"time"
)

type FrameType uint8

const (
	FrameAudio FrameType = iota
	FrameSpeechStarted
	FrameSpeechStopped
	FrameText
	FrameControl
	FrameError
	FrameInternal // async result re-entering the pipeline (e.g. VAD inference result)
)

type Frame interface{ Type() FrameType }

type Direction uint8

const (
	Downstream Direction = iota
	Upstream
)

// AudioFrame is 16 kHz mono PCM. Samples may alias shared read-only buffers: never mutate.
type AudioFrame struct {
	Samples []float32
	Seq     uint64
	Due     time.Time // wall time the last sample of this chunk was due to arrive
	Output  bool      // true for TTS output audio
}

type SpeechStartedFrame struct {
	Seq       uint64    // chunk that triggered detection
	Due       time.Time // due time of that chunk
	EmittedAt time.Time
}

type SpeechStoppedFrame struct {
	Seq       uint64
	EmittedAt time.Time
}

type TextFrame struct {
	Text string
	At   time.Time // origin time for turn-latency measurement
}

type ControlKind uint8

const (
	ControlStart ControlKind = iota
	ControlEnd
)

type ControlFrame struct{ Kind ControlKind }

type ErrorFrame struct{ Err error }

func (*AudioFrame) Type() FrameType         { return FrameAudio }
func (*SpeechStartedFrame) Type() FrameType { return FrameSpeechStarted }
func (*SpeechStoppedFrame) Type() FrameType { return FrameSpeechStopped }
func (*TextFrame) Type() FrameType          { return FrameText }
func (*ControlFrame) Type() FrameType       { return FrameControl }
func (*ErrorFrame) Type() FrameType         { return FrameError }

// Processor is the Pipecat-style process_frame(). It runs on the session's run
// loop, never concurrently with itself, and emits via e (push_frame()).
type Processor interface {
	ProcessFrame(ctx context.Context, f Frame, dir Direction, e Emitter) error
}

type Emitter interface {
	Push(f Frame) error
	PushDirection(f Frame, dir Direction) error
}

// Attacher is optional: called once at session start with the processor's index,
// giving access to managed concurrency (s.Go) and async re-entry (s.Post).
type Attacher interface {
	Attach(s *Session, index int) error
}

// Closer is optional: called at teardown after the run loop has exited.
type Closer interface{ Close() error }
