//go:build xaispeech && offlinestack

package server

import (
	"os"
	"strings"
	"testing"
	"time"

	"govoice/core/netpool"
)

// A real call through the server on the real local models: Whisper hears a recorded clip, Ollama
// answers, Kokoro speaks. Needs `make offline-up offline-models` first.
//
//	OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 go test -tags "xaispeech offlinestack" -run TestOfflineStackCall ./server/...
//
// OFFLINE_LLM_URL overrides the Ollama endpoint (default providers.offline.llm_endpoint).
func TestOfflineStackCall(t *testing.T) {
	base := os.Getenv("OFFLINE_ADAPTER_URL")
	if base == "" {
		t.Skip("OFFLINE_ADAPTER_URL not set")
	}
	rig := newFlowRigE2E(t, newProbe(), func(c *Config) {
		c.Providers.Offline.STTEndpoint = "ws" + strings.TrimPrefix(base, "http") + "/v1/stt"
		c.Providers.Offline.TTSEndpoint = base + "/v1/tts"
		if u := os.Getenv("OFFLINE_LLM_URL"); u != "" {
			c.Providers.Offline.LLMEndpoint = u
		}
	})
	realVAD(t, rig.s)
	// The xai clients to Ollama and the adapter use process-lifetime shared pools: drop their idle
	// keep-alive connections so the package goleak check sees only leaks.
	t.Cleanup(func() {
		for _, o := range []netpool.Options{{Provider: "xai", Stage: "llm"}, {Provider: "xai", Stage: "tts"}} {
			netpool.Get(o).Close()
		}
	})
	call := rig.dial(offlineFlow(t, rig, offlineFlowConfig))
	call.sendAudio(callerSpeech(t))

	// Real models are slow on CPU: wait far longer than the hermetic tests do.
	for end := time.Now().Add(120 * time.Second); call.replyCount() == 0 || call.maxPeak() == 0; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("no spoken reply in 120 s: %d reply fragments, peak %d", call.replyCount(), call.maxPeak())
		}
	}
}
