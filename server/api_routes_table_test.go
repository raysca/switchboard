package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"govoice/server/api"
)

// Characterization of the management prefix routing: which handler serves a path,
// the 503 a missing handler gives, and that live-call and demo paths win over /sessions.
func TestManagementPrefixRouting(t *testing.T) {
	type install func(*Server, http.Handler)
	prefixes := []struct {
		prefix, code string
		set          install
	}{
		{"/api/v1/agents", "agent_management_unavailable", (*Server).setAgentManagementAPI},
		{"/api/v1/providers", "provider_management_unavailable", (*Server).setProviderManagementAPI},
		{"/api/v1/mcp-servers", "mcp_management_unavailable", (*Server).setMCPManagementAPI},
		{"/api/v1/secrets", "secrets_management_unavailable", (*Server).setSecretsManagementAPI},
		{"/api/v1/sessions", "session_management_unavailable", (*Server).setSessionManagementAPI},
	}
	errCode := func(t *testing.T, body []byte) string {
		t.Helper()
		var e api.ErrorResponse
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		return e.Error.Code
	}
	for _, p := range prefixes {
		t.Run(p.prefix, func(t *testing.T) {
			s := apiTestServer(t)
			w := apiRequest(s, http.MethodGet, p.prefix, testMgmtKey, "")
			if w.Code != http.StatusServiceUnavailable || errCode(t, w.Body.Bytes()) != p.code || w.Header().Get("X-Request-ID") == "" {
				t.Fatalf("nil handler: %d %s", w.Code, w.Body)
			}
			var gotID string
			p.set(s, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotID = api.RequestIDFromContext(r.Context())
				w.Header().Set("X-Served-By", p.prefix)
				w.WriteHeader(http.StatusTeapot)
			}))
			for _, path := range []string{p.prefix, p.prefix + "/x"} {
				gotID = ""
				w := apiRequest(s, http.MethodGet, path, testMgmtKey, "")
				if w.Code != http.StatusTeapot || w.Header().Get("X-Served-By") != p.prefix || gotID == "" || gotID != w.Header().Get("X-Request-ID") {
					t.Fatalf("%s: %d served-by %q id %q header %q", path, w.Code, w.Header().Get("X-Served-By"), gotID, w.Header().Get("X-Request-ID"))
				}
			}
			w = apiRequest(s, http.MethodGet, p.prefix+"X", testMgmtKey, "")
			if w.Code != http.StatusNotFound || errCode(t, w.Body.Bytes()) != "not_found" {
				t.Fatalf("lookalike: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestManagementLiveCallAndDemoRoutesWinOverSessions(t *testing.T) {
	s := apiTestServer(t)
	s.setSessionManagementAPI(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	for _, action := range []string{"run", "context"} {
		w := apiRequest(s, http.MethodGet, "/api/v1/sessions/x/"+action, testMgmtKey, "")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s: %d allow %q, want the live-call handler's 405", action, w.Code, w.Header().Get("Allow"))
		}
	}
	if w := apiRequest(s, http.MethodGet, "/api/v1/sessions/x", testMgmtKey, ""); w.Code != http.StatusTeapot {
		t.Fatalf("plain session path: %d", w.Code)
	}
	w := apiRequest(s, http.MethodGet, "/api/v1/demo", testMgmtKey, "")
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("X-Request-ID") == "" {
		t.Fatalf("demo: %d %s", w.Code, w.Body)
	}
	var e api.ErrorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Error.Code != "management_unavailable" {
		t.Fatalf("demo code %q", e.Error.Code)
	}
}
