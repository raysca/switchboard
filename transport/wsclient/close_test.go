package wsclient

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

// Close ends the reader goroutine: Closed fires within a second.
func TestCloseEndsReader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		out, _ := ws.EncodeMessageFrame(`{"id":"2","label":"rtvi-ai","type":"bot-ready","data":{}}`)
		_ = c.Write(r.Context(), websocket.MessageBinary, out)
		_, _, _ = c.Read(r.Context()) // until the client closes
	}))
	defer srv.Close()
	c, err := Dial(context.Background(), strings.TrimPrefix(srv.URL, "http://"), "tok", false)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case <-c.Closed():
	case <-time.After(time.Second):
		t.Fatal("reader did not end after Close")
	}
}
