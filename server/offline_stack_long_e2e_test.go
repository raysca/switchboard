//go:build xaispeech && offlinestack

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/transport/ws"

	"github.com/coder/websocket"
)

// realtimeAudio streams pcm at real-time pace (20 ms frames every 20 ms): the VAD and the turn timers
// are clocks, so a long utterance sent faster than real time would not behave like a caller.
func (c *e2eCall) realtimeAudio(pcm []byte) {
	c.t.Helper()
	const step = 640
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for i := 0; i < len(pcm); i += step {
		<-tick.C
		frame, err := ws.EncodeAudioFrame(pcm[i:min(i+step, len(pcm))], 16000, 1)
		if err != nil {
			c.t.Fatal(err)
		}
		if err := c.conn.Write(c.ctx, websocket.MessageBinary, frame); err != nil {
			c.t.Fatal(err)
		}
	}
}

// callerTurnLog is a Responses-API LLM that records what the caller said in every request (one
// request per turn) and answers with a fixed reply.
type callerTurnLog struct {
	mu    sync.Mutex
	turns []string
}

func (r *callerTurnLog) handler(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Input []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"input"`
	}
	raw, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(raw, &body)
	last := ""
	for _, m := range body.Input {
		if m.Role == "user" {
			last = m.Content
		}
	}
	r.mu.Lock()
	r.turns = append(r.turns, last)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Understood.\"}\n\n")
	fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\ndata: [DONE]\n\n")
}

func (r *callerTurnLog) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.turns...)
}

// callerTurns plays caller audio (PCM16 16 kHz, in real time) into a call on the offline stack and
// returns what the LLM was asked: one entry per turn.
func callerTurns(t *testing.T, base string, speech []byte) []string {
	t.Helper()
	rec := &callerTurnLog{}
	llm := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer llm.Close()
	rig := newFlowRigE2E(t, newProbe(), func(c *Config) {
		c.Providers.Offline.STTEndpoint = "ws" + strings.TrimPrefix(base, "http") + "/v1/stt"
		c.Providers.Offline.TTSEndpoint = base + "/v1/tts"
		c.Providers.Offline.LLMEndpoint = llm.URL + "/v1/responses"
	})
	realVAD(t, rig.s)
	call := rig.dial(offlineFlow(t, rig, offlineFlowConfig))
	call.realtimeAudio(append(speech, make([]byte, 2*16000*3)...))
	for end := time.Now().Add(60 * time.Second); len(rec.snapshot()) == 0 || call.replyCount() == 0; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("no turn reached the LLM; turns=%q", rec.snapshot())
		}
	}
	time.Sleep(4 * time.Second) // any further turn the call would start
	turns := rec.snapshot()
	for i, turn := range turns {
		t.Logf("turn %d reached the LLM: %q", i+1, turn)
	}
	return turns
}

func stackBase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("OFFLINE_ADAPTER_URL")
	if base == "" {
		t.Skip("OFFLINE_ADAPTER_URL not set")
	}
	return base
}

// The caller's words must reach the LLM in full. The server tells the STT stream "audio.done" when the
// caller pauses and waits about 2 s for the final transcript; a decode slower than that loses text (found
// in a real call: a clip saying "Hi, I'd like to check the status of my order please" arrived as "Hi!").
func TestOfflineStackShortClipIsHeardInFull(t *testing.T) {
	wav, err := os.ReadFile("../assets/speech/clip1.wav")
	if err != nil {
		t.Fatal(err)
	}
	turns := callerTurns(t, stackBase(t), wav[bytes.Index(wav, []byte("data"))+8:])
	if got := strings.ToLower(strings.Join(turns, " ")); !strings.Contains(got, "order") {
		t.Errorf("the end of the caller's sentence never reached the LLM: %q", got)
	}
}

// A caller who speaks for about 22 seconds, with ordinary pauses between sentences, must be heard as
// ONE turn containing everything said, not answered after the first sentence.
func TestOfflineStackLongCallerSpeechIsOneTurn(t *testing.T) {
	base := stackBase(t)
	speaches := os.Getenv("OFFLINE_SPEACHES_URL")
	if speaches == "" {
		speaches = "http://127.0.0.1:8000"
	}
	const paragraph = "Thank you for calling the clinic today. We are open Monday to Friday from eight in the morning until six in the evening, " +
		"and on Saturdays from nine until one. If you need an appointment, I can check the next free slot for you. " +
		"Please have your insurance card ready, and remember to arrive ten minutes early so we can finish the paperwork. " +
		"Is there anything else I can help you with before we end this call?"
	reqBody, _ := json.Marshal(map[string]any{"model": "speaches-ai/Kokoro-82M-v1.0-ONNX", "input": paragraph, "voice": "af_heart", "response_format": "pcm", "sample_rate": 16000})
	resp, err := http.Post(speaches+"/v1/audio/speech", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	speech, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(speech) < 16000*2*15 {
		t.Fatalf("caller audio: status %d, %d bytes", resp.StatusCode, len(speech))
	}
	turns := callerTurns(t, base, speech)
	joined := strings.ToLower(strings.Join(turns, " "))
	if len(turns) != 1 {
		t.Errorf("the caller's single 22 s statement became %d turns (the bot answered before the caller finished)", len(turns))
	}
	for _, want := range []string{"calling the clinic", "saturdays", "insurance card", "end this call"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q never reached the LLM: %q", want, joined)
		}
	}
}
