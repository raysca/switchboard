package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func apiRequestFrom(s *Server, remote, xff, method, path, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(""))
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	s.adminMux.ServeHTTP(w, r)
	return w
}

func TestManagementAuthFailuresAreThrottledPerAddress(t *testing.T) {
	s := apiTestServer(t)
	for i := 0; i < 10; i++ {
		if w := apiRequestFrom(s, "203.0.113.9:1000", "", http.MethodGet, "/api/v1/server-config", "wrong-key"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := apiRequestFrom(s, "203.0.113.9:1001", "", http.MethodGet, "/api/v1/server-config", "wrong-key")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "too_many_attempts") {
		t.Fatalf("11th attempt: %d %s", w.Code, w.Body.String())
	}
	// Even the right key is refused from a blocked address.
	if w := apiRequestFrom(s, "203.0.113.9:1002", "", http.MethodGet, "/api/v1/server-config", testMgmtKey); w.Code != http.StatusTooManyRequests {
		t.Fatalf("right key while blocked: %d", w.Code)
	}
	// Another address is unaffected, and a success never counts.
	for i := 0; i < 20; i++ {
		if w := apiRequestFrom(s, "198.51.100.7:1", "", http.MethodGet, "/api/v1/server-config", testMgmtKey); w.Code != http.StatusOK {
			t.Fatalf("good key from a clean address: %d", w.Code)
		}
	}
	if s.metrics.ManagementAuthFailuresTotal.Load() != 10 || s.metrics.ManagementAuthThrottledTotal.Load() != 2 {
		t.Fatalf("counters: failures=%d throttled=%d", s.metrics.ManagementAuthFailuresTotal.Load(), s.metrics.ManagementAuthThrottledTotal.Load())
	}
}

func TestThrottleIgnoresAnUntrustedForwardedFor(t *testing.T) {
	s := apiTestServer(t) // no trusted proxies: the header must not pick the key
	for i := 0; i < 10; i++ {
		apiRequestFrom(s, "203.0.113.9:1", "10.0.0."+string(rune('1'+i)), http.MethodGet, "/api/v1/server-config", "wrong")
	}
	if w := apiRequestFrom(s, "203.0.113.9:1", "10.9.9.9", http.MethodGet, "/api/v1/server-config", "wrong"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a client must not dodge the throttle by rotating X-Forwarded-For: %d", w.Code)
	}
}

func TestAPIResponsesAreNotCached(t *testing.T) {
	s := apiTestServer(t)
	if w := apiRequest(s, http.MethodGet, "/api/v1/server-config", testMgmtKey, ""); w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
}

func TestThrottleCountersAreExported(t *testing.T) {
	s := apiTestServer(t)
	apiRequest(s, http.MethodGet, "/api/v1/server-config", "wrong", "")
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(w.Body.String(), "voice_management_auth_failures_total 1") {
		t.Fatalf("metrics = %s", w.Body.String())
	}
}
