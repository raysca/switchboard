package ws

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

// enqueueOutput runs inline on the session run loop, so any wait for pacer room
// delays caller audio and VAD results behind it (stale VAD chunks). The default
// pacer must absorb a TTS burst of ahead-of-realtime audio without blocking.
func TestEnqueueOutputDefaultPacerAbsorbsBurst(t *testing.T) {
	cfg := DefaultPacerConfig()
	c := &Connection{cfg: ServerConfig{Pacer: cfg}, pacer: NewPacedWriter(cfg, func(context.Context, []byte) error { return nil })} // Run never started: nothing drains
	start := time.Now()
	for i := 0; i < 50; i++ { // 1 s of 20 ms frames at 24 kHz
		f := &core.OutputAudioFrame{Samples: make([]byte, 960), Encoding: core.EncodingPCM16LE, SampleRate: 24000, Channels: 1, Gen: 1, Turn: "t", UnitID: 1}
		if err := c.enqueueOutput(context.Background(), f); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 10*time.Millisecond {
		t.Fatalf("enqueueing a 1 s burst blocked the caller for %v", d)
	}
}
