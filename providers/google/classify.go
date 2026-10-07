package google

import (
	"context"
	"errors"
	"strings"

	"govoice/core/resilience"
)

// classify reads a Google error for the shared breaker and retry loop. Only
// client errors (bad credential, bad request, unknown model) and superseded or
// hung-up requests are exempt from the breaker; outages (5xx, Internal,
// Unavailable, timeouts) count.
func classify(err error) resilience.Verdict {
	if errors.Is(err, ErrGenerationMismatch) || errors.Is(err, context.Canceled) {
		return resilience.Verdict{ClientError: true}
	}
	pe := MapError("", err)
	if pe == nil {
		return resilience.Verdict{}
	}
	return resilience.Verdict{Retryable: pe.Retryable, ClientError: isClientError(pe), RetryAfter: pe.RetryAfter}
}

func isClientError(pe *ProviderError) bool {
	switch pe.BaseErr {
	case ErrProviderAuth, ErrProviderBadRequest, ErrRequestTooLarge:
		return true
	}
	// ponytail: same substring classification MapError uses for HTTP errors.
	return strings.Contains(pe.Error(), "404")
}
