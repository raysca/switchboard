package cartesia

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
)

// replyFake speaks the context protocol: audio per transcript, flush_done on a
// flush, done when continue is false. Every received message is recorded as raw JSON.
type replyFake struct {
	*httptest.Server
	mu         sync.Mutex
	opens      int
	msgs       []map[string]any
	hungUp     chan struct{}
	onNonEmpty func(ctx context.Context, c *websocket.Conn, m map[string]any) bool // true = handled
}

func newReplyFake(t *testing.T) *replyFake {
	f := &replyFake{hungUp: make(chan struct{}, 8)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.opens++
		f.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		defer func() { f.hungUp <- struct{}{} }()
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var m map[string]any
			_ = json.Unmarshal(data, &m)
			f.mu.Lock()
			f.msgs = append(f.msgs, m)
			f.mu.Unlock()
			if m["cancel"] == true {
				continue
			}
			if f.onNonEmpty != nil && f.onNonEmpty(r.Context(), c, m) {
				continue
			}
			if m["transcript"] != "" {
				send(r.Context(), c, chunk(1000))
			}
			switch {
			case m["flush"] == true:
				send(r.Context(), c, map[string]any{"type": "flush_done", "flush_done": true, "flush_id": 1})
			case m["continue"] == false:
				send(r.Context(), c, map[string]any{"type": "done", "done": true})
			}
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *replyFake) cfg() TTSConfig {
	cfg := DefaultTTSConfig()
	cfg.Endpoint = "ws" + strings.TrimPrefix(f.URL, "http")
	cfg.APIKey = "cartesia-test-key-0123456789"
	cfg.Timeout = 2 * time.Second
	cfg.RetryPolicy.MaxAttempts = 1
	return cfg
}

func (f *replyFake) sent() (out []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(out, f.msgs...)
}

func (f *replyFake) opened() int { f.mu.Lock(); defer f.mu.Unlock(); return f.opens }

func (h *harness) unit(t *testing.T, turn string, id uint64, text string, final bool) {
	t.Helper()
	if err := h.sess.Post(&core.SynthesisUnitFrame{Turn: turn, Gen: 1, UnitID: id, Text: text, IsFinal: final}); err != nil {
		t.Fatal(err)
	}
}

// Three units of a reply share one websocket and one context: continue:true +
// flush on all but the last, continue:false closes it. Audio stays attributable per unit.
func TestReply_OneSocketManyUnits(t *testing.T) {
	f := newReplyFake(t)
	h := start(t, f.cfg())
	h.unit(t, "t1", 1, "One. ", false)
	h.unit(t, "t1", 2, "Two. ", false)
	h.unit(t, "t1", 3, "Three.", true)
	waitFor(t, 3*time.Second, "3 stops", func() bool { return h.stopped() == 3 })

	if f.opened() != 1 {
		t.Fatalf("opens=%d, want 1", f.opened())
	}
	m := f.sent()
	type w struct {
		tr       string
		cont, fl bool
	}
	want := []w{{"One. ", true, false}, {"", true, true}, {"Two. ", true, false}, {"", true, true}, {"Three.", false, false}}
	if len(m) != len(want) {
		t.Fatalf("messages: %v", m)
	}
	for i, x := range want {
		fl, _ := m[i]["flush"].(bool)
		if m[i]["transcript"] != x.tr || m[i]["continue"] != x.cont || fl != x.fl || m[i]["context_id"] != m[0]["context_id"] || m[i]["max_buffer_delay_ms"] != float64(0) || m[i]["model_id"] == nil {
			t.Fatalf("message %d: %v", i, m[i])
		}
	}
	last := map[uint64]*core.OutputAudioFrame{}
	for _, a := range h.audio() {
		last[a.UnitID] = a
	}
	for id := uint64(1); id <= 3; id++ {
		if a := last[id]; a == nil || !a.IsFinal || a.TurnFinal != (id == 3) {
			t.Fatalf("unit %d last frame: %+v", id, a)
		}
	}
	waitFor(t, time.Second, "socket closed", func() bool { return len(f.hungUp) == 1 })
	if len(h.errors()) != 0 || h.proc.RequestsTotal.Load() != 3 {
		t.Fatalf("errs=%v", h.errors())
	}
}

// Barge-in mid-reply sends cancel for the context and hangs up; no stop for the cut unit.
func TestReply_CancelMidReply(t *testing.T) {
	f := newReplyFake(t)
	stuck := false
	f.onNonEmpty = func(ctx context.Context, c *websocket.Conn, m map[string]any) bool {
		if m["transcript"] == "Two. " {
			send(ctx, c, chunk(3000))
			stuck = true
			return true
		}
		return stuck // swallow unit 2's flush: it never finishes
	}
	h := start(t, f.cfg())
	h.unit(t, "t1", 1, "One. ", false)
	h.unit(t, "t1", 2, "Two. ", false)
	waitFor(t, 3*time.Second, "unit 1 stop and unit 2 audio", func() bool { return h.stopped() == 1 && len(h.audio()) > 2 })
	_ = h.sess.Post(&core.InterruptionFrame{})
	select {
	case <-f.hungUp:
	case <-time.After(3 * time.Second):
		t.Fatal("socket not closed on interruption")
	}
	var cancels int
	for _, m := range f.sent() {
		if m["cancel"] == true && m["context_id"] == f.sent()[0]["context_id"] {
			cancels++
		}
	}
	if cancels != 1 || h.stopped() != 1 || len(h.errors()) != 0 {
		t.Fatalf("cancels=%d stopped=%d errs=%v msgs=%v", cancels, h.stopped(), h.errors(), f.sent())
	}
}

// An error on unit 2 is attributed to unit 2; unit 1 already completed.
func TestReply_ErrorAttributedToUnit(t *testing.T) {
	f := newReplyFake(t)
	f.onNonEmpty = func(ctx context.Context, c *websocket.Conn, m map[string]any) bool {
		if m["transcript"] == "Two. " {
			send(ctx, c, map[string]any{"type": "error", "error": "bad", "status_code": 400})
			return true
		}
		return false
	}
	h := start(t, f.cfg())
	h.unit(t, "t1", 1, "One. ", false)
	h.unit(t, "t1", 2, "Two. ", true)
	waitFor(t, 3*time.Second, "error", func() bool { return len(frames[*core.ErrorFrame](h)) == 1 })
	if e := frames[*core.ErrorFrame](h)[0]; e.UnitID != 2 || h.stopped() != 1 {
		t.Fatalf("error unit %d, stopped %d", e.UnitID, h.stopped())
	}
	waitFor(t, time.Second, "socket closed", func() bool { return len(f.hungUp) == 1 })
}

// The next reply gets its own socket and context.
func TestReply_NextReplyReconnects(t *testing.T) {
	f := newReplyFake(t)
	h := start(t, f.cfg())
	h.unit(t, "t1", 1, "One.", true)
	waitFor(t, 3*time.Second, "reply 1", func() bool { return h.stopped() == 1 })
	h.unit(t, "t2", 2, "Two.", true)
	waitFor(t, 3*time.Second, "reply 2", func() bool { return h.stopped() == 2 })
	m := f.sent()
	if f.opened() != 2 || len(m) != 2 || m[0]["context_id"] == m[1]["context_id"] {
		t.Fatalf("opens=%d msgs=%v", f.opened(), m)
	}
}

// Past contextTTL idle, the next unit opens a fresh context on the same socket
// (Cartesia expires contexts 1 s after their last output).
func TestReply_ExpiredContextReopens(t *testing.T) {
	f := newReplyFake(t)
	h := start(t, f.cfg())
	h.unit(t, "t1", 1, "One. ", false)
	waitFor(t, 3*time.Second, "unit 1", func() bool { return h.stopped() == 1 })
	time.Sleep(contextTTL + 100*time.Millisecond)
	h.unit(t, "t1", 2, "Two.", true)
	waitFor(t, 3*time.Second, "unit 2", func() bool { return h.stopped() == 2 })
	m := f.sent()
	if f.opened() != 1 || len(m) != 3 || m[2]["context_id"] == m[0]["context_id"] {
		t.Fatalf("opens=%d msgs=%v", f.opened(), m)
	}
}

// A blank final unit ends the reply through EndReply: the socket is closed.
func TestReply_BlankFinalClosesSocket(t *testing.T) {
	f := newReplyFake(t)
	h := start(t, f.cfg())
	h.unit(t, "t1", 1, "One. ", false)
	h.unit(t, "t1", 2, " ", true)
	waitFor(t, 3*time.Second, "2 stops", func() bool { return h.stopped() == 2 })
	select {
	case <-f.hungUp:
	case <-time.After(3 * time.Second):
		t.Fatal("socket left open after the reply ended")
	}
}
