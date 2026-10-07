package core

import "time"

// DTMF frame kinds. Numbered apart from the 8xx lifecycle block so concurrent
// additions there cannot collide.
const (
	FrameInputDTMF  FrameType = 840
	FrameOutputDTMF FrameType = 841
)

// InputDTMFFrame is one key press from the caller: detected in inbound audio
// or sent by a client as an RTVI dtmf message. Digit is one of 0-9 * # A-D.
type InputDTMFFrame struct {
	Digit     string
	Timestamp time.Time
	Gen       uint64
}

func (f *InputDTMFFrame) Type() FrameType    { return FrameInputDTMF }
func (f *InputDTMFFrame) Generation() uint64 { return f.Gen }

// OutputDTMFFrame asks the output stage to play Digits as DTMF tones toward
// the caller. Gen ties it to a turn generation: a barge-in drops it.
type OutputDTMFFrame struct {
	Digits string
	Gen    uint64
}

func (f *OutputDTMFFrame) Type() FrameType    { return FrameOutputDTMF }
func (f *OutputDTMFFrame) Generation() uint64 { return f.Gen }
