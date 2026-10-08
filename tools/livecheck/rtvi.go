package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/transport/ws"
)

type ev struct {
	t     time.Time
	kind  string // audio | RTVI type
	text  string
	final bool
	dur   time.Duration // audio: playback length of the chunk
}

// client is one RTVI WebSocket call: it records every server event with its arrival time and
// streams audio at real-time pace.
type client struct {
	conn     *websocket.Conn
	mu       sync.Mutex
	evs      []ev
	next     time.Time
	closed   chan struct{} // closed when the connection ends; closeErr/closeAt are set before
	closeErr error
	closeAt  time.Time
	t0       time.Time // bot-ready
	n        int       // server session number (1-based dial order)
}

func dial(ctx context.Context, s *server, agent string) (*client, error) {
	tok, err := s.token(agent)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, "ws://"+s.public+"/ws?token="+tok, nil)
	if err != nil {
		return nil, fmt.Errorf("dial: %v", err)
	}
	conn.SetReadLimit(1 << 20)
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if err := conn.Write(dctx, websocket.MessageBinary, ready); err != nil {
		return nil, err
	}
	s.calls++
	c := &client{conn: conn, closed: make(chan struct{}), n: s.calls}
	for {
		_, data, err := conn.Read(dctx)
		if err != nil {
			conn.CloseNow()
			return nil, fmt.Errorf("no bot-ready: %v", err)
		}
		if f, err := ws.DecodeBinaryFrame(data, 1, time.Now()); err == nil {
			if m, ok := f.(*ws.WireMessageFrame); ok && strings.Contains(m.Data, "bot-ready") {
				break
			}
		}
	}
	c.t0 = time.Now()
	go c.reader(context.Background())
	c.next = time.Now()
	return c, nil
}

func (c *client) reader(ctx context.Context) {
	seq := uint64(1)
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			c.closeErr, c.closeAt = err, time.Now()
			close(c.closed)
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		seq++
		f, err := ws.DecodeBinaryFrame(data, seq, time.Now())
		if err != nil {
			continue
		}
		e := ev{t: time.Now()}
		switch fr := f.(type) {
		case *core.InputAudioFrame: // bot audio as seen by the client
			e.kind = "audio"
			e.dur = fr.Duration
			if e.dur == 0 && fr.SampleRate > 0 {
				e.dur = time.Duration(len(fr.Samples)/2/max(fr.Channels, 1)) * time.Second / time.Duration(fr.SampleRate)
			}
		case *ws.WireMessageFrame:
			var m struct {
				Type string
				Data struct {
					Text  string
					Final bool
					Error string
				}
			}
			if json.Unmarshal([]byte(fr.Data), &m) != nil {
				continue
			}
			e.kind, e.text, e.final = m.Type, m.Data.Text, m.Data.Final
			if m.Type == "error" {
				e.text = m.Data.Error // category only
			}
		default:
			continue
		}
		c.mu.Lock()
		c.evs = append(c.evs, e)
		c.mu.Unlock()
	}
}

func (c *client) snap() []ev {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ev(nil), c.evs...)
}

func (c *client) pace(b []byte) error {
	if d := time.Until(c.next); d > 0 {
		time.Sleep(d)
	}
	c.next = c.next.Add(chunkMS * time.Millisecond)
	f, _ := ws.EncodeAudioFrame(b, rate, 1)
	return c.conn.Write(context.Background(), websocket.MessageBinary, f)
}

// sendText sends an RTVI send-text client message (a text turn, no audio).
func (c *client) sendText(text string) error {
	b, _ := json.Marshal(map[string]any{"id": "t1", "label": "rtvi-ai", "type": "send-text", "data": map[string]string{"content": text}})
	f, err := ws.EncodeMessageFrame(string(b))
	if err != nil {
		return err
	}
	return c.conn.Write(context.Background(), websocket.MessageBinary, f)
}

// stream sends PCM in real time; the caller gets the wall-clock start and end.
func (c *client) stream(pcm []byte) (start, end time.Time, err error) {
	start = time.Now()
	for off := 0; off < len(pcm); off += chunkBytes {
		ch := pcm[off:min(off+chunkBytes, len(pcm))]
		if len(ch) < chunkBytes {
			ch = append(append([]byte(nil), ch...), silenceChunk[:chunkBytes-len(ch)]...)
		}
		if err = c.pace(ch); err != nil {
			return
		}
	}
	return start, time.Now(), nil
}

