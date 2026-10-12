package server

import (
	"encoding/json"
	"os"

	"govoice/server/flow"
	"net/http"
	"path/filepath"
	"testing"
)

// The shipped binary calls EnableManagement; with storage configured every
// management API must be served (not 503), and Shutdown must release it.
func TestEnableManagementWiresAPIsAndRetention(t *testing.T) {
	s := apiTestServer(t)
	key := "management-test-key-32-bytes-long-and-separate"
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "mgmt.db"))
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

// enableTestManagement wires a throwaway SQLite flow store into s.
func enableTestManagement(t *testing.T, s *Server) {
	t.Helper()
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "mgmt.db"))
	s.cfg.Storage.Dialect = "sqlite"
	if err := s.EnableManagement(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeManagement)
}

// readyTestFlow enables the flow store and creates a complete Google flow,
// ready on the test provider environment; it returns the flow ID.
func readyTestFlow(t *testing.T, s *Server) string {
	t.Helper()
	if s.flows == nil {
		enableTestManagement(t, s)
	}
	a, err := s.flows.Create(t.Context(), flow.CreateRequest{Name: "Support", Config: json.RawMessage(
		`{"stt":{"provider":"google","model":"chirp_2"},"llm":{"provider":"google","model":"gemini-2.0-flash"},"tts":{"provider":"google","voice":"en-US-Journey-F"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Readiness.State != flow.ReadinessReady {
		t.Fatalf("test agent not ready: %+v", a.Readiness)
	}
	return a.ID
}

// A flow whose provider/stage has no factory in this build (e.g. xAI speech
// without -tags xaispeech) must be reported at startup, not at call time.
func TestUnavailableFlowProvidersReported(t *testing.T) {
	s := apiTestServer(t)
	id := readyTestFlow(t, s)
	if bad := s.unavailableFlowProviders(t.Context()); len(bad) != 0 {
		t.Fatalf("complete build reported %v", bad)
	}
	delete(s.registry.factories, factoryKey("google", "tts"))
	bad := s.unavailableFlowProviders(t.Context())
	if len(bad) != 1 || bad[0] != "agent "+id+" tts=google" {
		t.Fatalf("got %v", bad)
	}
}

func TestProductionRequiresExplicitStore(t *testing.T) {
	t.Setenv("GOVOICE_DEV_DIR", t.TempDir())
	t.Setenv("GOVOICE_DATABASE_DSN", "")
	s := apiTestServer(t)
	s.cfg.Profile = ProfileProduction
	if err := s.EnableManagement(t.Context()); err == nil {
		s.closeManagement()
		t.Fatal("production enabled storage without an explicit DSN")
	}
}

// The environment is the only secret store: development needs no key and
// writes no key file.
func TestManagementStartsWithoutAKeyring(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOVOICE_DEV_DIR", dir)
	t.Setenv("GOVOICE_DATABASE_DSN", "")
	s := apiTestServer(t)
	if err := s.EnableManagement(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeManagement)
	if _, err := os.Stat(filepath.Join(dir, "credential.key")); !os.IsNotExist(err) {
		t.Fatalf("credential.key exists or unexpected error: %v", err)
	}
}
