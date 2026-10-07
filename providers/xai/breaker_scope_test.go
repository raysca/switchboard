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
		{&ProviderError{Code: "RATE_LIMITED", Class: ClassificationRateLimit, StatusCode: 429}, true},
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
