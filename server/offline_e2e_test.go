//go:build xaispeech

package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"govoice/server/storage"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/offline/adapter"
	"govoice/server/agent"
	"govoice/transport/ws"
	"govoice/vad"
)

// callerSpeech is the PCM16 16 kHz mono data of a real recorded clip (assets/speech): the VAD must see
// speech, and a sine tone is not. A second of silence follows so the adapter ends the utterance.
func callerSpeech(t *testing.T) []byte {
	t.Helper()
	wav, err := os.ReadFile("../assets/speech/clip1.wav")
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(wav, []byte("data"))
	if i < 0 || i+8 > len(wav) {
		t.Fatal("clip1.wav has no data chunk")
	}
	return append(append([]byte(nil), wav[i+8:]...), make([]byte, 2*16000)...)
}

// realVAD attaches the in-process Silero executor, as cmd/server builds it. The shared rig's mock
// VAD never reports speech, and a turn only opens on speech: without this the adapter would hear
// the caller and the server would ignore it. It needs the ONNX Runtime (`make ort`, which `make
// build` already requires); a machine without it skips these tests with that hint.
func realVAD(t *testing.T, s *Server) {
	t.Helper()
	cfg := vad.DefaultConfig()
	cfg.Mode, cfg.Workers = "sharded", 1
	vad.ConfigureArtifacts("", "")
	exec, err := vad.New(cfg)
	if err != nil {
		t.Skipf("no VAD executor (run `make ort`): %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	s.SetVADExecutor(exec)
}

// A caller's AUDIO through the real server, on the offline provider: the other e2e tests send text.
// Caller audio goes to the adapter's STT (speech_final), the turn runs, the LLM streams a reply
// over xAI's Responses events, and the adapter's TTS audio comes back to the caller. Model backends
// are fakes here, so it runs in `make verify`; offline_stack_e2e_test.go runs the real models.

// offlineFakeTTS speaks a 440 Hz tone, so "bot audio arrived" is a non-zero peak.
type offlineFakeTTS struct{}

func (offlineFakeTTS) Synthesize(_ context.Context, _, _ string, rate int) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(offlineTone(time.Second, rate, 0.2))), nil
}

// offlineFakeSTT hears whatever the caller said as one fixed sentence.
type offlineFakeSTT struct{}

func (offlineFakeSTT) Transcribe(context.Context, []byte, int, string) (string, error) {
	return "I would like to book an appointment", nil
}

func offlineTone(d time.Duration, rate int, amp float64) []byte {
	n := int(d * time.Duration(rate) / time.Second)
	b := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(amp*32767*math.Sin(2*math.Pi*440*float64(i)/float64(rate)))))
	}
	return b
}

// sendAudio streams pcm to the server as 20 ms frames at 16 kHz, a little faster than real time.
func (c *e2eCall) sendAudio(pcm []byte) {
	c.t.Helper()
	const step = 640
	for i := 0; i < len(pcm); i += step {
		frame, err := ws.EncodeAudioFrame(pcm[i:min(i+step, len(pcm))], 16000, 1)
		if err != nil {
			c.t.Fatal(err)
		}
		if err := c.conn.Write(c.ctx, websocket.MessageBinary, frame); err != nil {
			c.t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sendAudioLoose is sendAudio for a server that may hang up mid-stream.
func (c *e2eCall) sendAudioLoose(pcm []byte) {
	const step = 640
	for i := 0; i < len(pcm); i += step {
		frame, err := ws.EncodeAudioFrame(pcm[i:min(i+step, len(pcm))], 16000, 1)
		if err != nil || c.conn.Write(c.ctx, websocket.MessageBinary, frame) != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeResponsesLLM streams xAI's Responses events for a fixed reply.
func fakeResponsesLLM(t *testing.T, reply []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, d := range reply {
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", d)
			fl.Flush()
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":6,\"total_tokens\":11}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func offlineAgent(t *testing.T, r *agentRigE2E, cfg string) string {
	t.Helper()
	f, err := r.s.agents.Create(t.Context(), agent.CreateRequest{Name: "offline", Config: []byte(cfg)})
	if err != nil || f.Readiness.State != agent.ReadinessReady {
		t.Fatalf("offline flow: %+v err=%v", f.Readiness, err)
	}
	return f.ID
}

const offlineAgentConfig = `{"stt":{"provider":"offline","model":"s"},"llm":{"provider":"offline","model":"govoice-test","system_prompt":"Be brief."},"tts":{"provider":"offline","voice":"eve"}}`

func TestOfflineCallEndToEnd(t *testing.T) {
	acfg, err := adapter.LoadConfig(func(k string) string {
		return map[string]string{"OFFLINE_INTERIM_EVERY": "100ms", "OFFLINE_ENDPOINT_SILENCE": "300ms"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	ad := httptest.NewServer(adapter.New(acfg, offlineFakeTTS{}, offlineFakeSTT{}, func(context.Context) error { return nil }).Handler())
	defer ad.Close()
	llm := fakeResponsesLLM(t, []string{"Hello ", "from the offline stack."})

	rig := newAgentRigE2E(t, newProbe(), func(c *Config) {
		c.Providers.Offline.STTEndpoint = "ws" + strings.TrimPrefix(ad.URL, "http") + "/v1/stt"
		c.Providers.Offline.TTSEndpoint = ad.URL + "/v1/tts"
		c.Providers.Offline.LLMEndpoint = llm.URL + "/v1/responses"
	})
	realVAD(t, rig.s)
	call := rig.dial(offlineAgent(t, rig, offlineAgentConfig))

	// A recorded clip, then a second of silence for the adapter to end the utterance.
	call.sendAudio(callerSpeech(t))

	call.waitReplies("from the offline stack", 1)
	eventually(t, "bot audio from the adapter's TTS", func() bool { return call.maxPeak() > 0 })
}

func TestOfflineAdapterDownFailsTheStage(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close() // nothing listens here any more
	llm := fakeResponsesLLM(t, []string{"never spoken"})
	rig := newAgentRigE2E(t, newProbe(), func(c *Config) {
		c.Providers.Offline.STTEndpoint = "ws" + strings.TrimPrefix(url, "http") + "/v1/stt"
		c.Providers.Offline.TTSEndpoint = url + "/v1/tts"
		c.Providers.Offline.LLMEndpoint = llm.URL + "/v1/responses"
		c.Turn.NoTranscriptTimeout = time.Second // the hang-up counts failures a window apart
	})
	realVAD(t, rig.s)
	call := rig.dial(offlineAgent(t, rig, offlineAgentConfig))
	// The first failing turn is reported but the call stays up; the second ends it.
	timelineErrors := func() (n int) {
		for _, e := range rig.timeline() {
			if e.Type == EventError {
				n++
			}
		}
		return n
	}
	call.sendAudioLoose(callerSpeech(t))
	eventually(t, "the first STT failure on the timeline", func() bool { return timelineErrors() >= 1 })
	time.Sleep(1200 * time.Millisecond) // a miscounted first failure would hang up here; and the window has passed
	if call.ended() {
		t.Fatal("the call ended after the first failed turn")
	}
	call.sendAudioLoose(callerSpeech(t))
	// A call whose STT cannot be reached must end, not hang deaf.
	eventually(t, "the call to end after its STT failed twice", call.ended)
	rig.waitEnded(call.sessionID, storage.EndError)
	if call.replies("never spoken") != 0 {
		t.Fatal("the bot replied although its STT was unreachable")
	}
}
