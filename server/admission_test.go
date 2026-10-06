package server

import (
	"errors"
	"testing"
)

func TestAdmission_LimitsAndRejections(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Limits.MaxSessions = 2
	cfg.Limits.MaxSessionsPerPrincipal = 1
	cfg.Providers.GoogleSTT.MaxStreams = 2 // no longer halves capacity
	ac := NewAdmissionController(cfg, NewMetrics())

	if err := ac.AcquireSession("user-1", false); !errors.Is(err, ErrVADUnavailable) {
		t.Fatalf("expected ErrVADUnavailable when VAD capacity false, got: %v", err)
	}
	if err := ac.AcquireSession("user-1", true); err != nil {
		t.Fatalf("acquire session user-1: %v", err)
	}
	if err := ac.AcquireSession("user-1", true); !errors.Is(err, ErrPrincipalLimit) {
		t.Fatalf("expected ErrPrincipalLimit for duplicate principal, got: %v", err)
	}
	if err := ac.AcquireSession("user-2", true); err != nil {
		t.Fatalf("second call within max_sessions refused: %v", err)
	}
	if err := ac.AcquireSession("user-3", true); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("expected ErrSessionLimitExceeded, got: %v", err)
	}
	ac.ReleaseSession("user-1")
	if err := ac.AcquireSession("user-3", true); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}
