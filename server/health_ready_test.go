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
	srv.registry.assumeLive = true

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

	// 3. Admission disabled -> 503
	srv.admission.SetEnabled(false)
	w = httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when admission disabled, got %d", w.Code)
	}
	srv.admission.SetEnabled(true)

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

func TestReady_CircuitsAndPipelineUsability(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	// Configure two pipelines: default (google.llm) and xai (xai.llm)
	cfg.Pipelines["default"] = PipelineConfig{STT: "google.stt", LLM: "google.llm", TTS: "google.tts", MaxMessages: 10, MaxTokens: 1000}
	cfg.Pipelines["xai"] = PipelineConfig{STT: "google.stt", LLM: "xai.llm", TTS: "google.tts", MaxMessages: 10, MaxTokens: 1000}

	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	mockExec := &mockVADExecutor{ready: true, available: 4, total: 4}
	srv.SetVADExecutor(mockExec)
	srv.registry.assumeLive = true
	srv.ready.Store(true)

	// Both pipelines usable -> 200
	req := httptest.NewRequest("GET", "/ready", nil)
	w := httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when all circuits closed, got %d", w.Code)
	}

	// Trip Google LLM circuit (open = 1). xAI pipeline remains usable -> still 200!
	srv.metrics.LLMCircuitStateGoogle.Store(1)
	w = httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when alternate pipeline (xai) is usable, got %d", w.Code)
	}

	var resp struct {
		Status          string            `json:"status"`
		UsablePipelines []string          `json:"usable_pipelines"`
		Circuits        map[string]string `json:"circuits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.UsablePipelines) != 1 || resp.UsablePipelines[0] != "xai" {
		t.Fatalf("expected usable pipelines to be [xai], got: %v", resp.UsablePipelines)
	}
	if resp.Circuits["google_llm"] != "open" {
		t.Fatalf("expected google_llm circuit to report open, got: %s", resp.Circuits["google_llm"])
	}

	// Now trip xAI LLM circuit as well (open = 1). NO pipeline is usable -> 503!
	srv.metrics.LLMCircuitStateXAI.Store(1)
	w = httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no configured pipeline is usable, got %d", w.Code)
	}

	// Recover Google LLM circuit (closed = 0) -> 200
	srv.metrics.LLMCircuitStateGoogle.Store(0)
	w = httptest.NewRecorder()
	srv.adminMux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 when Google LLM circuit recovers, got %d", w.Code)
	}
}
