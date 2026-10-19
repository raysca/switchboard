package stack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/transport/ws"
)

func TestRandHexLength(t *testing.T) {
	if got := RandHex(32); len(got) != 64 || got == RandHex(32) {
		t.Fatalf("RandHex(32) = %q", got)
	}
}

func TestHandshakeAnswersBotReady(t *testing.T) {
	var gotPTT bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		if f, err := ws.DecodeBinaryFrame(data, 1, time.Now()); err == nil {
			if m, ok := f.(*ws.WireMessageFrame); ok {
				gotPTT = strings.Contains(m.Data, `"push_to_talk":true`) && strings.Contains(m.Data, "client-ready")
			}
		}
		out, _ := ws.EncodeMessageFrame(`{"id":"2","label":"rtvi-ai","type":"bot-ready","data":{}}`)
		_ = c.Write(r.Context(), websocket.MessageBinary, out)
		_, _, _ = c.Read(r.Context())
	}))
	defer srv.Close()
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if err := Handshake(ctx, conn, true); err != nil {
		t.Fatal(err)
	}
	if !gotPTT {
		t.Fatal("server did not see client-ready with push_to_talk")
	}
}

// Call must never echo a response body (it can carry secrets) nor the key.
func TestCallErrorDoesNotEchoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "SECRET-BODY "+r.Header.Get("Authorization"), 500)
	}))
	defer srv.Close()
	s := &Server{Admin: srv.URL, Key: "k-123"}
	_, err := s.Call("POST", "flows", map[string]string{"a": "b"})
	if err == nil || strings.Contains(err.Error(), "SECRET-BODY") || strings.Contains(err.Error(), "k-123") {
		t.Fatalf("err = %v", err)
	}
}
