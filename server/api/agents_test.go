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

func TestAgentMCPToolReplacementPublishesVersionedAssociation(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/api-mcp.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server := storage.MCPServer{ID: storage.NewID(), Name: "orders", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true, ValidationState: "healthy"}
	if err := store.MCP().CreateServer(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	discovery, err := store.MCP().AddDiscovery(context.Background(), server.ID, []storage.MCPTool{{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	service := agent.NewService(store)
	created, err := service.Create(context.Background(), agent.CreateRequest{Name: "api", Config: json.RawMessage(`{"stt":"s","llm":"l","tts":"t"}`)})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"tools":[{"mcp_server_id":"` + server.ID + `","remote_name":"lookup","schema_hash":"` + discovery.Tools[0].SchemaHash + `","required":true}]}`
	r := httptest.NewRequest(http.MethodPut, "/api/v1/agents/"+created.ID+"/mcp-tools", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	NewAgentsHandler(service).ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"number":2`) || !strings.Contains(w.Body.String(), discovery.Tools[0].SchemaHash) {
		t.Fatalf("selection response=%d %s", w.Code, w.Body.String())
	}
	current, err := service.Get(context.Background(), created.ID)
	if err != nil || len(current.Version.MCPTools) != 1 || !current.Version.MCPTools[0].Required {
		t.Fatalf("current agent=%+v err=%v", current, err)
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
