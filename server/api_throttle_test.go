package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
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

func TestManagementAuthFailuresShareOneAllowance(t *testing.T) {
	s := apiTestServer(t)
	for i := 0; i < 10; i++ { // any address, any forwarded header: the allowance is global
		if w := apiRequestFrom(s, "203.0.113."+strconv.Itoa(i+1)+":1000", "", http.MethodGet, "/api/v1/server-config", "wrong-key"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := apiRequestFrom(s, "198.51.100.7:1001", "", http.MethodGet, "/api/v1/server-config", "wrong-key")
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "too_many_attempts") {
		t.Fatalf("11th attempt: %d %s", w.Code, w.Body.String())
	}
	if s.metrics.ManagementAuthFailuresTotal.Load() != 11 || s.metrics.ManagementAuthThrottledTotal.Load() != 1 {
		t.Fatalf("counters: failures=%d throttled=%d", s.metrics.ManagementAuthFailuresTotal.Load(), s.metrics.ManagementAuthThrottledTotal.Load())
	}
}

func TestCorrectKeyIsNeverThrottledAndAllowanceRefills(t *testing.T) {
	s := apiTestServer(t)
	now := time.Unix(1000, 0)
	s.authNow = func() time.Time { return now }
	for i := 0; i < 11; i++ {
		apiRequestFrom(s, "203.0.113.9:1", "", http.MethodGet, "/api/v1/server-config", "wrong-key")
	}
	if w := apiRequestFrom(s, "203.0.113.9:1", "", http.MethodGet, "/api/v1/server-config", testMgmtKey); w.Code != http.StatusOK {
		t.Fatalf("right key with the allowance exhausted: %d", w.Code)
	}
	if w := apiRequestFrom(s, "203.0.113.9:1", "", http.MethodGet, "/api/v1/server-config", "wrong-key"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("still exhausted: %d", w.Code)
	}
	now = now.Add(6 * time.Second)
	if w := apiRequestFrom(s, "203.0.113.9:1", "", http.MethodGet, "/api/v1/server-config", "wrong-key"); w.Code != http.StatusUnauthorized {
		t.Fatalf("after one refill period: %d", w.Code)
	}
}

func TestThrottleDoesNotMaskTheDisabledAPI(t *testing.T) {
	s := apiTestServer(t)
	s.management = nil // disabled
	for i := 0; i < 20; i++ {
		if w := apiRequestFrom(s, "203.0.113.9:1", "", http.MethodGet, "/api/v1/server-config", "wrong"); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
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
