package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"govoice/core"
	"govoice/server/agent"
	"govoice/server/storage"
	"govoice/transport/ws"
)

// End to end: a stored agent and stored credential drive a real WebSocket call.
// The call's provider factories must receive exactly the agent's settings and
// the decrypted credential, the session row must be persisted while the call
// runs and ended when it hangs up.
func TestStoredAgentDrivesCall(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)
	cred, err := s.credentials.Create(t.Context(), credentialInput("google", "stored-google-key"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.agents.Create(t.Context(), agent.CreateRequest{Name: "Billing", Config: json.RawMessage(strings.ReplaceAll(
		`{"stt":{"provider":"google","model":"chirp_2","language":"en-GB","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"google","model":"gemini-test","system_prompt":"You handle billing.","credential_id":"018f0000-0000-7000-8000-000000000001"},"tts":{"provider":"google","voice":"en-GB-Neural2-A","credential_id":"018f0000-0000-7000-8000-000000000001"},"context":{"max_messages":12,"max_tokens":900}}`, "018f0000-0000-7000-8000-000000000001", cred.ID))})
	if err != nil || created.Readiness.State != agent.ReadinessReady {
		t.Fatalf("agent=%+v err=%v", created.Readiness, err)
	}

	var mu sync.Mutex
	got := map[string]StageSpec{}
	var received [][]byte // the slices the server handed over, to check they are wiped
	stub := func(stage string) ProcessorFactory {
		return func(_ *Config, spec StageSpec) (core.Processor, error) {
			mu.Lock()
			received = append(received, spec.Secret)
			spec.Secret = append([]byte(nil), spec.Secret...) // adapters copy what they keep
			got[stage] = spec
			mu.Unlock()
			return &core.PassthroughProcessor{}, nil
		}
	}
	for _, stage := range []string{"stt", "llm", "tts"} {
		s.registry.Register("google."+stage, stub(stage))
	}

	ts := httptest.NewServer(s.wsServer)
	defer ts.Close()
	lastTokenID := ""
	call := func(agentID string) (*websocket.Conn, error) {
		w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","agent_id":"`+agentID+`"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("token: %d %s", w.Code, w.Body)
		}
		var resp struct {
			Data struct{ Token string } `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		claims := jwt.MapClaims{}
		if _, _, err := jwt.NewParser().ParseUnverified(resp.Data.Token, claims); err != nil {
			t.Fatal(err)
		}
		lastTokenID, _ = claims["jti"].(string)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+resp.Data.Token, nil)
		if err != nil {
			return nil, err
		}
		ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
		if err := conn.Write(ctx, websocket.MessageBinary, ready); err != nil {
			return nil, err
		}
		_, _, err = conn.Read(ctx) // bot-ready
		return conn, err
	}

	conn, err := call(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	activeSessions := func(status string) int {
		rows, err := s.store.Sessions().List(t.Context(), storage.SessionFilter{AgentID: created.ID, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 3 && activeSessions("active") == 1 })
	// The session is stored under the token's jti, so the caller can find it.
	if rows, _ := s.store.Sessions().List(t.Context(), storage.SessionFilter{AgentID: created.ID}); len(rows) != 1 || rows[0].ID != lastTokenID {
		t.Fatalf("session id %v, want the token jti %q", rows, lastTokenID)
	}

	mu.Lock()
	stt, llm, tts := got["stt"], got["llm"], got["tts"]
	mu.Unlock()
	if stt.Model != "chirp_2" || stt.Language != "en-GB" || string(stt.Secret) != "stored-google-key" {
		t.Fatalf("stt spec = %+v", stt)
	}
	if llm.Model != "gemini-test" || llm.SystemPrompt != "You handle billing." || string(llm.Secret) != "stored-google-key" || llm.History == nil {
		t.Fatalf("llm spec = %+v", llm)
	}
	if tts.Voice != "en-GB-Neural2-A" || string(tts.Secret) != "stored-google-key" {
		t.Fatalf("tts spec = %+v", tts)
	}

	mu.Lock()
	for _, b := range received {
		for _, c := range b {
			if c != 0 {
				t.Fatal("decrypted credential was not wiped after the pipeline was built")
			}
		}
	}
	mu.Unlock()

	conn.Close(websocket.StatusNormalClosure, "bye")
	waitFor(t, func() bool { return activeSessions("active") == 0 && activeSessions("ended") == 1 })

	// An agent that is no longer ready cannot take calls, even with a token
	// minted while it was ready.
	w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","agent_id":"`+created.ID+`"}`)
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	disabled := false
	if _, err := s.agents.Update(t.Context(), created.ID, agent.UpdateRequest{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	late, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+resp.Data.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	_ = late.Write(ctx, websocket.MessageBinary, ready)
	for {
		if _, _, err := late.Read(ctx); err != nil {
			break // server refuses the call
		}
	}
	if n := activeSessions(""); n != 1 {
		t.Fatalf("disabled agent started a session: %d sessions", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// userTurnRecorder stands in for the LLM and records the user turns it receives.
type userTurnRecorder struct {
	mu    sync.Mutex
	turns []string
}

func (r *userTurnRecorder) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	if m, ok := f.(*core.UserMessageFrame); ok {
		r.mu.Lock()
		r.turns = append(r.turns, m.Content)
		r.mu.Unlock()
		if err := e.PushDirection(f, d); err != nil {
			return err
		}
		// Reply like an LLM: deltas, then the complete text.
		if err := e.Push(&core.LLMTextDeltaFrame{Delta: "Four.", Turn: m.Turn, Gen: m.Gen}); err != nil {
			return err
		}
		return e.Push(&core.LLMTextFrame{Text: "Four.", Turn: m.Turn, Gen: m.Gen})
	}
	return e.PushDirection(f, d)
}

// A client text turn (RTVI send-text) is echoed back as a final user
// transcription and reaches the agent's LLM as a committed user turn.
func TestTextTurnReachesAgentLLM(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	agentID := readyTestAgent(t, s)
	llm := &userTurnRecorder{}
	s.registry.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })
	s.registry.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return llm, nil })
	s.registry.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })

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
	defer conn.Close(websocket.StatusNormalClosure, "")
	send := func(raw string) {
		frame, _ := ws.EncodeMessageFrame(raw)
		if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if _, _, err := conn.Read(ctx); err != nil { // bot-ready
		t.Fatal(err)
	}
	waitFor(t, func() bool { return activeSessionCount(t, s, agentID) == 1 })

	send(`{"id":"2","label":"rtvi-ai","type":"send-text","data":{"content":"What is two plus two?"}}`)
	echoed := false
	for !echoed {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("no echo: %v", err)
		}
		if f, err := ws.DecodeBinaryFrame(data, 1, time.Now()); err == nil {
			if m, ok := f.(*ws.WireMessageFrame); ok && strings.Contains(m.Data, "user-transcription") && strings.Contains(m.Data, "What is two plus two?") {
				echoed = true
			}
		}
	}
	waitFor(t, func() bool {
		llm.mu.Lock()
		defer llm.mu.Unlock()
		return len(llm.turns) == 1 && llm.turns[0] == "What is two plus two?"
	})
	// The client sees the reply text and an explicit end-of-reply marker.
	for sawText, sawStop := false, false; !sawText || !sawStop; {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("reply events missing (text=%v stop=%v): %v", sawText, sawStop, err)
		}
		if f, err := ws.DecodeBinaryFrame(data, 1, time.Now()); err == nil {
			if m, ok := f.(*ws.WireMessageFrame); ok {
				sawText = sawText || (strings.Contains(m.Data, "bot-llm-text") && strings.Contains(m.Data, "Four."))
				sawStop = sawStop || strings.Contains(m.Data, "bot-llm-stopped")
			}
		}
	}

	// An empty text turn is refused without ending the call.
	send(`{"id":"3","label":"rtvi-ai","type":"send-text","data":{"content":"  "}}`)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("call ended after a rejected text turn: %v", err)
		}
		if f, err := ws.DecodeBinaryFrame(data, 1, time.Now()); err == nil {
			if m, ok := f.(*ws.WireMessageFrame); ok && strings.Contains(m.Data, `"error"`) {
				break
			}
		}
	}
	send(`{"id":"4","label":"rtvi-ai","type":"send-text","data":{"content":"And three plus three?"}}`)
	waitFor(t, func() bool { llm.mu.Lock(); defer llm.mu.Unlock(); return len(llm.turns) == 2 })
}

