package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestErrorMapping(t *testing.T) {
	// 1. gRPC status mappings
	unauthErr := status.Error(codes.Unauthenticated, "bad token")
	mappedUnauth := MapError("stt", unauthErr)
	if !errors.Is(mappedUnauth, ErrProviderAuth) {
		t.Fatalf("expected ErrProviderAuth, got: %v", mappedUnauth)
	}
	if mappedUnauth.Retryable {
		t.Fatal("auth error should not be retryable")
	}

	resExhErr := status.Error(codes.ResourceExhausted, "quota exceeded")
	mappedQuota := MapError("llm", resExhErr)
	if !errors.Is(mappedQuota, ErrProviderRateLimit) {
		t.Fatalf("expected ErrProviderRateLimit, got: %v", mappedQuota)
	}
	if !mappedQuota.Retryable {
		t.Fatal("quota error should be retryable")
	}

	unavailErr := status.Error(codes.Unavailable, "backend down")
	mappedUnavail := MapError("tts", unavailErr)
	if !errors.Is(mappedUnavail, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got: %v", mappedUnavail)
	}
	if !mappedUnavail.Retryable {
		t.Fatal("unavailable error should be retryable")
	}

	// 2. HTTP error string mappings
	http401 := fmt.Errorf("googleapi: got HTTP response code 401 with body: API key not valid")
	mapped401 := MapError("llm", http401)
	if !errors.Is(mapped401, ErrProviderAuth) {
		t.Fatalf("expected ErrProviderAuth, got: %v", mapped401)
	}

	http429 := fmt.Errorf("HTTP 429 Too Many Requests: resource exhausted")
	mapped429 := MapError("stt", http429)
	if !errors.Is(mapped429, ErrProviderRateLimit) {
		t.Fatalf("expected ErrProviderRateLimit, got: %v", mapped429)
	}

	// 3. Sensitive secret redaction
	leakyErr := fmt.Errorf("request to https://speech.googleapis.com?key=AIzaSyD123456789SecretKey failed with Bearer ya29.a0AfH6_SecretBearerToken")
	mappedLeaky := MapError("stt", leakyErr)
	errStr := mappedLeaky.Error()
	if strings.Contains(errStr, "AIzaSyD123456789SecretKey") {
		t.Fatal("API key was not redacted from error message!")
	}
	if strings.Contains(errStr, "ya29.a0AfH6_SecretBearerToken") {
		t.Fatal("Bearer token was not redacted from error message!")
	}
	if !strings.Contains(errStr, "key=[REDACTED]") {
		t.Fatalf("expected key=[REDACTED], got: %s", errStr)
	}
}

func TestCircuitBreaker_StateTransitions(t *testing.T) {
	cb := NewCircuitBreaker(CircuitConfig{
		FailureThreshold: 5,
		CooldownDuration: 50 * time.Millisecond,
	})

	if cb.State() != CircuitClosed {
		t.Fatalf("expected CLOSED, got %s", cb.State())
	}
	if !cb.Allow() {
		t.Fatal("expected Allow() to be true for CLOSED circuit")
	}

	// 4 consecutive failures -> still CLOSED
	for i := 0; i < 4; i++ {
		cb.ReportFailure()
		if cb.State() != CircuitClosed {
			t.Fatalf("expected CLOSED at failure %d, got %s", i+1, cb.State())
		}
	}

	// 5th failure -> OPEN
	cb.ReportFailure()
	if cb.State() != CircuitOpen {
		t.Fatalf("expected OPEN after 5 failures, got %s", cb.State())
	}
	if cb.Allow() {
		t.Fatal("expected Allow() to be false for OPEN circuit")
	}
	if cb.IsReady() {
		t.Fatal("expected IsReady() to be false when circuit is OPEN")
	}

	// Wait for cooldown
	time.Sleep(60 * time.Millisecond)

	// Now state is HALF_OPEN
	if cb.State() != CircuitHalfOpen {
		t.Fatalf("expected HALF_OPEN after cooldown, got %s", cb.State())
	}

	// First probe allowed
	if !cb.Allow() {
		t.Fatal("expected first probe in HALF_OPEN to be allowed")
	}
	// Second concurrent probe while probing is disallowed
	if cb.Allow() {
		t.Fatal("expected concurrent probe in HALF_OPEN to be rejected")
	}

	// Report success -> transitions back to CLOSED
	cb.ReportSuccess()
	if cb.State() != CircuitClosed {
		t.Fatalf("expected CLOSED after probe success, got %s", cb.State())
	}
	if cb.Failures() != 0 {
		t.Fatalf("expected failure count reset to 0, got %d", cb.Failures())
	}

	// Probe failure test: open -> half-open -> fail probe -> open
	for i := 0; i < 5; i++ {
		cb.ReportFailure()
	}
	time.Sleep(60 * time.Millisecond)
	_ = cb.Allow()     // Trigger half-open probe
	cb.ReportFailure() // Probe failed
	if cb.State() != CircuitOpen {
		t.Fatalf("expected OPEN after probe failure, got %s", cb.State())
	}
}

