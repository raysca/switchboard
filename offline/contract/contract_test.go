//go:build xaispeech

package contract

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/offline/adapter"
	"govoice/providers/xai"
)

type fakeTTS struct{ pcm []byte }

func (f fakeTTS) Synthesize(context.Context, string, string, int) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.pcm)), nil
}

type fakeSTT struct{}

func (fakeSTT) Transcribe(_ context.Context, pcm []byte, _ int, _ string) (string, error) {
	return "I would like an appointment", nil
}

func tone(d time.Duration, rate int, amp float64) []byte {
	n := int(d * time.Duration(rate) / time.Second)
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(amp*32767*math.Sin(2*math.Pi*440*float64(i)/float64(rate)))))
	}
	return b
}

func adapterServer(t *testing.T, tts adapter.TTSBackend, stt adapter.STTBackend) *httptest.Server {
	t.Helper()
	cfg, err := adapter.LoadConfig(func(k string) string {
		return map[string]string{"OFFLINE_INTERIM_EVERY": "100ms", "OFFLINE_ENDPOINT_SILENCE": "300ms"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(adapter.New(cfg, tts, stt, func(context.Context) error { return nil }).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// The xAI STT client dials ws://.../v1/stt?..., sends PCM then audio.done and reads partials.
func TestRealXAISTTClientAgainstAdapter(t *testing.T) {
	srv := adapterServer(t, nil, fakeSTT{})
	cfg := xai.DefaultSTTConfig()
	cfg.Endpoint = "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/stt"
	cfg.APIKey = "test-key-from-env"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := (&xai.RealXAISTTOpener{}).OpenStream(ctx, cfg)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer stream.Close()
	if m, err := stream.Recv(ctx); err != nil || m.Type != "transcript.created" {
		t.Fatalf("first message %+v err=%v", m, err)
	}
	audio := append(tone(time.Second, 16000, 0.3), make([]byte, 2*16000/2)...)
	for i := 0; i < len(audio); i += 640 {
		if err := stream.SendAudio(ctx, audio[i:min(i+640, len(audio))]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
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
		if m.Type == "transcript.partial" && m.SpeechFinal {
			final = m.Text
		}
		if m.Type == "transcript.done" {
			break
		}
		if m.Type == "error" {
			t.Fatalf("error from adapter: %+v", m)
		}
	}
	if final != "I would like an appointment" {
		t.Fatalf("speech_final text %q", final)
	}
}

// The xAI unary TTS client POSTs JSON and reads the raw PCM body.
func TestRealXAIUnaryTTSAgainstAdapter(t *testing.T) {
	want := tone(500*time.Millisecond, 24000, 0.3)
	srv := adapterServer(t, fakeTTS{pcm: want}, nil)
	cfg := xai.DefaultTTSConfig()
	cfg.Endpoint = srv.URL + "/v1/tts"
	cfg.APIKey = "test-key-from-env"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := xai.NewRealXAITTSClient().Synthesize(ctx, "hello", cfg)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("len(got)=%d len(want)=%d err=%v", len(got), len(want), err)
	}
}

// The real StreamingTTSProcessor dials the WebSocket, sends text.delta/text.done and turns
// audio.delta into OutputAudioFrames in a real core session.
func TestRealXAIStreamingTTSAgainstAdapter(t *testing.T) {
	pcm := tone(time.Second, 24000, 0.3)
	srv := adapterServer(t, fakeTTS{pcm: pcm}, nil)
	cfg := xai.DefaultTTSConfig()
	cfg.Endpoint = srv.URL + "/v1/tts"
	cfg.APIKey = "test-key-from-env"
	proc, err := xai.NewStreamingTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	for i, text := range []string{"First sentence.", "Second sentence."} {
		if err := sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 1, UnitID: uint64(i + 1), Text: text, IsFinal: i == 1}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var bytesOut, finals int
		for _, f := range sink.GetFrames() {
			if a, ok := f.(*core.OutputAudioFrame); ok {
				bytesOut += len(a.Samples)
				if a.IsFinal {
					finals++
				}
			}
		}
		if finals >= 2 && bytesOut >= 2*len(pcm)-2*960 { // the processor holds back at most one 20 ms frame per unit
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %d bytes out, %d finals", bytesOut, finals)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
