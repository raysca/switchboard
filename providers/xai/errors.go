package xai

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ErrorClassification defines the retryability of a provider failure.
type ErrorClassification int

const (
	ClassificationNonRetryable ErrorClassification = iota
	ClassificationRetryable
	ClassificationRateLimit
)

func (c ErrorClassification) String() string {
	switch c {
	case ClassificationNonRetryable:
		return "NON_RETRYABLE"
	case ClassificationRetryable:
		return "RETRYABLE"
	case ClassificationRateLimit:
		return "RATE_LIMIT"
	default:
		return "UNKNOWN"
	}
}

var (
	ErrCircuitOpen          = errors.New("xai: circuit breaker is open")
	ErrRetryCommitted       = errors.New("xai: retry prohibited after output committed")
	ErrMissingAPIKey        = errors.New("xai: missing API key (XAI_API_KEY)")
	ErrProviderUnavailable  = errors.New("xai: provider unavailable")
	ErrMissingTerminalEvent = errors.New("xai sse: stream ended without terminal completion event")
	ErrOversizedLine        = errors.New("xai sse: line exceeded maximum size")
	ErrOversizedEvent       = errors.New("xai sse: event exceeded maximum size")
	ErrOversizedBody        = errors.New("xai sse: total response body exceeded maximum size")
	ErrInvalidEvent         = errors.New("xai sse: invalid event format")
)

// ProviderError is a safe, sanitized error representation.
type ProviderError struct {
	Capability string
	Code       string
	Message    string
	Class      ErrorClassification
	StatusCode int
	RetryAfter time.Duration
	Cause      error
}

func (e *ProviderError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("xai %s error [%s] (status %d, retryable=%s, retry-after=%v): %s",
			e.Capability, e.Code, e.StatusCode, e.Class, e.RetryAfter, e.Message)
	}
	return fmt.Sprintf("xai %s error [%s] (status %d, retryable=%s): %s",
		e.Capability, e.Code, e.StatusCode, e.Class, e.Message)
}

func (e *ProviderError) Unwrap() error {
	return e.Cause
}

var (
	bearerPattern = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9_\-\.]{8,}`)
	keyPattern    = regexp.MustCompile(`(?i)(api[_-]?key[:=]\s*)[A-Za-z0-9_\-\.]{8,}`)
	xaiKeyPattern = regexp.MustCompile(`(?i)xai-[a-zA-Z0-9_-]{16,}`)
)

// SanitizeString strips API keys, tokens, and credentials from log strings.
func SanitizeString(s string) string {
	s = bearerPattern.ReplaceAllString(s, "${1}[REDACTED]")
	s = keyPattern.ReplaceAllString(s, "${1}[REDACTED]")
	s = xaiKeyPattern.ReplaceAllString(s, "xai-[REDACTED]")
	return s
}

// SanitizeError returns a sanitized error safe for user logs and metrics.
func SanitizeError(err error) error {
	if err == nil {
		return nil
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		sanitized := *pe
		sanitized.Message = SanitizeString(pe.Message)
		return &sanitized
	}
	return errors.New(SanitizeString(err.Error()))
}

// ClassifyHTTPResponse determines the ErrorClassification and RetryAfter from an HTTP response.
func ClassifyHTTPResponse(resp *http.Response) (ErrorClassification, time.Duration) {
	if resp == nil {
		return ClassificationRetryable, 0
	}

	var retryAfter time.Duration
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if d, err := time.ParseDuration(ra + "s"); err == nil && d > 0 {
			retryAfter = d
		}
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return ClassificationRateLimit, retryAfter
	}

	if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
		// Server errors are transient and retryable
		return ClassificationRetryable, retryAfter
	}

	if resp.StatusCode == http.StatusBadRequest ||
		resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusNotFound {
		// Client errors are non-retryable
		return ClassificationNonRetryable, 0
	}

	return ClassificationNonRetryable, 0
}

// MapHTTPError creates a safe ProviderError from an HTTP error response.
func MapHTTPError(capability string, resp *http.Response, body string, cause error) *ProviderError {
	class, retryAfter := ClassifyHTTPResponse(resp)
	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}

	code := "HTTP_ERROR"
	switch statusCode {
	case http.StatusBadRequest:
		code = "BAD_REQUEST"
	case http.StatusUnauthorized:
		code = "UNAUTHORIZED"
	case http.StatusForbidden:
		code = "FORBIDDEN"
	case http.StatusNotFound:
		code = "NOT_FOUND"
	case http.StatusTooManyRequests:
		code = "RATE_LIMIT_EXCEEDED"
	case http.StatusBadGateway:
		code = "BAD_GATEWAY"
	case http.StatusServiceUnavailable:
		code = "SERVICE_UNAVAILABLE"
	case http.StatusGatewayTimeout:
		code = "GATEWAY_TIMEOUT"
	}

	msg := SanitizeString(body)
	if msg == "" && cause != nil {
		msg = SanitizeString(cause.Error())
	}
	if len(msg) > 512 {
		msg = msg[:512] + "... [truncated]"
	}

	return &ProviderError{
		Capability: capability,
		Code:       code,
		Message:    strings.TrimSpace(msg),
		Class:      class,
		StatusCode: statusCode,
		RetryAfter: retryAfter,
		Cause:      cause,
	}
}

// MapError sanitizes any error and returns a classified ProviderError.
func MapError(capability string, err error) *ProviderError {
	if err == nil {
		return nil
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	return &ProviderError{
		Capability: capability,
		Code:       "ERROR",
		Message:    SanitizeString(err.Error()),
		Class:      ClassificationNonRetryable,
		Cause:      err,
	}
}
