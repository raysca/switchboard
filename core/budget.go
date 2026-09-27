package core

import (
	"errors"
	"time"
)

var (
	ErrInvalidBudget = errors.New("invalid resource budget configuration")
)

// ResourceBudget defines the operational bounds and limits for a voice session.
type ResourceBudget struct {
	MaxQueuedFrames    int           // Bounded data inbox capacity
	MaxControlFrames   int           // Bounded control inbox capacity
	MaxAsyncTasks      int           // Maximum concurrent supervised tasks via session.Go
	MaxEmitsPerFrame   int           // Maximum emits allowed from a single input envelope
	MaxFrameProcessing time.Duration // Watchdog timeout for a single envelope delivery
	MaxSessionDuration time.Duration // Maximum call duration before orderly termination
	TeardownTimeout    time.Duration // Maximum wait time during session shutdown
	MaxBufferedAudio   time.Duration // Maximum audio buffered before shedding
}

// DefaultResourceBudget returns a production-safe baseline budget.
func DefaultResourceBudget() ResourceBudget {
	return ResourceBudget{
		MaxQueuedFrames:    64,
		MaxControlFrames:   32,
		MaxAsyncTasks:      16,
		MaxEmitsPerFrame:   64,
		MaxFrameProcessing: 2 * time.Second,
		MaxSessionDuration: 1 * time.Hour,
		TeardownTimeout:    2 * time.Second,
		MaxBufferedAudio:   256 * time.Millisecond,
	}
}

// Validate checks that all budget values are positive and coherent.
func (b ResourceBudget) Validate() error {
	if b.MaxQueuedFrames <= 0 || b.MaxControlFrames <= 0 {
		return errors.New("queue capacities must be positive")
	}
	if b.MaxAsyncTasks <= 0 {
		return errors.New("MaxAsyncTasks must be positive")
	}
	if b.MaxEmitsPerFrame <= 0 {
		return errors.New("MaxEmitsPerFrame must be positive")
	}
	if b.MaxFrameProcessing <= 0 {
		return errors.New("MaxFrameProcessing must be positive")
	}
	if b.TeardownTimeout <= 0 {
		return errors.New("TeardownTimeout must be positive")
	}
	return nil
}
