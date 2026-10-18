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

// disconnects sums the disconnect_* counters: every closed connection adds exactly one.
func disconnects(e *pingEnv) uint64 {
	return e.count("disconnect_normal") + e.count("disconnect_client") + e.count("disconnect_server") + e.count("disconnect_timeout")
}

// A message up to the advertised max reaches the frame decoder (here: malformed protobuf), which closes with
// 1007: a server-side disconnect, counted once.
func TestMessageUpToAdvertisedMaxReachesTheFrameDecoder(t *testing.T) {
	e, got := sendRaw(t, 18*1024)
	if got != 18*1024 || e.count("protocol_error") != 1 {
		t.Fatalf("decoder saw %d bytes, protocol_error=%d", got, e.count("protocol_error"))
	}
	if e.count("disconnect_server") != 1 || disconnects(e) != e.count("client_disconnected") {
		t.Fatalf("disconnect_server=%d, disconnects=%d, client_disconnected=%d", e.count("disconnect_server"), disconnects(e), e.count("client_disconnected"))
	}
}

// Over MaxEncodedFrameSize but inside the headroom: the decoder rejects it as a protocol error.
func TestMessageOverAdvertisedMaxIsAProtocolError(t *testing.T) {
	n := ws.MaxEncodedFrameSize + 512
	e, got := sendRaw(t, n)
	if got != uint64(n) || e.count("protocol_error") != 1 {
		t.Fatalf("decoder saw %d bytes, protocol_error=%d", got, e.count("protocol_error"))
	}
	if e.count("disconnect_server") != 1 || disconnects(e) != e.count("client_disconnected") {
		t.Fatalf("disconnect_server=%d, disconnects=%d, client_disconnected=%d", e.count("disconnect_server"), disconnects(e), e.count("client_disconnected"))
	}
}

// Beyond the read limit the library closes with 1009 before any frame is decoded; the
// server records it as a protocol error and a server-side disconnect, not a client one.
func TestMessageBeyondReadLimitEndsAsProtocolError(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)
	base := e.count("frame_in")
	// Below the library's 32 KB default: only the explicit read limit refuses it.
	_ = c.Write(ctx, websocket.MessageBinary, make([]byte, 24*1024))
	_, _, err = c.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
		t.Fatalf("client saw close status %d (%v), want 1009", got, err)
	}
	if !waitFor(t, 5*time.Second, func() bool { return e.count("client_disconnected") == 1 }) {
		t.Fatal("connection was not closed")
	}
	if e.count("frame_in") != base || e.count("protocol_error") != 1 ||
		e.count("disconnect_server") != 1 || e.count("disconnect_client") != 0 {
		t.Fatalf("decoded=%d protocol_error=%d server=%d client=%d",
			e.count("frame_in")-base, e.count("protocol_error"), e.count("disconnect_server"), e.count("disconnect_client"))
	}
}

// The client's own disconnect-bot is a normal disconnect, counted once.
func TestDisconnectBotCountsOneNormalDisconnect(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)
	msg, _ := ws.EncodeMessageFrame(`{"id":"2","label":"rtvi-ai","type":"disconnect-bot"}`)
	if err := c.Write(ctx, websocket.MessageBinary, msg); err != nil {
		t.Fatal(err)
	}
	// readLoop's Close counts client_disconnected before its deferred disconnect count: wait for the latter.
	if !waitFor(t, 5*time.Second, func() bool { return disconnects(e) == 1 }) {
		t.Fatal("no disconnect was counted")
	}
	if e.count("disconnect_normal") != 1 || disconnects(e) != 1 {
		t.Fatalf("disconnect_normal=%d, disconnects=%d", e.count("disconnect_normal"), disconnects(e))
	}
}

// The server ending a call from outside the read loop (the end of the call, a fatal error, an idle hang-up,
// a slow client) cancels the connection: a server-side disconnect, not the client's.
func TestServerSideCloseCountsOneServerDisconnect(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)
	if !waitFor(t, 5*time.Second, func() bool { return e.conn.Load() != nil }) {
		t.Fatal("no session")
	}
	e.conn.Load().Close()
	if !waitFor(t, 5*time.Second, func() bool { return disconnects(e) == 1 }) {
		t.Fatal("no disconnect was counted")
	}
	if e.count("disconnect_server") != 1 || e.count("disconnect_client") != 0 {
		t.Fatalf("disconnect_server=%d disconnect_client=%d", e.count("disconnect_server"), e.count("disconnect_client"))
	}
}
