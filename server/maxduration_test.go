package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/server/storage"
	"govoice/transport/ws"
)

// The max-call-duration guard has one owner: it counts, records the timeline event and tells the
// client why (RTVI error), then closes normally (M2-04).
func TestMaxCallDurationFiresOnceWithReason(t *testing.T) {
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", "management-test-key-32-bytes-long-and-separate")
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Token.Secret = "voice-test-secret-32-bytes-long-and-separate"
	cfg.Limits.MaxCallDuration = 700 * time.Millisecond
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.env = testProviderEnv(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	flowID := readyTestFlow(t, s)
	for _, stage := range []string{"stt", "llm", "tts"} {
		s.registry.Register("google."+stage, func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	}
	ts := httptest.NewServer(s.wsServer)
	defer ts.Close()

	w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","flow_id":"`+flowID+`"}`)
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+resp.Data.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if err := conn.Write(ctx, websocket.MessageBinary, ready); err != nil {
		t.Fatal(err)
	}
	sawReason := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Fatalf("close = %v, want normal closure", err)
			}
			break
		}
		sawReason = sawReason || strings.Contains(string(data), "max_call_duration")
	}
	if !sawReason {
		t.Error("client never received the max_call_duration RTVI error")
	}
	waitFor(t, func() bool { return s.metrics.MaxDurationTermTotal.Load() == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := s.metrics.MaxDurationTermTotal.Load(); n != 1 {
		t.Fatalf("max duration terminations = %d, want exactly 1", n)
	}
	// The server ended the call (the client merely echoed the close): one server disconnect, no normal one.
	waitFor(t, func() bool { return s.metrics.DisconnectsServerError.Load() == 1 })
	if n, srv := s.metrics.DisconnectsNormal.Load(), s.metrics.DisconnectsServerError.Load(); n != 0 || srv != 1 {
		t.Fatalf("disconnects: normal=%d server=%d, want 0 and 1", n, srv)
	}
}

// K10: the call's timer runs for the CallSpec's limit (the flow's, already clamped to the server's), not
// the server's own: a flow limit tighter than the server's cannot be waited for in an e2e (60 s minimum).
func TestTheCallLimitTimerRunsForTheCallSpecsLimit(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Limits.MaxCallDuration = 30 * time.Minute
	s := &Server{cfg: cfg}
	timer := s.callLimitTimer(CallSpec{MaxCallDuration: 30 * time.Millisecond})
	if timer == nil {
		t.Fatal("a limit was set: expected a timer")
	}
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-time.After(5 * time.Second):
		t.Fatal("the timer used the server's 30 minutes, not the call's limit")
	}
	if s.callLimitTimer(CallSpec{}) != nil {
		t.Fatal("no limit, no timer")
	}
}

// K10 end to end: a 30 minute server limit and a call whose spec says 700 ms ends with the time limit,
// so the timer reads the CallSpec, not the server's own limit.
func TestCallBehaviorTheCallEndsAtItsSpecsLimitNotTheServers(t *testing.T) {
	r := newFlowRigE2E(t, newProbe(), serverIdle(0, ""), func(c *Config) { c.Limits.MaxCallDuration = 30 * time.Minute })
	r.s.adjustCall = func(call *CallSpec) { call.MaxCallDuration = 700 * time.Millisecond }
	c := r.dial(behaviorFlow(t, r, "Plain", "plainvoice", ``))
	r.waitEnded(c.sessionID, storage.EndTimeLimit)
}
