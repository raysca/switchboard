package ws_test

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/transport/ws"
)

func TestCloseCauseNamesAPeerThatStoppedAnsweringPings(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {
		c.PingInterval = 50 * time.Millisecond
		c.PingTimeout = 50 * time.Millisecond
		c.PingMaxFailures = 2
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c) // then never read again: the client library answers pings only while reading
	if !waitFor(t, 5*time.Second, func() bool { return e.count("client_disconnected") == 1 }) {
		t.Fatal("peer was not dropped")
	}
	if got := e.conn.Load().CloseCause(); got != "peer_unresponsive" {
		t.Fatalf("cause %q", got)
	}
}

func TestCloseCauseNamesTheConnectionLifetime(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) { c.MaxConnectionLifetime = 300 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)
	cctx := c.CloseRead(ctx)
	select {
	case <-cctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the connection outlived its lifetime")
	}
	if !waitFor(t, 3*time.Second, func() bool { return e.conn.Load().CloseCause() != "" }) {
		t.Fatal("no cause recorded")
	}
	if got := e.conn.Load().CloseCause(); got != "connection_lifetime" {
		t.Fatalf("cause %q", got)
	}
}

func TestACallerWhoClosesTheSocketLeavesNoCause(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	performHandshake(t, ctx, c)
	_ = c.Close(websocket.StatusNormalClosure, "bye")
	if !waitFor(t, 3*time.Second, func() bool { return e.count("client_disconnected") == 1 }) {
		t.Fatal("close not seen")
	}
	if got := e.conn.Load().CloseCause(); got != "" {
		t.Fatalf("a client close names no cause, got %q", got)
	}
}
