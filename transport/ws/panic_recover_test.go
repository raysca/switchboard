package ws_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/transport/token"
	"govoice/transport/ws"
)

// A panic in OnSessionReady (server flow or provider-constructor code) runs on
// the read goroutine: it must end that call, not the process.
func TestPanicInOnSessionReadyEndsTheCallNotTheProcess(t *testing.T) {
	now := time.Now()
	val, err := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	server := ws.NewServer(ws.DefaultServerConfig(val))
	var conn atomic.Pointer[ws.Connection]
	server.OnSessionReady = func(c *ws.Connection) (*core.Session, error) {
		conn.Store(c)
		panic("x")
	}
	ts := httptest.NewServer(server)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+createTestToken(t, "jti-panic", now, now.Add(time.Minute)), nil)
	if err != nil {
		t.Fatal(err)
	}
	performHandshake(t, ctx, c)
	var closeErr error
	for {
		if _, _, closeErr = c.Read(ctx); closeErr != nil {
			break
		}
	}
	if got := websocket.CloseStatus(closeErr); got != websocket.StatusInternalError {
		t.Fatalf("close status %d (%v), want 1011", got, closeErr)
	}
	if errors.Is(closeErr, context.DeadlineExceeded) {
		t.Fatal("connection did not close")
	}
	if got := conn.Load().CloseCause(); got != "panic" {
		t.Fatalf("cause %q", got)
	}
}
