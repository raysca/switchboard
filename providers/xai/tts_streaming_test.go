//go:build xaispeech

package xai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/core/resilience"
)

// wsFake serves the xAI TTS WebSocket; handle runs once per accepted connection.
type wsFake struct {
	*httptest.Server
	opens   atomic.Int32
	queries chan url.Values
	auth    chan string
	closed  chan struct{} // one signal per finished connection handler
}

func newWSFake(t *testing.T, handle func(ctx context.Context, c *websocket.Conn)) *wsFake {
	f := &wsFake{queries: make(chan url.Values, 8), auth: make(chan string, 8), closed: make(chan struct{}, 8)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.opens.Add(1)
		f.queries <- r.URL.Query()
		f.auth <- r.Header.Get("Authorization")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { f.closed <- struct{}{} }()
		defer c.CloseNow()
		handle(r.Context(), c)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *wsFake) cfg(client TTSClient) TTSConfig {
	cfg := DefaultTTSConfig()
	cfg.Endpoint = f.URL + "/v1/tts" // http:// becomes ws://
	cfg.APIKey = "xai-test-key-0123456789"
	cfg.Timeout = 2 * time.Second
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	cfg.Client = client
	return cfg
}

func sendMsg(ctx context.Context, c *websocket.Conn, m wsMsg) {
	b, _ := json.Marshal(m)
	_ = c.Write(ctx, websocket.MessageText, b)
}

func audioDelta(n int) wsMsg {
	return wsMsg{Type: "audio.delta", Delta: base64.StdEncoding.EncodeToString(make([]byte, n))}
}

// readUtterance reads text.delta messages until text.done and returns the text.
func readUtterance(ctx context.Context, c *websocket.Conn) (string, error) {
	var text strings.Builder
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return "", err
		}
		var m wsMsg
		_ = json.Unmarshal(data, &m)
		switch m.Type {
		case "text.delta":
			text.WriteString(m.Delta)
		case "text.done":
			return text.String(), nil
		}
	}
}

type wsHarness struct {
	proc *StreamingTTSProcessor
	sess *core.Session
	sink *core.PassthroughProcessor
}

func wsStart(t *testing.T, cfg TTSConfig) *wsHarness {
	t.Helper()
	proc, err := NewStreamingTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &core.PassthroughProcessor{}
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
	t.Cleanup(func() { sess.Close() })
	return &wsHarness{proc: proc, sess: sess, sink: sink}
}

func (h *wsHarness) say(t *testing.T, turn string, id uint64, text string, final bool) {
	t.Helper()
	if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: turn, Gen: 1, UnitID: id, Text: text, IsFinal: final}); err != nil {
		t.Fatal(err)
	}
}

func wsFrames[T core.Frame](h *wsHarness) (out []T) {
	for _, f := range h.sink.GetFrames() {
		if v, ok := f.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func wsWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestStreamingTTS_WireAndFirstAudioBeforeDone(t *testing.T) {
	release := make(chan struct{})
	got := make(chan string, 1)
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		text, err := readUtterance(ctx, c)
		if err != nil {
			return
		}
		got <- text
		sendMsg(ctx, c, audioDelta(1920))
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		sendMsg(ctx, c, audioDelta(960))
		sendMsg(ctx, c, wsMsg{Type: "audio.done"})
		_, _, _ = c.Read(ctx) // until the client hangs up
	})
	h := wsStart(t, f.cfg(&mockTTSClient{}))
	h.say(t, "t1", 1, "Hello world", true)

	q := <-f.queries
	for k, want := range map[string]string{"voice": "eve", "language": "en", "codec": "pcm", "sample_rate": "24000", "optimize_streaming_latency": "1"} {
		if q.Get(k) != want {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if a := <-f.auth; a != "Bearer xai-test-key-0123456789" {
		t.Errorf("Authorization = %q", a)
	}
	if text := <-got; text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	wsWait(t, "the first audio frame while the utterance is still open", func() bool { return len(wsFrames[*core.OutputAudioFrame](h)) >= 1 })
	if n := len(wsFrames[*core.TTSStoppedFrame](h)); n != 0 {
		t.Fatalf("%d TTSStopped before audio.done", n)
	}
	close(release)
	wsWait(t, "TTSStopped", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 1 })
	audio := wsFrames[*core.OutputAudioFrame](h)
	if last := audio[len(audio)-1]; !last.IsFinal || !last.TurnFinal {
		t.Errorf("last frame final markers = %t %t, want true true", last.IsFinal, last.TurnFinal)
	}
}

func TestStreamingTTS_OneConnectionPerReply(t *testing.T) {
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		for {
			if _, err := readUtterance(ctx, c); err != nil {
				return
			}
			sendMsg(ctx, c, audioDelta(1920))
			sendMsg(ctx, c, wsMsg{Type: "audio.done"})
		}
	})
	h := wsStart(t, f.cfg(&mockTTSClient{}))
	h.say(t, "t1", 1, "one", false)
	h.say(t, "t1", 2, "two", true)
	wsWait(t, "both units", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 2 })
	if n := f.opens.Load(); n != 1 {
		t.Fatalf("%d connections for one reply, want 1", n)
	}
	h.say(t, "t2", 3, "three", true) // a new reply must not reuse the old connection
	wsWait(t, "the next reply", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 3 })
	if n := f.opens.Load(); n != 2 {
		t.Fatalf("%d connections after a second reply, want 2", n)
	}
}

