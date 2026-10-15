package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func dialWS(t *testing.T, srv *httptest.Server, path string) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c, ctx
}

func sendJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// readUnit reads audio.delta messages until audio.done (or error) and returns the decoded audio.
func readUnit(t *testing.T, ctx context.Context, c *websocket.Conn) ([]byte, ttsMsg) {
	t.Helper()
	var audio []byte
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var m ttsMsg
		_ = json.Unmarshal(data, &m)
		switch m.Type {
		case "audio.delta":
			b, err := base64.StdEncoding.DecodeString(m.Delta)
			if err != nil || len(b)%2 != 0 {
				t.Fatalf("bad audio.delta: err=%v len=%d", err, len(b))
			}
			audio = append(audio, b...)
		case "audio.done", "error":
			return audio, m
		}
	}
}

func TestTTSWSServesTwoUnitsOnOneConnection(t *testing.T) {
	ft := &fakeTTS{pcm: bytes.Repeat([]byte{3, 0}, 12000)} // 24000 bytes: several 100 ms chunks
	srv := httptest.NewServer(New(mustConfig(t), ft, nil, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/tts?voice=eve&language=en&codec=pcm&sample_rate=24000")
	for i, text := range []string{"First sentence.", "Second sentence."} {
		sendJSON(t, ctx, c, ttsMsg{Type: "text.delta", Delta: text})
		sendJSON(t, ctx, c, ttsMsg{Type: "text.done"})
		audio, end := readUnit(t, ctx, c)
		if end.Type != "audio.done" || len(audio) != 24000 {
			t.Fatalf("unit %d: end=%s audio=%d", i, end.Type, len(audio))
		}
		if got := ft.gotText.Load(); got != text {
			t.Fatalf("unit %d: backend got %q", i, got)
		}
	}
	if ft.voice.Load() != "af_heart" {
		t.Fatalf("voice %v", ft.voice.Load())
	}
}

func TestTTSWSEmptyUnitAndTextCap(t *testing.T) {
	srv := httptest.NewServer(New(mustConfig(t), &fakeTTS{pcm: []byte{1, 0}}, nil, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/tts")
	sendJSON(t, ctx, c, ttsMsg{Type: "text.done"})
	if _, end := readUnit(t, ctx, c); end.Type != "audio.done" {
		t.Fatalf("empty unit: %s", end.Type)
	}
	sendJSON(t, ctx, c, ttsMsg{Type: "text.delta", Delta: strings.Repeat("a", maxTTSText+1)})
	if _, end := readUnit(t, ctx, c); end.Type != "error" {
		t.Fatalf("oversized text: %s", end.Type)
	}
}

func TestTTSWSBackendErrorSendsError(t *testing.T) {
	srv := httptest.NewServer(New(mustConfig(t), &fakeTTS{err: &BackendError{Status: 404, Msg: "no voice"}}, nil, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/tts?voice=brokenvoice")
	sendJSON(t, ctx, c, ttsMsg{Type: "text.delta", Delta: "hi"})
	sendJSON(t, ctx, c, ttsMsg{Type: "text.done"})
	if _, end := readUnit(t, ctx, c); end.Type != "error" || end.Message == "" {
		t.Fatalf("got %+v", end)
	}
}

func TestTTSWSCancelsSynthesisOnClose(t *testing.T) {
	ft := &fakeTTS{block: make(chan struct{}), cancelled: make(chan struct{})}
	srv := httptest.NewServer(New(mustConfig(t), ft, nil, okReady).Handler())
	defer srv.Close()
	c, ctx := dialWS(t, srv, "/v1/tts")
	sendJSON(t, ctx, c, ttsMsg{Type: "text.delta", Delta: "a long sentence"})
	sendJSON(t, ctx, c, ttsMsg{Type: "text.done"})
	time.Sleep(50 * time.Millisecond) // let the handler reach Synthesize
	c.CloseNow()                      // barge-in: the client just hangs up
	select {
	case <-ft.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("backend request was not cancelled after the client closed")
	}
}
