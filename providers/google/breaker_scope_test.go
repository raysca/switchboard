package google

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHalfOpenProbeEndedByCancellationIsReleased(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Millisecond})
	cb.ReportFailure()
	time.Sleep(2 * time.Millisecond)
	for _, err := range []error{context.Canceled, ErrGenerationMismatch} {
		_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(context.Context, int) error { return err }, nil)
		if !cb.Allow() {
			t.Fatalf("half-open probe ended by %v was never released", err)
		}
		cb.release()
	}
}

func TestClientErrorsDoNotOpenSharedBreaker(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Hour})
	for _, err := range []error{errors.New("401 unauthenticated"), errors.New("400 bad request: unknown model")} {
		_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(context.Context, int) error { return err }, nil)
	}
	if !cb.Allow() {
		t.Fatal("a tenant's bad credential/request opened the process-wide breaker")
	}
	_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(context.Context, int) error { return errors.New("503 unavailable") }, nil)
	if cb.Allow() {
		t.Fatal("an availability failure did not count")
	}
}
