package core

import (
	"errors"
	"time"
)

// ErrTranscriptInvalidated marks a provider assembly failure whose partial
// candidate must never be committed.
var ErrTranscriptInvalidated = errors.New("core: transcript candidate invalidated")

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
	FrameSynthesisUnit

	// TTS frames
	FrameTTSStarted FrameType = 500 + iota
	FrameTTSAudio
	FrameTTSStopped

	// Conversation context frames
	FrameUserMessage FrameType = 600 + iota
	FrameAssistantMessage
	FrameSystemMessage
	FrameToolCall
	FrameToolResult

	// Lifecycle and control frames
	FrameStart FrameType = 700 + iota
	FrameEnd
	FrameCancel
	FrameError
	FrameInterruption
	FrameFlush
	FrameFlushProcessed
	FrameInputMute
	FrameInputUnmute
	FrameInputMutedState
	FrameSettingsControl

	// Internal async re-entry
	FrameInternal               FrameType = 821
	FrameWriteCompletion        FrameType = 822
	FrameTurnEvent              FrameType = 823
	FrameSpeakingState          FrameType = 824
	FrameOutputTurnFinal        FrameType = 825
	FrameTranscriptCandidate    FrameType = 826
	FrameTranscriptAdvance      FrameType = 827
	FrameTranscriptInvalidation FrameType = 828
	FrameUsageMetrics           FrameType = 829
	FramePlaybackState          FrameType = 830
	FrameDTMF                   FrameType = 831
	FrameTone                   FrameType = 832
	FrameBackgroundAudioControl FrameType = 833
	FrameEarconControl          FrameType = 834
	FrameReadiness              FrameType = 835
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
	Samples     []byte
	Encoding    AudioEncoding
	SampleRate  int
	Channels    int
	Duration    time.Duration
	Timestamp   time.Time
	Seq         uint64
	Gen         uint64
	Turn        string
	UnitID      uint64
	IsFinal     bool // true if this is the final audio frame of the synthesis unit
	TurnFinal   bool // true only on the final output marker for the whole assistant turn
	Disposition OutputDisposition
}

func (f *OutputAudioFrame) Type() FrameType    { return FrameOutputAudio }
func (f *OutputAudioFrame) Generation() uint64 { return f.Gen }

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

// FinalTranscriptionFrame carries a finalized STT transcript.
type FinalTranscriptionFrame struct {
	Text      string
	UserID    string
	Timestamp time.Time
	Turn      string
	Gen       uint64
	// Committed is set only by the provider-neutral transcript finalizer.
	// Provider adapters emit TranscriptCandidateFrame instead.
	Committed bool
}

func (f *FinalTranscriptionFrame) Type() FrameType    { return FrameFinalTranscription }
func (f *FinalTranscriptionFrame) Generation() uint64 { return f.Gen }

// TranscriptCandidateFrame carries a raw provider hypothesis into the common
// transcript policy. Final means the provider will not revise this hypothesis;
// EndOfUtterance is the stronger speech boundary.
type TranscriptCandidateFrame struct {
	Text           string
	Timestamp      time.Time
	Turn           TurnID
	Gen            Generation
	Final          bool
	EndOfUtterance bool
	Stability      float64
	HasStability   bool
	Provider       string
}

func (*TranscriptCandidateFrame) Type() FrameType      { return FrameTranscriptCandidate }
func (f *TranscriptCandidateFrame) Generation() uint64 { return uint64(f.Gen) }

// TranscriptAdvanceFrame supplies policy time explicitly. It is internal to
// the transcript finalizer and is not a wall-clock timer notification.
type TranscriptAdvanceFrame struct{ At time.Time }

func (*TranscriptAdvanceFrame) Type() FrameType { return FrameTranscriptAdvance }

// TranscriptInvalidationFrame retires one affected raw transcript identity
// without committing its partial text. Providers emit a separate ErrorFrame
// when the underlying failure must also remain observable.
type TranscriptInvalidationFrame struct {
	Turn TurnID
	Gen  Generation
	Err  error
}

func (*TranscriptInvalidationFrame) Type() FrameType      { return FrameTranscriptInvalidation }
func (f *TranscriptInvalidationFrame) Generation() uint64 { return uint64(f.Gen) }

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

// LLMTextFrame carries the complete generated text response from the LLM.
type LLMTextFrame struct {
	Text string
	Turn string
	Gen  uint64
}

func (f *LLMTextFrame) Type() FrameType    { return FrameLLMText }
func (f *LLMTextFrame) Generation() uint64 { return f.Gen }

// SynthesisUnitFrame carries an explicit synthesis unit segmented from LLM text.
type SynthesisUnitFrame struct {
	Turn    string
	Gen     uint64
	UnitID  uint64
	Text    string
	IsFinal bool // true if this is the final synthesis unit for the turn
}

