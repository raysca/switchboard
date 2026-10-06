package xai

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHalfOpenProbeEndedByCancellationIsReleased(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Millisecond})
	cb.RecordFailure()
	time.Sleep(2 * time.Millisecond)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// pre-call ctx.Done
	_ = ExecuteWithRetry(cancelled, cb, RetryPolicy{MaxAttempts: 1}, nil, func(int) error { return nil })
	ok, probe := cb.allow()
	if !ok {
		t.Fatal("half-open probe ended before the call was never released")
	}
	cb.release(probe)
	// cancelled mid-call
	_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(int) error { return context.Canceled })
	if !cb.Allow() {
		t.Fatal("half-open probe ended by cancellation was never released")
	}
}

func TestClientErrorsDoNotOpenSharedBreaker(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Hour})
	auth := &ProviderError{Code: "UNAUTHORIZED", Class: ClassificationNonRetryable, StatusCode: 401}
	_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(int) error { return auth })
	if !cb.Allow() {
		t.Fatal("a tenant's bad credential opened the process-wide breaker")
	}
	_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(int) error { return errors.New("connection reset") })
	if cb.Allow() {
		t.Fatal("an availability failure did not count")
	}
}

// Only the holder of the half-open probe can release it (STT reports errors
// without holding one).
func TestStaleCallCannotReleaseAnotherProbe(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Millisecond})
	cb.RecordFailure()
	time.Sleep(2 * time.Millisecond)
	if ok, _ := cb.allow(); !ok {
		t.Fatal("probe not granted after cooldown")
	}
	cb.recordError(&ProviderError{Class: ClassificationNonRetryable}, 0)
	if ok, _ := cb.allow(); ok {
		t.Fatal("an error with no probe freed the live probe")
	}
}

// xAI's own request deadline means xAI hung and counts; a caller's deadline
// (e.g. a slow tool round) does not.
func TestOnlyProviderDeadlineCounts(t *testing.T) {
	run := func(ctx context.Context) *CircuitBreaker {
		cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Hour})
		committed := false
		_ = ExecuteWithRetry(ctx, cb, RetryPolicy{MaxAttempts: 1}, &committed, func(int) error { <-ctx.Done(); return ctx.Err() })
		return cb
	}
	own, cancel := context.WithTimeoutCause(context.Background(), time.Millisecond, ErrRequestTimeout)
	defer cancel()
	if run(own).Allow() {
		t.Fatal("an xAI request timeout did not count")
	}
	caller, cancel2 := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel2()
	if !run(caller).Allow() {
		t.Fatal("a caller's deadline opened the shared breaker")
	}
}
