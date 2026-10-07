// Package deepgram is a streaming Deepgram speech-to-text adapter. Server-side
// endpointing is off: the pipeline owns turn boundaries and finalizes the
// stream when the turn ends.
package deepgram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"govoice/core/resilience"
)

var (
	ErrCircuitOpen         = resilience.ErrCircuitOpen // shared with every provider
	ErrMissingAPIKey       = errors.New("deepgram: missing API key (DEEPGRAM_API_KEY)")
	ErrProviderUnavailable = errors.New("deepgram: provider unavailable")
)

// ProviderError is a sanitized, classified Deepgram failure.
type ProviderError struct {
	Capability  string
	Code        string
	Message     string
	StatusCode  int
	Retryable   bool
	ClientError bool // the caller's fault (bad key, bad request): not an outage
	RetryAfter  time.Duration
	Cause       error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("deepgram %s error [%s] (status %d, retryable=%v): %s", e.Capability, e.Code, e.StatusCode, e.Retryable, e.Message)
}
func (e *ProviderError) Unwrap() error { return e.Cause }

var secretPattern = regexp.MustCompile(`(?i)((?:token|bearer)\s+|api[_-]?key[:=]\s*)[A-Za-z0-9_\-\.]{8,}`)

// SanitizeString strips credentials from text bound for logs or errors.
func SanitizeString(s string) string { return secretPattern.ReplaceAllString(s, "${1}[REDACTED]") }

// MapError classifies any STT error. resp is the failed upgrade response, if any.
func MapError(capability string, err error, resp *http.Response) *ProviderError {
	if err == nil {
		return nil
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	out := &ProviderError{Capability: capability, Code: "ERROR", Message: SanitizeString(err.Error()), Retryable: true, Cause: err}
	if len(out.Message) > 512 {
		out.Message = out.Message[:512] + "... [truncated]"
	}
	switch {
	case resp != nil:
		out.StatusCode = resp.StatusCode
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			out.Code, out.Retryable = "RATE_LIMIT_EXCEEDED", true
			if s, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && s > 0 {
				out.RetryAfter = time.Duration(s) * time.Second
			}
		case resp.StatusCode >= 500:
			out.Code = "UNAVAILABLE"
		case resp.StatusCode >= 400:
			out.Code, out.Retryable, out.ClientError = "CLIENT_ERROR", false, true
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				out.Code = "UNAUTHORIZED"
			}
		}
	case websocket.CloseStatus(err) == websocket.StatusPolicyViolation: // 1008: bad audio/params
		out.Code, out.Retryable, out.ClientError = "BAD_REQUEST", false, true
	}
	return out
}

// classify reads an error for the shared breaker and retry loop.
func classify(err error) resilience.Verdict {
	if errors.Is(err, context.Canceled) {
		return resilience.Verdict{ClientError: true}
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return resilience.Verdict{Retryable: pe.Retryable, ClientError: pe.ClientError, RetryAfter: pe.RetryAfter}
	}
	return resilience.Verdict{Retryable: true}
}
