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
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	agentID := readyTestAgent(t, s)
	for _, stage := range []string{"stt", "llm", "tts"} {
		s.registry.Register("google."+stage, func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })
	}
	ts := httptest.NewServer(s.wsServer)
	defer ts.Close()

	w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","agent_id":"`+agentID+`"}`)
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
}
