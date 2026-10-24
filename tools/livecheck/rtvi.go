package main

import (
	"context"
	"fmt"
	"time"

	"github.com/coder/websocket"

	"govoice/transport/ws"
	"govoice/transport/wsclient"
)

// client adds audio pacing to the shared wsclient.Client. Its own wait streams silence while it
// waits: the embedded Wait and Close do not, so use wait and keep audio calls on the paced helpers.
type client struct {
	*wsclient.Client
	next time.Time
	n    int // server session number (1-based dial order)
}

// liveMaxEvents bounds a livecheck call's recorded events (audio frames included).
const liveMaxEvents = 1 << 20

func dial(ctx context.Context, s *server, fl string, pushToTalk bool) (*client, error) {
	tok, err := s.st.Token("livecheck", fl)
	if err != nil {
		return nil, err
	}
	sc, err := wsclient.DialN(ctx, s.public, tok, pushToTalk, liveMaxEvents)
	if err != nil {
		return nil, err
	}
	s.calls++
	return &client{Client: sc, n: s.calls, next: time.Now()}, nil
}

func (c *client) pace(b []byte) error {
	if d := time.Until(c.next); d > 0 {
		time.Sleep(d)
	}
	c.next = c.next.Add(chunkMS * time.Millisecond)
	f, _ := ws.EncodeAudioFrame(b, rate, 1)
	return c.Conn.Write(context.Background(), websocket.MessageBinary, f)
}

// ptt presses (true) or releases (false) the push-to-talk button.
func (c *client) ptt(pressed bool) error {
	return c.SendMsg("push-to-talk", map[string]bool{"pressed": pressed})
}

// setMuted mutes or unmutes the caller server-side.
func (c *client) setMuted(muted bool) error {
	return c.SendMsg("set-muted", map[string]bool{"muted": muted})
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
		case <-c.Closed():
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
	case <-c.Closed():
		return false
	case <-time.After(d):
		return true
	}
}

func (c *client) isClosed() bool {
	select {
	case <-c.Closed():
		return true
	default:
		return false
	}
}

func (c *client) quiet(d time.Duration) { c.wait(d, nil) }

// interims counts non-final user transcripts after t: nonzero means the STT sends interim results.
func (c *client) interims(t time.Time) (n int) {
	for _, e := range c.Snap() {
		if e.At.After(t) && e.Kind == "user-transcription" && !e.Final {
			n++
		}
	}
	return
}

// audioRunEnd returns the end of the contiguous bot audio that covers `from`: the first
// frame after from followed by a gap >= gap. ok=false when audio never paused.
func (c *client) audioRunEnd(from time.Time, gap time.Duration) (time.Time, bool) {
	var last time.Time
	for _, e := range c.Snap() {
		if e.Kind != "audio" || e.At.Before(from) {
			continue
		}
		if !last.IsZero() && e.At.Sub(last) >= gap {
			return last, true
		}
		last = e.At
	}
	if !last.IsZero() && time.Since(last) >= gap { // audio ended and nothing followed
		return last, true
	}
	return last, false
}

func (c *client) audioNow(within time.Duration) bool {
	ev := c.Snap()
	for i := len(ev) - 1; i >= 0; i-- {
		if ev[i].Kind == "audio" {
			return time.Since(ev[i].At) < within
		}
	}
	return false
}

func (c *client) close() {
	for i := 0; i < 100 && c.audioNow(300*time.Millisecond); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	_ = c.Conn.Close(websocket.StatusNormalClosure, "")
}

// timeline prints every non-audio event and bot audio runs (gap >= 250 ms starts a new run),
// relative to bot-ready. Transcript text is shown; error events carry a category only.
func (c *client) timeline() {
	var runStart, runEnd time.Time
	n := 0
	flush := func() {
		if n > 0 {
			fmt.Fprintf(stdout, "    %7.2fs audio run %d frames until %.2fs\n", runStart.Sub(c.T0).Seconds(), n, runEnd.Sub(c.T0).Seconds())
			n = 0
		}
	}
	for _, e := range c.Snap() {
		if e.Kind == "audio" {
			if n > 0 && e.At.Sub(runEnd) >= 250*time.Millisecond {
				flush()
			}
			if n == 0 {
				runStart = e.At
			}
			n++
			runEnd = e.At
			continue
		}
		flush()
		fmt.Fprintf(stdout, "    %7.2fs %s %q final=%v\n", e.At.Sub(c.T0).Seconds(), e.Kind, truncate(e.Text, 60), e.Final)
	}
	flush()
}
