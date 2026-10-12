package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"govoice/server/api"
	"govoice/server/storage"
)

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Config{
		Dialect: storage.SQLite,
		DSN:     filepath.Join(t.TempDir(), "voice.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func createTestFlowAndVersion(t *testing.T, s *storage.Store) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	flowID := storage.NewID()
	a := storage.Flow{ID: flowID, Name: "APIAgent", Enabled: true, CreatedAt: time.Now()}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := s.Flows().PublishVersion(ctx, flowID, 1, []byte(`{"llm":"test"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	return flowID, v.ID, v.ConfigHash
}

func setupTracesHandler(t *testing.T) (*storage.Store, http.Handler, string, string) {
	t.Helper()
	s := newTestStore(t)
	flowID, vID, hash := createTestFlowAndVersion(t, s)

	sessID := storage.NewID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := storage.SessionRecord{
		ID:                  sessID,
		FlowID:              flowID,
		FlowVersionID:       vID,
		ConfigHash:          hash,
		Status:              "active",
		StartedAt:           now,
		TurnCount:           0, // AddTurn below counts the turn
		SafeSummary:         "test session",
		ProviderStackJSON:   `{"llm":"test"}`,
		RuntimeSnapshotJSON: `{}`,
	}
	if err := s.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	turnID := storage.NewID()
	turn := storage.TurnRecord{
		ID:            turnID,
		SessionID:     sessID,
		Ordinal:       1,
		StartedAt:     now.Add(time.Second),
		Transcript:    "user query",
		AssistantText: "assistant response",
		LatenciesJSON: `{"ttft":150}`,
		ToolsJSON:     `[]`,
	}
	if err := s.Sessions().AddTurn(context.Background(), turn); err != nil {
		t.Fatal(err)
	}

	spanID := storage.NewID()
	if err := s.Traces().AddSpan(context.Background(), storage.TraceSpan{
		ID:             spanID,
		SessionID:      sessID,
		TurnID:         turnID,
		Name:           "turn.span",
		StartedAt:      now.Add(time.Second),
		AttributesJSON: `{"status":"ok"}`,
	}); err != nil {
		t.Fatal(err)
	}

	eventID := storage.NewID()
	if err := s.Traces().AddEvent(context.Background(), storage.TraceEvent{
		ID:             eventID,
		SpanID:         spanID,
		SessionID:      sessID,
		TurnID:         turnID,
		Name:           "turn.event",
		OccurredAt:     now.Add(1200 * time.Millisecond),
		AttributesJSON: `{"count":1}`,
	}); err != nil {
		t.Fatal(err)
	}

	tcID := storage.NewID()
	if err := s.Traces().AddToolCall(context.Background(), storage.ToolCallRecord{
		ID:               tcID,
		SessionID:        sessID,
		TurnID:           turnID,
		RemoteName:       "get_weather",
		SchemaHash:       "hashweather",
		StartedAt:        now.Add(1100 * time.Millisecond),
		Status:           "completed",
		ArgumentsSummary: `{"city":"london"}`,
		ResultSummary:    `{"temp":20}`,
	}); err != nil {
		t.Fatal(err)
	}

	handler := api.NewTracesHandler(s.Sessions(), s.Traces())
	return s, handler, sessID, turnID
}

func TestListSessions(t *testing.T) {
	_, handler, sessID, _ := setupTracesHandler(t)

	// 1. Success list
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?limit=10&offset=0", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data []api.SessionDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != sessID {
		t.Fatalf("unexpected list data: %+v", resp.Data)
	}

	// 2. Filter by status
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions?status=ended", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var emptyResp struct {
		Data []api.SessionDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &emptyResp); err != nil {
		t.Fatal(err)
	}
	if len(emptyResp.Data) != 0 {
		t.Fatalf("expected 0 ended sessions, got %d", len(emptyResp.Data))
	}

	// 3. Invalid pagination limit
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions?limit=0", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for limit=0, got %d", w.Code)
	}

	// 4. Invalid pagination offset
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions?offset=-1", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for offset=-1, got %d", w.Code)
	}
}

func TestGetSessionDetail(t *testing.T) {
	_, handler, sessID, _ := setupTracesHandler(t)

	// 1. Existing session
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessID, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data api.SessionDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data.ID != sessID || resp.Data.Status != "active" || resp.Data.TurnCount != 1 {
		t.Fatalf("unexpected session detail: %+v", resp.Data)
	}

	// 2. Non-existent session
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+storage.NewID(), nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetSessionTurns(t *testing.T) {
	_, handler, sessID, turnID := setupTracesHandler(t)

	// 1. Existing turns
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessID+"/turns", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data []api.TurnDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != turnID || resp.Data[0].Transcript != "user query" {
		t.Fatalf("unexpected turns data: %+v", resp.Data)
	}

	// 2. Non-existent session
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+storage.NewID()+"/turns", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetSessionTraces(t *testing.T) {
	_, handler, sessID, turnID := setupTracesHandler(t)

	// 1. Traces for session
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessID+"/traces", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data api.SessionTracesDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data.SessionID != sessID {
		t.Fatalf("session id mismatch: %s", resp.Data.SessionID)
	}
	if len(resp.Data.Spans) != 1 || resp.Data.Spans[0].Name != "turn.span" {
		t.Fatalf("unexpected spans: %+v", resp.Data.Spans)
	}
	if len(resp.Data.Events) != 1 || resp.Data.Events[0].Name != "turn.event" {
		t.Fatalf("unexpected events: %+v", resp.Data.Events)
	}
	if len(resp.Data.ToolCalls) != 1 || resp.Data.ToolCalls[0].RemoteName != "get_weather" {
		t.Fatalf("unexpected tool calls: %+v", resp.Data.ToolCalls)
	}

	// 2. Filter by turn_id
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessID+"/traces?turn_id="+turnID, nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// 3. Non-existent session
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+storage.NewID()+"/traces", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestTracesMethodNotAllowedAndNotFound(t *testing.T) {
	_, handler, sessID, _ := setupTracesHandler(t)

	// 1. POST /api/v1/sessions
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	if w.Header().Get("Allow") != "GET" {
		t.Fatalf("expected Allow: GET, got %s", w.Header().Get("Allow"))
	}

	// 2. POST /api/v1/sessions/{id}/turns
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessID+"/turns", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}

	// 3. POST /api/v1/sessions/{id}/traces
	req = httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+sessID+"/traces", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}

	// 4. Unknown subpath /api/v1/sessions/{id}/unknown
	req = httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessID+"/unknown", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
