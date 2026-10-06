package server

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrSessionLimitExceeded = errors.New("maximum active sessions exceeded")
	ErrPrincipalLimit       = errors.New("maximum concurrent sessions per principal exceeded")
	ErrVADUnavailable       = errors.New("VAD executor has zero available capacity")
)

// AdmissionController caps concurrent calls, in total and per principal, before
// a call allocates its pipeline. Pending handshakes are capped by the ws server.
type AdmissionController struct {
	mu         sync.Mutex
	cfg        *Config
	metrics    *Metrics
	active     int
	principals map[string]int
}

func NewAdmissionController(cfg *Config, metrics *Metrics) *AdmissionController {
	return &AdmissionController{cfg: cfg, metrics: metrics, principals: make(map[string]int)}
}

// AcquireSession admits a call; ReleaseSession must follow once it ends.
func (ac *AdmissionController) AcquireSession(principal string, vadCapacityAvailable bool) error {
	if !vadCapacityAvailable {
		ac.metrics.AdmissionRejectsVAD.Add(1)
		return ErrVADUnavailable
	}
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.active >= ac.cfg.Limits.MaxSessions {
		ac.metrics.AdmissionRejectsLimit.Add(1)
		return ErrSessionLimitExceeded
	}
	if principal != "" && ac.principals[principal] >= ac.cfg.Limits.MaxSessionsPerPrincipal {
		ac.metrics.AdmissionRejectsPrincipal.Add(1)
		return fmt.Errorf("%w: %s has %d active sessions", ErrPrincipalLimit, principal, ac.principals[principal])
	}
	ac.active++
	if principal != "" {
		ac.principals[principal]++
	}
	ac.metrics.ActiveSessions.Store(int64(ac.active))
	return nil
}

func (ac *AdmissionController) ReleaseSession(principal string) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.active > 0 {
		ac.active--
	}
	ac.metrics.ActiveSessions.Store(int64(ac.active))
	if principal != "" {
		if ac.principals[principal]--; ac.principals[principal] <= 0 {
			delete(ac.principals, principal)
		}
	}
}
