package core

import (
	"fmt"
	"time"
)

// ProcessorSnapshot captures read-only diagnostic information about a processor in the pipeline.
type ProcessorSnapshot struct {
	Index       int    `json:"index"`
	Type        string `json:"type"`
	Disabled    bool   `json:"disabled"`
	Description string `json:"description,omitempty"`
}

// SessionSnapshot provides an immutable, read-only diagnostic view of a session's current runtime state.
type SessionSnapshot struct {
	SessionID      uint64              `json:"session_id"`
	State          string              `json:"state"`
	Generation     uint64              `json:"generation"`
	InputMuted     bool                `json:"input_muted"`
	CreatedAt      time.Time           `json:"created_at"`
	Uptime         time.Duration       `json:"uptime"`
	FramesIn       uint64              `json:"frames_in"`
	FramesOut      uint64              `json:"frames_out"`
	StaleDiscarded uint64              `json:"stale_discarded"`
	AudioDropped   uint64              `json:"audio_dropped"`
	ActiveTasks    int                 `json:"active_tasks"`
	LeakedTasks    int                 `json:"leaked_tasks"`
	Settings       RuntimeSettings     `json:"settings"`
	Processors     []ProcessorSnapshot `json:"processors"`
	ObserverCount  int                 `json:"observer_count"`
}

// Snapshot returns an immutable point-in-time diagnostic inspection of the session runtime and pipeline.
func (s *Session) Snapshot() SessionSnapshot {
	s.settingsMu.RLock()
	settingsCopy := s.settings
	s.settingsMu.RUnlock()

	var procs []ProcessorSnapshot
	if s.pipe != nil {
		for i, p := range s.pipe.Processors() {
			ps := ProcessorSnapshot{
				Index: i,
				Type:  fmt.Sprintf("%T", p),
			}
			if describer, ok := p.(interface{ Description() string }); ok {
				ps.Description = describer.Description()
			}
			procs = append(procs, ps)
		}
	}

	obsCount := len(s.observers)

	return SessionSnapshot{
		SessionID:      s.id,
		State:          s.State().String(),
		Generation:     s.CurrentGeneration(),
		InputMuted:     s.InputMuted(),
		CreatedAt:      s.Created,
		Uptime:         time.Since(s.Created),
		FramesIn:       s.FramesIn.Load(),
		FramesOut:      s.FramesOut.Load(),
		StaleDiscarded: s.StaleDiscarded.Load(),
		AudioDropped:   s.AudioDropped.Load(),
		ActiveTasks:    int(s.activeTasks.Load()),
		LeakedTasks:    int(s.leakedTasks.Load()),
		Settings:       settingsCopy,
		Processors:     procs,
		ObserverCount:  obsCount,
	}
}