func TestStreamingTTS_InterruptionClosesTheConnection(t *testing.T) {
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		if _, err := readUtterance(ctx, c); err != nil {
			return
		}
		sendMsg(ctx, c, audioDelta(3000))
		_, _, _ = c.Read(ctx) // returns when the client hangs up
	})
	h := wsStart(t, f.cfg(&mockTTSClient{}))
	h.say(t, "t1", 1, "a long sentence", true)
	wsWait(t, "audio", func() bool { return len(wsFrames[*core.OutputAudioFrame](h)) >= 1 })
	_ = h.sess.Post(&core.InterruptionFrame{})
	select {
	case <-f.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the connection was not closed on interruption")
	}
	time.Sleep(30 * time.Millisecond)
	if n := len(wsFrames[*core.TTSStoppedFrame](h)); n != 0 {
		t.Errorf("%d TTSStopped after an interruption", n)
	}
	if n := len(wsFrames[*core.ErrorFrame](h)); n != 0 {
		t.Errorf("%d ErrorFrames after an interruption", n)
	}
}

func TestStreamingTTS_ErrorBeforeAudioRetriesAsUnary(t *testing.T) {
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		if _, err := readUtterance(ctx, c); err != nil {
			return
		}
		sendMsg(ctx, c, wsMsg{Type: "error", Message: "voice unavailable"})
		_, _, _ = c.Read(ctx)
	})
	unary := &mockTTSClient{pcmData: make([]byte, 1920)}
	h := wsStart(t, f.cfg(unary))
	h.say(t, "t1", 1, "Hello", true)
	wsWait(t, "TTSStopped", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 1 })
	unary.mu.Lock()
	calls := unary.calls
	unary.mu.Unlock()
	if calls != 1 {
		t.Fatalf("unary calls = %d, want 1", calls)
	}
	if n := len(wsFrames[*core.ErrorFrame](h)); n != 0 {
		t.Errorf("%d ErrorFrames, want none: the fallback recovered", n)
	}
	if len(wsFrames[*core.OutputAudioFrame](h)) == 0 {
		t.Error("the unary fallback produced no audio")
	}
}

func TestStreamingTTS_DialFailureRetriesAsUnary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	t.Cleanup(srv.Close)
	cfg := DefaultTTSConfig()
	cfg.Endpoint, cfg.APIKey, cfg.Timeout = srv.URL+"/v1/tts", "xai-test-key-0123456789", 2*time.Second
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	unary := &mockTTSClient{pcmData: make([]byte, 1920)}
	cfg.Client = unary
	h := wsStart(t, cfg)
	h.say(t, "t1", 1, "Hello", true)
	wsWait(t, "TTSStopped", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 1 })
	unary.mu.Lock()
	defer unary.mu.Unlock()
	if unary.calls != 1 {
		t.Fatalf("unary calls = %d, want 1", unary.calls)
	}
}

