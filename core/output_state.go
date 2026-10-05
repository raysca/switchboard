package core

import "sync"

// SpeakingState is the typed, provider-neutral assistant speaking state.
type SpeakingState uint8

const (
	SpeakingStateUnknown SpeakingState = iota
	SpeakingStarted
	SpeakingStopped
)

// SpeakingStopReason records why real output stopped. It intentionally does
// not claim that a remote client has drained or cleared its playback buffer.
type SpeakingStopReason string

const (
	SpeakingStopDrained    SpeakingStopReason = "drained"
	SpeakingStopFlushed    SpeakingStopReason = "flushed"
	SpeakingStopWriteError SpeakingStopReason = "write_error"
	SpeakingStopClosed     SpeakingStopReason = "closed"
)

// SpeakingTracker balances speaking transitions from successful non-empty
// output writes. It owns no callbacks, timers, transports, or provider state.
type SpeakingTracker struct {
	mu          sync.Mutex
	speaking    bool
	turn        string
	gen         uint64
	disposition OutputDisposition
}

func NewSpeakingTracker() *SpeakingTracker { return &SpeakingTracker{} }

// ObserveWrite records a successful, non-empty transport write. The first
// write starts speaking; later writes only refresh the active output metadata.
func (t *SpeakingTracker) ObserveWrite(turn string, gen uint64, disposition OutputDisposition) (SpeakingStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turn = turn
	t.gen = gen
	t.disposition = disposition
	if t.speaking {
		return SpeakingStateFrame{}, false
	}
	t.speaking = true
	return SpeakingStateFrame{State: SpeakingStarted, Turn: turn, Gen: gen}, true
}

// StopThrough stops only interruptible speaking output at or before targetGen.
// This fences late/overlapping flush completion from a newer generation.
func (t *SpeakingTracker) StopThrough(targetGen uint64, reason SpeakingStopReason) (SpeakingStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.speaking || t.disposition == OutputUninterruptible || t.gen > targetGen {
		return SpeakingStateFrame{}, false
	}
	return t.stopLocked(reason)
}

// Stop ends any active speaking state for final drain, fatal failure, or close.
func (t *SpeakingTracker) Stop(reason SpeakingStopReason) (SpeakingStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.speaking {
		return SpeakingStateFrame{}, false
	}
	return t.stopLocked(reason)
}

func (t *SpeakingTracker) stopLocked(reason SpeakingStopReason) (SpeakingStateFrame, bool) {
	event := SpeakingStateFrame{
		State: SpeakingStopped, Turn: t.turn, Gen: t.gen, Reason: reason,
	}
	t.speaking = false
	t.turn = ""
	t.gen = 0
	t.disposition = OutputInterruptible
	return event, true
}

func (t *SpeakingTracker) IsSpeaking() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.speaking
}
