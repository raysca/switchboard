//go:build xaispeech

package xai

import (
	"testing"
	"time"

	"govoice/core"
)

// Live finding: xAI TTS emitted no usage frame (the provider-matrix usage_chars check). It must
// report characters sent and audio duration, and drop a trailing odd byte.
func TestXAITTS_UsageFrameAndOddByte(t *testing.T) {
	cfg := DefaultTTSConfig()
	cfg.Client = &mockTTSClient{pcmData: make([]byte, 48001)} // 1 s @ 24 kHz plus a stray byte
	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(21, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Post(&core.SynthesisUnitFrame{Text: "héllo", Turn: "t", Gen: 1, UnitID: 1, IsFinal: true}); err != nil {
		t.Fatal(err)
	}
	var u *core.UsageMetricsFrame
	for end := time.Now().Add(2 * time.Second); u == nil && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		for _, f := range sink.GetFrames() {
			if v, ok := f.(*core.UsageMetricsFrame); ok {
				u = v
			}
		}
	}
	if u == nil {
		t.Fatal("no usage frame")
	}
	if *u.Characters != 5 || *u.AudioDurationMs != 1000 || u.Service != "tts" {
		t.Fatalf("usage = chars %d, ms %d, service %s", *u.Characters, *u.AudioDurationMs, u.Service)
	}
	for _, f := range sink.GetFrames() {
		if a, ok := f.(*core.OutputAudioFrame); ok && len(a.Samples)%2 != 0 {
			t.Fatalf("odd-length audio frame %d", len(a.Samples))
		}
	}
}
