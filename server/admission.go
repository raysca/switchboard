package server

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

var (
	ErrAdmissionDisabled    = errors.New("admission control is globally disabled")
	ErrPendingLimitExceeded = errors.New("maximum pending handshakes exceeded")
	ErrSessionLimitExceeded = errors.New("maximum active sessions exceeded")
	ErrPrincipalLimit       = errors.New("maximum concurrent sessions per principal exceeded")
	ErrSTTQuotaExhausted    = errors.New("insufficient STT stream quota or rotation headroom")
	ErrVADUnavailable       = errors.New("VAD executor has zero available capacity")
)

// AdmissionController manages admission before allocating heavy pipeline resources.
type AdmissionController struct {
	mu           sync.Mutex
	cfg          *Config
	metrics      *Metrics
	enabled      atomic.Bool
	pendingCount atomic.Int64
	activeCount  atomic.Int64
	principals   map[string]int
	activeSTT    atomic.Int64
}

// NewAdmissionController creates a new admission controller.
func NewAdmissionController(cfg *Config, metrics *Metrics) *AdmissionController {
	ac := &AdmissionController{
		cfg:        cfg,
		metrics:    metrics,
		principals: make(map[string]int),
	}
	ac.enabled.Store(true)
	return ac
}

// SetEnabled toggles global admission.
func (ac *AdmissionController) SetEnabled(enabled bool) {
	ac.enabled.Store(enabled)
}

// IsEnabled returns true if admission is enabled.
func (ac *AdmissionController) IsEnabled() bool {
	return ac.enabled.Load()
}

// AcquireHandshake reserves a lightweight pending-handshake slot.
func (ac *AdmissionController) AcquireHandshake() error {
	if !ac.enabled.Load() {
		ac.metrics.AdmissionRejectsDisabled.Add(1)
		return ErrAdmissionDisabled
	}

	current := ac.pendingCount.Add(1)
	if current > int64(ac.cfg.Limits.MaxPendingHandshakes) {
		ac.pendingCount.Add(-1)
		ac.metrics.AdmissionRejectsPending.Add(1)
		return ErrPendingLimitExceeded
	}
	ac.metrics.PendingHandshakes.Store(current)
	return nil
}

// ReleaseHandshake releases the pending-handshake slot.
func (ac *AdmissionController) ReleaseHandshake() {
	current := ac.pendingCount.Add(-1)
	if current < 0 {
		ac.pendingCount.Store(0)
		current = 0
	}
	ac.metrics.PendingHandshakes.Store(current)
}

// AcquireSession admits a full voice session with VAD and provider resources.
func (ac *AdmissionController) AcquireSession(principal string, vadCapacityAvailable bool) error {
	if !ac.enabled.Load() {
		ac.metrics.AdmissionRejectsDisabled.Add(1)
		return ErrAdmissionDisabled
	}

	if !vadCapacityAvailable {
		ac.metrics.AdmissionRejectsVAD.Add(1)
		return ErrVADUnavailable
	}

	ac.mu.Lock()
	defer ac.mu.Unlock()

	// Check total active sessions
	if ac.activeCount.Load() >= int64(ac.cfg.Limits.MaxSessions) {
		ac.metrics.AdmissionRejectsLimit.Add(1)
		return ErrSessionLimitExceeded
	}

	// Check per-principal limit
	if principal != "" {
		currentPrinc := ac.principals[principal]
		if currentPrinc >= ac.cfg.Limits.MaxSessionsPerPrincipal {
			ac.metrics.AdmissionRejectsPrincipal.Add(1)
			return fmt.Errorf("%w: %s has %d active sessions", ErrPrincipalLimit, principal, currentPrinc)
		}
	}

	// Check STT capacity including rotation headroom
	// Required headroom multiplier (default 2.0 per spec §24 & §32)
	maxStreams := ac.cfg.Providers.GoogleSTT.MaxStreams
	multiplier := ac.cfg.Providers.GoogleSTT.RotationHeadroomMultiplier
	if multiplier <= 0 {
		multiplier = 2.0
	}
	if maxStreams > 0 {
		projectedStreams := float64(ac.activeSTT.Load()+1) * multiplier
		if projectedStreams > float64(maxStreams) {
			ac.metrics.AdmissionRejectsQuota.Add(1)
			return ErrSTTQuotaExhausted
		}
	}

	// Admit
	ac.activeCount.Add(1)
	ac.activeSTT.Add(1)
	if principal != "" {
		ac.principals[principal]++
	}

	ac.metrics.ActiveSessions.Store(ac.activeCount.Load())
	ac.metrics.SessionsStartedTotal.Add(1)
	ac.metrics.STTActiveStreams.Store(ac.activeSTT.Load())
	return nil
}

// ReleaseSession releases all resources for an admitted session.
func (ac *AdmissionController) ReleaseSession(principal string) {
	ac.mu.Lock()
	defer ac.mu.Unlock()

	current := ac.activeCount.Add(-1)
	if current < 0 {
		ac.activeCount.Store(0)
		current = 0
	}
	ac.metrics.ActiveSessions.Store(current)

	sttCurrent := ac.activeSTT.Add(-1)
	if sttCurrent < 0 {
		ac.activeSTT.Store(0)
		sttCurrent = 0
	}
	ac.metrics.STTActiveStreams.Store(sttCurrent)

	if principal != "" {
		ac.principals[principal]--
		if ac.principals[principal] <= 0 {
			delete(ac.principals, principal)
		}
	}
}

// ActiveSessionCount returns currently active voice sessions.
func (ac *AdmissionController) ActiveSessionCount() int64 {
	return ac.activeCount.Load()
}