func (f *SynthesisUnitFrame) Type() FrameType    { return FrameSynthesisUnit }
func (f *SynthesisUnitFrame) Generation() uint64 { return f.Gen }

// OutputTurnFinalFrame marks an already-streamed synthesis unit as the final
// unit of the whole assistant turn. It carries no audio and is ordered through
// providers to the transport before queued async audio re-entry is delivered.
type OutputTurnFinalFrame struct {
	Turn   string
	Gen    uint64
	UnitID uint64
}

func (f *OutputTurnFinalFrame) Type() FrameType    { return FrameOutputTurnFinal }
func (f *OutputTurnFinalFrame) Generation() uint64 { return f.Gen }

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

// TTSAudioFrame carries synthesized audio from the TTS provider.
type TTSAudioFrame struct {
	Audio       []byte
	SampleRate  int
	Channels    int
	Duration    time.Duration
	Turn        string
	Gen         uint64
	UnitID      uint64
	IsFinal     bool
	Disposition OutputDisposition
}

func (f *TTSAudioFrame) Type() FrameType    { return FrameTTSAudio }
func (f *TTSAudioFrame) Generation() uint64 { return f.Gen }

// TTSStoppedFrame marks completion of synthesis for a turn/unit.
type TTSStoppedFrame struct {
	Turn   string
	Gen    uint64
	UnitID uint64
}

func (f *TTSStoppedFrame) Type() FrameType    { return FrameTTSStopped }
func (f *TTSStoppedFrame) Generation() uint64 { return f.Gen }

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

// SystemMessageFrame injects or replaces the provider-neutral system instruction.
type SystemMessageFrame struct {
	Content   string
	Timestamp time.Time
}

func (f *SystemMessageFrame) Type() FrameType { return FrameSystemMessage }

// ToolCallFrame and ToolResultFrame carry the public V2 tool contracts through
// the pipeline while retaining turn/generation correlation on results.
type ToolCallFrame struct {
	Call      ToolCall
	Timestamp time.Time
}

func (f *ToolCallFrame) Type() FrameType    { return FrameToolCall }
func (f *ToolCallFrame) Generation() uint64 { return uint64(f.Call.Generation) }

type ToolResultFrame struct {
	Turn      TurnID
	Gen       Generation
	Result    ToolResult
	Timestamp time.Time
}

func (f *ToolResultFrame) Type() FrameType    { return FrameToolResult }
func (f *ToolResultFrame) Generation() uint64 { return uint64(f.Gen) }

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
	// Turn and Gen scope a transcript invalidation error when set. Legacy
	// uncorrelated errors leave both zero.
	Turn TurnID
	Gen  Generation
}

func (f *ErrorFrame) Type() FrameType { return FrameError }

// InterruptionFrame signals user barge-in on an active assistant turn.
type InterruptionFrame struct {
	Turn      string
	Gen       uint64
	committed bool // Set only by the assistant after advancing the session fence.
}

func (f *InterruptionFrame) Type() FrameType    { return FrameInterruption }
func (f *InterruptionFrame) Generation() uint64 { return f.Gen }

// FlushFrame travels downstream to flush old-generation audio at the transport.
type FlushFrame struct {
	Gen       uint64
	committed bool // Set when a session admission or assistant barrier advanced the fence.
}

func (f *FlushFrame) Type() FrameType    { return FrameFlush }
func (f *FlushFrame) Generation() uint64 { return f.Gen }

// FlushProcessedFrame is a server-local transport barrier acknowledgement.
type FlushProcessedFrame struct {
	Gen uint64
}

func (f *FlushProcessedFrame) Type() FrameType    { return FrameFlushProcessed }
func (f *FlushProcessedFrame) Generation() uint64 { return f.Gen }

// WriteCompletionFrame reports asynchronous completion of an audio frame write.
type WriteCompletionFrame struct {
	Turn      string
	Gen       uint64
	UnitID    uint64
	Seq       uint64
	IsFinal   bool // true if this completed the final audio frame of the synthesis unit
	TurnFinal bool // true if this completed the final output marker for the whole assistant turn
	Err       error
}

func (f *WriteCompletionFrame) Type() FrameType    { return FrameWriteCompletion }
func (f *WriteCompletionFrame) Generation() uint64 { return f.Gen }

// SpeakingStateFrame is a provider-neutral transition derived from actual
// transport output. It is not a client playback acknowledgement.
type SpeakingStateFrame struct {
	State  SpeakingState
	Turn   string
	Gen    uint64
	Reason SpeakingStopReason
}

func (f *SpeakingStateFrame) Type() FrameType    { return FrameSpeakingState }
func (f *SpeakingStateFrame) Generation() uint64 { return f.Gen }
