package google

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// RetryPolicy defines backoff and attempt boundaries.
type RetryPolicy struct {
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	Multiplier     float64
	MaxAttempts    int
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     2000 * time.Millisecond,
		Multiplier:     2.0,
		MaxAttempts:    3,
	}
}

// sleep waits d or until ctx ends. Tests swap it to observe backoff.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ExecuteWithRetry executes an operation according to the retry policy, circuit breaker,
// and committed-output protection rules.
// - If committed() returns true at any point, no further retry is permitted.
// - If circuit is open, returns ErrCircuitOpen.
// - Honors Retry-After and applies capped exponential backoff with full jitter.
func ExecuteWithRetry(
	ctx context.Context,
	cb *CircuitBreaker,
	policy RetryPolicy,
	committed func() bool,
	op func(ctx context.Context, attempt int) error,
) error {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 1
	}

	backoff := policy.InitialBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
	}
	if policy.MaxBackoff <= 0 {
		policy.MaxBackoff = 2000 * time.Millisecond
	}
	if policy.Multiplier <= 1.0 {
		policy.Multiplier = 2.0
	}

	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		// Circuit breaker check
		var probe uint64
		if cb != nil {
			var ok bool
			if ok, probe = cb.allow(); !ok {
				return ErrCircuitOpen
			}
		}

		// Execute operation
		err := op(ctx, attempt)
		if err == nil {
			if cb != nil {
				cb.ReportSuccess()
			}
			return nil
		}

		// A cancelled or superseded request (barge-in), or a caller's deadline
		// (e.g. a slow tool round), is not a provider failure.
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrGenerationMismatch) {
			if cb != nil {
				cb.release(probe)
			}
			return err
		}

		// Map error to classified ProviderError
		provErr := MapError("retry", err)

		if cb != nil {
			cb.reportError(provErr, probe)
		}

		// Check if user-visible output has been committed
		if committed != nil && committed() {
			// CRITICAL: No retries after committed user-visible output!
			return ErrRetryCommitted
		}

		// Check if error is retryable
		if !provErr.Retryable || attempt >= policy.MaxAttempts {
			return provErr
		}

		// Calculate backoff delay with jitter
		delay := calculateDelay(backoff, provErr.RetryAfter, policy.MaxBackoff)

		// Check deadline
		if deadline, ok := ctx.Deadline(); ok {
			if time.Until(deadline) <= delay {
				// Not enough time to retry before deadline expires
				return provErr
			}
		}

		// Sleep
		if err := sleep(ctx, delay); err != nil {
			return err
		}

		// Increase backoff for next attempt
		backoff = time.Duration(float64(backoff) * policy.Multiplier)
		if backoff > policy.MaxBackoff {
			backoff = policy.MaxBackoff
		}
	}

	return ErrProviderTimeout
}

func calculateDelay(backoff time.Duration, retryAfter time.Duration, maxBackoff time.Duration) time.Duration {
	delay := backoff
	if retryAfter > 0 {
		delay = retryAfter
	}
	// Full jitter: uniformly random between 0 and delay
	jittered := time.Duration(rand.Float64() * float64(delay))
	if jittered > maxBackoff {
		jittered = maxBackoff
	}
	return jittered
}
