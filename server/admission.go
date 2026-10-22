package server

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrSessionLimitExceeded = errors.New("maximum active sessions exceeded")
	ErrPrincipalLimit       = errors.New("maximum concurrent sessions per principal exceeded")
	ErrAgentLimit           = errors.New("maximum concurrent sessions per flow exceeded")
	ErrStartRateLimit       = errors.New("session start rate limit exceeded")
	ErrVADUnavailable       = errors.New("VAD executor has zero available capacity")
)

// AdmissionController caps concurrent calls, in total, per principal (the
// tenant: token principal, else subject) and optionally per flow, plus an
// optional per-principal start-rate token bucket, before a call allocates its
// pipeline. Quotas are per key, so a noisy principal at its cap is rejected
// while others are still admitted under the global cap.
// ponytail: reject only, no queueing or weighted fair share; a tenant can still
// consume the whole global cap if its per-principal cap is set that high.
// Pending handshakes are capped by the ws server.
type AdmissionController struct {
	mu         sync.Mutex
	cfg        *Config
	metrics    *Metrics
	now        func() time.Time // injectable for tests
	active     int
	principals map[string]int
	agents     map[string]int
	buckets    map[string]*startBucket
}

type startBucket struct {
	tokens float64
	last   time.Time
}

func NewAdmissionController(cfg *Config, metrics *Metrics) *AdmissionController {
	return &AdmissionController{cfg: cfg, metrics: metrics, now: time.Now,
		principals: make(map[string]int), agents: make(map[string]int), buckets: make(map[string]*startBucket)}
}

// takeStart consumes one start token for principal; caller holds ac.mu.
func (ac *AdmissionController) takeStart(principal string) bool {
	rate, burst := ac.cfg.Limits.StartRatePerPrincipal, float64(ac.cfg.Limits.StartBurstPerPrincipal)
	now := ac.now()
	// ponytail: full sweep when the map grows; idle (refilled) buckets are
	// equivalent to absent ones. Bounded by distinct principals per refill window.
	if len(ac.buckets) > 1024 {
		for k, b := range ac.buckets {
			if b.tokens+now.Sub(b.last).Seconds()*rate >= burst {
				delete(ac.buckets, k)
			}
		}
	}
	b := ac.buckets[principal]
	if b == nil {
		b = &startBucket{tokens: burst, last: now}
		ac.buckets[principal] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// AcquireSession admits a call; ReleaseSession must follow once it ends.
func (ac *AdmissionController) AcquireSession(principal, agentID string, vadCapacityAvailable bool) error {
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
	if max := ac.cfg.Limits.MaxSessionsPerAgent; max > 0 && agentID != "" && ac.agents[agentID] >= max {
		ac.metrics.AdmissionRejectsAgent.Add(1)
		return fmt.Errorf("%w: flow has %d active sessions", ErrAgentLimit, ac.agents[agentID])
	}
	// Last, so a call rejected above never burns a start token.
	if principal != "" && ac.cfg.Limits.StartRatePerPrincipal > 0 && !ac.takeStart(principal) {
		ac.metrics.AdmissionRejectsRate.Add(1)
		return fmt.Errorf("%w: %s", ErrStartRateLimit, principal)
	}
	ac.active++
	if principal != "" {
		ac.principals[principal]++
	}
	if agentID != "" {
		ac.agents[agentID]++
	}
	ac.metrics.ActiveSessions.Store(int64(ac.active))
	return nil
}

func (ac *AdmissionController) ReleaseSession(principal, agentID string) {
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
	if agentID != "" {
		if ac.agents[agentID]--; ac.agents[agentID] <= 0 {
			delete(ac.agents, agentID)
		}
	}
}

// admissionReason maps an admission error to the stable reason code sent to the
// client in the RTVI error (docs/pipecat-compatibility.md).
func admissionReason(err error) string {
	switch {
	case errors.Is(err, ErrStartRateLimit):
		return "rate_limited"
	case errors.Is(err, ErrAgentLimit):
		return "flow_limit"
	case errors.Is(err, ErrPrincipalLimit):
		return "principal_limit"
	default: // global cap, VAD unavailable
		return "capacity_exceeded"
	}
}
