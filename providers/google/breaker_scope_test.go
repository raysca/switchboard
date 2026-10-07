package google

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"govoice/core/resilience"
)

// Google's classification for the shared breaker: client errors (bad key,
// bad request, unknown model) and superseded or hung-up requests are exempt;
// outages count, including the 500/Internal/Unknown errors MapError calls
// non-retryable.
func TestClassifyForSharedBreaker(t *testing.T) {
	for _, tc := range []struct {
		err    error
		counts bool
	}{
		{errors.New("401 unauthenticated"), false},
		{errors.New("400 bad request: unknown model"), false},
		{status.Error(codes.NotFound, "model"), false},
		{ErrGenerationMismatch, false},
		{context.Canceled, false},
		{errors.New("503 unavailable"), true},
		{errors.New("Error 500, Message: internal error, Status: INTERNAL"), true},
		{status.Error(codes.Internal, "boom"), true},
		{status.Error(codes.Unknown, "boom"), true},
	} {
		b := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
		_ = resilience.Execute(context.Background(), b, resilience.Policy{MaxAttempts: 1}, classify, nil, func(context.Context, int) error { return tc.err })
		b.Report(tc.err, classify) // the STT path
		if opened := !b.Allow(); opened != tc.counts {
			t.Errorf("%v: breaker opened=%v, want %v", tc.err, opened, tc.counts)
		}
	}
}

// A timed-out request maps to ErrProviderTimeout for the error frame.
func TestTimeoutMapsToProviderTimeout(t *testing.T) {
	err := resilience.Execute(context.Background(), nil, resilience.Policy{MaxAttempts: 1, AttemptTimeout: time.Millisecond}, classify, nil,
		func(ctx context.Context, _ int) error { <-ctx.Done(); return ctx.Err() })
	if pe := MapError("llm", err); !errors.Is(pe, ErrProviderTimeout) || !pe.Retryable {
		t.Fatalf("mapped %v, want a retryable ErrProviderTimeout", pe)
	}
}
