package server

import (
	"encoding/json"
	"os"

	"govoice/server/agent"
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
	if s.mgmt.Load() == nil || s.mgmt.Load().retention == nil {
		t.Fatal("retention job not started")
	}
	if s.mgmt.Load().mcpSync == nil {
		t.Fatal("MCP background sync not started (mcp.sync_interval defaults to 5m)")
	}
}

func TestMCPSyncIntervalZeroStartsNoSyncer(t *testing.T) {
	s := apiTestServer(t)
	s.cfg.MCP.SyncInterval = 0
	enableTestManagement(t, s)
	if s.mgmt.Load().mcpSync != nil {
		t.Fatal("mcp.sync_interval 0 started a syncer")
	}
}

// enableTestManagement wires a throwaway SQLite agent store into s.
func enableTestManagement(t *testing.T, s *Server) {
	t.Helper()
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "mgmt.db"))
	s.cfg.Storage.Dialect = "sqlite"
	if err := s.EnableManagement(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeManagement)
}

// readyTestAgent enables the agent store and creates a complete Google agent,
// ready on the test provider environment; it returns the agent ID.
func readyTestAgent(t *testing.T, s *Server) string {
	t.Helper()
	if s.agents == nil {
		enableTestManagement(t, s)
	}
	a, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Support", Config: json.RawMessage(
		`{"stt":{"provider":"google","model":"chirp_2"},"llm":{"provider":"google","model":"gemini-2.0-flash"},"tts":{"provider":"google","voice":"en-US-Journey-F"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Readiness.State != agent.ReadinessReady {
		t.Fatalf("test agent not ready: %+v", a.Readiness)
	}
	return a.ID
}

// An agent whose provider/stage has no factory in this build (e.g. xAI speech
// without -tags xaispeech) must be reported at startup, not at call time.
func TestUnavailableAgentProvidersReported(t *testing.T) {
	s := apiTestServer(t)
	id := readyTestAgent(t, s)
	if bad := s.unavailableAgentProviders(t.Context(), s.agents); len(bad) != 0 {
		t.Fatalf("complete build reported %v", bad)
	}
	delete(s.registry.factories, factoryKey("google", "tts"))
	bad := s.unavailableAgentProviders(t.Context(), s.agents)
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

// A start EnableManagement refuses (a production store with agents this build cannot run, an unusable
// recording directory) closes the store: nothing may still point at it and the APIs stay unavailable.
func TestRefusedManagementStartLeavesNoClosedStoreBehind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse func(t *testing.T, s *Server)
	}{
		{"production agent this build cannot run", func(t *testing.T, s *Server) {
			s.cfg.Profile = ProfileProduction
			delete(s.registry.factories, factoryKey("google", "tts"))
		}},
		{"recording directory is a file", func(t *testing.T, s *Server) {
			file := filepath.Join(t.TempDir(), "not-a-dir")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			s.cfg.Recording.Enabled, s.cfg.Recording.Dir = true, file
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "mgmt.db"))
			seed := apiTestServer(t)
			seed.cfg.Storage.Dialect = "sqlite"
			if err := seed.EnableManagement(t.Context()); err != nil {
				t.Fatal(err)
			}
			readyTestAgent(t, seed)
			seed.closeManagement()

			s := apiTestServer(t)
			s.cfg.Storage.Dialect = "sqlite"
			tc.refuse(t, s)
			if err := s.EnableManagement(t.Context()); err == nil {
				s.closeManagement()
				t.Fatal("the start was not refused")
			}
			if s.store != nil || s.agents != nil || s.mcpService != nil || s.mgmt.Load() != nil || s.recSink != nil {
				t.Fatalf("a refused start left store=%v agents=%v mcp=%v mgmt=%v recSink=%v",
					s.store != nil, s.agents != nil, s.mcpService != nil, s.mgmt.Load() != nil, s.recSink != nil)
			}
			if code := apiRequest(s, http.MethodGet, "/api/v1/agents", "management-test-key-32-bytes-long-and-separate", "").Code; code != http.StatusServiceUnavailable {
				t.Fatalf("/api/v1/agents = %d after a refused start, want 503", code)
			}
		})
	}
}
