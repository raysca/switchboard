//go:build integration

package deepgram

import (
	"context"
	"os"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/netpool"
)

// Live smoke test: go test -tags integration -run Live ./...
// Needs RUN_LIVE_DEEPGRAM_TEST=1 and DEEPGRAM_API_KEY. Sends one second of
// silence, so it checks auth, handshake, finalize and teardown, not accuracy.
func TestLiveDeepgramHandshakeAndTeardown(t *testing.T) {
	if os.Getenv("RUN_LIVE_DEEPGRAM_TEST") != "1" {
		t.Skip("RUN_LIVE_DEEPGRAM_TEST=1 not set")
	}
	key := os.Getenv("DEEPGRAM_API_KEY")
	if key == "" {
		t.Skip("DEEPGRAM_API_KEY not set")
	}
	cfg := DefaultSTTConfig()
	cfg.APIKey = key
	h := start(t, cfg, nil)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for i := 0; i < 50; i++ {
		_ = h.sess.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000, Channels: 1, Gen: 1})
	}
	waitFor(t, "stream", func() bool { return h.proc.activeCount() == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	waitFor(t, "drain", func() bool { return h.proc.activeCount() == 0 })
	if errs := h.errors(); len(errs) > 0 {
		t.Fatalf("provider errors: %v", errs)
	}
}

// Live warm-up check (manual, same gating as above): measures the first
// websocket connect with and without a pre-dialed TCP/TLS spare against the
// real endpoint. Run several times and compare; the gain is about 2-3 RTTs.
//
//	RUN_LIVE_DEEPGRAM_TEST=1 DEEPGRAM_API_KEY=... go test -tags integration -run LiveWarm -v ./providers/deepgram
func TestLiveWarmFirstConnect(t *testing.T) {
	if os.Getenv("RUN_LIVE_DEEPGRAM_TEST") != "1" || os.Getenv("DEEPGRAM_API_KEY") == "" {
		t.Skip("RUN_LIVE_DEEPGRAM_TEST=1 and DEEPGRAM_API_KEY required")
	}
	netpool.Configure(netpool.Settings{Shared: false, Predial: true, SpareTTL: 10 * time.Second})
	defer netpool.Configure(netpool.Settings{Shared: true})
	for _, warm := range []bool{false, true, false, true, false, true} {
		cfg := DefaultSTTConfig()
		cfg.APIKey = os.Getenv("DEEPGRAM_API_KEY")
		cfg.Pool = netpool.Get(netpool.Options{Provider: "deepgram", Stage: "live", HTTP1Only: true}) // fresh pool: cold DNS cache excluded, TLS cache not
		p, err := NewSTTProcessor(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if warm {
			if err := p.Warm(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		start := time.Now()
		if err := p.ensureStream(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Logf("warm=%v first connect %v", warm, time.Since(start))
		p.Close()
	}
}
