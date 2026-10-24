package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"govoice/server/agent"
	"govoice/server/storage"
)

func TestAgentVersionsRestoreAndUnarchiveEndpoints(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "agent-api.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := NewAgentsHandler(agent.NewService(store, allAvailable{}))
	do := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Request-ID", "request-test")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	config := func(prompt string) string {
		return `{"stt":{"provider":"google","model":"g.stt"},"llm":{"provider":"google","model":"g.llm","system_prompt":"` + prompt + `"},"tts":{"provider":"google","voice":"g.tts"}}`
	}
	created := do(http.MethodPost, "/api/v1/agents", `{"name":"Voice","config":`+config("one")+`}`)
	var env struct {
		Data agent.Agent `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &env); err != nil || env.Data.ID == "" {
		t.Fatalf("create: %d %s", created.Code, created.Body)
	}
	id := env.Data.ID
	if w := do(http.MethodPut, "/api/v1/agents/"+id, `{"config":`+config("two")+`}`); w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}

	w := do(http.MethodGet, "/api/v1/agents/"+id+"/versions", "")
	var versions struct {
		Data []agent.VersionSummary `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &versions); err != nil || w.Code != 200 || len(versions.Data) != 2 || versions.Data[0].Number != 2 || !versions.Data[0].Current {
		t.Fatalf("versions: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "system_prompt") {
		t.Fatalf("the history list carries config: %s", w.Body)
	}

	if w := do(http.MethodPost, "/api/v1/agents/"+id+"/versions/2/restore", ""); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "already_current") {
		t.Fatalf("restore current: %d %s", w.Code, w.Body)
	}
	if w := do(http.MethodPost, "/api/v1/agents/"+id+"/versions/9/restore", ""); w.Code != http.StatusNotFound {
		t.Fatalf("restore unknown: %d %s", w.Code, w.Body)
	}
	if w := do(http.MethodPost, "/api/v1/agents/"+id+"/versions/x/restore", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("restore bad number: %d %s", w.Code, w.Body)
	}
	if w := do(http.MethodGet, "/api/v1/agents/"+id+"/versions/1/restore", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("restore by GET: %d", w.Code)
	}
	w = do(http.MethodPost, "/api/v1/agents/"+id+"/versions/1/restore", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"number":3`) || !strings.Contains(w.Body.String(), `\"one\"`) && !strings.Contains(w.Body.String(), `"one"`) {
		t.Fatalf("restore v1: %d %s", w.Code, w.Body)
	}

	if w := do(http.MethodPost, "/api/v1/agents/"+id+"/archive", ""); w.Code != 200 {
		t.Fatalf("archive: %d", w.Code)
	}
	if w := do(http.MethodGet, "/api/v1/agents/"+id+"/unarchive", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unarchive by GET: %d", w.Code)
	}
	if w := do(http.MethodPost, "/api/v1/agents/"+id+"/unarchive", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"archived":false`) {
		t.Fatalf("unarchive: %d %s", w.Code, w.Body)
	}
	if w := do(http.MethodPost, "/api/v1/agents/01a16a7b-0000-7000-8000-000000000000/unarchive", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unarchive unknown: %d", w.Code)
	}
}
