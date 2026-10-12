package server

import (
	"errors"
	"testing"
	"time"
)

func TestAdmission_LimitsAndRejections(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Limits.MaxSessions = 2
	cfg.Limits.MaxSessionsPerPrincipal = 1
	ac := NewAdmissionController(cfg, NewMetrics())

	if err := ac.AcquireSession("user-1", "", false); !errors.Is(err, ErrVADUnavailable) {
		t.Fatalf("expected ErrVADUnavailable when VAD capacity false, got: %v", err)
	}
	if err := ac.AcquireSession("user-1", "", true); err != nil {
		t.Fatalf("acquire session user-1: %v", err)
	}
	if err := ac.AcquireSession("user-1", "", true); !errors.Is(err, ErrPrincipalLimit) {
		t.Fatalf("expected ErrPrincipalLimit for duplicate principal, got: %v", err)
	}
	if err := ac.AcquireSession("user-2", "", true); err != nil {
		t.Fatalf("second call within max_sessions refused: %v", err)
	}
	if err := ac.AcquireSession("user-3", "", true); !errors.Is(err, ErrSessionLimitExceeded) {
		t.Fatalf("expected ErrSessionLimitExceeded, got: %v", err)
	}
	ac.ReleaseSession("user-1", "")
	if err := ac.AcquireSession("user-3", "", true); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestAdmission_NoisyTenantCannotStarveOthers(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Limits.MaxSessions = 10
	cfg.Limits.MaxSessionsPerPrincipal = 3
	ac := NewAdmissionController(cfg, NewMetrics())
	for i := 0; i < 3; i++ {
		if err := ac.AcquireSession("noisy", "a", true); err != nil {
			t.Fatalf("noisy %d: %v", i, err)
		}
	}
	for i := 0; i < 50; i++ {
		if err := ac.AcquireSession("noisy", "a", true); !errors.Is(err, ErrPrincipalLimit) {
			t.Fatalf("noisy over cap: %v", err)
		}
	}
	// Global capacity remains (3 of 10): quiet tenants are admitted.
	for _, p := range []string{"q1", "q2", "q3"} {
		if err := ac.AcquireSession(p, "b", true); err != nil {
			t.Fatalf("%s starved: %v", p, err)
		}
	}
}

func TestAdmission_FlowCap(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Limits.MaxSessionsPerFlow = 1
	m := NewMetrics()
	ac := NewAdmissionController(cfg, m)
	if err := ac.AcquireSession("u1", "agent", true); err != nil {
		t.Fatal(err)
	}
	if err := ac.AcquireSession("u2", "agent", true); !errors.Is(err, ErrFlowLimit) {
		t.Fatalf("want ErrAgentLimit, got %v", err)
	}
	if err := ac.AcquireSession("u2", "other", true); err != nil {
		t.Fatalf("other agent: %v", err)
	}
	ac.ReleaseSession("u1", "agent")
	if err := ac.AcquireSession("u2", "agent", true); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if m.AdmissionRejectsFlow.Load() != 1 {
		t.Fatal("agent reject metric")
	}
}

func TestAdmission_StartRate(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Limits.MaxSessionsPerPrincipal = 100
	cfg.Limits.StartRatePerPrincipal = 1 // per second
	cfg.Limits.StartBurstPerPrincipal = 2
	m := NewMetrics()
	ac := NewAdmissionController(cfg, m)
	clk := &fakeClock{t: time.Unix(1000, 0)}
	ac.now = clk.now

	for i := 0; i < 2; i++ {
		if err := ac.AcquireSession("noisy", "", true); err != nil {
			t.Fatalf("burst %d: %v", i, err)
		}
		ac.ReleaseSession("noisy", "") // rate counts starts, not concurrency
	}
	if err := ac.AcquireSession("noisy", "", true); !errors.Is(err, ErrStartRateLimit) {
		t.Fatalf("want rate limit, got %v", err)
	}
	if err := ac.AcquireSession("quiet", "", true); err != nil {
		t.Fatalf("other tenant throttled: %v", err)
	}
	clk.t = clk.t.Add(1500 * time.Millisecond) // refills 1.5 tokens
	if err := ac.AcquireSession("noisy", "", true); err != nil {
		t.Fatalf("after refill: %v", err)
	}
	if err := ac.AcquireSession("noisy", "", true); !errors.Is(err, ErrStartRateLimit) {
		t.Fatalf("refill should allow only one: %v", err)
	}
	if m.AdmissionRejectsRate.Load() != 2 {
		t.Fatalf("rate rejects = %d", m.AdmissionRejectsRate.Load())
	}
}

func TestAdmission_DisabledByDefault(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	if cfg.Limits.MaxSessionsPerFlow != 0 || cfg.Limits.StartRatePerPrincipal != 0 {
		t.Fatal("new limits must default to disabled")
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	cfg.Limits.StartRatePerPrincipal = 1 // burst left at 0
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("rate > 0 with burst 0 must be rejected")
	}
	cfg.Limits.StartRatePerPrincipal, cfg.Limits.MaxSessionsPerFlow = 0, -1
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("negative agent cap must be rejected")
	}
}
