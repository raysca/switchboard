package ws_test

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/transport/token"
	"govoice/transport/ws"
)

type pingEnv struct {
	ts       *httptest.Server
	counters sync.Map // event -> *atomic.Uint64
	texts    atomic.Int32
	setup    atomic.Int64 // OnSessionReady delay (ns): setup runs in the read goroutine
	conn     atomic.Pointer[ws.Connection]
	sess     atomic.Pointer[core.Session]
}

func (e *pingEnv) count(ev string) uint64 {
	v, ok := e.counters.Load(ev)
	if !ok {
		return 0
	}
	return v.(*atomic.Uint64).Load()
}

func newPingEnv(t *testing.T, tweak func(*ws.ServerConfig)) *pingEnv {
	t.Helper()
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	cfg := ws.DefaultServerConfig(val)
	cfg.Security.AllowMissingOrigin = true
	cfg.RTVIReadyTimeout = time.Minute
	e := &pingEnv{}
	cfg.Observe = func(ev string, n uint64) {
		v, _ := e.counters.LoadOrStore(ev, new(atomic.Uint64))
		v.(*atomic.Uint64).Add(n)
	}
	tweak(&cfg)
	srv := ws.NewServer(cfg)
	srv.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		time.Sleep(time.Duration(e.setup.Load()))
		pipe, _ := core.NewPipeline(&core.PassthroughProcessor{})
		sess, err := core.NewSession(conn.Pacer().FramesWritten.Load()+1, pipe, core.DefaultResourceBudget())
		e.conn.Store(conn)
		e.sess.Store(sess)
		return sess, err
	}
	srv.OnText = func(*ws.Connection, string) error { e.texts.Add(1); return nil }
	e.ts = httptest.NewServer(srv)
	t.Cleanup(e.ts.Close)
	return e
}

func (e *pingEnv) url(t *testing.T) string {
	now := time.Now()
	return "ws" + strings.TrimPrefix(e.ts.URL, "http") + "?token=" + createTestToken(t, "jti-"+t.Name(), now, now.Add(time.Minute))
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// A silent (muted / text-only) client that answers pings stays connected well
// past the legacy idle read deadline, and can still send a text turn after.
func TestPing_QuietButAliveClientStaysConnected(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {
		c.ReadTimeout = 150 * time.Millisecond // would drop a silent client in legacy mode
		c.PingInterval = 50 * time.Millisecond
		c.PingTimeout = 200 * time.Millisecond
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)

	// The client library answers pings only while a Read is active.
	cctx := c.CloseRead(ctx)
	time.Sleep(700 * time.Millisecond) // > 4x ReadTimeout, > 10 pings
	if cctx.Err() != nil {
		t.Fatalf("quiet client was disconnected: %v", context.Cause(cctx))
	}
	if n, d := e.count("ping_failure"), e.count("client_disconnected"); n != 0 || d != 0 {
		t.Fatalf("ping failures = %d, disconnects = %d, want 0", n, d)
	}
}

func TestPing_TextOnlyClientAfterLongSilence(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {
		c.ReadTimeout = 100 * time.Millisecond
		c.PingInterval = 50 * time.Millisecond
		c.PingTimeout = 200 * time.Millisecond
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)
	cctx := c.CloseRead(ctx)
	time.Sleep(400 * time.Millisecond)
	msg, _ := ws.EncodeMessageFrame(`{"id":"2","label":"rtvi-ai","type":"send-text","data":{"content":"hello"}}`)
	if err := c.Write(ctx, websocket.MessageBinary, msg); err != nil {
		t.Fatalf("text send after silence: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return e.texts.Load() == 1 }) {
		t.Fatalf("text turn not delivered (ctx err %v)", cctx.Err())
	}
}

// A peer whose TCP stack is alive but never reads (no pongs) is dropped within
// at most PingMaxFailures*PingInterval + PingTimeout.
func TestPing_UnresponsivePeerIsDropped_NoLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	e := newPingEnv(t, func(c *ws.ServerConfig) {
		c.PingInterval = 100 * time.Millisecond
		c.PingTimeout = 100 * time.Millisecond
		c.PingMaxFailures = 2
	})
	addr := strings.TrimPrefix(e.ts.URL, "http://")
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	now := time.Now()
	req := "GET /?token=" + createTestToken(t, "jti-raw", now, now.Add(time.Minute)) + " HTTP/1.1\r\nHost: " + addr +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if _, err := raw.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(raw), nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade failed: %v %v", resp, err)
	}
	start := time.Now()
	// never read again
	if !waitFor(t, 3*time.Second, func() bool { return e.count("client_disconnected") == 1 }) {
		e.counters.Range(func(k, v any) bool { t.Logf("%v=%d", k, v.(*atomic.Uint64).Load()); return true })
		buf := make([]byte, 1<<20)
		t.Log(string(buf[:runtime.Stack(buf, true)]))
		t.Fatal("unresponsive peer was not disconnected")
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("detected after %v, want about interval+2*timeout", d)
	}
	if n := e.count("ping_failure"); n < 2 {
		t.Fatalf("ping failures = %d, want >= 2", n)
	}
	raw.Close()
	e.ts.Close()
	if !waitFor(t, 3*time.Second, func() bool { return runtime.NumGoroutine() <= base+2 }) {
		t.Fatalf("goroutine leak: base %d now %d", base, runtime.NumGoroutine())
	}
}

