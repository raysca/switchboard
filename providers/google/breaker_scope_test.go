package google

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestHalfOpenProbeEndedByCancellationIsReleased(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Millisecond})
	cb.ReportFailure()
	time.Sleep(2 * time.Millisecond)
	for _, err := range []error{context.Canceled, ErrGenerationMismatch} {
		_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(context.Context, int) error { return err })
		ok, probe := cb.allow()
		if !ok {
			t.Fatalf("half-open probe ended by %v was never released", err)
		}
		cb.release(probe)
	}
}

func TestClientErrorsDoNotOpenSharedBreaker(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Hour})
	for _, err := range []error{errors.New("401 unauthenticated"), errors.New("400 bad request: unknown model")} {
		_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(context.Context, int) error { return err })
	}
	if !cb.Allow() {
		t.Fatal("a tenant's bad credential/request opened the process-wide breaker")
	}
	_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(context.Context, int) error { return errors.New("503 unavailable") })
	if cb.Allow() {
		t.Fatal("an availability failure did not count")
	}
}

// Outages the mapper calls non-retryable (HTTP 500, gRPC Internal/Unknown)
// must still open the breaker; only client errors are exempt.
func TestServerErrorsOpenTheBreaker(t *testing.T) {
	for _, outage := range []error{errors.New("Error 500, Message: internal error, Status: INTERNAL"), status.Error(codes.Internal, "boom"), status.Error(codes.Unknown, "boom")} {
		cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Hour})
		_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(context.Context, int) error { return outage })
		if cb.Allow() {
			t.Fatalf("%v did not count against the breaker", outage)
		}
	}
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Hour})
	_ = ExecuteWithRetry(context.Background(), cb, RetryPolicy{MaxAttempts: 1}, nil, func(context.Context, int) error { return status.Error(codes.NotFound, "model") })
	if !cb.Allow() {
		t.Fatal("an unknown model (NotFound) opened the shared breaker")
	}
}

// Only the holder of the half-open probe can release it.
func TestStaleCallCannotReleaseAnotherProbe(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Millisecond})
	_, stale := cb.allow() // admitted while closed: token 0
	cb.ReportFailure()
	time.Sleep(2 * time.Millisecond)
	if ok, _ := cb.allow(); !ok {
		t.Fatal("probe not granted after cooldown")
	}
	cb.release(stale)
	if ok, _ := cb.allow(); ok {
		t.Fatal("a stale call freed the live probe: a second probe got through")
	}
}

// A missed deadline means the provider hung: it counts.
func TestDeadlineCountsAsFailure(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_ = ExecuteWithRetry(ctx, cb, RetryPolicy{MaxAttempts: 1}, nil, func(ctx context.Context, _ int) error { <-ctx.Done(); return ctx.Err() })
	if cb.Allow() {
		t.Fatal("a provider timeout did not count")
	}
}
