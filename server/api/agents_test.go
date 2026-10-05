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

func TestAgentManagementCRUDDuplicateArchiveAndV1Import(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "agent-api.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := NewAgentsHandler(agent.NewService(store))
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Request-ID", "request-test")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	created := request(http.MethodPost, "/api/v1/agents", `{"name":"Voice","config":{"stt":"g.stt","llm":"g.llm","tts":"g.tts"}}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body)
	}
	var envelope struct {
		Data agent.Agent `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &envelope); err != nil || envelope.Data.ID == "" || envelope.Data.Version == nil || envelope.Data.Version.Number != 1 {
		t.Fatalf("create response=%s err=%v", created.Body, err)
	}
	id := envelope.Data.ID
	updated := request(http.MethodPut, "/api/v1/agents/"+id, `{"name":"Revised","config":{"tts":"g.tts","stt":"g.stt","llm":"g.llm"}}`)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"number":2`) {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body)
	}
	listed := request(http.MethodGet, "/api/v1/agents?limit=10", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"Revised"`) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body)
	}
	duplicated := request(http.MethodPost, "/api/v1/agents/"+id+"/duplicate", `{"name":"Clone"}`)
	if duplicated.Code != http.StatusCreated || !strings.Contains(duplicated.Body.String(), `"Clone"`) {
		t.Fatalf("duplicate status=%d body=%s", duplicated.Code, duplicated.Body)
	}
	archived := request(http.MethodPost, "/api/v1/agents/"+id+"/archive", "")
	if archived.Code != http.StatusOK {
		t.Fatalf("archive status=%d body=%s", archived.Code, archived.Body)
	}
	imported := request(http.MethodPost, "/api/v1/agents/import-v1", `{"legacy_name":"old-default","name":"Imported","pipeline":{"stt":"g.stt","llm":"x.llm","tts":"g.tts","max_messages":12}}`)
	if imported.Code != http.StatusCreated || !strings.Contains(imported.Body.String(), `"max_messages":12`) {
		t.Fatalf("import status=%d body=%s", imported.Code, imported.Body)
	}
}

func TestAgentManagementRejectsUnknownInputAndMissingAgent(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "agent-api.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := NewAgentsHandler(agent.NewService(store))
	for _, tc := range []struct {
		path string
		body string
		want int
	}{
		{"/api/v1/agents", `{"name":"X","config":{},"secret":"no"}`, http.StatusBadRequest},
		{"/api/v1/agents/not-a-real-id", "", http.StatusNotFound},
	} {
		method := http.MethodPost
		if tc.path == "/api/v1/agents/not-a-real-id" {
			method = http.MethodGet
		}
		r := httptest.NewRequest(method, tc.path, strings.NewReader(tc.body))
		if strings.Contains(tc.body, "{") {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s status=%d want=%d body=%s", tc.path, w.Code, tc.want, w.Body)
		}
	}
}
