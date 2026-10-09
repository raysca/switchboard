package google

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/genai"

	"govoice/core/resilience"
)

// M3-05: Gemini HTTP errors are classified by status code, not by words in the
// message.
func TestMapErrorClassifiesGenaiAPIErrorByStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		retryable bool
		client    bool
		noBreaker bool
	}{
		{"500 INTERNAL is transient", genai.APIError{Code: 500, Status: "INTERNAL", Message: "An internal error has occurred. Please retry."}, true, false, false},
		{"503", genai.APIError{Code: 503, Status: "UNAVAILABLE", Message: "overloaded"}, true, false, false},
		{"504", genai.APIError{Code: 504, Status: "DEADLINE_EXCEEDED"}, true, false, false},
		{"400 is never an outage, even if it says unavailable", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "model is unavailable in this region"}, false, true, false},
		{"400 bad key", genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "API key not valid. Please pass a valid API key."}, false, true, false},
		{"429 quota naming 401 is not auth", genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "Quota exceeded for metric ...-401 per minute"}, true, false, true},
		{"401", genai.APIError{Code: 401, Status: "UNAUTHENTICATED"}, false, true, false},
		{"404 unknown model", genai.APIError{Code: 404, Status: "NOT_FOUND", Message: "models/x is not found"}, false, true, false},
		{"500 mentioning 404 stays an outage", genai.APIError{Code: 500, Status: "INTERNAL", Message: "backend 404 upstream"}, true, false, false},
		{"wrapped", fmt.Errorf("generate: %w", genai.APIError{Code: 500, Status: "INTERNAL"}), true, false, false},
	} {
		v := classify(tc.err)
		if v.Retryable != tc.retryable || v.ClientError != tc.client || v.NoBreaker != tc.noBreaker {
			t.Errorf("%s: verdict %+v, want retryable=%v client=%v noBreaker=%v", tc.name, v, tc.retryable, tc.client, tc.noBreaker)
		}
	}
}

// M3-04: a tenant's quota errors are retried but never open the shared breaker;
// a real outage still does.
func TestQuotaErrorsDoNotOpenSharedBreaker(t *testing.T) {
	quota := genai.APIError{Code: 429, Status: "RESOURCE_EXHAUSTED", Message: "quota"}
	b := resilience.NewBreaker(resilience.Config{FailureThreshold: 2, Cooldown: time.Hour})
	pol := resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Multiplier: 1, AttemptTimeout: time.Second}
	attempts := 0
	for i := 0; i < 5; i++ {
		_ = resilience.Execute(context.Background(), b, pol, classify, nil, func(context.Context, int) error { attempts++; return quota })
	}
	if attempts != 15 {
		t.Fatalf("attempts = %d, want 15 (quota stays retryable)", attempts)
	}
	if !b.Allow() {
		t.Fatal("quota errors opened the breaker shared by every tenant")
	}
	outage := genai.APIError{Code: 503, Status: "UNAVAILABLE"}
	_ = resilience.Execute(context.Background(), b, pol, classify, nil, func(context.Context, int) error { return outage })
	if b.Allow() {
		t.Fatal("a 503 outage did not open the breaker")
	}
}
