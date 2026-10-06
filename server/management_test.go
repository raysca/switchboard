package server

import (
	"encoding/base64"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped binary calls EnableManagement; with storage configured every
// management API must be served (not 503), and Shutdown must release it.
func TestEnableManagementWiresAPIsAndRetention(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "mgmt.db"))
	t.Setenv("GOVOICE_CREDENTIAL_KEYS", "k1:"+base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	s.cfg.Storage.Dialect = "sqlite"
	s.cfg.Retention.Enabled = true

	for _, path := range []string{"/api/v1/agents", "/api/v1/providers", "/api/v1/mcp-servers", "/api/v1/sessions"} {
		if code := apiRequest(s, http.MethodGet, path, key, "").Code; code != http.StatusServiceUnavailable {
			t.Fatalf("precondition: %s status=%d before wiring", path, code)
		}
	}
	if err := s.EnableManagement(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeManagement)
	for _, path := range []string{"/api/v1/agents", "/api/v1/providers", "/api/v1/mcp-servers", "/api/v1/sessions"} {
		if w := apiRequest(s, http.MethodGet, path, key, ""); w.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, w.Code, w.Body)
		}
	}
	if s.mgmt == nil || s.mgmt.retention == nil {
		t.Fatal("retention job not started")
	}
}

func TestEnableManagementFailsClosedWithoutSecrets(t *testing.T) {
	s := apiTestServer(t)
	s.cfg.Storage.Dialect = "sqlite"
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "mgmt.db"))
	t.Setenv("GOVOICE_CREDENTIAL_KEYS", "")
	if err := s.EnableManagement(t.Context()); err == nil {
		t.Fatal("expected failure without a credential keyring")
	}
	t.Setenv("GOVOICE_CREDENTIAL_KEYS", "k1:short")
	if err := s.EnableManagement(t.Context()); err == nil {
		t.Fatal("expected failure with an invalid key")
	}
}
