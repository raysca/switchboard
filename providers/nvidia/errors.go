package nvidia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"govoice/core/resilience"
)

var (
	ErrCircuitOpen         = resilience.ErrCircuitOpen // shared with every provider
	ErrProviderUnavailable = errors.New("nvidia: provider unavailable")
	ErrMissingFunctionID   = errors.New("nvidia: function_id is required for the hosted endpoint")
	ErrMissingAPIKey       = errors.New("nvidia: api_key is required for the hosted endpoint")
)

// ProviderError is a sanitized, classified Riva failure.
type ProviderError struct {
	Capability  string // "stt", "tts"
	Code        string // gRPC code name, or TIMEOUT / ERROR
	Message     string
	GRPCCode    codes.Code
	Retryable   bool
	ClientError bool // the caller's fault (bad key, bad request): not an outage
	RetryAfter  time.Duration
	Cause       error
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("nvidia %s error [%s] (retryable=%v): %s", e.Capability, e.Code, e.Retryable, e.Message)
}
func (e *ProviderError) Unwrap() error { return e.Cause }

// nvapi-... keys, bearer tokens and api_key=... fragments.
var secretPattern = regexp.MustCompile(`(?i)(bearer\s+|api[_-]?key[:=]\s*)[A-Za-z0-9_\-\.]{8,}|\bnvapi-[A-Za-z0-9_\-]{8,}`)

// SanitizeString strips credentials from text bound for logs or errors.
func SanitizeString(s string) string { return secretPattern.ReplaceAllString(s, "[REDACTED]") }

// MapError classifies any Riva error. Unauthenticated, PermissionDenied,
// InvalidArgument and friends are the caller's; Unavailable, DeadlineExceeded,
// ResourceExhausted, Internal and unknown failures are outages.
func MapError(capability string, err error) *ProviderError {
	if err == nil {
		return nil
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	out := &ProviderError{Capability: capability, Code: "ERROR", Message: SanitizeString(err.Error()), Retryable: true, Cause: err}
	switch {
	case errors.Is(err, ErrCircuitOpen):
		out.Code, out.Retryable = "CIRCUIT_OPEN", false
	// gRPC flattens the dialer's error into an Unavailable status message.
	case errors.Is(err, ErrBlockedAddress), strings.Contains(err.Error(), ErrBlockedAddress.Error()):
		out.Code, out.Retryable, out.ClientError = "BLOCKED_ADDRESS", false, true
	case errors.Is(err, resilience.ErrTimeout):
		out.Code = "TIMEOUT"
	case errors.Is(err, context.Canceled):
		out.Code, out.Retryable, out.ClientError = "CANCELED", false, true
	}
	if out.Code == "ERROR" {
		if s, ok := status.FromError(err); ok && s.Code() != codes.OK {
			out.GRPCCode, out.Code = s.Code(), s.Code().String()
			out.Message = SanitizeString(s.Message())
			switch s.Code() {
			case codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument, codes.NotFound,
				codes.FailedPrecondition, codes.OutOfRange, codes.Unimplemented, codes.AlreadyExists:
				out.Retryable, out.ClientError = false, true
			case codes.Canceled:
				out.Retryable, out.ClientError = false, true
			}
		}
	}
	if len(out.Message) > 512 {
		out.Message = out.Message[:512] + "... [truncated]"
	}
	return out
}

// classify reads an error for the shared breaker and retry loop.
func classify(err error) resilience.Verdict {
	if errors.Is(err, context.Canceled) {
		return resilience.Verdict{ClientError: true}
	}
	pe := MapError("", err)
	return resilience.Verdict{Retryable: pe.Retryable, ClientError: pe.ClientError, NoBreaker: pe.GRPCCode == codes.ResourceExhausted, RetryAfter: pe.RetryAfter}
}

// streamEnded reports a normal end of a Riva stream (server EOF).
func streamEnded(err error) bool { return errors.Is(err, io.EOF) }
