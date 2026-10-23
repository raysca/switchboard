package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/storage"
)

func TestAgentRoutesUseManagementBoundaryAndTrustedRequestID(t *testing.T) {
	s := apiTestServer(t)
	store, err := storage.Open(t.Context(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "agents.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s.setAgentManagementAPI(api.NewAgentsHandler(agent.NewService(store, s.availability)))
	key := "management-test-key-32-bytes-long-and-separate"
	path := "/api/v1/agents"

	unauthorized := apiRequest(s, http.MethodGet, path, "", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized flow request status=%d", unauthorized.Code)
	}
	wrong := apiRequest(s, http.MethodGet, path, "bad-management-key", "")
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("bad management key status=%d", wrong.Code)
	}
	voice := apiRequest(s, http.MethodGet, path, s.cfg.Token.Secret, "")
	if voice.Code != http.StatusUnauthorized {
		t.Fatalf("voice secret authenticated management API: %d", voice.Code)
	}
	public := httptest.NewRecorder()
	s.publicMux.ServeHTTP(public, httptest.NewRequest(http.MethodGet, path, nil))
	if public.Code != http.StatusNotFound {
		t.Fatalf("flow management route exposed on public listener: %d", public.Code)
	}

	request := httptest.NewRequest(http.MethodGet, path+"/missing-flow", nil)
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("X-Request-ID", "attacker-controlled-id")
	w := httptest.NewRecorder()
	s.adminMux.ServeHTTP(w, request)
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing flow status=%d body=%s", w.Code, w.Body)
	}
	var response struct {
		Error struct {
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	generated := w.Header().Get("X-Request-ID")
	if generated == "" || generated == "attacker-controlled-id" || response.Error.RequestID != generated {
		t.Fatalf("request id header=%q body=%q", generated, response.Error.RequestID)
	}

	created := apiRequest(s, http.MethodPost, path, key, `{"name":"Mounted","config":{"stt":{"provider":"google","model":"g.stt"},"llm":{"provider":"google","model":"g.llm"},"tts":{"provider":"google","voice":"g.tts"}}}`)
	if created.Code != http.StatusCreated || created.Header().Get("X-Request-ID") == "" || strings.Contains(created.Body.String(), key) {
		t.Fatalf("authenticated route status=%d headers=%v body=%s", created.Code, created.Header(), created.Body)
	}
}
