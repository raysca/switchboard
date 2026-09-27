package xai

import (
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

// Allow reports whether a request is allowed to proceed.
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	switch cb.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		if now.Sub(cb.lastFailure) >= cb.cfg.CooldownDuration {
			cb.state = CircuitHalfOpen
			cb.probing = true
			return true
		}
		return false
	case CircuitHalfOpen:
		if !cb.probing {
			cb.probing = true
			return true
		}
		return false
	default:
		return false
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

// State returns the current CircuitState.
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == CircuitOpen && time.Since(cb.lastFailure) >= cb.cfg.CooldownDuration {
		return CircuitHalfOpen
	}
	return cb.state
}

// Failures returns the consecutive failure count.
func (cb *CircuitBreaker) Failures() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.failures
}