// Session setup runs in the read goroutine, so pongs wait for it; a setup
// longer than PingMaxFailures round trips must not drop a healthy client.
func TestPing_SlowSetupIsNotADeadPeer(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {
		c.PingInterval = 50 * time.Millisecond
		c.PingTimeout = 40 * time.Millisecond
	})
	e.setup.Store(int64(600 * time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c) // bot-ready is sent before setup
	_ = c.CloseRead(ctx)        // answers pings; its ctx cannot see a server drop (the RTVI error is a data message)
	time.Sleep(time.Second)
	if e.sess.Load() == nil {
		t.Fatal("session was not set up")
	}
	if n, d := e.count("ping_failure"), e.count("client_disconnected"); n != 0 || d != 0 {
		t.Fatalf("healthy client penalised during slow setup: ping_failure=%d disconnected=%d", n, d)
	}
}

// A ready client that stops reading (stuck tab, vanished peer) is dropped by
// the pinger and its session torn down.
func TestPing_StuckClientSessionTornDown(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) {
		c.PingInterval = 50 * time.Millisecond
		c.PingTimeout = 40 * time.Millisecond
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)
	if !waitFor(t, 2*time.Second, func() bool { return e.sess.Load() != nil }) {
		t.Fatal("no session")
	}
	select { // client never reads again: no pongs
	case <-e.sess.Load().Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session not torn down after ping failures")
	}
	if !waitFor(t, time.Second, func() bool { return e.count("client_disconnected") == 1 && e.count("disconnect_client") == 1 }) {
		t.Fatalf("disconnected=%d disconnect_client=%d", e.count("client_disconnected"), e.count("disconnect_client"))
	}
}

// Hangup to a peer that never answers the close handshake returns within its
// timeout and ends the connection (CloseNow during Close is a no-op in
// coder/websocket, so the bound must come from elsewhere).
func TestHangup_BoundedForNonReadingPeer(t *testing.T) {
	e := newPingEnv(t, func(c *ws.ServerConfig) { c.PingInterval = 0; c.ReadTimeout = time.Minute })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, e.url(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	performHandshake(t, ctx, c)
	if !waitFor(t, 2*time.Second, func() bool { return e.conn.Load() != nil }) {
		t.Fatal("no connection")
	}
	start := time.Now()
	e.conn.Load().Hangup("user_idle", 200*time.Millisecond)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Hangup took %v", d)
	}
	if !waitFor(t, time.Second, func() bool { return e.count("client_disconnected") == 1 }) {
		t.Fatal("connection not ended after bounded Hangup")
	}
}
