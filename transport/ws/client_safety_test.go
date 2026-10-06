package ws_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/transport/token"
	"govoice/transport/ws"
)

func dialReady(t *testing.T, jti string, onReady func(*ws.Connection)) (*websocket.Conn, context.Context) {
	t.Helper()
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	server := ws.NewServer(ws.DefaultServerConfig(val))
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, _ := core.NewPipeline(conn)
		if onReady != nil {
			go func() {
				for conn.Session() == nil {
					time.Sleep(5 * time.Millisecond)
				}
				onReady(conn)
			}()
		}
		return core.NewSession(conn.ID(), pipe, core.DefaultResourceBudget())
	}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+createTestToken(t, jti, now, now.Add(time.Minute)), nil)
	if err != nil {
		t.Fatal(err)
	}
	performHandshake(t, ctx, c)
	return c, ctx
}

// Provider error text (bodies, GCP project paths) must not reach the client.
func TestErrorFrameSendsOnlyACategoryToTheClient(t *testing.T) {
	c, ctx := dialReady(t, "jti-error-leak", func(conn *ws.Connection) {
		_ = conn.Session().Post(&core.ErrorFrame{Err: errors.New("rpc error: projects/acme-secret-123/locations/eu: quota body")})
	})
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("no error message reached the client: %v", err)
		}
		f, err := ws.DecodeBinaryFrame(data, 1, time.Now())
		if m, ok := f.(*ws.WireMessageFrame); err == nil && ok && strings.Contains(m.Data, `"type":"error"`) {
			if strings.Contains(m.Data, "acme-secret") || !strings.Contains(m.Data, "provider_error") {
				t.Fatalf("client error message = %s, want only the provider_error category", m.Data)
			}
			return
		}
	}
}

// A client-chosen protocol version must not become the close reason: a long
// one exceeds the 123-byte limit and the close frame is never sent.
func TestIncompatibleClientCloseUsesFixedReason(t *testing.T) {
	now := time.Now()
	val, _ := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	ts := httptest.NewServer(ws.NewServer(ws.DefaultServerConfig(val)))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+createTestToken(t, "jti-long-version", now, now.Add(time.Minute)), nil)
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"` + strings.Repeat("9", 200) + `"}}`)
	if err := c.Write(ctx, websocket.MessageBinary, msg); err != nil {
		t.Fatal(err)
	}
	for {
		if _, _, err = c.Read(ctx); err != nil {
			break
		}
	}
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v (%v), want policy violation", got, err)
	}
}