type mockSleeper struct {
	sleptDurations []time.Duration
}

func (m *mockSleeper) Sleep(ctx context.Context, d time.Duration) error {
	m.sleptDurations = append(m.sleptDurations, d)
	return nil
}

func TestRetry_TransientErrorsAndJitter(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitConfig())
	policy := RetryPolicy{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     1000 * time.Millisecond,
		Multiplier:     2.0,
		MaxAttempts:    3,
	}

	sleeper := &mockSleeper{}
	attemptsCount := 0

	err := ExecuteWithRetry(context.Background(), cb, policy, nil, func(ctx context.Context, attempt int) error {
		attemptsCount++
		if attempt < 3 {
			// Return retryable 503 error
			return &ProviderError{
				Capability: "llm",
				BaseErr:    ErrProviderUnavailable,
				StatusCode: http.StatusServiceUnavailable,
				Retryable:  true,
			}
		}
		return nil // Success on 3rd attempt
	}, sleeper)

	if err != nil {
		t.Fatalf("expected success on 3rd attempt, got: %v", err)
	}
	if attemptsCount != 3 {
		t.Fatalf("expected 3 attempts, got %d", attemptsCount)
	}
	if len(sleeper.sleptDurations) != 2 {
		t.Fatalf("expected 2 sleeps, got %d", len(sleeper.sleptDurations))
	}
	if cb.Failures() != 0 {
		t.Fatalf("expected circuit breaker failure count 0 after success, got %d", cb.Failures())
	}
}

func TestRetry_HonorsRetryAfter(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitConfig())
	policy := RetryPolicy{
		InitialBackoff: 50 * time.Millisecond,
		MaxBackoff:     5000 * time.Millisecond,
		Multiplier:     2.0,
		MaxAttempts:    2,
	}

	sleeper := &mockSleeper{}
	_ = ExecuteWithRetry(context.Background(), cb, policy, nil, func(ctx context.Context, attempt int) error {
		if attempt == 1 {
			return &ProviderError{
				Capability: "llm",
				BaseErr:    ErrProviderRateLimit,
				Retryable:  true,
				RetryAfter: 500 * time.Millisecond,
			}
		}
		return nil
	}, sleeper)

	if len(sleeper.sleptDurations) != 1 {
		t.Fatalf("expected 1 sleep, got %d", len(sleeper.sleptDurations))
	}
	// With full jitter, sleep is between 0 and 500ms
	if sleeper.sleptDurations[0] > 500*time.Millisecond {
		t.Fatalf("sleep %v exceeded RetryAfter 500ms", sleeper.sleptDurations[0])
	}
}

func TestRetry_NonRetryableErrorFailsImmediately(t *testing.T) {
	cb := NewCircuitBreaker(DefaultCircuitConfig())
	policy := RetryPolicy{
		InitialBackoff: 100 * time.Millisecond,
		MaxAttempts:    3,
	}
	sleeper := &mockSleeper{}
	attemptsCount := 0

	err := ExecuteWithRetry(context.Background(), cb, policy, nil, func(ctx context.Context, attempt int) error {
		attemptsCount++
		return &ProviderError{
			Capability: "tts",
			BaseErr:    ErrProviderAuth,
			StatusCode: 401,
			Retryable:  false,
		}
	}, sleeper)

	if !errors.Is(err, ErrProviderAuth) {
		t.Fatalf("expected ErrProviderAuth, got: %v", err)
	}
	if attemptsCount != 1 {
		t.Fatalf("expected exactly 1 attempt for non-retryable error, got %d", attemptsCount)
	}
	if len(sleeper.sleptDurations) != 0 {
		t.Fatalf("expected 0 sleeps, got %d", len(sleeper.sleptDurations))
	}
}

