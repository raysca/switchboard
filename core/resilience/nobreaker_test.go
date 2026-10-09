package resilience

import (
	"context"
	"errors"
	"testing"
	"time"
)

// NoBreaker errors (one credential's quota) are retried but never counted, in
// Execute and in Report.
func TestNoBreakerVerdictIsRetriedButNotCounted(t *testing.T) {
	errQuota := errors.New("429 quota")
	cl := func(error) Verdict { return Verdict{Retryable: true, NoBreaker: true} }
	b := NewBreaker(Config{FailureThreshold: 1, Cooldown: time.Hour})
	calls := 0
	_ = Execute(context.Background(), b, Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Multiplier: 1, AttemptTimeout: time.Second}, cl, nil,
		func(context.Context, int) error { calls++; return errQuota })
	b.Report(errQuota, cl)
	if calls != 3 || !b.Allow() {
		t.Fatalf("calls=%d allow=%v; want 3 attempts and a closed breaker", calls, b.Allow())
	}
}
