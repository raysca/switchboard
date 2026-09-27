package xai

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// RetryPolicy defines capped exponential backoff with full jitter.
type RetryPolicy struct {
	InitialBackoff time.Duration // default 100ms
	MaxBackoff     time.Duration // default 2000ms
	Multiplier     float64       // default 2.0
	MaxAttempts    int           // default 3
}

func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		InitialBackoff: 100 * time.Millisecond,
		MaxBackoff:     2000 * time.Millisecond,
		Multiplier:     2.0,
		MaxAttempts:    3,
	}
}

// ComputeBackoff calculates jittered backoff for attempt (0-indexed).
func (p RetryPolicy) ComputeBackoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > p.MaxBackoff {
			return p.MaxBackoff
		}
		return retryAfter
	}

	backoff := float64(p.InitialBackoff)
	for i := 0; i < attempt; i++ {
		backoff *= p.Multiplier
	}
	if backoff > float64(p.MaxBackoff) {
		backoff = float64(p.MaxBackoff)
	}

	// Full jitter: uniform random in [0, backoff]
	jittered := rand.Float64() * backoff
	return time.Duration(jittered)
}

// ExecuteWithRetry runs an operation with retry logic, honoring Retry-After, circuit state,
// and the strict prohibition of retrying after output has been committed.
func ExecuteWithRetry(
	ctx context.Context,
	cb *CircuitBreaker,
	policy RetryPolicy,
	committed *bool,
	fn func(attempt int) error,
) error {
	if policy.MaxAttempts <= 0 {
		policy = DefaultRetryPolicy()
	}

	var lastErr error

	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		if cb != nil && !cb.Allow() {
			return ErrCircuitOpen
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := fn(attempt)
		if err == nil {
			if cb != nil {
				cb.RecordSuccess()
			}
			return nil
		}

		lastErr = err

		// Strict invariant: no retries after committed user-visible output
		if committed != nil && *committed {
			if cb != nil {
				cb.RecordFailure()
			}
			return errors.Join(ErrRetryCommitted, lastErr)
		}

		// Check if error is retryable
		var pe *ProviderError
		if errors.As(err, &pe) {
			if pe.Class == ClassificationNonRetryable {
				if cb != nil {
					cb.RecordFailure()
				}
				return lastErr
			}
		}

		if cb != nil {
			cb.RecordFailure()
		}

		if attempt == policy.MaxAttempts-1 {
			break
		}

		var retryAfter time.Duration
		if pe != nil {
			retryAfter = pe.RetryAfter
		}
		sleepDuration := policy.ComputeBackoff(attempt, retryAfter)

		timer := time.NewTimer(sleepDuration)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}

	return lastErr
}
