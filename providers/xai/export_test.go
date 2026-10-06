package xai

import "time"

// State returns the circuit state, reporting a cooled-down open circuit as half-open.
func (cb *CircuitBreaker) State() CircuitState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state == CircuitOpen && time.Since(cb.lastFailure) >= cb.cfg.CooldownDuration {
		return CircuitHalfOpen
	}
	return cb.state
}

func (cb *CircuitBreaker) Failures() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.failures
}
