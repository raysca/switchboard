//go:build xaispeech && offlinestack

package contract

import (
	"os"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/providers/xai"
)

// The unary xAI TTS processor against the real adapter in a real core.Session: the server builds
// this path only with xai_tts.disable_streaming (paid), so this is its only stack-level proof.
func TestStackUnaryTTSProcessor(t *testing.T) {
	base := os.Getenv("OFFLINE_ADAPTER_URL")
	if base == "" {
		t.Skip("OFFLINE_ADAPTER_URL not set")
	}
	cfg := xai.DefaultTTSConfig()
	cfg.Endpoint, cfg.APIKey, cfg.SampleRate = base+"/v1/tts", "test-key-from-env", 16000
	proc, err := xai.NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := core.NewPipeline(proc)
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var frames []core.Frame
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	}
	if err := proc.Attach(session, 0); err != nil {
		t.Fatal(err)
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Post(&core.SynthesisUnitFrame{Turn: "t", Gen: 1, UnitID: 1, IsFinal: true,
		Text: "I would like to book an appointment for next Tuesday."}); err != nil {
		t.Fatal(err)
	}

	var started, stopped, usage, finals, errs int
	var audioBytes int
	done := func() bool {
		mu.Lock()
		defer mu.Unlock()
		started, stopped, usage, finals, errs, audioBytes = 0, 0, 0, 0, 0, 0
		for _, f := range frames {
			switch v := f.(type) {
			case *core.TTSStartedFrame:
				started++
			case *core.TTSStoppedFrame:
				stopped++
			case *core.UsageMetricsFrame:
				usage++
			case *core.ErrorFrame:
				errs++
			case *core.OutputAudioFrame:
				audioBytes += len(v.Samples)
				if v.IsFinal {
					finals++
				}
			}
		}
		return usage > 0 || errs > 0
	}
	deadline := time.Now().Add(60 * time.Second)
	for !done() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if errs != 0 || started != 1 || stopped != 1 || usage != 1 || finals != 1 {
		t.Fatalf("frames: errors=%d started=%d stopped=%d usage=%d finals=%d", errs, started, stopped, usage, finals)
	}
	if audioBytes < 2*16000 || audioBytes%2 != 0 {
		t.Fatalf("audio: %d bytes, want >= 1 s of sample-aligned PCM16 at 16 kHz", audioBytes)
	}
}
