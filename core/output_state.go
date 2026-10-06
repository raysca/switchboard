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
	unitID      uint64
	disposition OutputDisposition
}

func NewSpeakingTracker() *SpeakingTracker { return &SpeakingTracker{} }

// ObserveWrite records a successful, non-empty transport write. The first
// write starts speaking; later writes only refresh the active output metadata.
func (t *SpeakingTracker) ObserveWrite(turn string, gen uint64, disposition OutputDisposition, unitID ...uint64) (SpeakingStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turn = turn
	t.gen = gen
	if len(unitID) > 0 {
		t.unitID = unitID[0]
	}
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

// StopOutput stops only the exact output identity. It is used by late
// whole-turn final markers without allowing them to stop newer output.
func (t *SpeakingTracker) StopOutput(turn string, gen uint64, reason SpeakingStopReason, unitID ...uint64) (SpeakingStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.speaking || t.turn != turn || t.gen != gen || (len(unitID) > 0 && t.unitID != unitID[0]) {
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
	t.unitID = 0
	t.disposition = OutputInterruptible
	return event, true
}

func (t *SpeakingTracker) IsSpeaking() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.speaking
}

// PlaybackState represents the fine-grained lifecycle state of bot audio output.
type PlaybackState string

const (
	PlaybackGenerating  PlaybackState = "generating"  // LLM / TTS generating speech content
	PlaybackBuffered    PlaybackState = "buffered"    // Pacer has received/buffered audio frames
	PlaybackPlaying     PlaybackState = "playing"     // First audio chunk dispatched to transport
	PlaybackDrained     PlaybackState = "drained"     // Pacer queue completely drained to client
	PlaybackInterrupted PlaybackState = "interrupted" // Output was cancelled or interrupted mid-playback
	PlaybackCompleted   PlaybackState = "completed"   // Turn output cleanly finished
)

// PlaybackStateFrame is an observable event representing bot output state transitions.
type PlaybackStateFrame struct {
	State  PlaybackState `json:"state"`
	Turn   string        `json:"turn"`
	Gen    uint64        `json:"gen"`
	UnitID uint64        `json:"unit_id,omitempty"`
}

func (f *PlaybackStateFrame) Type() FrameType    { return FramePlaybackState }
func (f *PlaybackStateFrame) Generation() uint64 { return f.Gen }

// PlaybackTracker tracks ordered lifecycle transitions of bot output across a turn.
// State sequence: generating -> buffered -> playing -> (drained | interrupted) -> completed.
type PlaybackTracker struct {
	mu          sync.Mutex
	turn        string
	gen         uint64
	state       PlaybackState
	hasBuffered bool
	hasPlayed   bool
}

func NewPlaybackTracker() *PlaybackTracker {
	return &PlaybackTracker{}
}

// ObserveGenerating transitions to PlaybackGenerating for the turn/generation.
func (t *PlaybackTracker) ObserveGenerating(turn string, gen uint64) (PlaybackStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen > gen {
		return PlaybackStateFrame{}, false
	}
	t.turn = turn
	t.gen = gen
	t.state = PlaybackGenerating
	t.hasBuffered = false
	t.hasPlayed = false
	return PlaybackStateFrame{State: PlaybackGenerating, Turn: turn, Gen: gen}, true
}

// ObserveBuffered records that audio has been buffered for the turn.
func (t *PlaybackTracker) ObserveBuffered(turn string, gen uint64, unitID uint64) (PlaybackStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn != turn || t.gen != gen || t.state == PlaybackInterrupted || t.state == PlaybackCompleted {
		return PlaybackStateFrame{}, false
	}
	if t.hasBuffered {
		return PlaybackStateFrame{}, false
	}
	t.hasBuffered = true
	t.state = PlaybackBuffered
	return PlaybackStateFrame{State: PlaybackBuffered, Turn: turn, Gen: gen, UnitID: unitID}, true
}

// ObservePlaying records that playback has begun (first audio dispatched).
func (t *PlaybackTracker) ObservePlaying(turn string, gen uint64, unitID uint64) (PlaybackStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn != turn || t.gen != gen || t.state == PlaybackInterrupted || t.state == PlaybackCompleted {
		return PlaybackStateFrame{}, false
	}
	if t.hasPlayed {
		return PlaybackStateFrame{}, false
	}
	t.hasPlayed = true
	t.state = PlaybackPlaying
	return PlaybackStateFrame{State: PlaybackPlaying, Turn: turn, Gen: gen, UnitID: unitID}, true
}

// ObserveDrained records that the pacer queue has finished draining.
func (t *PlaybackTracker) ObserveDrained(turn string, gen uint64) (PlaybackStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn != turn || t.gen != gen || t.state == PlaybackInterrupted || t.state == PlaybackCompleted {
		return PlaybackStateFrame{}, false
	}
	t.state = PlaybackDrained
	return PlaybackStateFrame{State: PlaybackDrained, Turn: turn, Gen: gen}, true
}

// ObserveInterrupted records that the turn was interrupted before or during playback.
func (t *PlaybackTracker) ObserveInterrupted(turn string, gen uint64) (PlaybackStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gen > gen || t.state == PlaybackCompleted || t.state == PlaybackInterrupted {
		return PlaybackStateFrame{}, false
	}
	t.turn = turn
	t.gen = gen
	t.state = PlaybackInterrupted
	return PlaybackStateFrame{State: PlaybackInterrupted, Turn: turn, Gen: gen}, true
}

// ObserveCompleted records final turn output completion.
func (t *PlaybackTracker) ObserveCompleted(turn string, gen uint64) (PlaybackStateFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turn != turn || t.gen != gen || t.state == PlaybackCompleted {
		return PlaybackStateFrame{}, false
	}
	t.state = PlaybackCompleted
	return PlaybackStateFrame{State: PlaybackCompleted, Turn: turn, Gen: gen}, true
}

// CurrentState returns the active playback state.
func (t *PlaybackTracker) CurrentState() PlaybackState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

