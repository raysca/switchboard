package openai

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"govoice/core/resilience"
)

var (
	ErrMissingTerminalEvent = errors.New("openai sse: stream ended without [DONE]")
	ErrOversizedBody        = errors.New("openai sse: response exceeded maximum size")
	ErrInvalidEvent         = errors.New("openai sse: invalid event")
	// ErrEmptyReply: the stream ended without any spoken text, typically a
	// reasoning model that spent its budget thinking.
	ErrEmptyReply = errors.New("openai llm: model returned no text (reasoning only or empty reply); use a non-reasoning model or raise its output budget")
)

// ProviderError is a sanitized HTTP failure. Retryable covers 5xx and 429;
// other 4xx (bad key, unknown model, bad request) are the caller's fault.
type ProviderError struct {
	Status     int
	Message    string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("openai llm error (status %d, retryable=%v): %s", e.Status, e.Retryable, e.Message)
}

// ClientError reports a caller-side failure (bad key, unknown model).
func (e *ProviderError) ClientError() bool { return !e.Retryable }

// sanitizeErr redacts secrets; with nothing to redact it keeps err's chain.
func sanitizeErr(err error) error {
	if msg := SanitizeString(err.Error()); msg != err.Error() {
		return errors.New(msg)
	}
	return err
}

var secretPattern = regexp.MustCompile(`(?i)(bearer\s+|api[_-]?key[:=]\s*)[A-Za-z0-9_\-\.]{8,}|\b(sk|gsk|xai|nvapi)-[A-Za-z0-9_-]{16,}`)

// SanitizeString strips API keys and bearer tokens from text bound for logs.
func SanitizeString(s string) string { return secretPattern.ReplaceAllString(s, "[REDACTED]") }

func newHTTPError(resp *http.Response, body, model string) *ProviderError {
	pe := &ProviderError{Status: resp.StatusCode, Message: strings.TrimSpace(SanitizeString(body))}
	if len(pe.Message) > 512 {
		pe.Message = pe.Message[:512] + "... [truncated]"
	}
	// Hosted catalogs (NIM) retire models: 404/410 here is the operator's to fix.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		pe.Message = fmt.Sprintf("model %q not found or deprecated (or wrong base_url): set the model explicitly; %s", model, pe.Message)
	}
	pe.Retryable = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	if d, err := time.ParseDuration(resp.Header.Get("Retry-After") + "s"); err == nil && d > 0 {
		pe.RetryAfter = d
	}
	return pe
}

// classify feeds the shared breaker and retry loop: client errors are exempt
// from the breaker, and so is a 429 (one key's quota, not an outage); everything
// else (5xx, network) counts.
func classify(err error) resilience.Verdict {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return resilience.Verdict{Retryable: pe.Retryable, ClientError: !pe.Retryable, NoBreaker: pe.Status == http.StatusTooManyRequests, RetryAfter: pe.RetryAfter}
	}
	return resilience.Verdict{Retryable: true}
}
