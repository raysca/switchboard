// Package elevenlabs is a streaming ElevenLabs text-to-speech adapter (stream-input
// websocket). Audio is requested as raw pcm_<rate> with character alignment.
package elevenlabs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"govoice/core/resilience"
)

var (
	ErrCircuitOpen   = resilience.ErrCircuitOpen // shared with every provider
	ErrMissingAPIKey = errors.New("elevenlabs: missing API key (ELEVENLABS_API_KEY)")
)

// ProviderError is a sanitized, classified ElevenLabs failure.
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
	return fmt.Sprintf("elevenlabs %s error [%s] (status %d, retryable=%v): %s", e.Capability, e.Code, e.StatusCode, e.Retryable, e.Message)
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
	if cs := websocket.CloseStatus(err); cs == websocket.StatusPolicyViolation && !transient(strings.ToLower(err.Error())) {
		out.Code, out.Retryable, out.ClientError = "CLIENT_ERROR", false, true
	}
	if resp != nil {
		classifyStatus(out, resp.StatusCode)
		if s, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && s > 0 {
			out.RetryAfter = time.Duration(s) * time.Second
		}
	}
	return out
}

// transient reports whether an error text names a load/availability problem
// (ElevenLabs sends e.g. "too_many_concurrent_requests" with code 1008).
// ponytail: keyword heuristic; the format is undocumented. Switch to an exact
// code table if ElevenLabs publishes one.
func transient(l string) bool {
	for _, k := range []string{"concurren", "rate_limit", "rate limit", "busy", "overload", "unavailable", "internal", "try again"} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// messageError maps an in-band error message ({"error"|"message", "code"}).
// The docs do not specify the format: load errors retry; auth, quota, bad
// voice/input and other 1008 policy errors are the caller's fault; anything else retries.
func messageError(kind, msg string, code int) *ProviderError {
	out := &ProviderError{Capability: "tts", Code: "ERROR", Message: SanitizeString(strings.TrimSpace(kind + " " + msg)), Retryable: true}
	if len(out.Message) > 512 {
		out.Message = out.Message[:512] + "... [truncated]"
	}
	l := strings.ToLower(kind + " " + msg)
	switch {
	case transient(l):
		out.Code = "UNAVAILABLE"
	case code == 1008 || strings.Contains(l, "auth") || strings.Contains(l, "api key") || strings.Contains(l, "api_key") ||
		strings.Contains(l, "quota") || strings.Contains(l, "invalid") || strings.Contains(l, "not_found") ||
		strings.Contains(l, "not found") || strings.Contains(l, "payment") || strings.Contains(l, "character_limit"):
		out.Code, out.Retryable, out.ClientError = "CLIENT_ERROR", false, true
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

// sttMessageError maps a realtime-STT error message (message_type + error text,
// per the ElevenLabs realtime STT reference). Auth, quota, terms and malformed
// input are the caller's fault; throttling, capacity and transcriber failures
// are outages. Unknown types retry.
func sttMessageError(kind, msg string) *ProviderError {
	out := &ProviderError{Capability: "stt", Code: "UNAVAILABLE", Retryable: true, Message: SanitizeString(strings.TrimSpace(kind + ": " + msg))}
	if len(out.Message) > 512 {
		out.Message = out.Message[:512] + "... [truncated]"
	}
	switch kind {
	case "auth_error", "unaccepted_terms":
		out.Code, out.Retryable, out.ClientError, out.StatusCode = "UNAUTHORIZED", false, true, 401
	case "quota_exceeded":
		out.Code, out.Retryable, out.ClientError, out.StatusCode = "QUOTA_EXCEEDED", false, true, 402
	case "input_error", "invalid_request", "chunk_size_exceeded":
		out.Code, out.Retryable, out.ClientError, out.StatusCode = "BAD_REQUEST", false, true, 400
	case "rate_limited", "commit_throttled":
		out.Code, out.StatusCode = "RATE_LIMIT_EXCEEDED", 429
	}
	return out
}

// sttErrorKinds are the server error message types we surface as failures.
var sttErrorKinds = map[string]bool{
	"error": true, "auth_error": true, "quota_exceeded": true, "commit_throttled": true, "unaccepted_terms": true,
	"rate_limited": true, "queue_overflow": true, "resource_exhausted": true, "input_error": true,
	"invalid_request": true, "chunk_size_exceeded": true, "transcriber_error": true,
}
