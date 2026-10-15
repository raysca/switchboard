//go:build xaispeech && offlinestack

package contract

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"govoice/providers/xai"
)

// Round trip through the real compose stack: synthesize a phrase with the adapter's TTS (asking for
// 16 kHz directly), feed it to the adapter's STT, expect the key word back.
// Run with: OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 go test -tags "xaispeech offlinestack" ./...
func TestStackRoundTrip(t *testing.T) {
	base := os.Getenv("OFFLINE_ADAPTER_URL")
	if base == "" {
		t.Skip("OFFLINE_ADAPTER_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ttsCfg := xai.DefaultTTSConfig()
	ttsCfg.Endpoint, ttsCfg.APIKey, ttsCfg.SampleRate = base+"/v1/tts", "test-key-from-env", 16000
	pcm, err := xai.NewRealXAITTSClient().Synthesize(ctx, "I would like to book an appointment for next Tuesday.", ttsCfg)
	if err != nil || len(pcm) < 16000 {
		t.Fatalf("tts: %d bytes err=%v", len(pcm), err)
	}

	sttCfg := xai.DefaultSTTConfig()
	sttCfg.Endpoint = "ws" + strings.TrimPrefix(base, "http") + "/v1/stt"
	sttCfg.APIKey = "test-key-from-env"
	stream, err := (&xai.RealXAISTTOpener{}).OpenStream(ctx, sttCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	audio := append(pcm, make([]byte, 2*16000)...) // 1 s of trailing silence ends the utterance
	for i := 0; i < len(audio); i += 640 {
		if err := stream.SendAudio(ctx, audio[i:min(i+640, len(audio))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.CloseSend(ctx); err != nil {
		t.Fatal(err)
	}
	var final string
	for {
		m, err := stream.Recv(ctx)
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		if m == nil {
			continue
		}
		if m.Type == "error" {
			t.Fatalf("adapter error: %+v", m)
		}
		if m.Type == "transcript.partial" && m.SpeechFinal {
			final = m.Text
		}
		if m.Type == "transcript.done" {
			break
		}
	}
	if !strings.Contains(strings.ToLower(final), "appointment") {
		t.Fatalf("round trip lost the key word: %q", final)
	}
}
