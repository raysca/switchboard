package core

import (
	"testing"
	"time"
)

func TestReadinessFrame_Validation(t *testing.T) {
	// Valid ready frame
	fValid := &ReadinessFrame{
		Component: "stt.google",
		Status:    ReadinessReady,
		Timestamp: time.Now(),
		Gen:       1,
	}
	if err := fValid.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if fValid.Type() != FrameReadiness {
		t.Errorf("expected FrameReadiness type, got %v", fValid.Type())
	}
	if fValid.Generation() != 1 {
		t.Errorf("expected generation 1, got %d", fValid.Generation())
	}

	// Missing component name
	fNoComp := &ReadinessFrame{
		Status: ReadinessReady,
	}
	if err := fNoComp.Validate(); err != ErrReadinessComponentRequired {
		t.Errorf("expected ErrReadinessComponentRequired, got %v", err)
	}

	// Invalid status
	fBadStatus := &ReadinessFrame{
		Component: "llm.xai",
		Status:    "unknown_status",
	}
	if err := fBadStatus.Validate(); err != ErrReadinessInvalidStatus {
		t.Errorf("expected ErrReadinessInvalidStatus, got %v", err)
	}

	// Degraded status with reason
	fDegraded := &ReadinessFrame{
		Component: "tts.cartesia",
		Status:    ReadinessDegraded,
		Reason:    "high upstream latency",
		Latency:   450 * time.Millisecond,
		Timestamp: time.Now(),
	}
	if err := fDegraded.Validate(); err != nil {
		t.Fatalf("unexpected error for degraded frame: %v", err)
	}
}
