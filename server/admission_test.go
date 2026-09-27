package server

import (
	"errors"
	"testing"
)

func TestAdmission_LimitsAndRejections(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Limits.MaxPendingHandshakes = 2
	cfg.Limits.MaxSessions = 2
	cfg.Limits.MaxSessionsPerPrincipal = 1
	cfg.Providers.GoogleSTT.MaxStreams = 4 // with 2.0 multiplier, supports max 2 sessions

	metrics := NewMetrics()
	ac := NewAdmissionController(cfg, metrics)

	// 1. Pending Handshake Limit
	if err := ac.AcquireHandshake(); err != nil {
		t.Fatalf("acquire handshake 1: %v", err)
	}
	if err := ac.AcquireHandshake(); err != nil {
		t.Fatalf("acquire handshake 2: %v", err)
	}
	if err := ac.AcquireHandshake(); !errors.Is(err, ErrPendingLimitExceeded) {
		t.Fatalf("expected ErrPendingLimitExceeded, got: %v", err)
	}
	ac.ReleaseHandshake()
	if err := ac.AcquireHandshake(); err != nil {
		t.Fatalf("re-acquire handshake after release: %v", err)
	}
	ac.ReleaseHandshake()
	ac.ReleaseHandshake()

	// 2. VAD Capacity Check
	if err := ac.AcquireSession("user-1", false); !errors.Is(err, ErrVADUnavailable) {
		t.Fatalf("expected ErrVADUnavailable when VAD capacity false, got: %v", err)
	}

	// 3. Principal Limit
	if err := ac.AcquireSession("user-1", true); err != nil {
		t.Fatalf("acquire session user-1: %v", err)
	}
	if err := ac.AcquireSession("user-1", true); !errors.Is(err, ErrPrincipalLimit) {
		t.Fatalf("expected ErrPrincipalLimit for duplicate principal, got: %v", err)
	}

	// 4. Session Limit
	if err := ac.AcquireSession("user-2", true); err != nil {
		t.Fatalf("acquire session user-2: %v", err)
	}
	if err := ac.AcquireSession("user-3", true); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("expected ErrSessionLimitExceeded, got: %v", err)
	}

	// Release session
	ac.ReleaseSession("user-1")

	// 5. STT Quota Headroom Limit
	// Set MaxStreams to 2. With multiplier 2.0, 1 active session needs 2 streams.
	// So 2nd session needs projected 4 streams > 2, which rejects!
	cfg.Providers.GoogleSTT.MaxStreams = 2
	if err := ac.AcquireSession("user-3", true); !errors.Is(err, ErrSTTQuotaExhausted) {
		t.Fatalf("expected ErrSTTQuotaExhausted due to rotation headroom, got: %v", err)
	}

	// 6. Globally Disabled Admission
	ac.SetEnabled(false)
	if err := ac.AcquireSession("user-4", true); !errors.Is(err, ErrAdmissionDisabled) {
		t.Fatalf("expected ErrAdmissionDisabled, got: %v", err)
	}
}
