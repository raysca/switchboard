package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderRoutesRemainManagementAuthenticated(t *testing.T) {
	s := apiTestServer(t)
	enableTestManagement(t, s)
	path := "/api/v1/providers"
	if got := apiRequest(s, http.MethodGet, path, "", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated provider route: %d %s", got.Code, got.Body.String())
	}
	public := httptest.NewRecorder()
	s.publicMux.ServeHTTP(public, httptest.NewRequest("GET", path, nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("provider management route exposed on public listener: %d", public.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/providers/no-such-route", nil)
	request.Header.Set("Authorization", "Bearer management-test-key-32-bytes-long-and-separate")
	request.Header.Set("X-Request-ID", "client-forged-request-id")
	responseRecorder := httptest.NewRecorder()
	s.adminMux.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusNotFound || responseRecorder.Header().Get("X-Request-ID") == "client-forged-request-id" || responseRecorder.Header().Get("X-Request-ID") == "" {
		t.Fatalf("unknown provider route: %d request-id=%q", responseRecorder.Code, responseRecorder.Header().Get("X-Request-ID"))
	}
}

func TestCredentialRoutesAreGone(t *testing.T) {
	s := apiTestServer(t)
	enableTestManagement(t, s)
	key := "management-test-key-32-bytes-long-and-separate"
	for _, path := range []string{"/api/v1/providers/credentials", "/api/v1/providers/credentials/x"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			if got := apiRequest(s, method, path, key, `{}`); got.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, path, got.Code)
			}
		}
	}
}
