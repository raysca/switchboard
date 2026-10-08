//go:build integration

package nvidia

import (
	"context"
	"os"
	"testing"
	"time"

	"govoice/providers/nvidia/rivapb"
)

// Synthesizes a short phrase on the hosted Riva TTS function (NVIDIA_API_KEY,
// NVIDIA_TTS_FUNCTION_ID, e.g. Magpie's from build.nvidia.com; optional
// NVIDIA_TTS_VOICE) at the 24 kHz pipeline rate, which also checks that Riva
// resamples server side. Bills a few characters. Not run in CI.
func TestIntegration_HostedSynthesize(t *testing.T) {
	key, fn := os.Getenv("NVIDIA_API_KEY"), os.Getenv("NVIDIA_TTS_FUNCTION_ID")
	if key == "" || fn == "" {
		t.Skip("NVIDIA_API_KEY and NVIDIA_TTS_FUNCTION_ID not set")
	}
	cfg := DefaultTTSConfig()
	cfg.APIKey, cfg.FunctionID = key, fn
	if v := os.Getenv("NVIDIA_TTS_VOICE"); v != "" {
		cfg.Voice = v
	}
	p, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := p.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	st, err := p.client.SynthesizeOnline(p.conn.Context(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&rivapb.SynthesizeSpeechRequest{Text: "Hello from Riva.", LanguageCode: cfg.Language,
		Encoding: rivapb.AudioEncoding_LINEAR_PCM, SampleRateHz: int32(p.cfg.RivaRate), VoiceName: cfg.Voice}); err != nil {
		t.Fatal(err)
	}
	_ = st.CloseSend()
	n := 0
	for {
		r, err := st.Recv()
		if err != nil {
			break
		}
		n += len(r.GetAudio())
	}
	if n == 0 {
		t.Fatal("no audio returned")
	}
}