func TestStreamingTTS_UnauthorizedIsNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(srv.Close)
	cfg := DefaultTTSConfig()
	cfg.Endpoint, cfg.APIKey, cfg.Timeout = srv.URL+"/v1/tts", "xai-test-key-0123456789", 2*time.Second
	cfg.RetryPolicy = resilience.Policy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	unary := &mockTTSClient{pcmData: make([]byte, 1920)}
	cfg.Client = unary
	h := wsStart(t, cfg)
	h.say(t, "t1", 1, "Hello", true)
	wsWait(t, "the error", func() bool { return len(wsFrames[*core.ErrorFrame](h)) == 1 })
	unary.mu.Lock()
	defer unary.mu.Unlock()
	if unary.calls != 0 {
		t.Fatalf("unary calls = %d: a rejected key must surface, not be retried", unary.calls)
	}
}

func TestStreamingTTS_NoReplayOnceAudioHasStarted(t *testing.T) {
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		if _, err := readUtterance(ctx, c); err != nil {
			return
		}
		sendMsg(ctx, c, audioDelta(3000)) // full 20 ms frames go out
		sendMsg(ctx, c, wsMsg{Type: "error", Message: "synthesis failed"})
		_, _, _ = c.Read(ctx)
	})
	unary := &mockTTSClient{pcmData: make([]byte, 1920)}
	h := wsStart(t, f.cfg(unary))
	h.say(t, "t1", 1, "Hello", true)
	wsWait(t, "the error", func() bool { return len(wsFrames[*core.ErrorFrame](h)) == 1 })
	wsWait(t, "the closing TTSStopped", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 1 })
	unary.mu.Lock()
	defer unary.mu.Unlock()
	if unary.calls != 0 {
		t.Fatalf("unary calls = %d: audio already played, a retry would replay the sentence", unary.calls)
	}
}

func TestNewStreamingTTSProcessor_RequiresAKey(t *testing.T) {
	if _, err := NewStreamingTTSProcessor(DefaultTTSConfig()); err == nil {
		t.Fatal("a missing API key was accepted")
	}
}

// A reply may outlive the attempt timeout while audio keeps arriving (the producer is paced to
// playback speed); only a silent connection is given up on.
func TestStreamingTTS_LongStreamIsNotCutByTheTimeout(t *testing.T) {
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		if _, err := readUtterance(ctx, c); err != nil {
			return
		}
		for i := 0; i < 8; i++ { // 8 x 100 ms = 800 ms against a 300 ms limit
			time.Sleep(100 * time.Millisecond)
			sendMsg(ctx, c, audioDelta(3000))
		}
		sendMsg(ctx, c, wsMsg{Type: "audio.done"})
		_, _, _ = c.Read(ctx)
	})
	cfg := f.cfg(&mockTTSClient{})
	cfg.Timeout = 300 * time.Millisecond
	h := wsStart(t, cfg)
	h.say(t, "t1", 1, "A long sentence", true)
	wsWait(t, "TTSStopped", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 1 })
	if n := len(wsFrames[*core.ErrorFrame](h)); n != 0 {
		t.Errorf("%d ErrorFrames: a stream that kept delivering was cut", n)
	}
}

func TestStreamingTTS_SilentConnectionFallsBackToUnary(t *testing.T) {
	f := newWSFake(t, func(ctx context.Context, c *websocket.Conn) {
		_, _ = readUtterance(ctx, c)
		_, _, _ = c.Read(ctx) // never answers
	})
	unary := &mockTTSClient{pcmData: make([]byte, 1920)}
	cfg := f.cfg(unary)
	cfg.Timeout = 150 * time.Millisecond
	h := wsStart(t, cfg)
	h.say(t, "t1", 1, "Hello", true)
	wsWait(t, "TTSStopped", func() bool { return len(wsFrames[*core.TTSStoppedFrame](h)) == 1 })
	unary.mu.Lock()
	defer unary.mu.Unlock()
	if unary.calls != 1 {
		t.Fatalf("unary calls = %d, want 1", unary.calls)
	}
}

// An in-band error is not evidence about provider health: the unary retry reports that. Counting it
// would let one agent with a bad voice open the process-wide breaker for every call.
func TestWSError_DoesNotCountAgainstTheBreaker(t *testing.T) {
	v := classify(wsError(wsMsg{Type: "error", Message: "bad voice"}))
	if !v.Retryable || !v.NoBreaker {
		t.Fatalf("verdict = %+v, want retryable and NoBreaker", v)
	}
}
