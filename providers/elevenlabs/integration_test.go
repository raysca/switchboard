//go:build integration

package elevenlabs

import (
	"os"
	"testing"
	"time"

	"govoice/core"
)

// Live smoke test: go test -tags integration -run Live ./...
// Needs RUN_LIVE_ELEVENLABS_TEST=1 and ELEVENLABS_API_KEY. Synthesizes one short
// sentence and checks audio and word timings come back.
func TestLiveElevenLabsSynthesis(t *testing.T) {
	if os.Getenv("RUN_LIVE_ELEVENLABS_TEST") != "1" {
		t.Skip("RUN_LIVE_ELEVENLABS_TEST=1 not set")
	}
	key := os.Getenv("ELEVENLABS_API_KEY")
	if key == "" {
		t.Skip("ELEVENLABS_API_KEY not set")
	}
	cfg := DefaultTTSConfig()
	cfg.APIKey = key
	h := start(t, cfg)
	h.say(t, "Hello there, this is a test.", true)
	waitFor(t, 15*time.Second, "audio", func() bool { return h.stopped() == 1 })
	if errs := h.errors(); len(errs) > 0 {
		t.Fatalf("provider errors: %v", errs)
	}
	if len(h.audio()) == 0 || len(h.timings()) == 0 {
		t.Fatalf("audio=%d timings=%d", len(h.audio()), len(h.timings()))
	}
}

// Live STT smoke test (same gating): one second of silence, so it checks auth,
// handshake, manual commit and teardown, not accuracy.
func TestLiveElevenLabsSTTHandshakeAndTeardown(t *testing.T) {
	if os.Getenv("RUN_LIVE_ELEVENLABS_TEST") != "1" {
		t.Skip("RUN_LIVE_ELEVENLABS_TEST=1 not set")
	}
	key := os.Getenv("ELEVENLABS_API_KEY")
	if key == "" {
		t.Skip("ELEVENLABS_API_KEY not set")
	}
	cfg := DefaultSTTConfig()
	cfg.APIKey = key
	h := startSTT(t, cfg, nil)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for i := 0; i < 50; i++ {
		h.audio(1)
	}
	sttWait(t, "stream", func() bool { return h.proc.activeCount() == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	sttWait(t, "drain", func() bool { return h.proc.activeCount() == 0 })
	for _, e := range h.errors() {
		t.Errorf("provider error: %v", e)
	}
}
