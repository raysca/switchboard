package core

import (
	"errors"
	"time"
)

// ControlEnvelope carries typed correlation for future control adapters. Runtime
// settings application and queue priority remain outside this P0 contract.
type ControlEnvelope struct {
	Session    SessionID
	Turn       TurnID
	Generation Generation
	Frame      Frame
}

// RuntimeSettingsUpdate declares the revisioned P1 settings envelope.
type RuntimeSettingsUpdate struct {
	Revision            uint64
	InputMuted          *bool
	InterruptionEnabled *bool
	SilenceTimeout      *time.Duration
	TurnTimeout         *time.Duration
	Language            *string
}

func (*RuntimeSettingsUpdate) Type() FrameType { return FrameSettingsControl }

var (
	ErrInvalidRuntimeSettings = errors.New("core: invalid runtime settings update")
	ErrStaleSettingsRevision  = errors.New("core: stale runtime settings revision")
)

// Validate checks that the update contains a positive revision and valid parameter values.
func (u *RuntimeSettingsUpdate) Validate() error {
	if u == nil || u.Revision == 0 {
		return errors.New("core: runtime settings revision must be greater than zero")
	}
	if u.SilenceTimeout != nil && *u.SilenceTimeout < 0 {
		return errors.New("core: silence timeout cannot be negative")
	}
	if u.TurnTimeout != nil && *u.TurnTimeout < 0 {
		return errors.New("core: turn timeout cannot be negative")
	}
	if u.Language != nil && *u.Language == "" {
		return errors.New("core: language cannot be empty string")
	}
	return nil
}

// RuntimeSettings captures the atomic snapshot of active runtime service settings.
type RuntimeSettings struct {
	Revision            uint64
	InputMuted          bool
	InterruptionEnabled bool
	SilenceTimeout      time.Duration
	TurnTimeout         time.Duration
	Language            string
}

// DefaultRuntimeSettings returns initial default settings.
func DefaultRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{
		Revision:            0,
		InputMuted:          false,
		InterruptionEnabled: true,
		SilenceTimeout:      700 * time.Millisecond,
		TurnTimeout:         60 * time.Second,
		Language:            "en-US",
	}
}

// RuntimeSettingsAppliedEvent signals that a settings revision was validated and applied atomically.
type RuntimeSettingsAppliedEvent struct {
	Revision uint64
	Settings RuntimeSettings
}

func (*RuntimeSettingsAppliedEvent) Type() FrameType { return FrameSettingsControl }

// RuntimeSettingsErrorEvent signals that an update was rejected without mutating processor state.
type RuntimeSettingsErrorEvent struct {
	Revision uint64
	Err      error
}

func (*RuntimeSettingsErrorEvent) Type() FrameType { return FrameError }

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
