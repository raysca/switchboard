package turn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"govoice/core"
)

var (
	ErrReplayEmptyLog     = errors.New("turn: replay log is empty")
	ErrReplayClosed       = errors.New("turn: replay engine is closed")
	ErrReplayFrameCorrupt = errors.New("turn: replayed frame entry is corrupt")
)

// RecordedTurnEntry captures a single frame event within a recorded turn.
type RecordedTurnEntry struct {
	OffsetMs  int64  `json:"offset_ms"`
	Direction string `json:"direction"`
	FrameType string `json:"frame_type"`
	Payload   string `json:"payload,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	Gen       uint64 `json:"gen,omitempty"`
}

// TurnRecording captures an entire conversation turn for replay and regression testing.
type TurnRecording struct {
	TurnID    string              `json:"turn_id"`
	Gen       uint64              `json:"gen"`
	StartTime time.Time           `json:"start_time"`
	Duration  time.Duration       `json:"duration"`
	Entries   []RecordedTurnEntry `json:"entries"`
}

// TurnRecorder records frames traversing the pipeline for a specific turn.
type TurnRecorder struct {
	mu        sync.Mutex
	recording *TurnRecording
	startTime time.Time
	active    bool
}

// NewTurnRecorder creates a new recorder for a turn.
func NewTurnRecorder(turnID string, gen uint64) *TurnRecorder {
	now := time.Now()
	return &TurnRecorder{
		startTime: now,
		recording: &TurnRecording{
			TurnID:    turnID,
			Gen:       gen,
			StartTime: now,
			Entries:   make([]RecordedTurnEntry, 0),
		},
		active: true,
	}
}

// Record captures an incoming frame and appends it to the turn log.
func (r *TurnRecorder) Record(f core.Frame, dir core.Direction) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.active || r.recording == nil {
		return
	}

	offset := time.Since(r.startTime).Milliseconds()
	dirStr := "downstream"
	if dir == core.Upstream {
		dirStr = "upstream"
	}

	payload := ""
	if str, ok := f.(fmt.Stringer); ok {
		payload = str.String()
	}

	entry := RecordedTurnEntry{
		OffsetMs:  offset,
		Direction: dirStr,
		FrameType: fmt.Sprintf("%T", f),
		Payload:   payload,
		TurnID:    r.recording.TurnID,
		Gen:       r.recording.Gen,
	}

	r.recording.Entries = append(r.recording.Entries, entry)
}

// Stop finalizes the recording and returns the immutable result.
func (r *TurnRecorder) Stop() *TurnRecording {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.active {
		return r.recording
	}

	r.active = false
	r.recording.Duration = time.Since(r.startTime)
	return r.recording
}

// TurnReplayer injects recorded events back into a pipeline with accurate or accelerated timing.
type TurnReplayer struct {
	recording *TurnRecording
	speed     float64
}

// NewTurnReplayer creates a replayer for a recorded turn.
// Speed controls the replay rate (e.g. 1.0 = real-time, 0 = as fast as possible).
func NewTurnReplayer(rec *TurnRecording, speed float64) (*TurnReplayer, error) {
	if rec == nil || len(rec.Entries) == 0 {
		return nil, ErrReplayEmptyLog
	}
	if speed < 0 {
		speed = 1.0
	}
	return &TurnReplayer{
		recording: rec,
		speed:     speed,
	}, nil
}

// Replay executes the replay loop, invoking onEntry for each recorded event.
func (p *TurnReplayer) Replay(ctx context.Context, onEntry func(entry RecordedTurnEntry) error) error {
	if len(p.recording.Entries) == 0 {
		return ErrReplayEmptyLog
	}

	var lastOffset int64 = 0
	for _, entry := range p.recording.Entries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if p.speed > 0 {
			diff := entry.OffsetMs - lastOffset
			if diff > 0 {
				sleepDur := time.Duration(float64(diff)/p.speed) * time.Millisecond
				timer := time.NewTimer(sleepDur)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			lastOffset = entry.OffsetMs
		}

		if err := onEntry(entry); err != nil {
			return err
		}
	}
	return nil
}

// SerializeJSON exports the turn recording to JSON.
func (rec *TurnRecording) SerializeJSON() ([]byte, error) {
	return json.Marshal(rec)
}

// DeserializeJSON parses a turn recording from JSON.
func DeserializeJSON(data []byte) (*TurnRecording, error) {
	var rec TurnRecording
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrReplayFrameCorrupt, err)
	}
	return &rec, nil
}
