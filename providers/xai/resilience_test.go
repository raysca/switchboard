package xai

import (
	"net/http"
	"strings"
	"testing"
)

func TestResilience_SecretRedaction(t *testing.T) {
	rawSecretKey := "xai-9876543210abcdef9876543210"
	rawBearer := "Bearer my-super-secret-token-12345"

	leakMsg := "failed connecting with " + rawBearer + " and key=" + rawSecretKey
	sanitized := SanitizeString(leakMsg)

	if strings.Contains(sanitized, rawSecretKey) {
		t.Fatalf("API key not redacted: %s", sanitized)
	}
	if strings.Contains(sanitized, "my-super-secret-token-12345") {
		t.Fatalf("Bearer token not redacted: %s", sanitized)
	}
	if !strings.Contains(sanitized, "xai-[REDACTED]") {
		t.Fatalf("expected xai-[REDACTED], got: %s", sanitized)
	}

	pe := &ProviderError{
		Capability: "llm",
		Code:       "AUTH_ERROR",
		Message:    "invalid token: " + rawBearer,
		Class:      ClassificationNonRetryable,
		StatusCode: http.StatusUnauthorized,
	}
	sanitizedErr := SanitizeError(pe)
	if strings.Contains(sanitizedErr.Error(), "my-super-secret-token-12345") {
		t.Fatalf("SanitizeError failed to redact Bearer token: %s", sanitizedErr.Error())
	}
}

// Barge-ins cancel requests constantly; they must never open the breaker.
