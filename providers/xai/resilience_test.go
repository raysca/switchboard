package xai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestResilience_CircuitBreakerStateTransitions(t *testing.T) {
	cfg := CircuitConfig{
		FailureThreshold: 3,
		CooldownDuration: 50 * time.Millisecond,
	}
	cb := NewCircuitBreaker(cfg)

	// Starts closed
	if cb.State() != CircuitClosed || !cb.Allow() {
		t.Fatal("expected closed state initially")
	}

	// 2 failures: remains closed
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.State() != CircuitClosed || !cb.Allow() {
		t.Fatal("expected closed state after 2 failures")
	}

	// 3rd failure: transitions to OPEN
	cb.RecordFailure()
	if cb.State() != CircuitOpen {
		t.Fatalf("expected OPEN state after 3 failures, got %s", cb.State())
	}
	if cb.Allow() {
		t.Fatal("expected Allow() == false when circuit is OPEN")
	}

	// During cooldown: remains open
	time.Sleep(20 * time.Millisecond)
	if cb.Allow() {
		t.Fatal("expected Allow() == false during cooldown")
	}

	// Cooldown expires: transitions to HALF_OPEN
	time.Sleep(40 * time.Millisecond)
	if cb.State() != CircuitHalfOpen {
		t.Fatalf("expected HALF_OPEN state, got %s", cb.State())
	}

	// Exactly one probe allowed
	if !cb.Allow() {
		t.Fatal("expected first probe allowed in HALF_OPEN")
	}
	if cb.Allow() {
		t.Fatal("expected second probe blocked while probing in HALF_OPEN")
	}

	// Success closes the circuit
	cb.RecordSuccess()
	if cb.State() != CircuitClosed {
		t.Fatalf("expected CLOSED state after probe success, got %s", cb.State())
	}
	if cb.Failures() != 0 {
		t.Fatalf("expected failures reset to 0, got %d", cb.Failures())
	}
}

func TestResilience_NoRetryAfterCommittedOutput(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitConfig())
	policy := RetryPolicy{
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
		Multiplier:     2.0,
		MaxAttempts:    3,
	}

	var attempts int
	committed := true // User-visible output was already committed

	err := ExecuteWithRetry(context.Background(), cb, policy, &committed, func(attempt int) error {
		attempts++
		return errors.New("mid-stream failure")
	})

	// Exactly one attempt allowed; retry is strictly prohibited
	if attempts != 1 {
		t.Fatalf("expected exactly 1 attempt when output is committed, got %d", attempts)
	}
	if !errors.Is(err, ErrRetryCommitted) {
		t.Fatalf("expected ErrRetryCommitted, got %v", err)
	}
}

func TestResilience_RetryWithBackoffAndRetryAfter(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitConfig())
	policy := RetryPolicy{
		InitialBackoff: 2 * time.Millisecond,
		MaxBackoff:     10 * time.Millisecond,
		Multiplier:     2.0,
		MaxAttempts:    3,
	}

	var attempts int
	committed := false

	err := ExecuteWithRetry(context.Background(), cb, policy, &committed, func(attempt int) error {
		attempts++
		if attempt < 2 {
			return &ProviderError{
				Class:      ClassificationRateLimit,
				StatusCode: 429,
				RetryAfter: 5 * time.Millisecond,
				Message:    "rate limited",
			}
		}
		return nil
	})

	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestResilience_NonRetryableErrorNoRetry(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitConfig())
	policy := DefaultRetryPolicy()

	var attempts int
	committed := false

	err := ExecuteWithRetry(context.Background(), cb, policy, &committed, func(attempt int) error {
		attempts++
		return &ProviderError{
			Class:      ClassificationNonRetryable,
			StatusCode: 400,
			Message:    "invalid request format",
		}
	})

	if attempts != 1 {
		t.Fatalf("non-retryable error should only be attempted once, got %d", attempts)
	}
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.StatusCode != 400 {
		t.Fatalf("expected status 400 error, got %v", err)
	}
}

func TestResilience_SecretRedaction(t *testing.T) {
	rawSecretKey := "xai-9876543210abcdef9876543210"
	rawBearer := "Bearer my-super-secret-token-12345"

	leakMsg := "failed connecting with " + rawBearer + " and key=" + rawSecretKey
	sanitized := SanitizeString(leakMsg)

	if strings.Contains(sanitized, rawSecretKey) {
		t.Fatalf("API key not redacted: %s", sanitized)
	}
	if strings.Contains(sanitized, "my-super-secret-token-12345") {
		t.Fatalf("Bearer token not redacted: %s", sanitized)
	}
	if !strings.Contains(sanitized, "xai-[REDACTED]") {
		t.Fatalf("expected xai-[REDACTED], got: %s", sanitized)
	}

	pe := &ProviderError{
		Capability: "llm",
		Code:       "AUTH_ERROR",
		Message:    "invalid token: " + rawBearer,
		Class:      ClassificationNonRetryable,
		StatusCode: http.StatusUnauthorized,
	}
	sanitizedErr := SanitizeError(pe)
	if strings.Contains(sanitizedErr.Error(), "my-super-secret-token-12345") {
		t.Fatalf("SanitizeError failed to redact Bearer token: %s", sanitizedErr.Error())
	}
}

// Barge-ins cancel requests constantly; they must never open the breaker.
func TestResilience_CancellationIsNotAProviderFailure(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{FailureThreshold: 1, CooldownDuration: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	committed := false
	err := ExecuteWithRetry(ctx, cb, RetryPolicy{MaxAttempts: 3}, &committed, func(int) error {
		cancel() // the caller barges in mid-request
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteWithRetry() = %v, want context.Canceled", err)
	}
	if !cb.Allow() || cb.Failures() != 0 {
		t.Fatalf("breaker recorded a cancellation: failures=%d", cb.Failures())
	}
}
