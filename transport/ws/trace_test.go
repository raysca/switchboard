package ws_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"govoice/core"
	"govoice/core/coretest"
	"govoice/transport/token"
	"govoice/transport/ws"
)

type boundaryRecorder struct {
	mu     sync.Mutex
	events []core.TraceEvent
}
type boundarySpan struct{ r *boundaryRecorder }

func (r *boundaryRecorder) StartSpan(_ context.Context, e core.TraceEvent) core.Span {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	return boundarySpan{r}
}
func (boundarySpan) SetAttribute(string, any) {}
func (s boundarySpan) AddEvent(e core.TraceEvent) {
	s.r.mu.Lock()
	s.r.events = append(s.r.events, e)
	s.r.mu.Unlock()
}
func (boundarySpan) End(error) {}
func (r *boundaryRecorder) has(name string, session core.SessionID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Name == name && e.SessionID == session {
			return true
		}
	}
	return false
}

func TestWebSocketRTVITraceConnectDisconnectClose(t *testing.T) {
	now := time.Now()
	validator, err := token.NewValidator(token.DefaultConfig(testIssuer, testAudience, []byte(testSecret)), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	r := &boundaryRecorder{}
	server := ws.NewServer(ws.DefaultServerConfig(validator))
	server.OnSessionReady = func(conn *ws.Connection) (*core.Session, error) {
		pipe, _ := core.NewPipeline(&coretest.PassthroughProcessor{})
		s, err := core.NewSession(601, pipe, core.DefaultResourceBudget())
		if err != nil {
			return nil, err
		}
		if err := s.SetTracer(r, core.TracePolicy{}); err != nil {
			return nil, err
		}
		return s, nil
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	u := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "?token=" + createTestToken(t, "trace-boundary", now, now.Add(time.Minute))
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	performHandshake(t, ctx, conn)
	msg, _ := ws.EncodeMessageFrame(`{"id":"2","label":"rtvi-ai","type":"disconnect-bot"}`)
	if err := conn.Write(ctx, websocket.MessageBinary, msg); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(2 * time.Second)
	for !(r.has("transport.connection", "601") && r.has("transport.connect", "601") && r.has("transport.rtvi_ready", "601") && r.has("transport.rtvi_disconnect", "601") && r.has("transport.close", "601")) {
		if time.Now().After(until) {
			t.Fatal("missing correlated transport boundary events")
		}
		time.Sleep(time.Millisecond)
	}
}
