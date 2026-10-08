// Package cartesia is a streaming Cartesia text-to-speech adapter. Audio is
// requested as raw pcm_s16le at the pipeline's TTS rate, with word timestamps.
package cartesia

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
	ErrMissingAPIKey       = errors.New("cartesia: missing API key (CARTESIA_API_KEY)")
	ErrProviderUnavailable = errors.New("cartesia: provider unavailable")
)

// ProviderError is a sanitized, classified Cartesia failure.
type ProviderError struct {
	Capability  string
	Code        string
	Message     string
	StatusCode  int
	Retryable   bool
	ClientError bool // the caller's fault (bad key, bad voice, bad request): not an outage
	RetryAfter  time.Duration
	Cause       error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("cartesia %s error [%s] (status %d, retryable=%v): %s", e.Capability, e.Code, e.StatusCode, e.Retryable, e.Message)
}
func (e *ProviderError) Unwrap() error { return e.Cause }

var secretPattern = regexp.MustCompile(`(?i)((?:token|bearer)\s+|api[_-]?key[:=]\s*|x-api-key[:=]\s*)[A-Za-z0-9_\-\.]{8,}`)

// SanitizeString strips credentials from text bound for logs or errors.
func SanitizeString(s string) string { return secretPattern.ReplaceAllString(s, "${1}[REDACTED]") }

// classifyStatus fills Code/Retryable/ClientError from an HTTP-style status.
func classifyStatus(out *ProviderError, status int) {
	out.StatusCode = status
	switch {
	case status == http.StatusTooManyRequests:
		out.Code, out.Retryable = "RATE_LIMIT_EXCEEDED", true
	case status >= 500:
		out.Code, out.Retryable = "UNAVAILABLE", true
	case status == 401 || status == 403:
		out.Code, out.Retryable, out.ClientError = "UNAUTHORIZED", false, true
	case status >= 400:
		out.Code, out.Retryable, out.ClientError = "CLIENT_ERROR", false, true
	}
}

// MapError classifies a transport error. resp is the failed upgrade response, if any.
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
	if resp != nil {
		classifyStatus(out, resp.StatusCode)
		if s, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && s > 0 {
			out.RetryAfter = time.Duration(s) * time.Second
		}
	} else if websocket.CloseStatus(err) == websocket.StatusPolicyViolation { // 1008: bad params/audio
		out.Code, out.Retryable, out.ClientError = "BAD_REQUEST", false, true
	}
	return out
}

// messageError maps an in-band {"type":"error"} message.
func messageError(capability string, status int, msg string) *ProviderError {
	out := &ProviderError{Capability: capability, Code: "ERROR", Message: SanitizeString(msg), Retryable: true}
	if len(out.Message) > 512 {
		out.Message = out.Message[:512] + "... [truncated]"
	}
	classifyStatus(out, status)
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
