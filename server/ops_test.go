package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"govoice/core"
)

func scrape(t *testing.T, s *Server) string {
	t.Helper()
	w := httptest.NewRecorder()
	s.adminMux.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

func readyBody(s *Server) (int, map[string]any) {
	w := httptest.NewRecorder()
	s.adminMux.ServeHTTP(w, httptest.NewRequest("GET", "/ready", nil))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

// An Nvidia (per-endpoint) breaker opening is visible in /metrics and /ready (M4-05).
func TestNvidiaBreakerStateIsObservable(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)
	s.ready.Store(true)

	br := s.registry.nvidiaBreaker("llm|" + nvidiaNIMBaseURL)
	if out := scrape(t, s); !strings.Contains(out, `voice_llm_circuit_state{provider="nvidia",endpoint="integrate.api.nvidia.com"} 0`) {
		t.Fatalf("closed nvidia breaker not exported:\n%s", out)
	}
	for i := 0; i < 5; i++ {
		br.RecordFailure()
	}
	if out := scrape(t, s); !strings.Contains(out, `voice_llm_circuit_state{provider="nvidia",endpoint="integrate.api.nvidia.com"} 1`) {
		t.Fatalf("open nvidia breaker not exported:\n%s", out)
	}
	code, body := readyBody(s)
	circuits, _ := body["circuits"].(map[string]any)
	if code != http.StatusOK || circuits["nvidia_llm:integrate.api.nvidia.com"] != "open" {
		t.Fatalf("ready=%d circuits=%v", code, circuits)
	}
}

// /ready only lists providers built into this binary (M4-12).
func TestReadyCircuitsFollowBuiltProviders(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)
	s.ready.Store(true)
	_, body := readyBody(s)
	circuits, _ := body["circuits"].(map[string]any)
	if _, ok := circuits["google_llm"]; !ok {
		t.Fatalf("google_llm missing: %v", circuits)
	}
	if _, ok := circuits["xai_stt"]; ok != xaiSpeechEnabled {
		t.Fatalf("xai_stt listed=%v but xaiSpeechEnabled=%v", ok, xaiSpeechEnabled)
	}
}

// A store outage makes /ready 503 (M4-12).
func TestReadyFailsWhenStoreUnreachable(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)
	s.ready.Store(true)
	if code, _ := readyBody(s); code != http.StatusOK {
		t.Fatalf("healthy store: ready=%d", code)
	}
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}
	s.pingAt = s.pingAt.Add(-2e9) // expire the 1 s verdict cache
	if code, body := readyBody(s); code != http.StatusServiceUnavailable || !strings.Contains(body["reason"].(string), "store") {
		t.Fatalf("closed store: ready=%d body=%v", code, body)
	}
}

// Stage errors are exported per provider; a stage with fallbacks reports "failover" (M4-05).
func TestProviderErrorsLabelled(t *testing.T) {
	s := &Server{metrics: NewMetrics()}
	prov := stageProviders(CallSpec{
		STT: StageSpec{Provider: "nvidia"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "xai"},
		TTSFallbacks: []StageSpec{{Provider: "google"}},
	})
	s.recordProviderErrors(ttsCountsStub{1, 2, 0, 0}, prov)
	s.recordProviderErrors(sttErrStub(3), prov)
	var b strings.Builder
	s.metrics.WritePrometheus(&b)
	for _, want := range []string{
		`voice_provider_errors_total{provider="failover",stage="tts"} 2`,
		`voice_provider_errors_total{provider="nvidia",stage="stt"} 3`,
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
}

type sttErrStub uint64

func (sttErrStub) ProcessFrame(_ context.Context, _ core.Frame, _ core.Direction, _ core.Emitter) error {
	return nil
}
func (e sttErrStub) STTCounts() (uint64, uint64, uint64, uint64) { return 0, 0, 0, uint64(e) }

func TestProbeListenAddrValidation(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.ProbeListenAddr = cfg.AdminListenAddr
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("probe == admin must be rejected")
	}
	cfg.ProbeListenAddr = "127.0.0.1:0"
	if err := ValidateConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// The probe listener serves /health, /ready and /metrics, and nothing else (M4-02).
func TestProbeMuxIsReadOnlySubset(t *testing.T) {
	s := apiTestServer(t)
	for path, want := range map[string]int{
		"/health": 200, "/metrics": 200, "/ready": 503, // not started: unready
		"/debug/timeline?session=1": 404, "/api/v1/agents": 404,
	} {
		w := httptest.NewRecorder()
		s.probeMux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Errorf("probe %s = %d, want %d", path, w.Code, want)
		}
	}
}
