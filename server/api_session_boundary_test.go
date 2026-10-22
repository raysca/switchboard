package server

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

func TestSessionRoutesUseManagementBoundaryAndServeQueries(t *testing.T) {
	s := apiTestServer(t)
	store, err := storage.Open(context.Background(), storage.Config{
		Dialect: storage.SQLite,
		DSN:     filepath.Join(t.TempDir(), "sessions.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	s.setSessionManagementAPI(api.NewTracesHandler(store.Sessions(), store.Traces()))

	key := "management-test-key-32-bytes-long-and-separate"
	path := "/api/v1/sessions"

	// 1. Unauthenticated request rejected
	unauthorized := apiRequest(s, http.MethodGet, path, "", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized session request status=%d", unauthorized.Code)
	}

	// 2. Wrong management key rejected
	wrong := apiRequest(s, http.MethodGet, path, "bad-management-key", "")
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("bad management key status=%d", wrong.Code)
	}

	// 3. Voice token rejected on management route
	voice := apiRequest(s, http.MethodGet, path, s.cfg.Token.Secret, "")
	if voice.Code != http.StatusUnauthorized {
		t.Fatalf("voice secret authenticated management API: %d", voice.Code)
	}

	// 4. Public listener does not expose management route
	public := httptest.NewRecorder()
	s.publicMux.ServeHTTP(public, httptest.NewRequest(http.MethodGet, path, nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("session management route exposed on public listener: %d", public.Code)
	}

	// 5. Unavailable when handler not installed
	unmountedServer := apiTestServer(t)
	unavailable := apiRequest(unmountedServer, http.MethodGet, path, key, "")
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("unmounted session API status=%d", unavailable.Code)
	}

	// 6. Authorized request succeeds with empty list
	authed := apiRequest(s, http.MethodGet, path, key, "")
	if authed.Code != http.StatusOK {
		t.Fatalf("authorized request failed status=%d: %s", authed.Code, authed.Body.String())
	}
	var listResp struct {
		Data []api.SessionDTO `json:"data"`
	}
	if err := json.Unmarshal(authed.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(listResp.Data) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(listResp.Data))
	}

	// 7. Create a session and verify detail endpoint through server
	agentID := storage.NewID()
	a := storage.Agent{ID: agentID, Name: "A", Enabled: true, CreatedAt: time.Now()}
	if err := store.Agents().Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(context.Background(), store, agentID, []byte(`{"llm":"test"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	sessID := storage.NewID()
	sess := storage.SessionRecord{
		ID:                  sessID,
		AgentID:             agentID,
		AgentVersionID:      v.ID,
		ConfigHash:          v.ConfigHash,
		Status:              "active",
		StartedAt:           time.Now().UTC(),
		ProviderStackJSON:   `{}`,
		RuntimeSnapshotJSON: `{}`,
	}
	if err := store.Sessions().Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}

	detail := apiRequest(s, http.MethodGet, path+"/"+sessID, key, "")
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	var detailResp struct {
		Data api.SessionDTO `json:"data"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &detailResp); err != nil {
		t.Fatal(err)
	}
	if detailResp.Data.ID != sessID {
		t.Fatalf("got session id %s, want %s", detailResp.Data.ID, sessID)
	}

	// 8. Method not allowed returns 405
	postReq := apiRequest(s, http.MethodPost, path, key, "")
	if postReq.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST sessions returned %d, want 405", postReq.Code)
	}
}
