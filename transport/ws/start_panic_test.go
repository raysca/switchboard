package ws_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/transport/token"
	"govoice/transport/ws"
)

type panicAttach struct{}

func (panicAttach) Attach(core.SessionController, int) error { panic("attach bug") }
func (panicAttach) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	return e.PushDirection(f, d)
}

// A panic in session.Start (a processor's Attach) is recovered by the read loop; the session handed back must still be closed.
func TestPanicInStartClosesSession(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	server := ws.NewServer(ws.DefaultServerConfig(val))
	sessCh := make(chan *core.Session, 1)
	server.OnSessionReady = func(c *ws.Connection) (*core.Session, error) {
		pipe, _ := core.NewPipeline(panicAttach{}, c)
		s, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
		sessCh <- s
		return s, err
	}
	ts := httptest.NewServer(server)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+createTestToken(t, "jti-start-panic", now, now.Add(time.Minute)), nil)
	if err != nil {
		t.Fatal(err)
	}
	performHandshake(t, ctx, c)
	for {
		if _, _, err := c.Read(ctx); err != nil {
			t.Logf("closed: %v", err)
			break
		}
	}
	s := <-sessCh
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("session never closed after a panic in Start: state=%v", s.State())
	}
}
