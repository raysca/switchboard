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
	if !cb.Allow() {
		t.Fatal("half-open probe ended before the call was never released")
	}
	cb.release()
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
