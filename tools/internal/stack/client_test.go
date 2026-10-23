package stack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/goleak"

	"govoice/transport/ws"
)

// rtviServer accepts one call, answers client-ready with bot-ready, then sends msgs and closes.
func rtviServer(msgs []string) (addr string, stop func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err := c.Read(r.Context()); err != nil {
			return
		}
		send := func(m string) {
			f, _ := ws.EncodeMessageFrame(m)
			_ = c.Write(r.Context(), websocket.MessageBinary, f)
		}
		send(`{"id":"2","label":"rtvi-ai","type":"bot-ready","data":{}}`)
		for _, m := range msgs {
			send(m)
		}
		c.Close(websocket.StatusNormalClosure, "")
	}))
	return strings.TrimPrefix(srv.URL, "http://"), srv.Close
}

func msg(typ, data string) string {
	return fmt.Sprintf(`{"id":"x","label":"rtvi-ai","type":%q,"data":%s}`, typ, data)
}

func TestClientRecordsBotTextAndClose(t *testing.T) {
	defer goleak.VerifyNone(t)
	addr, stop := rtviServer([]string{
		msg("bot-llm-started", `{}`),
		msg("bot-llm-text", `{"text":"Two"}`),
		msg("bot-llm-text", `{"text":" plus two"}`),
		msg("bot-llm-stopped", `{}`),
		msg("error", `{"error":"boom","fatal":false}`),
	})
	defer stop()
	t0 := time.Now().Add(-time.Second)
	c, err := Dial(context.Background(), addr, "tok", false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("Closed() never fired")
	}
	if got := c.Texts(t0, "bot-llm-text", false); got != "Two plus two" {
		t.Errorf("Texts = %q", got)
	}
	if !c.Has(t0, "error") || c.Has(t0, "bot-ready-nope") {
		t.Error("Has")
	}
	c.Close()
}

func TestClientCapsEvents(t *testing.T) {
	defer goleak.VerifyNone(t)
	msgs := make([]string, 5000)
	for i := range msgs {
		msgs[i] = msg("bot-llm-text", `{"text":"x"}`)
	}
	addr, stop := rtviServer(msgs)
	defer stop()
	c, err := Dial(context.Background(), addr, "tok", false)
	if err != nil {
		t.Fatal(err)
	}
	<-c.Closed()
	if n := len(c.Snap()); n != DefaultMaxEvents {
		t.Errorf("kept %d events, want %d", n, DefaultMaxEvents)
	}
	if d := c.Dropped(); d != 5000-DefaultMaxEvents {
		t.Errorf("dropped %d", d)
	}
	c.Close()
}

// Streamed bot text keeps its own spacing; transcripts are trimmed and joined with one space.
func TestTextsSpacing(t *testing.T) {
	t0 := time.Now()
	c := &Client{}
	for i, e := range []Event{{Kind: "bot-llm-text", Text: "Two"}, {Kind: "bot-llm-text", Text: " plus two"}, {Kind: "bot-llm-text", Text: " is four. "},
		{Kind: "user-transcription", Text: " What is ", Final: true}, {Kind: "user-transcription", Text: "two plus", Final: false},
		{Kind: "user-transcription", Text: "two plus two? ", Final: true}} {
		e.At = t0.Add(time.Duration(i+1) * time.Millisecond)
		c.evs = append(c.evs, e)
	}
	if got := c.Texts(t0, "bot-llm-text", false); got != "Two plus two is four." {
		t.Errorf("bot text = %q", got)
	}
	if got := c.Texts(t0, "user-transcription", true); got != "What is two plus two?" {
		t.Errorf("finals = %q", got)
	}
}
