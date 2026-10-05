package google

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Sentinel provider-neutral errors.
var (
	ErrProviderAuth               = errors.New("google provider: authentication failed")
	ErrProviderRateLimit          = errors.New("google provider: rate limit exceeded")
	ErrProviderUnavailable        = errors.New("google provider: service unavailable")
	ErrProviderTimeout            = errors.New("google provider: operation timed out")
	ErrProviderInternal           = errors.New("google provider: internal provider error")
	ErrProviderBadRequest         = errors.New("google provider: invalid request parameters")
	ErrCircuitOpen                = errors.New("google provider: circuit breaker open")
	ErrGenerationMismatch         = errors.New("google provider: stale generation rejected")
	ErrRetryCommitted             = errors.New("google provider: retry prohibited after committed output")
	ErrUnitTooLong                = errors.New("google provider: synthesis unit exceeds maximum character limit")
	ErrRequestTooLarge            = errors.New("google provider: request payload exceeds maximum byte limit")
	ErrQuotaExhausted             = errors.New("google provider: stream quota headroom exhausted")
	ErrTranscriptAssemblyCapacity = errors.New("google provider: transcript assembly capacity exhausted")
)

// ProviderError represents a sanitized, provider-neutral error with classification.
type ProviderError struct {
	Capability  string        // "stt", "llm", "tts"
	BaseErr     error         // Sentinel error
	SafeMessage string        // Sanitized diagnostic message
	StatusCode  int           // HTTP or gRPC status code
	Retryable   bool          // Whether error is eligible for retry
	RetryAfter  time.Duration // Provider retry-after delay if specified
}

func (e *ProviderError) Error() string {
	if e.SafeMessage != "" {
		return fmt.Sprintf("%s (%s: %s)", e.BaseErr.Error(), e.Capability, e.SafeMessage)
	}
	return fmt.Sprintf("%s (%s)", e.BaseErr.Error(), e.Capability)
}

func (e *ProviderError) Unwrap() error {
	return e.BaseErr
}

// MapError inspects any raw error from Google SDKs or network calls, sanitizes sensitive
// credentials, tokens, and payloads, and maps it into a classified ProviderError.
func MapError(capability string, err error) *ProviderError {
	if err == nil {
		return nil
	}

	var provErr *ProviderError
	if errors.As(err, &provErr) {
		return provErr
	}

	// Context errors
	if errors.Is(err, context.Canceled) {
		return &ProviderError{
			Capability:  capability,
			BaseErr:     context.Canceled,
			SafeMessage: "operation was cancelled",
			Retryable:   false,
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderTimeout,
			SafeMessage: "operation deadline exceeded",
			Retryable:   true,
		}
	}

	// Network errors
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return &ProviderError{
				Capability:  capability,
				BaseErr:     ErrProviderTimeout,
				SafeMessage: "network connection timed out",
				Retryable:   true,
			}
		}
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderUnavailable,
			SafeMessage: "transient network connection failure",
			Retryable:   true,
		}
	}

	// gRPC status code mapping
	if st, ok := status.FromError(err); ok {
		return mapGRPCStatus(capability, st)
	}

	// Fallback substring inspection for HTTP status or SDK errors
	msg := sanitizeErrorMessage(err.Error())
	lower := strings.ToLower(msg)

	switch {
	case strings.Contains(lower, "401") || strings.Contains(lower, "unauthenticated") ||
		strings.Contains(lower, "api key not valid") || strings.Contains(lower, "invalid_api_key"):
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderAuth,
			SafeMessage: "invalid or missing Google credentials",
			StatusCode:  http.StatusUnauthorized,
			Retryable:   false,
		}
	case strings.Contains(lower, "403") || strings.Contains(lower, "permissiondenied") ||
		strings.Contains(lower, "permission denied"):
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderAuth,
			SafeMessage: "access forbidden or permission denied",
			StatusCode:  http.StatusForbidden,
			Retryable:   false,
		}
	case strings.Contains(lower, "429") || strings.Contains(lower, "resource_exhausted") ||
		strings.Contains(lower, "quota exceeded") || strings.Contains(lower, "rate limit"):
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderRateLimit,
			SafeMessage: "quota or rate limit exceeded",
			StatusCode:  http.StatusTooManyRequests,
			Retryable:   true,
		}
	case strings.Contains(lower, "503") || strings.Contains(lower, "unavailable") ||
		strings.Contains(lower, "502") || strings.Contains(lower, "504"):
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderUnavailable,
			SafeMessage: "service temporarily unavailable",
			StatusCode:  http.StatusServiceUnavailable,
			Retryable:   true,
		}
	case strings.Contains(lower, "400") || strings.Contains(lower, "invalid_argument") ||
		strings.Contains(lower, "bad request"):
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderBadRequest,
			SafeMessage: "invalid request configuration or parameter",
			StatusCode:  http.StatusBadRequest,
			Retryable:   false,
		}
	default:
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderInternal,
			SafeMessage: msg,
			Retryable:   false,
		}
	}
}

