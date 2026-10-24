// Package wsclient is the RTVI test client used by the agent eval engine and the free harnesses
// (livecheck, loadtest): it dials /ws, runs the handshake and records server events with arrival times.
package wsclient

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

// DefaultMaxEvents bounds the events a Client keeps; later ones are dropped and counted.
const DefaultMaxEvents = 4096

// Event is one server message with its arrival time.
type Event struct {
	At    time.Time
	Kind  string // "audio" or the RTVI message type
	Text  string
	Final bool
	Dur   time.Duration // audio: playback length of the chunk
}

// Client is one RTVI WebSocket call: it records every server event with its arrival time.
type Client struct {
	Conn     *websocket.Conn
	T0       time.Time // bot-ready
	CloseErr error     // set before Closed() fires
	CloseAt  time.Time // set before Closed() fires

	mu      sync.Mutex
	evs     []Event
	max     int
	dropped int
	closed  chan struct{} // closed when the reader ends

	ctx    context.Context // owner lifetime: ends with Close; bounds the reader and every send
	cancel context.CancelFunc
}

// sendTimeout bounds one write to the server.
const sendTimeout = 10 * time.Second

// Dial opens the call's WebSocket, runs the RTVI handshake and starts the reader, which ends
// when the connection ends.
func Dial(ctx context.Context, public, token string, pushToTalk bool) (*Client, error) {
	return DialN(ctx, public, token, pushToTalk, DefaultMaxEvents)
}

// DialN is Dial with a custom event cap (long audio calls record far more than a text call).
func DialN(ctx context.Context, public, token string, pushToTalk bool, maxEvents int) (*Client, error) {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, "ws://"+public+"/ws?token="+token, nil)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	if err := Handshake(dctx, conn, pushToTalk); err != nil {
		conn.CloseNow()
		return nil, err
	}
	c := &Client{Conn: conn, T0: time.Now(), max: maxEvents, closed: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	go c.reader(c.ctx)
	return c, nil
}

func (c *Client) reader(ctx context.Context) {
	defer close(c.closed)
	defer func() {
		if r := recover(); r != nil { // a client anomaly must not take the process down
			c.CloseErr, c.CloseAt = fmt.Errorf("reader panicked: %v", r), time.Now()
		}
	}()
	seq := uint64(1)
	for {
		typ, data, err := c.Conn.Read(ctx)
		if err != nil {
			c.CloseErr, c.CloseAt = err, time.Now()
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
		e := Event{At: time.Now()}
		switch fr := f.(type) {
		case *core.InputAudioFrame: // bot audio as seen by the client
			e.Kind = "audio"
			e.Dur = fr.Duration
			if e.Dur == 0 && fr.SampleRate > 0 {
				e.Dur = time.Duration(len(fr.Samples)/2/max(fr.Channels, 1)) * time.Second / time.Duration(fr.SampleRate)
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
			e.Kind, e.Text, e.Final = m.Type, m.Data.Text, m.Data.Final
			if m.Type == "error" {
				e.Text = m.Data.Error // category only
			}
		default:
			continue
		}
		c.mu.Lock()
		if len(c.evs) < c.max {
			c.evs = append(c.evs, e)
		} else {
			c.dropped++
		}
		c.mu.Unlock()
	}
}

// Snap returns a copy of the recorded events.
func (c *Client) Snap() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.evs...)
}

// Dropped counts events past the cap.
func (c *Client) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// SendMsg sends one RTVI client message.
func (c *Client) SendMsg(typ string, data any) error {
	b, _ := json.Marshal(map[string]any{"id": "m1", "label": "rtvi-ai", "type": typ, "data": data})
	f, err := ws.EncodeMessageFrame(string(b))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, sendTimeout)
	defer cancel()
	return c.Conn.Write(ctx, websocket.MessageBinary, f)
}

// SendText sends an RTVI send-text client message (a text turn, no audio).
func (c *Client) SendText(text string) error {
	return c.SendMsg("send-text", map[string]string{"content": text})
}

// Closed fires when the connection ends.
func (c *Client) Closed() <-chan struct{} { return c.closed }

// FirstAfter returns the first event after t whose kind is one of kinds.
func (c *Client) FirstAfter(t time.Time, kinds ...string) (Event, bool) {
	for _, e := range c.Snap() {
		if e.At.After(t) {
			for _, k := range kinds {
				if e.Kind == k {
					return e, true
				}
			}
		}
	}
	return Event{}, false
}

// Has reports whether an event of kind arrived after t.
func (c *Client) Has(t time.Time, kind string) bool { _, ok := c.FirstAfter(t, kind); return ok }

// Texts joins event texts of a kind after t ("final" restricts transcripts to finals). Streamed
// bot text keeps its own spacing; transcripts are joined with a space.
func (c *Client) Texts(t time.Time, kind string, finalOnly bool) string {
	var parts []string
	for _, e := range c.Snap() {
		if e.At.After(t) && e.Kind == kind && (!finalOnly || e.Final) {
			parts = append(parts, e.Text)
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

// Count counts events of kind after t.
func (c *Client) Count(t time.Time, kind string) (n int) {
	for _, e := range c.Snap() {
		if e.At.After(t) && e.Kind == kind {
			n++
		}
	}
	return
}

// Close ends the call normally and waits for the reader to exit.
func (c *Client) Close() {
	_ = c.Conn.Close(websocket.StatusNormalClosure, "")
	<-c.closed
	c.cancel()
}

// Handshake sends RTVI client-ready and waits up to 20 s for bot-ready.
func Handshake(ctx context.Context, conn *websocket.Conn, pushToTalk bool) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ptt := ""
	if pushToTalk {
		ptt = `,"push_to_talk":true`
	}
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"` + ptt + `}}`)
	if err := conn.Write(ctx, websocket.MessageBinary, ready); err != nil {
		return fmt.Errorf("client-ready: %w", err)
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("no bot-ready: %w", err)
		}
		if f, err := ws.DecodeBinaryFrame(data, 1, time.Now()); err == nil {
			if m, ok := f.(*ws.WireMessageFrame); ok && strings.Contains(m.Data, "bot-ready") {
				return nil
			}
		}
	}
}
