package server

import (
	"strings"
	"testing"
)

// The offline provider's breakers are exported like every other provider's: /metrics and /ready.
func TestOfflineBreakerStateIsObservable(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)
	s.ready.Store(true)
	if out := scrape(t, s); !strings.Contains(out, `voice_llm_circuit_state{provider="offline",endpoint="local"} 0`) {
		t.Fatalf("closed offline breaker not exported:\n%s", out)
	}
	for i := 0; i < 5; i++ {
		s.registry.offlineLLMBreaker.RecordFailure()
	}
	if out := scrape(t, s); !strings.Contains(out, `voice_llm_circuit_state{provider="offline",endpoint="local"} 1`) {
		t.Fatalf("open offline breaker not exported:\n%s", out)
	}
	_, body := readyBody(s)
	circuits, _ := body["circuits"].(map[string]any)
	if circuits["offline_llm:local"] != "open" {
		t.Fatalf("ready circuits %v", circuits)
	}
	// Speech stages exist only where the xAI speech adapters do; elsewhere no gauge may pretend otherwise.
	_, hasSTT := circuits["offline_stt:local"]
	if hasSTT != xaiSpeechEnabled {
		t.Fatalf("offline_stt gauge present=%v with xaiSpeechEnabled=%v", hasSTT, xaiSpeechEnabled)
	}
}

// A flow that fails over to or from offline uses offline's own breakers, never xai's.
func TestOfflineFailoverUsesItsOwnBreakers(t *testing.T) {
	r := NewRegistry()
	if r.sttBreaker("offline") != r.offlineSTTBreaker || r.ttsBreaker("offline") != r.offlineTTSBreaker ||
		r.llmBreaker(StageSpec{Provider: "offline"}) != r.offlineLLMBreaker {
		t.Fatal("offline failover is not wired to the offline breakers")
	}
	if r.sttBreaker("offline") == nil || r.ttsBreaker("offline") == nil || r.llmBreaker(StageSpec{Provider: "offline"}) == nil {
		t.Fatal("offline has no breaker for a failover stage")
	}
}
