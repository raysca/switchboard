//go:build integration

package cartesia

import (
	"os"
	"testing"
	"time"

	"govoice/core"
)

// Live smoke test: go test -tags integration -run LiveCartesiaSTT ./...
// Needs RUN_LIVE_CARTESIA_TEST=1 and CARTESIA_API_KEY. Streams 1 s of silence,
// finalizes, and checks the stream opens and closes without a provider error.
func TestLiveCartesiaSTT(t *testing.T) {
	if os.Getenv("RUN_LIVE_CARTESIA_TEST") != "1" {
		t.Skip("RUN_LIVE_CARTESIA_TEST=1 not set")
	}
	key := os.Getenv("CARTESIA_API_KEY")
	if key == "" {
		t.Skip("CARTESIA_API_KEY not set")
	}
	cfg := DefaultSTTConfig()
	cfg.APIKey = key
	h := sstart(t, cfg, nil)
	h.speak()
	for i := 0; i < 50; i++ {
		h.audio(1)
		time.Sleep(20 * time.Millisecond)
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{})
	swait(t, "stream closed", func() bool { return h.proc.activeCount() == 0 })
	if errs := h.errors(); len(errs) > 0 {
		t.Fatalf("provider errors: %v", errs)
	}
}
