package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func publicAdminServer(t *testing.T, mods ...func(*Config)) *Server {
	t.Helper()
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", testMgmtKey)
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.PublicAdmin.Enabled = true
	for _, m := range mods {
		m(cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = testProviderEnv(t)
	return s
}

func publicRequest(s *Server, method, path, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	s.publicMux.ServeHTTP(w, r)
	return w
}

func TestPublicAdminServesTheManagementAPIBehindTheKey(t *testing.T) {
	s := publicAdminServer(t)
	if w := publicRequest(s, http.MethodGet, "/api/v1/server-config", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", w.Code)
	}
	w := publicRequest(s, http.MethodGet, "/api/v1/server-config", testMgmtKey)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"client_ip"`) {
		t.Fatalf("with key: %d %s", w.Code, w.Body.String())
	}
}

func TestPublicAdminDoesNotExposeTheOperationalSurfaces(t *testing.T) {
	s := publicAdminServer(t)
	for _, p := range []string{"/metrics", "/ready", "/debug/timeline"} {
		if w := publicRequest(s, http.MethodGet, p, testMgmtKey); w.Code == http.StatusOK {
			t.Errorf("%s must not be served on the public port", p)
		}
	}
	if w := publicRequest(s, http.MethodGet, "/health", ""); w.Code != http.StatusOK {
		t.Fatalf("/health: %d", w.Code)
	}
}

func TestAPIIsNotOnThePublicPortByDefault(t *testing.T) {
	s := apiTestServer(t)
	if w := publicRequest(s, http.MethodGet, "/api/v1/server-config", testMgmtKey); w.Code != http.StatusNotFound {
		t.Fatal("the management API must stay on the admin listener unless public_admin is on")
	}
}

func TestPublicAdminNeedsAManagementKey(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.PublicAdmin.Enabled = true
	if _, err := NewServer(cfg); err == nil {
		t.Fatal("public_admin without a management key must refuse to start")
	}
}

func TestServerConfigReportsTheClientAddressTheServerSees(t *testing.T) {
	s := publicAdminServer(t, func(c *Config) {
		c.TLS.TrustedProxyMode = true
		c.TLS.TrustedProxies = []string{"169.254.0.0/16"}
	})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/server-config", nil)
	r.RemoteAddr = "169.254.8.129:5555"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.50") // the first entry is client-supplied; the proxy appended the real one
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Authorization", "Bearer "+testMgmtKey)
	w := httptest.NewRecorder()
	s.publicMux.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"client_ip":"203.0.113.50"`) || !strings.Contains(w.Body.String(), `"secure":true`) {
		t.Fatalf("%s", w.Body.String())
	}
}
func TestUIIsMountedWithoutShadowingTheAPIOrTheSocket(t *testing.T) {
	prev := adminUIFS
	adminUIFS = fstest.MapFS{} // an unbuilt stand-in: a local `make ui-embed` must not change this test
	t.Cleanup(func() { adminUIFS = prev })
	s := publicAdminServer(t)
	// The unbuilt UI: "/" says so, and the real routes still win.
	if w := publicRequest(s, http.MethodGet, "/", ""); w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "not built") {
		t.Fatalf("/: %d %q", w.Code, w.Body.String())
	}
	if w := publicRequest(s, http.MethodGet, "/api/v1/server-config", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1 must reach the management handler, got %d", w.Code)
	}
	if w := publicRequest(s, http.MethodGet, "/ws", ""); w.Code == http.StatusNotFound {
		t.Fatalf("/ws must reach the socket handler, got %d", w.Code)
	}
	if w := publicRequest(s, http.MethodGet, "/health", ""); w.Code != http.StatusOK {
		t.Fatalf("/health: %d", w.Code)
	}
}
func TestAFallingBackUIDoesNotAnswerForServerPaths(t *testing.T) {
	prev := adminUIFS
	adminUIFS = fstest.MapFS{"index.html": {Data: []byte(`<div id="root"></div>`)}}
	t.Cleanup(func() { adminUIFS = prev })
	s := publicAdminServer(t)
	for _, p := range []string{"/metrics", "/ready", "/debug/timeline", "/api/other", "/metrics/x", "/ready/x"} {
		if w := publicRequest(s, http.MethodGet, p, testMgmtKey); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), `id="root"`) {
			t.Errorf("%s: %d %q (must be a plain 404, not the UI)", p, w.Code, w.Body.String())
		}
	}
	if w := publicRequest(s, http.MethodGet, "/flows/abc", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="root"`) {
		t.Fatalf("a client route must get the UI: %d", w.Code)
	}
	if w := publicRequest(s, http.MethodGet, "/assets/gone.js", ""); w.Code != http.StatusNotFound {
		t.Fatalf("a missing asset must 404: %d", w.Code)
	}
	if w := publicRequest(s, http.MethodGet, "/api/v1/server-config", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/v1 must still reach the management handler: %d", w.Code)
	}
}

func TestForwardedHeadersFromAnUntrustedPeerAreIgnored(t *testing.T) {
	s := publicAdminServer(t, func(c *Config) {
		c.TLS.TrustedProxyMode = true
		c.TLS.TrustedProxies = []string{"169.254.0.0/16"}
	})
	r := httptest.NewRequest(http.MethodGet, "/api/v1/server-config", nil)
	r.RemoteAddr = "198.51.100.7:4444"
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Authorization", "Bearer "+testMgmtKey)
	w := httptest.NewRecorder()
	s.publicMux.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"client_ip":"198.51.100.7"`) || !strings.Contains(w.Body.String(), `"secure":false`) {
		t.Fatalf("%s", w.Body.String())
	}
}

func TestThrottleKeyMasksIPv6ToItsSlash64(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.5":               "203.0.113.5",
		"unknown":                   "unknown",
		"2001:db8:1:2:aaaa::1":      "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb:cccc::9": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":           "2001:db8:1:3::/64",
	} {
		if got := throttleKey(in); got != want {
			t.Errorf("throttleKey(%q) = %q, want %q", in, got, want)
		}
	}
}
