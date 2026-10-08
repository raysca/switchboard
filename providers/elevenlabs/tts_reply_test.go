package elevenlabs

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

// replyFake speaks the multi-context protocol: audio for a context's text, then
// isFinal for that context once it is closed. Every message is recorded.
type replyFake struct {
	*httptest.Server
	mu     sync.Mutex
	opens  int
	msgs   []map[string]any
	hungUp chan struct{}
	// onClose, when set, replaces the default reply to close_context.
	onClose func(ctx context.Context, c *websocket.Conn, cid string) bool
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
			cid, _ := m["context_id"].(string)
			switch {
			case m["close_socket"] == true:
				return
			case m["close_context"] == true:
				if f.onClose != nil && f.onClose(r.Context(), c, cid) {
					continue
				}
				a := audioMsg(1000, "", 0)
				a["contextId"] = cid
				send(r.Context(), c, a)
				send(r.Context(), c, map[string]any{"isFinal": true, "contextId": cid})
			}
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *replyFake) cfg() TTSConfig {
	cfg := DefaultTTSConfig()
	cfg.Endpoint = "ws" + strings.TrimPrefix(f.URL, "http")
	cfg.APIKey = "xi-test-key-0123456789"
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

// Units of a reply share one socket; each gets its own context (open, text with
// flush, close) and ends on that context's isFinal; the last unit closes the socket.
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
	if len(m) < 10 {
		t.Fatalf("messages: %v", m)
	}
	for u, text := range []string{"One. ", "Two. ", "Three."} {
		open, txt, cl := m[u*3], m[u*3+1], m[u*3+2]
		cid := open["context_id"]
		if open["text"] != " " || txt["text"] != text+" " || txt["flush"] != true || cl["close_context"] != true || txt["context_id"] != cid || cl["context_id"] != cid {
			t.Fatalf("unit %d wire: %v %v %v", u+1, open, txt, cl)
		}
		if u > 0 && cid == m[(u-1)*3]["context_id"] {
			t.Fatal("contexts must be distinct per unit")
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
	if m[len(m)-1]["close_socket"] != true || len(h.errors()) != 0 {
		t.Fatalf("tail %v errs %v", m[len(m)-1], h.errors())
	}
}

// Barge-in mid-reply closes the in-flight context and the socket.
func TestReply_CancelMidReply(t *testing.T) {
	f := newReplyFake(t)
	f.onClose = func(ctx context.Context, c *websocket.Conn, cid string) bool {
		if strings.HasPrefix(cid, "u2") {
			a := audioMsg(3000, "", 0)
			a["contextId"] = cid
			send(ctx, c, a) // then never finishes
			return true
		}
		return false
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
	m := f.sent()
	var closedU2, socket int
	for _, x := range m[6:] {
		if x["close_context"] == true && x["context_id"] == m[3]["context_id"] {
			closedU2++
		}
		if x["close_socket"] == true {
			socket++
		}
	}
	if closedU2 != 1 || socket != 1 || h.stopped() != 1 || len(h.errors()) != 0 { // after unit 2's own close: the abort's
		t.Fatalf("closeU2=%d socket=%d stopped=%d errs=%v", closedU2, socket, h.stopped(), h.errors())
	}
}

// An in-band error on unit 2 is attributed to unit 2.
func TestReply_ErrorAttributedToUnit(t *testing.T) {
	f := newReplyFake(t)
	f.onClose = func(ctx context.Context, c *websocket.Conn, cid string) bool {
		if strings.HasPrefix(cid, "u2") {
			send(ctx, c, map[string]any{"error": "bad voice", "code": 400, "contextId": cid})
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

// The next reply dials its own socket.
func TestReply_NextReplyReconnects(t *testing.T) {
	f := newReplyFake(t)
	h := start(t, f.cfg())
	h.unit(t, "t1", 1, "One.", true)
	waitFor(t, 3*time.Second, "reply 1", func() bool { return h.stopped() == 1 })
	h.unit(t, "t2", 2, "Two.", true)
	waitFor(t, 3*time.Second, "reply 2", func() bool { return h.stopped() == 2 })
	if f.opened() != 2 {
		t.Fatalf("opens=%d", f.opened())
	}
}
