package xai

import (
	"errors"
	"sync"
	"time"
)

// CircuitState represents the current state of a provider circuit breaker.
type CircuitState string

const (
	CircuitClosed   CircuitState = "CLOSED"
	CircuitOpen     CircuitState = "OPEN"
	CircuitHalfOpen CircuitState = "HALF_OPEN"
)

// CircuitConfig configures circuit breaker thresholds.
type CircuitConfig struct {
	FailureThreshold int           // Consecutive failures to open (default 5)
	CooldownDuration time.Duration // Cooldown before half-open probe (default 30s)
}

func DefaultCircuitConfig() CircuitConfig {
	return CircuitConfig{
		FailureThreshold: 5,
		CooldownDuration: 30 * time.Second,
	}
}

// CircuitBreaker implements the 5-failure / 30s cooldown circuit breaker.
type CircuitBreaker struct {
	mu          sync.Mutex
	cfg         CircuitConfig
	state       CircuitState
	failures    int
	lastFailure time.Time
	probing     bool
	probe       uint64 // token of the granted half-open probe; 0 when none
	onChange    func(CircuitState)
}

// NewCircuitBreaker creates a new CircuitBreaker.
func NewCircuitBreaker(cfg CircuitConfig) *CircuitBreaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.CooldownDuration <= 0 {
		cfg.CooldownDuration = 30 * time.Second
	}
	return &CircuitBreaker{
		cfg:   cfg,
		state: CircuitClosed,
	}
}

// Allow reports whether a request may proceed.
func (cb *CircuitBreaker) Allow() bool {
	ok, _ := cb.allow()
	return ok
}

// allow also returns the probe token when it grants the half-open probe (0
// otherwise); only that token can release the probe.
func (cb *CircuitBreaker) allow() (bool, uint64) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	switch cb.state {
	case CircuitClosed:
		return true, 0
	case CircuitOpen:
		if now.Sub(cb.lastFailure) >= cb.cfg.CooldownDuration {
			cb.state = CircuitHalfOpen
			cb.probing = true
			cb.probe++
			return true, cb.probe
		}
		return false, 0
	case CircuitHalfOpen:
		if !cb.probing {
			cb.probing = true
			cb.probe++
			return true, cb.probe
		}
		return false, 0
	default:
		return false, 0
	}
}

// SetObserver receives state transitions. The callback runs without the breaker lock.
func (cb *CircuitBreaker) SetObserver(fn func(CircuitState)) {
	cb.mu.Lock()
	cb.onChange = fn
	cb.mu.Unlock()
}

func (cb *CircuitBreaker) notify(st CircuitState) {
	fn := cb.onChange
	if fn != nil {
		fn(st)
	}
}

// RecordSuccess records a successful call, resetting the failure count and closing the circuit.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	cb.failures = 0
	cb.probing = false
	cb.state = CircuitClosed
	st := cb.state
	cb.mu.Unlock()
	cb.notify(st)
}

// RecordFailure records a failed call.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	cb.failures++
	cb.lastFailure = time.Now()
	cb.probing = false

	if cb.state == CircuitHalfOpen || cb.failures >= cb.cfg.FailureThreshold {
		cb.state = CircuitOpen
	}
	st := cb.state
	cb.mu.Unlock()
	cb.notify(st)
}

// recordError counts only availability failures. A non-retryable provider
// error (bad credential, bad request, unknown model) says nothing about xAI,
// so one tenant's mistake cannot open the shared breaker; it just frees a
// half-open probe.
func (cb *CircuitBreaker) recordError(err error, probe uint64) {
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Class != ClassificationNonRetryable {
		cb.RecordFailure()
		return
	}
	cb.release(probe)
}

// release frees the half-open probe slot of a call that ended without an
// outcome (cancelled), so the breaker does not stay half-open forever.
func (cb *CircuitBreaker) release(probe uint64) {
	cb.mu.Lock()
	if probe != 0 && probe == cb.probe { // a stale call must not free another call's probe
		cb.probing = false
	}
	cb.mu.Unlock()
}