func mapGRPCStatus(capability string, st *status.Status) *ProviderError {
	code := st.Code()
	safeMsg := sanitizeErrorMessage(st.Message())

	switch code {
	case codes.OK:
		return nil
	case codes.Canceled:
		return &ProviderError{
			Capability:  capability,
			BaseErr:     context.Canceled,
			SafeMessage: "operation cancelled by caller",
			StatusCode:  int(code),
			Retryable:   false,
		}
	case codes.DeadlineExceeded:
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderTimeout,
			SafeMessage: "gRPC deadline exceeded",
			StatusCode:  int(code),
			Retryable:   true,
		}
	case codes.Unauthenticated:
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderAuth,
			SafeMessage: "authentication failed",
			StatusCode:  int(code),
			Retryable:   false,
		}
	case codes.PermissionDenied:
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderAuth,
			SafeMessage: "permission denied for Google resource",
			StatusCode:  int(code),
			Retryable:   false,
		}
	case codes.ResourceExhausted:
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderRateLimit,
			SafeMessage: "rate limit or quota exhausted",
			StatusCode:  int(code),
			Retryable:   true,
		}
	case codes.Unavailable:
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderUnavailable,
			SafeMessage: "gRPC service unavailable",
			StatusCode:  int(code),
			Retryable:   true,
		}
	case codes.InvalidArgument:
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderBadRequest,
			SafeMessage: "invalid argument: " + safeMsg,
			StatusCode:  int(code),
			Retryable:   false,
		}
	default:
		return &ProviderError{
			Capability:  capability,
			BaseErr:     ErrProviderInternal,
			SafeMessage: safeMsg,
			StatusCode:  int(code),
			Retryable:   false,
		}
	}
}

// sanitizeErrorMessage strips potential tokens, keys, and private URLs from error strings.
func sanitizeErrorMessage(raw string) string {
	if raw == "" {
		return ""
	}
	// Redact API key query parameters
	cleaned := raw
	if idx := strings.Index(cleaned, "key="); idx != -1 {
		endIdx := strings.IndexAny(cleaned[idx:], " &\r\n\t")
		if endIdx == -1 {
			cleaned = cleaned[:idx] + "key=[REDACTED]"
		} else {
			cleaned = cleaned[:idx] + "key=[REDACTED]" + cleaned[idx+endIdx:]
		}
	}
	// Redact Bearer tokens
	if idx := strings.Index(cleaned, "Bearer "); idx != -1 {
		endIdx := strings.IndexAny(cleaned[idx+7:], " \r\n\t")
		if endIdx == -1 {
			cleaned = cleaned[:idx] + "Bearer [REDACTED]"
		} else {
			cleaned = cleaned[:idx] + "Bearer [REDACTED]" + cleaned[idx+7+endIdx:]
		}
	}
	// Limit error message length to prevent leaking huge response bodies
	const maxLen = 256
	if len(cleaned) > maxLen {
		cleaned = cleaned[:maxLen] + "..."
	}
	return cleaned
}