// wait streams silence until cond is true or limit passes; reports whether cond held.
func (c *client) wait(limit time.Duration, cond func() bool) bool {
	for end := time.Now().Add(limit); time.Now().Before(end); {
		if cond != nil && cond() {
			return true
		}
		select {
		case <-c.closed:
			return false
		default:
		}
		if c.pace(silenceChunk) != nil {
			return false
		}
	}
	return cond != nil && cond()
}

// idle waits without sending anything (a muted client); reports whether the connection stayed open.
func (c *client) idle(d time.Duration) (alive bool) {
	select {
	case <-c.closed:
		return false
	case <-time.After(d):
		return true
	}
}

func (c *client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *client) quiet(d time.Duration) { c.wait(d, nil) }

func (c *client) firstAfter(t time.Time, kinds ...string) (ev, bool) {
	for _, e := range c.snap() {
		if e.t.After(t) {
			for _, k := range kinds {
				if e.kind == k {
					return e, true
				}
			}
		}
	}
	return ev{}, false
}

func (c *client) has(t time.Time, kind string) bool { _, ok := c.firstAfter(t, kind); return ok }

// texts joins event texts of a kind after t ("final" restricts transcripts to finals). Streamed
// bot text keeps its own spacing; transcripts are joined with a space.
func (c *client) texts(t time.Time, kind string, finalOnly bool) string {
	var parts []string
	for _, e := range c.snap() {
		if e.t.After(t) && e.kind == kind && (!finalOnly || e.final) {
			parts = append(parts, e.text)
		}
	}
	if kind == "bot-llm-text" {
		return strings.TrimSpace(strings.Join(parts, ""))
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, " ")
}

func (c *client) count(t time.Time, kind string) (n int) {
	for _, e := range c.snap() {
		if e.t.After(t) && e.kind == kind {
			n++
		}
	}
	return
}

// interims counts non-final user transcripts after t: nonzero means the STT sends interim results.
func (c *client) interims(t time.Time) (n int) {
	for _, e := range c.snap() {
		if e.t.After(t) && e.kind == "user-transcription" && !e.final {
			n++
		}
	}
	return
}

// audioRunEnd returns the end of the contiguous bot audio that covers `from`: the first
// frame after from followed by a gap >= gap. ok=false when audio never paused.
func (c *client) audioRunEnd(from time.Time, gap time.Duration) (time.Time, bool) {
	var last time.Time
	for _, e := range c.snap() {
		if e.kind != "audio" || e.t.Before(from) {
			continue
		}
		if !last.IsZero() && e.t.Sub(last) >= gap {
			return last, true
		}
		last = e.t
	}
	if !last.IsZero() && time.Since(last) >= gap { // audio ended and nothing followed
		return last, true
	}
	return last, false
}

func (c *client) audioNow(within time.Duration) bool {
	ev := c.snap()
	for i := len(ev) - 1; i >= 0; i-- {
		if ev[i].kind == "audio" {
			return time.Since(ev[i].t) < within
		}
	}
	return false
}

func (c *client) close() {
	for i := 0; i < 100 && c.audioNow(300*time.Millisecond); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

// timeline prints every non-audio event and bot audio runs (gap >= 250 ms starts a new run),
// relative to bot-ready. Transcript text is shown; error events carry a category only.
func (c *client) timeline() {
	var runStart, runEnd time.Time
	n := 0
	flush := func() {
		if n > 0 {
			fmt.Fprintf(stdout, "    %7.2fs audio run %d frames until %.2fs\n", runStart.Sub(c.t0).Seconds(), n, runEnd.Sub(c.t0).Seconds())
			n = 0
		}
	}
	for _, e := range c.snap() {
		if e.kind == "audio" {
			if n > 0 && e.t.Sub(runEnd) >= 250*time.Millisecond {
				flush()
			}
			if n == 0 {
				runStart = e.t
			}
			n++
			runEnd = e.t
			continue
		}
		flush()
		fmt.Fprintf(stdout, "    %7.2fs %s %q final=%v\n", e.t.Sub(c.t0).Seconds(), e.kind, truncate(e.text, 60), e.final)
	}
	flush()
}
