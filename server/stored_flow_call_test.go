package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/server/catalog"
	"govoice/server/flow"
	"govoice/server/runtime"
	"govoice/server/storage"
	"govoice/transport/ws"
)

// End to end: a stored flow and the provider environment drive a real
// WebSocket call. The call's provider factories must receive exactly the
// flow's settings and the environment's secrets, the session row must be
// persisted while the call runs and ended when it hangs up.
func TestStoredFlowDrivesCall(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	vars := testProviderVars(t)
	vars["GOOGLE_API_KEY"] = "stored-google-key"
	s.env = mapEnv(vars)
	const serviceAccount = `{"type":"service_account"}` // testProviderVars' file, compacted
	enableTestManagement(t, s)
	created, err := s.flows.Create(t.Context(), flow.CreateRequest{Name: "Billing", Config: json.RawMessage(
		`{"stt":{"provider":"google","model":"chirp_2","language":"en-GB"},"llm":{"provider":"google","model":"gemini-test","system_prompt":"You handle billing."},"tts":{"provider":"google","voice":"en-GB-Neural2-A"},"context":{"max_messages":12,"max_tokens":900}}`)})
	if err != nil || created.Readiness.State != flow.ReadinessReady {
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
			return &coretest.PassthroughProcessor{}, nil
		}
	}
	for _, stage := range []string{"stt", "llm", "tts"} {
		s.registry.Register("google."+stage, stub(stage))
	}

	ts := httptest.NewServer(s.wsServer)
	defer ts.Close()
	lastTokenID := ""
	call := func(flowID string) (*websocket.Conn, error) {
		w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","flow_id":"`+flowID+`"}`)
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
		rows, err := s.store.Sessions().List(t.Context(), storage.SessionFilter{FlowID: created.ID, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 3 && activeSessions("active") == 1 })
	// The session is stored under the token's jti, so the caller can find it.
	if rows, _ := s.store.Sessions().List(t.Context(), storage.SessionFilter{FlowID: created.ID}); len(rows) != 1 || rows[0].ID != lastTokenID {
		t.Fatalf("session id %v, want the token jti %q", rows, lastTokenID)
	}

	mu.Lock()
	stt, llm, tts := got["stt"], got["llm"], got["tts"]
	mu.Unlock()
	if stt.Model != "chirp_2" || stt.Language != "en-GB" || string(stt.Secret) != serviceAccount {
		t.Fatalf("stt spec = %+v", stt)
	}
	if llm.Model != "gemini-test" || llm.SystemPrompt != "You handle billing." || string(llm.Secret) != "stored-google-key" || llm.History == nil {
		t.Fatalf("llm spec = %+v", llm)
	}
	if tts.Voice != "en-GB-Neural2-A" || string(tts.Secret) != serviceAccount {
		t.Fatalf("tts spec = %+v", tts)
	}

	mu.Lock()
	for _, b := range received {
		for _, c := range b {
			if c != 0 {
				t.Fatal("provider secret was not wiped after the pipeline was built")
			}
		}
	}
	mu.Unlock()

	conn.Close(websocket.StatusNormalClosure, "bye")
	waitFor(t, func() bool { return activeSessions("active") == 0 && activeSessions("ended") == 1 })

	// A flow that is no longer ready cannot take calls, even with a token
	// minted while it was ready.
	w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","flow_id":"`+created.ID+`"}`)
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	disabled := false
	if _, err := s.flows.Update(t.Context(), created.ID, flow.UpdateRequest{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	dialUntilRefused(t, ts, resp.Data.Token)
	if n := activeSessions(""); n != 1 {
		t.Fatalf("disabled agent started a session: %d sessions", n)
	}
}

// dialUntilRefused dials with token, says client-ready and reads until the
// server ends the connection.
func dialUntilRefused(t *testing.T, ts *httptest.Server, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	late, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	_ = late.Write(ctx, websocket.MessageBinary, ready)
	for {
		if _, _, err := late.Read(ctx); err != nil {
			return // server refuses the call
		}
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
// transcription and reaches the flow's LLM as a committed user turn.
func TestTextTurnReachesFlowLLM(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	flowID := readyTestFlow(t, s)
	llm := &userTurnRecorder{}
	s.registry.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	s.registry.Register("google.llm", func(*Config, StageSpec) (core.Processor, error) { return llm, nil })
	s.registry.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })

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
	waitFor(t, func() bool { return activeSessionCount(t, s, flowID) == 1 })

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

func activeSessionCount(t *testing.T, s *Server, flowID string) int {
	t.Helper()
	rows, err := s.store.Sessions().List(t.Context(), storage.SessionFilter{FlowID: flowID, Status: "active"})
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
	defer conn.CloseNow()
	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	_ = conn.Write(ctx, websocket.MessageBinary, ready)
	if _, _, err := conn.Read(ctx); err != nil { // bot-ready
		t.Fatal(err)
	}
	waitFor(t, func() bool { return activeSessionCount(t, s, flowID) == 1 })

	s.cfg.Limits.ImmediateCancel = true // force-cancel the live call
	_ = s.Shutdown(ctx)

	store, err := storage.Open(ctx, storage.Config{Dialect: storage.SQLite, DSN: os.Getenv("GOVOICE_DATABASE_DSN")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.Sessions().List(ctx, storage.SessionFilter{FlowID: flowID})
	if err != nil || len(rows) != 1 || rows[0].Status != "ended" {
		t.Fatalf("session rows after shutdown = %+v (err %v), want one ended", rows, err)
	}
}

// Each assistant reply becomes one turn row paired with the user message that
// started it; an interrupted reply is marked so.
func TestTurnRecorderPairsUserAndAssistant(t *testing.T) {
	var mu sync.Mutex
	var rows []storage.TurnRecord
	r := newTurnRecorder(func(_ context.Context, row storage.TurnRecord) error {
		mu.Lock()
		rows = append(rows, row)
		mu.Unlock()
		return nil
	}, "sess", slog.New(slog.NewTextHandler(io.Discard, nil)))
	pipe, _ := core.NewPipeline(r)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	at := time.Unix(100, 0)
	_ = session.Post(&core.UserMessageFrame{Turn: "t1", Content: "Hi", Timestamp: at})
	_ = session.Post(&core.AssistantMessageFrame{Turn: "t1", Content: "Hello!", Timestamp: at.Add(time.Second)})
	_ = session.Post(&core.UserMessageFrame{Turn: "t2", Content: "Tell me a story", Timestamp: at.Add(2 * time.Second)})
	_ = session.Post(&core.AssistantMessageFrame{Turn: "t2", Content: "Once", Partial: true, Timestamp: at.Add(3 * time.Second)})
	time.Sleep(50 * time.Millisecond)
	_ = session.Close()
	r.close(t.Context())

	mu.Lock()
	defer mu.Unlock()
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	first, second := rows[0], rows[1]
	if first.SessionID != "sess" || first.Ordinal != 1 || first.Transcript != "Hi" || first.AssistantText != "Hello!" || !first.StartedAt.Equal(at) || first.Interrupted {
		t.Fatalf("first turn = %+v", first)
	}
	if second.Ordinal != 2 || second.Transcript != "Tell me a story" || second.AssistantText != "Once" || !second.Interrupted {
		t.Fatalf("second turn = %+v", second)
	}
}

func TestStoredFlow_ConfiguresMaxParallelAndVADStopMS(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)
	cfgJSON := `{
		"vad_stop_ms": 450,
		"stt": {"provider": "google", "model": "chirp_2"},
		"llm": {"provider": "google", "model": "gemini-test", "max_parallel": 4},
		"tts": {"provider": "google", "voice": "en-GB-Neural2-A"}
	}`

	created, err := s.flows.Create(t.Context(), flow.CreateRequest{Name: "ParallelAgent", Config: json.RawMessage(cfgJSON)})
	if err != nil || created.Readiness.State != flow.ReadinessReady {
		t.Fatalf("agent creation failed: %v", err)
	}

	var mu sync.Mutex
	var gotLLM StageSpec
	s.registry.Register("google.stt", func(_ *Config, _ StageSpec) (core.Processor, error) {
		return &coretest.PassthroughProcessor{}, nil
	})
	s.registry.Register("google.tts", func(_ *Config, _ StageSpec) (core.Processor, error) {
		return &coretest.PassthroughProcessor{}, nil
	})
	s.registry.Register("google.llm", func(_ *Config, spec StageSpec) (core.Processor, error) {
		mu.Lock()
		gotLLM = spec
		mu.Unlock()
		return &coretest.PassthroughProcessor{}, nil
	})

	ts := httptest.NewServer(s.wsServer)
	defer ts.Close()

	w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","flow_id":"`+created.ID+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("token creation: %d %s", w.Code, w.Body)
	}
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"?token="+resp.Data.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	ready, _ := ws.EncodeMessageFrame(`{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`)
	if err := conn.Write(ctx, websocket.MessageBinary, ready); err != nil {
		t.Fatal(err)
	}
	_, _, _ = conn.Read(ctx)

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotLLM.Model == "gemini-test"
	})

	mu.Lock()
	defer mu.Unlock()
	if gotLLM.MaxParallel != 4 {
		t.Fatalf("expected MaxParallel 4, got %d", gotLLM.MaxParallel)
	}
	// Verify VAD processor config for this stop_ms
	vadCfg := s.cfg.VADProcessorConfigForStopMS(450)
	expectedChunks := (450 + 31) / 32
	if vadCfg.StopChunks != expectedChunks {
		t.Fatalf("expected StopChunks %d for 450ms, got %d", expectedChunks, vadCfg.StopChunks)
	}
}

func TestStoredFlow_ConfiguresMaxParallelFallbacksAndServerDefault(t *testing.T) {
	s := apiTestServer(t)
	s.cfg.Tools.MaxParallel = 3
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)

	// Flow config with no flow-level max_parallel and an LLM fallback
	cfgJSON := `{
		"stt": {"provider": "google", "model": "chirp_2"},
		"llm": {"provider": "google", "model": "gemini-primary",
			"fallbacks": [{"provider": "google", "model": "gemini-fallback"}]},
		"tts": {"provider": "google", "voice": "en-GB-Neural2-A"}
	}`

	created, err := s.flows.Create(t.Context(), flow.CreateRequest{Name: "FallbackParallelAgent", Config: json.RawMessage(cfgJSON)})
	if err != nil || created.Readiness.State != flow.ReadinessReady {
		t.Fatalf("agent creation failed: %v", err)
	}

	spec, release, err := s.resolveCall(t.Context(), created.ID, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if spec.LLM.MaxParallel != 3 {
		t.Fatalf("expected primary LLM MaxParallel 3 from server config, got %d", spec.LLM.MaxParallel)
	}
	if len(spec.LLMFallbacks) != 1 {
		t.Fatalf("expected 1 LLM fallback, got %d", len(spec.LLMFallbacks))
	}
	if spec.LLMFallbacks[0].MaxParallel != 3 {
		t.Fatalf("expected fallback LLM MaxParallel 3 from server config, got %d", spec.LLMFallbacks[0].MaxParallel)
	}
}

func TestStoredFlow_CleanTextConfiguration(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	enableTestManagement(t, s)

	// 1. Default (unspecified clean_text): text cleaning enabled -> DisableTextCleaning == false
	cfgDefault := `{
		"stt": {"provider": "google", "model": "chirp_2"},
		"llm": {"provider": "google", "model": "gemini-primary"},
		"tts": {"provider": "google", "voice": "en-GB-Neural2-A"}
	}`
	createdDefault, err := s.flows.Create(t.Context(), flow.CreateRequest{Name: "DefaultAgent", Config: json.RawMessage(cfgDefault)})
	if err != nil {
		t.Fatalf("agent creation failed: %v", err)
	}
	specDefault, releaseDefault, err := s.resolveCall(t.Context(), createdDefault.ID, "session-default")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDefault()
	if specDefault.DisableTextCleaning {
		t.Errorf("expected DisableTextCleaning=false by default, got true")
	}

	// 2. clean_text: false -> DisableTextCleaning == true
	cfgDisabled := `{
		"stt": {"provider": "google", "model": "chirp_2"},
		"llm": {"provider": "google", "model": "gemini-primary"},
		"tts": {"provider": "google", "voice": "en-GB-Neural2-A", "clean_text": false}
	}`
	createdDisabled, err := s.flows.Create(t.Context(), flow.CreateRequest{Name: "DisabledCleanAgent", Config: json.RawMessage(cfgDisabled)})
	if err != nil {
		t.Fatalf("agent creation failed: %v", err)
	}
	specDisabled, releaseDisabled, err := s.resolveCall(t.Context(), createdDisabled.ID, "session-disabled")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDisabled()
	if !specDisabled.DisableTextCleaning {
		t.Errorf("expected DisableTextCleaning=true when clean_text=false, got false")
	}

	// 3. clean_text: true -> DisableTextCleaning == false
	cfgEnabled := `{
		"stt": {"provider": "google", "model": "chirp_2"},
		"llm": {"provider": "google", "model": "gemini-primary"},
		"tts": {"provider": "google", "voice": "en-GB-Neural2-A", "clean_text": true}
	}`
	createdEnabled, err := s.flows.Create(t.Context(), flow.CreateRequest{Name: "ExplicitCleanAgent", Config: json.RawMessage(cfgEnabled)})
	if err != nil {
		t.Fatalf("agent creation failed: %v", err)
	}
	specEnabled, releaseEnabled, err := s.resolveCall(t.Context(), createdEnabled.ID, "session-enabled")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseEnabled()
	if specEnabled.DisableTextCleaning {
		t.Errorf("expected DisableTextCleaning=false when clean_text=true, got true")
	}
}

// liveEnv is a test environment that can change while a call runs.
type liveEnv struct {
	mu sync.Mutex
	m  map[string]string
}

func newLiveEnv(m map[string]string) *liveEnv { return &liveEnv{m: m} }

func (e *liveEnv) get(k string) string { e.mu.Lock(); defer e.mu.Unlock(); return e.m[k] }

// set changes a variable; "" removes it.
func (e *liveEnv) set(k, v string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v == "" {
		delete(e.m, k)
		return
	}
	e.m[k] = v
}

// A stage secret is read from the environment each time it is resolved, and a
// missing one names the variable, never a value.
func TestStageSecretsComeFromTheEnvironmentAtCallTime(t *testing.T) {
	s := apiTestServer(t)
	env := newLiveEnv(map[string]string{"GOOGLE_API_KEY": "k1"})
	s.env = env.get
	cfg, err := flow.DecodeConfig(json.RawMessage(`{"llm":{"provider":"google","model":"m"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resolve := func() (string, error) {
		secrets, err := s.resolveStageSecrets(t.Context(), cfg, "llm")
		return string(secrets["llm"]), err
	}
	if got, err := resolve(); err != nil || got != "k1" {
		t.Fatalf("got %q %v, want k1", got, err)
	}
	env.set("GOOGLE_API_KEY", "k2")
	if got, err := resolve(); err != nil || got != "k2" {
		t.Fatalf("got %q %v, want k2 (live)", got, err)
	}
	env.set("GOOGLE_API_KEY", "")
	_, err = resolve()
	var missing *catalog.MissingError
	if !errors.As(err, &missing) || !strings.Contains(err.Error(), "GOOGLE_API_KEY") || strings.Contains(err.Error(), "k2") {
		t.Fatalf("err = %v, want a MissingError naming GOOGLE_API_KEY", err)
	}
}

// A call whose provider variable disappeared after the token was minted is
// refused before any session row exists.
func TestCallRefusedWhenTheVariableIsGone(t *testing.T) {
	s := apiTestServer(t)
	s.SetVADExecutor(&mockVADExecutor{ready: true, available: 4, total: 4})
	env := newLiveEnv(testProviderVars(t))
	s.env = env.get
	flowID := readyTestFlow(t, s)
	for _, stage := range []string{"stt", "llm", "tts"} {
		s.registry.Register("google."+stage, func(*Config, StageSpec) (core.Processor, error) { return &coretest.PassthroughProcessor{}, nil })
	}
	w := apiRequest(s, http.MethodPost, "/api/v1/voice-tokens", "management-test-key-32-bytes-long-and-separate", `{"principal":"alice","flow_id":"`+flowID+`"}`)
	var resp struct {
		Data struct{ Token string } `json:"data"`
	}
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}
	env.set("GOOGLE_APPLICATION_CREDENTIALS", "")
	a, err := s.flows.Get(t.Context(), flowID)
	if err != nil || !slices.Contains(a.Readiness.Reasons, "missing_credential_stt") || !slices.Contains(a.Readiness.MissingEnv, "GOOGLE_APPLICATION_CREDENTIALS") {
		t.Fatalf("readiness = %+v (err %v), want missing_credential_stt naming GOOGLE_APPLICATION_CREDENTIALS", a.Readiness, err)
	}

	ts := httptest.NewServer(s.wsServer)
	defer ts.Close()
	dialUntilRefused(t, ts, resp.Data.Token)
	rows, err := s.store.Sessions().List(t.Context(), storage.SessionFilter{FlowID: flowID})
	if err != nil || len(rows) != 0 {
		t.Fatalf("refused call left sessions %v (err %v)", rows, err)
	}
}

// Fallbacks resolve their secret by provider; one whose variable is unset is
// skipped and counted, not fatal.
func TestFallbackSecretsResolveByProvider(t *testing.T) {
	s := apiTestServer(t)
	env := newLiveEnv(map[string]string{"GOOGLE_API_KEY": "g", "XAI_API_KEY": "x"})
	s.env = env.get
	cfg, err := flow.DecodeConfig(json.RawMessage(`{"llm":{"provider":"google","model":"m","fallbacks":[{"provider":"xai","model":"grok"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	rs := &runtime.Session{}
	build := func(c flow.Candidate, secret []byte) StageSpec {
		return StageSpec{Provider: c.Provider, Secret: secret}
	}
	got := s.resolveFallbacks(t.Context(), rs, "llm", cfg, &s.metrics.LLMFallbacksSkippedTotal, build)
	if len(got) != 1 || got[0].Provider != "xai" || string(got[0].Secret) != "x" {
		t.Fatalf("fallbacks = %+v", got)
	}
	env.set("XAI_API_KEY", "")
	before := s.metrics.LLMFallbacksSkippedTotal.Load()
	if got := s.resolveFallbacks(t.Context(), rs, "llm", cfg, &s.metrics.LLMFallbacksSkippedTotal, build); len(got) != 0 {
		t.Fatalf("unset fallback kept: %+v", got)
	}
	if n := s.metrics.LLMFallbacksSkippedTotal.Load() - before; n != 1 {
		t.Fatalf("skipped counter moved by %d, want 1", n)
	}
}
