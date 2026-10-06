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

type attachFails struct{}

func (attachFails) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	return e.PushDirection(f, d)
}
func (attachFails) Attach(core.SessionController, int) error { return errors.New("attach failed") }

// A call whose session fails to Start must tear down and free its slot.
// Previously Close hung on a run loop that never started, so the handler and
// the session slot leaked forever.
func TestSessionStartFailureReleasesSlot(t *testing.T) {
	now := time.Now()
	val, err := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	cfg := ws.DefaultServerConfig(val)
	cfg.MaxConcurrentSessions = 1
	cfg.MaxConcurrentSessionsPerIP = 1
	server := ws.NewServer(cfg)

	var calls atomic.Int32
	server.OnSessionReady = func(*ws.Connection) (*core.Session, error) {
		var proc core.Processor = &core.PassthroughProcessor{}
		if calls.Add(1) == 1 {
			proc = attachFails{}
		}
		pipe, _ := core.NewPipeline(proc)
		return core.NewSession(uint64(calls.Load()), pipe, core.DefaultResourceBudget())
	}
	ts := httptest.NewServer(server)
	defer ts.Close()
	url := func(jti string) string {
		return "ws" + strings.TrimPrefix(ts.URL, "http") + "?token=" + createTestToken(t, jti, now, now.Add(time.Minute))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, _, err := websocket.Dial(ctx, url("jti-start-fail"), nil)
	if err != nil {
		t.Fatal(err)
	}
	performHandshake(t, ctx, first)
	for { // server must close the failed call
		if _, _, err := first.Read(ctx); err != nil {
			break
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		second, _, err := websocket.Dial(ctx, url("jti-after-fail-"+time.Now().Format("150405.000000")), nil)
		if err == nil {
			performHandshake(t, ctx, second)
			second.Close(websocket.StatusNormalClosure, "")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session slot was never released after Start failure: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
