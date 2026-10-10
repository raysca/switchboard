package ws_test

import (
	"context"
	"errors"
	"fmt"
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

// set-muted over the socket mutes and unmutes the call's session.
func TestSetMutedTogglesSessionInputMute(t *testing.T) {
	ready := make(chan *ws.Connection, 1)
	c, ctx := dialReady(t, "jti-set-muted", func(conn *ws.Connection) { ready <- conn })
	conn := <-ready
	for _, muted := range []bool{true, false} {
		msg, _ := ws.EncodeMessageFrame(fmt.Sprintf(`{"id":"m","label":"rtvi-ai","type":"set-muted","data":{"muted":%t}}`, muted))
		if err := c.Write(ctx, websocket.MessageBinary, msg); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for conn.Session().InputMuted() != muted {
			if time.Now().After(deadline) {
				t.Fatalf("InputMuted() never became %t", muted)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

type userTap struct{ got chan []byte }

func (u userTap) User(f *core.InputAudioFrame) { u.got <- append([]byte(nil), f.Samples...) }
func (userTap) Bot(*core.OutputAudioFrame)     {}

// While muted the recording receives silence of the same length, never speech.
func TestMutedAudioReachesRecordingAsSilence(t *testing.T) {
	tap := userTap{got: make(chan []byte, 8)}
	ready := make(chan *ws.Connection, 1)
	c, ctx := dialReady(t, "jti-muted-tap", func(conn *ws.Connection) { conn.SetAudioTap(tap); ready <- conn })
	conn := <-ready
	msg, _ := ws.EncodeMessageFrame(`{"id":"m","label":"rtvi-ai","type":"set-muted","data":{"muted":true}}`)
	if err := c.Write(ctx, websocket.MessageBinary, msg); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); !conn.Session().InputMuted(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never muted")
		}
	}
	speech := make([]byte, 640)
	for i := range speech {
		speech[i] = 0x55
	}
	audio, _ := ws.EncodeAudioFrame(speech, 16000, 1)
	if err := c.Write(ctx, websocket.MessageBinary, audio); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-tap.got:
		if len(got) != len(speech) {
			t.Fatalf("recorded %d bytes, want %d", len(got), len(speech))
		}
		for _, b := range got {
			if b != 0 {
				t.Fatal("muted speech reached the recording")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tap never saw the muted frame")
	}
}
