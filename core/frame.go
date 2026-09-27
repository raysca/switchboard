package core

import (
	"time"
)

// FrameType identifies the concrete category of a framework frame.
type FrameType uint16

const (
	// Audio frames
	FrameInputAudio FrameType = 100 + iota
	FrameOutputAudio

	// Speech-state frames
	FrameUserStartedSpeaking FrameType = 200 + iota
	FrameUserStoppedSpeaking

	// Transcription frames
	FrameInterimTranscription FrameType = 300 + iota
	FrameFinalTranscription

	// LLM and text frames
	FrameLLMTextDelta FrameType = 400 + iota
	FrameLLMText

	// TTS frames
	FrameTTSStarted FrameType = 500 + iota
	FrameTTSAudio
	FrameTTSStopped

	// Conversation context frames
	FrameUserMessage FrameType = 600 + iota
	FrameAssistantMessage

	// Lifecycle and control frames
	FrameStart FrameType = 700 + iota
	FrameEnd
	FrameCancel
	FrameError
	FrameInterruption
	FrameFlush
	FrameFlushProcessed

	// Internal async re-entry
	FrameInternal FrameType = 800 + iota
)

// Frame is the common interface implemented by all frames in the pipeline.
type Frame interface {
	Type() FrameType
}

// GenerationalFrame is implemented by frames associated with a turn generation.
type GenerationalFrame interface {
	Frame
	Generation() uint64
}

// TurnFrame is implemented by frames associated with a conversation turn.
type TurnFrame interface {
	GenerationalFrame
	TurnID() string
}

// Direction represents the flow direction of a frame within the pipeline.
type Direction uint8

const (
	Downstream Direction = iota
	Upstream
)

func (d Direction) String() string {
	if d == Downstream {
		return "downstream"
	}
	return "upstream"
}

// -------------------------------------------------------------------------
// Audio Frames
// -------------------------------------------------------------------------

// AudioEncoding specifies the raw sample format.
type AudioEncoding string

const (
	EncodingPCM16LE AudioEncoding = "PCM16LE"
)

// InputAudioFrame carries raw audio received from the client.
type InputAudioFrame struct {
	Samples    []byte
	Encoding   AudioEncoding
	SampleRate int
	Channels   int
	Duration   time.Duration
	Timestamp  time.Time
	Seq        uint64
	Gen        uint64
}

func (f *InputAudioFrame) Type() FrameType    { return FrameInputAudio }
func (f *InputAudioFrame) Generation() uint64 { return f.Gen }

// OutputAudioFrame carries raw audio heading toward the client.
type OutputAudioFrame struct {
	Samples    []byte
	Encoding   AudioEncoding
	SampleRate int
	Channels   int
	Duration   time.Duration
	Timestamp  time.Time
	Seq        uint64
	Gen        uint64
	Turn       string
	UnitID     uint64
	IsFinal    bool // true if this is the final audio frame of the synthesis unit
}

func (f *OutputAudioFrame) Type() FrameType    { return FrameOutputAudio }
func (f *OutputAudioFrame) Generation() uint64 { return f.Gen }
func (f *OutputAudioFrame) TurnID() string     { return f.Turn }

// -------------------------------------------------------------------------
// Speech-State Frames
// -------------------------------------------------------------------------

// UserStartedSpeakingFrame indicates that VAD detected start of user speech.
type UserStartedSpeakingFrame struct {
	Seq       uint64
	Timestamp time.Time
	Gen       uint64
}

func (f *UserStartedSpeakingFrame) Type() FrameType    { return FrameUserStartedSpeaking }
func (f *UserStartedSpeakingFrame) Generation() uint64 { return f.Gen }

// UserStoppedSpeakingFrame indicates that VAD detected end of user speech.
type UserStoppedSpeakingFrame struct {
	Seq       uint64
	Timestamp time.Time
	Gen       uint64
}

func (f *UserStoppedSpeakingFrame) Type() FrameType    { return FrameUserStoppedSpeaking }
func (f *UserStoppedSpeakingFrame) Generation() uint64 { return f.Gen }

// -------------------------------------------------------------------------
// Transcription Frames
// -------------------------------------------------------------------------

// InterimTranscriptionFrame carries an intermediate STT result.
type InterimTranscriptionFrame struct {
	Text      string
	UserID    string
	Timestamp time.Time
	Turn      string
	Gen       uint64
}

func (f *InterimTranscriptionFrame) Type() FrameType    { return FrameInterimTranscription }
func (f *InterimTranscriptionFrame) Generation() uint64 { return f.Gen }
func (f *InterimTranscriptionFrame) TurnID() string     { return f.Turn }

// FinalTranscriptionFrame carries a finalized STT transcript.
type FinalTranscriptionFrame struct {
	Text      string
	UserID    string
	Timestamp time.Time
	Turn      string
	Gen       uint64
}

func (f *FinalTranscriptionFrame) Type() FrameType    { return FrameFinalTranscription }
func (f *FinalTranscriptionFrame) Generation() uint64 { return f.Gen }
func (f *FinalTranscriptionFrame) TurnID() string     { return f.Turn }

