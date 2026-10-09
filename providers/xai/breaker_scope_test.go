package xai

import (
	"context"
	"errors"
	"testing"
	"time"

	"govoice/core/resilience"
)

// xAI's classification for the shared breaker: a non-retryable ProviderError
// (bad key, bad request) is exempt; outages and raw network errors count.
func TestClassifyForSharedBreaker(t *testing.T) {
	for _, tc := range []struct {
		err    error
		counts bool
	}{
		{&ProviderError{Code: "UNAUTHORIZED", Class: ClassificationNonRetryable, StatusCode: 401}, false},
		{&ProviderError{Code: "UNAVAILABLE", Class: ClassificationRetryable, StatusCode: 503}, true},
		// 429 is one credential's quota, not provider health: retried, never counted.
		{&ProviderError{Code: "RATE_LIMITED", Class: ClassificationRateLimit, StatusCode: 429}, false},
		{errors.New("connection reset"), true},
	} {
		b := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
		_ = resilience.Execute(context.Background(), b, resilience.Policy{MaxAttempts: 1}, classify, nil, func(context.Context, int) error { return tc.err })
		b.Report(tc.err, classify) // the STT path
		if opened := !b.Allow(); opened != tc.counts {
			t.Errorf("%v: breaker opened=%v, want %v", tc.err, opened, tc.counts)
		}
	}
}

// classify keeps xAI's retry hints: rate limits and outages are retried with
// the provider's Retry-After; client errors are not retried.
func TestClassifyRetryHints(t *testing.T) {
	v := classify(&ProviderError{Class: ClassificationRateLimit, RetryAfter: 3 * time.Second})
	if !v.Retryable || v.RetryAfter != 3*time.Second || v.ClientError {
		t.Fatalf("rate limit: %+v", v)
	}
	if v := classify(&ProviderError{Class: ClassificationNonRetryable}); v.Retryable {
		t.Fatalf("client error retried: %+v", v)
	}
}

// Timeout is the single knob for xAI's per-attempt deadline, whatever retry
// policy the caller passes, and retries are on by default.
func TestTimeoutIsTheAttemptDeadline(t *testing.T) {
	llm, err := NewProcessor(Config{APIKey: "k", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if llm.cfg.RetryPolicy.AttemptTimeout != 5*time.Second || llm.cfg.RetryPolicy.MaxAttempts != 3 {
		t.Fatalf("llm policy %+v", llm.cfg.RetryPolicy)
	}
}