func TestRetry_ProhibitedAfterCommittedOutput(t *testing.T) {
	// CRITICAL TEST: Once output is committed to the client, retry is strictly prohibited
	// to prevent duplicate speech or duplicate text deltas!
	cb := NewCircuitBreaker(DefaultCircuitConfig())
	policy := RetryPolicy{
		InitialBackoff: 100 * time.Millisecond,
		MaxAttempts:    3,
	}

	sleeper := &mockSleeper{}
	attemptsCount := 0
	var committed bool

	err := ExecuteWithRetry(context.Background(), cb, policy, func() bool {
		return committed
	}, func(ctx context.Context, attempt int) error {
		attemptsCount++
		// Simulate emitting first token / audio chunk
		committed = true

		// Transient failure occurs after commitment
		return &ProviderError{
			Capability: "llm",
			BaseErr:    ErrProviderUnavailable,
			StatusCode: 503,
			Retryable:  true,
		}
	}, sleeper)

	if !errors.Is(err, ErrRetryCommitted) {
		t.Fatalf("expected ErrRetryCommitted, got: %v", err)
	}
	if attemptsCount != 1 {
		t.Fatalf("expected retry to be aborted immediately after committed output, got %d attempts", attemptsCount)
	}
	if len(sleeper.sleptDurations) != 0 {
		t.Fatalf("expected 0 sleeps, got %d", len(sleeper.sleptDurations))
	}
}

func TestQuotaManager_HeadroomAndAdmission(t *testing.T) {
	// Headroom multiplier 2.0. Max streams = 4.
	// Capacity for active calls = 4 / 2.0 = 2 sessions.
	qm := NewQuotaManager(4, 2.0)

	// Session 1: allowed
	if !qm.CanAdmitSession() {
		t.Fatal("expected session 1 to be admitted")
	}
	if err := qm.AdmitSession(); err != nil {
		t.Fatalf("unexpected error admitting session 1: %v", err)
	}

	// Session 2: allowed
	if !qm.CanAdmitSession() {
		t.Fatal("expected session 2 to be admitted")
	}
	if err := qm.AdmitSession(); err != nil {
		t.Fatalf("unexpected error admitting session 2: %v", err)
	}

	// Session 3: must be REJECTED before quota exhaustion because (2+1)*2.0 = 6 > 4!
	if qm.CanAdmitSession() {
		t.Fatal("expected session 3 to be rejected due to lack of rotation headroom")
	}
	if err := qm.AdmitSession(); !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted for session 3, got: %v", err)
	}

	// Rotation test: active sessions = 2, active streams = 2.
	// Session 1 rotates -> reserves 1 extra stream.
	if err := qm.ReserveRotationStream(); err != nil {
		t.Fatalf("unexpected error reserving rotation stream: %v", err)
	}
	if qm.ActiveStreams() != 3 {
		t.Fatalf("expected 3 active streams, got %d", qm.ActiveStreams())
	}
	if qm.ReservedStreams() != 1 {
		t.Fatalf("expected 1 reserved rotation stream, got %d", qm.ReservedStreams())
	}

	// Finish rotation -> release stream
	qm.ReleaseRotationStream()
	if qm.ActiveStreams() != 2 {
		t.Fatalf("expected 2 active streams after rotation release, got %d", qm.ActiveStreams())
	}

	// Release session 1
	qm.ReleaseSession()
	if qm.ActiveSessions() != 1 {
		t.Fatalf("expected 1 active session, got %d", qm.ActiveSessions())
	}

	// Now session 3 can be admitted
	if !qm.CanAdmitSession() {
		t.Fatal("expected new session to be admissible after release")
	}
}
