package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"govoice/vad"
)

type mockVADExecutor struct {
	ready     bool
	available int
	total     int
}

func (m *mockVADExecutor) Register(sessionID uint64) vad.Stream { return nil }
func (m *mockVADExecutor) Stats() vad.Stats {
	return vad.Stats{}
}
func (m *mockVADExecutor) Capacity() (int, int) { return m.available, m.total }
func (m *mockVADExecutor) IsReady() bool        { return m.ready }
func (m *mockVADExecutor) Close() error         { return nil }

func TestHealth_CheapLocalProcessLiveness(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	srv.publicMux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 on /health, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp["status"] != "live" {
		t.Fatalf("expected status live, got: %s", resp["status"])
	}
}

func TestReady_CompoundReadinessSemantics(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	mockExec := &mockVADExecutor{ready: true, available: 4, total: 4}
	srv.SetVADExecutor(mockExec)
	enableTestManagement(t, srv)

	// 1. Before Start (ready == false)
	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 before server is marked ready, got %d", w.Code)
	}

	// 2. Mark ready
	srv.ready.Store(true)
	w = httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when ready, got %d", w.Code)
	}

	// 4. VAD executor not ready -> 503
	mockExec.ready = false
	w = httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when VAD executor unready, got %d", w.Code)
	}
	mockExec.ready = true

	// 5. Server shutting down -> 503
	srv.shuttingDown.Store(true)
	w = httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 during shutdown, got %d", w.Code)
	}
}

func TestNetworkSurface_PublicAdminSeparation(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// Metrics must NOT be accessible on public listener
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	srv.publicMux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for /metrics on public listener, got %d", w.Code)
	}

	// Timeline must NOT be accessible on public listener
	req = httptest.NewRequest("GET", "/debug/timeline?session=1", nil)
	w = httptest.NewRecorder()
	srv.publicMux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for /debug/timeline on public listener, got %d", w.Code)
	}

	// Metrics MUST be accessible on admin listener
	req = httptest.NewRequest("GET", "/metrics", nil)
	w = httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /metrics on admin listener, got %d", w.Code)
	}
}

func TestReady_RequiresAgentStoreAndReportsCircuits(t *testing.T) {
	srv, err := NewServer(DefaultConfig(ProfileDevelopment))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	srv.ready.Store(true)

	ready := func() (int, map[string]any) {
		w := httptest.NewRecorder()
		srv.adminMux.ServeHTTP(w, httptest.NewRequest("GET", "/ready", nil))
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}
	// Calls run stored agents, so a host without the agent store is not ready.
	if code, body := ready(); code != http.StatusServiceUnavailable {
		t.Fatalf("ready without agent store: %d %v", code, body)
	}
	enableTestManagement(t, srv)
	if code, body := ready(); code != http.StatusOK {
		t.Fatalf("ready with agent store: %d %v", code, body)
	}
	// An open provider circuit is reported but only affects agents using that
	// provider, so the host stays ready.
	srv.metrics.circuits.set("llm", "google", "", 1)
	code, body := ready()
	circuits, _ := body["circuits"].(map[string]any)
	if code != http.StatusOK || circuits["google_llm"] != "open" {
		t.Fatalf("open circuit: %d %v", code, body)
	}
}
