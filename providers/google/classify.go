package google

import (
	"context"
	"errors"

	"govoice/core/resilience"
)

// classify reads a Google error for the shared breaker and retry loop. Only
// client errors (bad credential, bad request, unknown model) and superseded or
// hung-up requests are exempt from the breaker; outages (5xx, Internal,
// Unavailable, timeouts) count. Quota and rate limits are retried and failed
// over but never counted: they belong to one tenant's project, not to Google's
// health, and must not open a breaker every tenant shares.
func classify(err error) resilience.Verdict {
	if errors.Is(err, ErrGenerationMismatch) || errors.Is(err, context.Canceled) {
		return resilience.Verdict{ClientError: true}
	}
	pe := MapError("", err)
	if pe == nil {
		return resilience.Verdict{}
	}
	return resilience.Verdict{Retryable: pe.Retryable, ClientError: isClientError(pe), NoBreaker: errors.Is(pe.BaseErr, ErrProviderRateLimit), RetryAfter: pe.RetryAfter}
}

func isClientError(pe *ProviderError) bool {
	switch pe.BaseErr {
	case ErrProviderAuth, ErrProviderBadRequest, ErrRequestTooLarge:
		return true
	}
	return false
}
