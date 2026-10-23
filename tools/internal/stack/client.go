package stack

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
}

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
		return nil, fmt.Errorf("dial: %v", err)
	}
	conn.SetReadLimit(1 << 20)
	if err := Handshake(dctx, conn, pushToTalk); err != nil {
		conn.CloseNow()
		return nil, err
	}
	c := &Client{Conn: conn, T0: time.Now(), max: maxEvents, closed: make(chan struct{})}
	go c.reader(context.Background())
	return c, nil
}

func (c *Client) reader(ctx context.Context) {
	seq := uint64(1)
	for {
		typ, data, err := c.Conn.Read(ctx)
		if err != nil {
			c.CloseErr, c.CloseAt = err, time.Now()
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
	return c.Conn.Write(context.Background(), websocket.MessageBinary, f)
}

// SendText sends an RTVI send-text client message (a text turn, no audio).
func (c *Client) SendText(text string) error {
	return c.SendMsg("send-text", map[string]string{"content": text})
}

// Wait polls cond until it holds (true), limit passes or the connection ends (false).
func (c *Client) Wait(limit time.Duration, cond func() bool) bool {
	for end := time.Now().Add(limit); time.Now().Before(end); {
		if cond != nil && cond() {
			return true
		}
		select {
		case <-c.closed:
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return cond != nil && cond()
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
}