// -------------------------------------------------------------------------
// LLM and Text Frames
// -------------------------------------------------------------------------

// LLMTextDeltaFrame carries a streamed text chunk from the LLM.
type LLMTextDeltaFrame struct {
	Delta string
	Turn  string
	Gen   uint64
}

func (f *LLMTextDeltaFrame) Type() FrameType    { return FrameLLMTextDelta }
func (f *LLMTextDeltaFrame) Generation() uint64 { return f.Gen }
func (f *LLMTextDeltaFrame) TurnID() string     { return f.Turn }

// LLMTextFrame carries the complete generated text response from the LLM.
type LLMTextFrame struct {
	Text string
	Turn string
	Gen  uint64
}

func (f *LLMTextFrame) Type() FrameType    { return FrameLLMText }
func (f *LLMTextFrame) Generation() uint64 { return f.Gen }
func (f *LLMTextFrame) TurnID() string     { return f.Turn }

// -------------------------------------------------------------------------
// TTS Frames
// -------------------------------------------------------------------------

// TTSStartedFrame marks the beginning of synthesis for a turn/unit.
type TTSStartedFrame struct {
	Turn   string
	Gen    uint64
	UnitID uint64
}

func (f *TTSStartedFrame) Type() FrameType    { return FrameTTSStarted }
func (f *TTSStartedFrame) Generation() uint64 { return f.Gen }
func (f *TTSStartedFrame) TurnID() string     { return f.Turn }

// TTSAudioFrame carries synthesized audio from the TTS provider.
type TTSAudioFrame struct {
	Audio      []byte
	SampleRate int
	Channels   int
	Duration   time.Duration
	Turn       string
	Gen        uint64
	UnitID     uint64
	IsFinal    bool
}

func (f *TTSAudioFrame) Type() FrameType    { return FrameTTSAudio }
func (f *TTSAudioFrame) Generation() uint64 { return f.Gen }
func (f *TTSAudioFrame) TurnID() string     { return f.Turn }

// TTSStoppedFrame marks completion of synthesis for a turn/unit.
type TTSStoppedFrame struct {
	Turn   string
	Gen    uint64
	UnitID uint64
}

func (f *TTSStoppedFrame) Type() FrameType    { return FrameTTSStopped }
func (f *TTSStoppedFrame) Generation() uint64 { return f.Gen }
func (f *TTSStoppedFrame) TurnID() string     { return f.Turn }

// -------------------------------------------------------------------------
// Conversation Context Frames
// -------------------------------------------------------------------------

// UserMessageFrame represents a committed user conversation turn.
type UserMessageFrame struct {
	Turn      string
	Content   string
	Gen       uint64
	Timestamp time.Time
}

func (f *UserMessageFrame) Type() FrameType    { return FrameUserMessage }
func (f *UserMessageFrame) Generation() uint64 { return f.Gen }
func (f *UserMessageFrame) TurnID() string     { return f.Turn }

// AssistantMessageFrame represents a committed assistant conversation turn.
type AssistantMessageFrame struct {
	Turn      string
	Content   string
	Gen       uint64
	Partial   bool // true if synthesis/delivery was interrupted
	Timestamp time.Time
}

func (f *AssistantMessageFrame) Type() FrameType    { return FrameAssistantMessage }
func (f *AssistantMessageFrame) Generation() uint64 { return f.Gen }
func (f *AssistantMessageFrame) TurnID() string     { return f.Turn }

// -------------------------------------------------------------------------
// Lifecycle and Control Frames
// -------------------------------------------------------------------------

// StartFrame signals pipeline initialization.
type StartFrame struct{}

func (f *StartFrame) Type() FrameType { return FrameStart }

// EndFrame signals orderly session termination.
type EndFrame struct{}

func (f *EndFrame) Type() FrameType { return FrameEnd }

// CancelFrame signals immediate cancellation of the active operation.
type CancelFrame struct {
	Reason string
	Gen    uint64
}

func (f *CancelFrame) Type() FrameType    { return FrameCancel }
func (f *CancelFrame) Generation() uint64 { return f.Gen }

// ErrorFrame carries an unrecoverable or informational error.
type ErrorFrame struct {
	Err   error
	Fatal bool
}

func (f *ErrorFrame) Type() FrameType { return FrameError }

// InterruptionFrame signals user barge-in on an active assistant turn.
type InterruptionFrame struct {
	Turn string
	Gen  uint64
}

func (f *InterruptionFrame) Type() FrameType    { return FrameInterruption }
func (f *InterruptionFrame) Generation() uint64 { return f.Gen }
func (f *InterruptionFrame) TurnID() string     { return f.Turn }

// FlushFrame travels downstream to flush old-generation audio at the transport.
type FlushFrame struct {
	Gen uint64
}

func (f *FlushFrame) Type() FrameType    { return FrameFlush }
func (f *FlushFrame) Generation() uint64 { return f.Gen }

// FlushProcessedFrame is a server-local transport barrier acknowledgement.
type FlushProcessedFrame struct {
	Gen uint64
}

func (f *FlushProcessedFrame) Type() FrameType    { return FrameFlushProcessed }
func (f *FlushProcessedFrame) Generation() uint64 { return f.Gen }
