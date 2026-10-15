package ws_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/transport/ws"
)

// sendRaw completes the handshake, sends one binary message of n zero bytes and
// returns once the server has either rejected it or closed. got is the byte count
// the server's frame decoder saw for that message (0 if it never reached it).
func sendRaw(t *testing.T, n int) (e *pingEnv, got uint64) {
	t.Helper()
	e = newPingEnv(t, func(c *ws.ServerConfig) {})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)
	base := e.count("frame_in")
	c.CloseRead(ctx) // lets the library answer the server's close frame promptly
	_ = c.Write(ctx, websocket.MessageBinary, make([]byte, n))
	if !waitFor(t, 5*time.Second, func() bool { return e.count("client_disconnected") == 1 }) {
		t.Fatal("connection was not closed")
	}
	return e, e.count("frame_in") - base
}

// 32-64 KB messages exceed the library default read limit but are within the
// advertised max: they must reach the frame decoder (here: malformed protobuf).
func TestMessageUpToAdvertisedMaxReachesTheFrameDecoder(t *testing.T) {
	e, got := sendRaw(t, 48*1024)
	if got != 48*1024 || e.count("protocol_error") != 1 {
		t.Fatalf("decoder saw %d bytes, protocol_error=%d", got, e.count("protocol_error"))
	}
}

// Over MaxEncodedFrameSize but inside the headroom: the decoder rejects it as a protocol error.
func TestMessageOverAdvertisedMaxIsAProtocolError(t *testing.T) {
	n := ws.MaxEncodedFrameSize + 512
	e, got := sendRaw(t, n)
	if got != uint64(n) || e.count("protocol_error") != 1 {
		t.Fatalf("decoder saw %d bytes, protocol_error=%d", got, e.count("protocol_error"))
	}
}

// Beyond the read limit the library closes the connection before any frame is decoded.
func TestMessageBeyondReadLimitClosesTheConnection(t *testing.T) {
	e, got := sendRaw(t, 80*1024)
	if got != 0 || e.count("protocol_error") != 0 {
		t.Fatalf("decoder saw %d bytes, protocol_error=%d", got, e.count("protocol_error"))
	}
}
