package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"govoice/server/api"
)

func TestMCPHandlerUsesAuthenticatedManagementBoundaryAndTrustedRequestID(t *testing.T) {
	s := apiTestServer(t)
	var called bool
	s.SetMCPManagementAPI(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		api.WriteError(w, http.StatusNotFound, "not_found", "missing", api.RequestIDFromContext(r.Context()))
	}))
	path := "/api/v1/mcp-servers/missing"
	if unauthorized := apiRequest(s, http.MethodGet, path, "", ""); unauthorized.Code != http.StatusUnauthorized || called {
		t.Fatalf("unauthorized MCP request reached handler: status=%d called=%v", unauthorized.Code, called)
	}
	public := httptest.NewRecorder()
	s.publicMux.ServeHTTP(public, httptest.NewRequest(http.MethodGet, path, nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("MCP management route exposed on public listener: %d", public.Code)
	}
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer management-test-key-32-bytes-long-and-separate")
	request.Header.Set("X-Request-ID", "forged-client-id")
	response := httptest.NewRecorder()
	s.adminMux.ServeHTTP(response, request)
	if !called || response.Code != http.StatusNotFound {
		t.Fatalf("authenticated MCP request not routed: status=%d called=%v", response.Code, called)
	}
	if response.Header().Get("X-Request-ID") == "" || response.Header().Get("X-Request-ID") == "forged-client-id" || !strings.Contains(response.Body.String(), `"request_id":"`+response.Header().Get("X-Request-ID")+`"`) {
		t.Fatalf("MCP trusted request ID mismatch: header=%q body=%s", response.Header().Get("X-Request-ID"), response.Body.String())
	}
}
