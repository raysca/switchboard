package xai

import (
	"errors"

	"govoice/core/resilience"
)

// classify reads an xAI error for the shared breaker and retry loop: a
// non-retryable ProviderError (bad key, bad request, unknown model) is the
// caller's fault and exempt from the breaker; anything else, including raw
// network failures, is an outage worth retrying.
func classify(err error) resilience.Verdict {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return resilience.Verdict{Retryable: pe.Class != ClassificationNonRetryable, ClientError: pe.Class == ClassificationNonRetryable, NoBreaker: pe.Class == ClassificationRateLimit, RetryAfter: pe.RetryAfter}
	}
	return resilience.Verdict{Retryable: true}
}
