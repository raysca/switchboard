package server

import (
	"encoding/json"
	"os"

	"encoding/base64"
	"govoice/server/agent"
	"govoice/server/credential"
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
	t.Setenv("GOVOICE_CREDENTIAL_KEYS", "k1:short")
	if err := s.EnableManagement(t.Context()); err == nil {
		t.Fatal("expected failure with an invalid key")
	}
}

// enableTestManagement wires a throwaway SQLite agent store into s.
func enableTestManagement(t *testing.T, s *Server) {
	t.Helper()
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "mgmt.db"))
	t.Setenv("GOVOICE_CREDENTIAL_KEYS", "k1:"+base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	s.cfg.Storage.Dialect = "sqlite"
	if err := s.EnableManagement(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeManagement)
}

// readyTestAgent enables the agent store and creates a complete agent backed
// by a stored Google credential; it returns the agent ID.
func readyTestAgent(t *testing.T, s *Server) string {
	t.Helper()
	if s.agents == nil {
		enableTestManagement(t, s)
	}
	cred, err := s.credentials.Create(t.Context(), credential.CreateInput{Provider: "google", Secret: "test-google-api-key"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Support", Config: json.RawMessage(strings.ReplaceAll(
		`{"stt":{"provider":"google","model":"chirp_2","credential_id":"CRED"},"llm":{"provider":"google","model":"gemini-2.0-flash","credential_id":"CRED"},"tts":{"provider":"google","voice":"en-US-Journey-F","credential_id":"CRED"}}`, "CRED", cred.ID))})
	if err != nil {
		t.Fatal(err)
	}
	if a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("test agent not ready: %+v", a.Readiness)
	}
	return a.ID
}

func credentialInput(provider, secret string) credential.CreateInput {
	return credential.CreateInput{Provider: provider, Secret: secret}
}

// Development needs no setup: a local store and a persisted key are created,
// and credentials stored in one run are still decryptable in the next.
func TestDevelopmentAutoCreatesLocalStoreAndKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOVOICE_DEV_DIR", dir)
	t.Setenv("GOVOICE_DATABASE_DSN", "")
	t.Setenv("GOVOICE_CREDENTIAL_KEYS", "")

	first := apiTestServer(t)
	if err := first.EnableManagement(t.Context()); err != nil {
		t.Fatal(err)
	}
	cred, err := first.credentials.Create(t.Context(), credential.CreateInput{Provider: "google", Secret: "dev-google-key"})
	if err != nil {
		t.Fatal(err)
	}
	first.closeManagement()
	info, err := os.Stat(filepath.Join(dir, "credential.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("dev key file: %v mode=%v", err, info.Mode())
	}

	second := apiTestServer(t)
	if err := second.EnableManagement(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.closeManagement)
	if got, err := second.credentials.ProviderSecret(t.Context(), cred.ID, "google"); err != nil || string(got) != "dev-google-key" {
		t.Fatalf("credential not readable after restart: %q %v", got, err)
	}
}

func TestProductionRequiresExplicitStoreAndKey(t *testing.T) {
	t.Setenv("GOVOICE_DEV_DIR", t.TempDir())
	t.Setenv("GOVOICE_DATABASE_DSN", "")
	t.Setenv("GOVOICE_CREDENTIAL_KEYS", "")
	s := apiTestServer(t)
	s.cfg.Profile = ProfileProduction
	if err := s.EnableManagement(t.Context()); err == nil {
		s.closeManagement()
		t.Fatal("production enabled storage without explicit DSN and keyring")
	}
}

func TestDeleteCredentialAttachedToAgentIsRefused(t *testing.T) {
	s := apiTestServer(t)
	agentID := readyTestAgent(t, s)
	creds, err := s.credentials.List(t.Context())
	if err != nil || len(creds) != 1 {
		t.Fatalf("creds=%v err=%v", creds, err)
	}
	key := "management-test-key-32-bytes-long-and-separate"
	path := "/api/v1/providers/credentials/" + creds[0].ID
	w := apiRequest(s, http.MethodDelete, path, key, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"credential_in_use"`) || !strings.Contains(w.Body.String(), agentID) {
		t.Fatalf("delete attached credential: %d %s", w.Code, w.Body)
	}
	if err := s.agents.Archive(t.Context(), agentID); err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(s, http.MethodDelete, path, key, ""); w.Code != http.StatusOK {
		t.Fatalf("delete after archiving the agent: %d %s", w.Code, w.Body)
	}
}
