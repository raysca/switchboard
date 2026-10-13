package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if w := publicRequest(s, http.MethodGet, "/api/v1/server-config", testMgmtKey); w.Code == http.StatusOK {
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
