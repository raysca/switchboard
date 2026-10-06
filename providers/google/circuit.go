package google

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
		// In HalfOpen, allow only one probe request at a time
		if !cb.probing {
			cb.probing = true
			return true
		}
		return false

	default:
		return true
	}
}

// SetObserver receives state transitions. The callback runs without the breaker lock.
func (cb *CircuitBreaker) SetObserver(fn func(CircuitState)) {
	cb.mu.Lock()
	cb.onChange = fn
	cb.mu.Unlock()
}

// ReportSuccess records a successful call and resets the circuit breaker.
func (cb *CircuitBreaker) ReportSuccess() {
	cb.mu.Lock()
	cb.failures = 0
	cb.state = CircuitClosed
	cb.probing = false
	fn := cb.onChange
	st := cb.state
	cb.mu.Unlock()
	if fn != nil {
		fn(st)
	}
}

// ReportFailure records a failed call and potentially opens the circuit.
func (cb *CircuitBreaker) ReportFailure() {
	cb.mu.Lock()

	cb.failures++
	cb.lastFailure = time.Now()
	cb.probing = false

	if cb.state == CircuitHalfOpen || cb.failures >= cb.cfg.FailureThreshold {
		cb.state = CircuitOpen
	}
	fn := cb.onChange
	st := cb.state
	cb.mu.Unlock()
	if fn != nil {
		fn(st)
	}
}

// reportError counts only availability failures (retryable: 5xx, timeout,
// rate limit). A bad credential, bad request or unknown model says nothing
// about the provider, so one tenant's mistake cannot open the shared breaker;
// it just frees a half-open probe.
func (cb *CircuitBreaker) reportError(err error) {
	if MapError("", err).Retryable {
		cb.ReportFailure()
		return
	}
	cb.release()
}

// release frees the half-open probe slot of a call that ended without an
// outcome (cancelled, superseded), so the breaker does not stay half-open forever.
func (cb *CircuitBreaker) release() {
	cb.mu.Lock()
	cb.probing = false
	cb.mu.Unlock()
}
