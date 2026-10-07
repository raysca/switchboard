package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/transport/ws"
)

// drainFixture is a server with fake providers and one live agent.
func drainFixture(t *testing.T, drain time.Duration) (*Server, *httptest.Server, func(clientReady bool) (*websocket.Conn, error)) {
	t.Helper()
	s := apiTestServer(t)
	s.cfg.Limits.DrainTimeout = drain
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	agentID := readyTestAgent(t, s)
	for _, stage := range []string{"stt", "llm", "tts"} {
		s.registry.Register("google."+stage, func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })
	}
	s.ready.Store(true)
	ts := httptest.NewServer(s.publicMux)
	t.Cleanup(ts.Close)
	dial := func(clientReady bool) (*websocket.Conn, error) {
		w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","agent_id":"`+agentID+`"}`)
		var resp struct {
			Data struct{ Token string } `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		conn, hresp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws?token="+resp.Data.Token, nil)
		if err != nil {
			if hresp != nil && hresp.StatusCode == http.StatusServiceUnavailable {
				return nil, errDrainRefused
			}
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.CloseNow() })
		if clientReady {
			sendReady(t, conn)
		}
		return conn, nil
	}
	return s, ts, dial
}

var errDrainRefused = errors.New("503 before upgrade")

func sendReady(t *testing.T, conn *websocket.Conn) {
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if err := conn.Write(t.Context(), websocket.MessageBinary, ready); err != nil {
		t.Fatal(err)
	}
}

func readyStatus(s *Server) int {
	w := httptest.NewRecorder()
	s.adminMux.ServeHTTP(w, httptest.NewRequest("GET", "/ready", nil))
	return w.Code
}

// Rolling restart, happy path: /ready flips at once, a new call is refused,
// the live call keeps running, and Shutdown returns nil when it ends.
func TestDrain_ActiveSessionSurvivesAndNewRejected(t *testing.T) {
	base := runtime.NumGoroutine()
	s, ts, dial := drainFixture(t, 10*time.Second)
	live, _ := dial(true)
	pending, _ := dial(false) // upgraded before drain, client-ready after
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.activeCalls) == 1 })
	if readyStatus(s) != http.StatusOK {
		t.Fatal("expected ready before drain")
	}

	done := make(chan error, 1)
	go func() { done <- s.Shutdown(context.Background()) }()
	waitFor(t, func() bool { return readyStatus(s) == http.StatusServiceUnavailable })
	waitFor(t, func() bool { return s.metrics.Draining.Load() == 1 && s.metrics.DrainRemaining.Load() == 1 })

	// A new connection during drain is refused before auth with 503.
	if _, err := dial(true); err != errDrainRefused {
		t.Fatalf("new dial during drain: %v, want 503", err)
	}
	// One upgraded before drain is refused at client-ready: server_draining + 1001.
	rejected := pending
	sendReady(t, rejected)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var rerr error
	sawReason := false
	for {
		var data []byte
		if _, data, rerr = rejected.Read(ctx); rerr != nil {
			break
		}
		sawReason = sawReason || bytes.Contains(data, []byte(DrainReason))
	}
	if websocket.CloseStatus(rerr) != websocket.StatusGoingAway || !sawReason {
		t.Fatalf("new session: status %d reason=%v err=%v", websocket.CloseStatus(rerr), sawReason, rerr)
	}
	s.mu.Lock()
	n := len(s.activeCalls)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("activeCalls=%d, want only the original", n)
	}

	select {
	case err := <-done:
		t.Fatalf("shutdown returned %v while a session was live", err)
	case <-time.After(300 * time.Millisecond):
	}

	_ = live.Close(websocket.StatusNormalClosure, "bye") // the call ends on its own
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean drain returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after the last session ended")
	}
	ts.Close()
	waitFor(t, func() bool { return runtime.NumGoroutine() <= base+3 })
}

// At the deadline the client is told server_draining and closed with 1001.
func TestDrain_DeadlineSendsGoingAway(t *testing.T) {
	base := runtime.NumGoroutine()
	s, ts, dial := drainFixture(t, 200*time.Millisecond)
	live, _ := dial(true)
	waitFor(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.activeCalls) == 1 })

	done := make(chan error, 1)
	go func() { done <- s.Shutdown(context.Background()) }()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sawReason := false
	var err error
	for {
		var data []byte
		if _, data, err = live.Read(ctx); err != nil {
			break
		}
		if bytes.Contains(data, []byte(DrainReason)) {
			sawReason = true
		}
	}
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Fatalf("close status %d (%v), want 1001", got, err)
	}
	if !sawReason {
		t.Fatal("no server_draining RTVI error before close")
	}
	if e := <-done; e == nil || !strings.Contains(e.Error(), "forced cancellation of 1") {
		t.Fatalf("shutdown err = %v", e)
	}
	ts.Close()
	waitFor(t, func() bool { return runtime.NumGoroutine() <= base+3 })
}
