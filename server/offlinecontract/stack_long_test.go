//go:build xaispeech && offlinestack

package offlinecontract

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/providers/xai"
)

const longParagraph = "Thank you for calling the clinic today. We are open Monday to Friday from eight in the morning until six in the evening, " +
	"and on Saturdays from nine until one. If you need an appointment, I can check the next free slot for you. " +
	"Please have your insurance card ready, and remember to arrive ten minutes early so we can finish the paperwork. " +
	"Is there anything else I can help you with before we end this call?"

// Truncation check on real models: long speech must not lose its tail on the way out (TTS) or in
// (STT). Run with OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 (and OFFLINE_SPEACHES_URL, default
// http://127.0.0.1:8000, for the direct reference).
func TestStackLongSpeechIsNotCutOff(t *testing.T) {
	base := os.Getenv("OFFLINE_ADAPTER_URL")
	if base == "" {
		t.Skip("OFFLINE_ADAPTER_URL not set")
	}
	speaches := os.Getenv("OFFLINE_SPEACHES_URL")
	if speaches == "" {
		speaches = "http://127.0.0.1:8000"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	const rate = 16000

	// Reference: the same text straight from Speaches.
	reqBody, _ := json.Marshal(map[string]any{"model": "speaches-ai/Kokoro-82M-v1.0-ONNX", "input": longParagraph, "voice": "af_heart", "response_format": "pcm", "sample_rate": rate})
	resp, err := http.Post(speaches+"/v1/audio/speech", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(ref) < rate*2*15 {
		t.Fatalf("reference synthesis: status %d, %d bytes", resp.StatusCode, len(ref))
	}

	// Unary path through the adapter.
	cfg := xai.DefaultTTSConfig()
	cfg.Endpoint, cfg.APIKey, cfg.SampleRate = base+"/v1/tts", "test-key-from-env", rate
	unary, err := xai.NewRealXAITTSClient().Synthesize(ctx, longParagraph, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reference %d bytes (%.1fs), unary %d bytes", len(ref), float64(len(ref))/2/rate, len(unary))
	if d := len(ref) - len(unary); d > 2 || d < -2 {
		t.Errorf("unary TTS returned %d bytes, Speaches directly %d: the audio was cut", len(unary), len(ref))
	}

	// Streaming path through the adapter: one unit, the way the xai processor sends it.
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/v1/tts?voice=eve&language=en&codec=pcm&sample_rate=16000", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	for _, m := range []map[string]string{{"type": "text.delta", "delta": longParagraph}, {"type": "text.done"}} {
		b, _ := json.Marshal(m)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	var streamed []byte
	for done := false; !done; {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("stream read after %d bytes: %v", len(streamed), err)
		}
		var m struct{ Type, Delta, Message string }
		_ = json.Unmarshal(data, &m)
		switch m.Type {
		case "audio.delta":
			b, _ := base64.StdEncoding.DecodeString(m.Delta)
			streamed = append(streamed, b...)
		case "audio.done":
			done = true
		case "error":
			t.Fatalf("stream error after %d bytes: %s", len(streamed), m.Message)
		}
	}
	if d := len(ref) - len(streamed); d > 2 || d < -2 {
		t.Errorf("streamed TTS returned %d bytes, Speaches directly %d: the audio was cut", len(streamed), len(ref))
	}

	// STT: the whole paragraph, then silence. Every sentence must come back.
	sttCfg := xai.DefaultSTTConfig()
	sttCfg.Endpoint = "ws" + strings.TrimPrefix(base, "http") + "/v1/stt"
	sttCfg.APIKey = "test-key-from-env"
	stream, err := (&xai.RealXAISTTOpener{}).OpenStream(ctx, sttCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	audio := append(append([]byte(nil), ref...), make([]byte, 2*rate*2)...)
	for i := 0; i < len(audio); i += 640 {
		if err := stream.SendAudio(ctx, audio[i:min(i+640, len(audio))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.CloseSend(ctx); err != nil {
		t.Fatal(err)
	}
	var finals []string
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
			finals = append(finals, m.Text)
		}
		if m.Type == "transcript.done" {
			break
		}
	}
	heard := strings.ToLower(strings.Join(finals, " "))
	t.Logf("STT heard %d utterance(s): %q", len(finals), heard)
	for _, want := range []string{"calling the clinic", "saturdays", "insurance card", "end this call"} {
		if !strings.Contains(heard, want) {
			t.Errorf("transcript is missing %q: the speech was cut off", want)
		}
	}
}
