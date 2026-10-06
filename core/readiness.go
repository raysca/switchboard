package core

import (
	"errors"
	"time"
)

var (
	ErrReadinessComponentRequired = errors.New("core: readiness event component name is required")
	ErrReadinessInvalidStatus     = errors.New("core: readiness status must be ready, degraded, or not_ready")
)

// ReadinessStatus indicates the functional health of a component or processor.
type ReadinessStatus string

const (
	ReadinessReady    ReadinessStatus = "ready"
	ReadinessDegraded ReadinessStatus = "degraded"
	ReadinessNotReady ReadinessStatus = "not_ready"
)

// ReadinessFrame announces the initialization, availability, or degraded health of a pipeline processor or provider.
type ReadinessFrame struct {
	Component string          `json:"component"`
	Status    ReadinessStatus `json:"status"`
	Reason    string          `json:"reason,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Latency   time.Duration   `json:"latency,omitempty"`
	Gen       uint64          `json:"gen,omitempty"`
}

func (f *ReadinessFrame) Type() FrameType    { return FrameReadiness }
func (f *ReadinessFrame) Generation() uint64 { return f.Gen }

// Validate checks the structural constraints of a ReadinessFrame.
func (f *ReadinessFrame) Validate() error {
	if f.Component == "" {
		return ErrReadinessComponentRequired
	}
	switch f.Status {
	case ReadinessReady, ReadinessDegraded, ReadinessNotReady:
		return nil
	default:
		return ErrReadinessInvalidStatus
	}
}

// ReadyNotifier is implemented by processors or adapters that emit structured readiness state transitions.
type ReadyNotifier interface {
	Readiness() ReadinessFrame
}