func activeSessionCount(t *testing.T, s *Server, agentID string) int {
	t.Helper()
	rows, err := s.store.Sessions().List(t.Context(), storage.SessionFilter{AgentID: agentID, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// Shutdown must end the session rows of calls it cancels before it closes the
// store; otherwise they stay "active" forever (blocking MCP deletes and purge).
func TestShutdownEndsActiveSessionRows(t *testing.T) {
	s := apiTestServer(t)
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
	defer conn.CloseNow()
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	_ = conn.Write(ctx, websocket.MessageBinary, ready)
	if _, _, err := conn.Read(ctx); err != nil { // bot-ready
		t.Fatal(err)
	}
	waitFor(t, func() bool { return activeSessionCount(t, s, agentID) == 1 })

	s.cfg.Limits.ImmediateCancel = true // force-cancel the live call
	_ = s.Shutdown(ctx)

	store, err := storage.Open(ctx, storage.Config{Dialect: storage.SQLite, DSN: os.Getenv("GOVOICE_DATABASE_DSN")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.Sessions().List(ctx, storage.SessionFilter{AgentID: agentID})
	if err != nil || len(rows) != 1 || rows[0].Status != "ended" {
		t.Fatalf("session rows after shutdown = %+v (err %v), want one ended", rows, err)
	}
}
