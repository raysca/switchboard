package google

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestErrorMapping(t *testing.T) {
	// 1. gRPC status mappings
	unauthErr := status.Error(codes.Unauthenticated, "bad token")
	mappedUnauth := MapError("stt", unauthErr)
	if !errors.Is(mappedUnauth, ErrProviderAuth) {
		t.Fatalf("expected ErrProviderAuth, got: %v", mappedUnauth)
	}
	if mappedUnauth.Retryable {
		t.Fatal("auth error should not be retryable")
	}

	resExhErr := status.Error(codes.ResourceExhausted, "quota exceeded")
	mappedQuota := MapError("llm", resExhErr)
	if !errors.Is(mappedQuota, ErrProviderRateLimit) {
		t.Fatalf("expected ErrProviderRateLimit, got: %v", mappedQuota)
	}
	if !mappedQuota.Retryable {
		t.Fatal("quota error should be retryable")
	}

	unavailErr := status.Error(codes.Unavailable, "backend down")
	mappedUnavail := MapError("tts", unavailErr)
	if !errors.Is(mappedUnavail, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable, got: %v", mappedUnavail)
	}
	if !mappedUnavail.Retryable {
		t.Fatal("unavailable error should be retryable")
	}

	// 2. HTTP error string mappings
	http401 := fmt.Errorf("googleapi: got HTTP response code 401 with body: API key not valid")
	mapped401 := MapError("llm", http401)
	if !errors.Is(mapped401, ErrProviderAuth) {
		t.Fatalf("expected ErrProviderAuth, got: %v", mapped401)
	}

	http429 := fmt.Errorf("HTTP 429 Too Many Requests: resource exhausted")
	mapped429 := MapError("stt", http429)
	if !errors.Is(mapped429, ErrProviderRateLimit) {
		t.Fatalf("expected ErrProviderRateLimit, got: %v", mapped429)
	}

	// 3. Sensitive secret redaction
	leakyErr := fmt.Errorf("request to https://speech.googleapis.com?key=AIzaSyD123456789SecretKey failed with Bearer ya29.a0AfH6_SecretBearerToken")
	mappedLeaky := MapError("stt", leakyErr)
	errStr := mappedLeaky.Error()
	if strings.Contains(errStr, "AIzaSyD123456789SecretKey") {
		t.Fatal("API key was not redacted from error message!")
	}
	if strings.Contains(errStr, "ya29.a0AfH6_SecretBearerToken") {
		t.Fatal("Bearer token was not redacted from error message!")
	}
	if !strings.Contains(errStr, "key=[REDACTED]") {
		t.Fatalf("expected key=[REDACTED], got: %s", errStr)
	}
}
