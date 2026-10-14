package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"govoice/server/flow"
	"govoice/server/storage"
)

func TestFlowManagementCRUDDuplicateArchiveAndV1Import(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "flow-api.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := NewFlowsHandler(flow.NewService(store, allAvailable{}))
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Request-ID", "request-test")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	created := request(http.MethodPost, "/api/v1/flows", `{"name":"Voice","config":{"stt":{"provider":"google","model":"g.stt"},"llm":{"provider":"google","model":"g.llm"},"tts":{"provider":"google","voice":"g.tts"}}}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body)
	}
	var envelope struct {
		Data flow.Flow `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &envelope); err != nil || envelope.Data.ID == "" || envelope.Data.Version == nil || envelope.Data.Version.Number != 1 {
		t.Fatalf("create response=%s err=%v", created.Body, err)
	}
	id := envelope.Data.ID
	updated := request(http.MethodPut, "/api/v1/flows/"+id, `{"name":"Revised","config":{"stt":{"provider":"google","model":"g.stt"},"llm":{"provider":"google","model":"g.llm"},"tts":{"provider":"google","voice":"g.tts"}}}`)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"number":2`) {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body)
	}
	listed := request(http.MethodGet, "/api/v1/flows?limit=10", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"Revised"`) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body)
	}
	duplicated := request(http.MethodPost, "/api/v1/flows/"+id+"/duplicate", `{"name":"Clone"}`)
	if duplicated.Code != http.StatusCreated || !strings.Contains(duplicated.Body.String(), `"Clone"`) {
		t.Fatalf("duplicate status=%d body=%s", duplicated.Code, duplicated.Body)
	}
	archived := request(http.MethodPost, "/api/v1/flows/"+id+"/archive", "")
	if archived.Code != http.StatusOK {
		t.Fatalf("archive status=%d body=%s", archived.Code, archived.Body)
	}

}

func TestFlowMCPToolReplacementPublishesVersionedAssociation(t *testing.T) {
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
	service := flow.NewService(store, allAvailable{})
	created, err := service.Create(context.Background(), flow.CreateRequest{Name: "api", Config: json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"tools":[{"mcp_server_id":"` + server.ID + `","remote_name":"lookup","schema_hash":"` + discovery.Tools[0].SchemaHash + `","required":true}]}`
	r := httptest.NewRequest(http.MethodPut, "/api/v1/flows/"+created.ID+"/mcp-tools", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	NewFlowsHandler(service).ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"number":2`) || !strings.Contains(w.Body.String(), discovery.Tools[0].SchemaHash) {
		t.Fatalf("selection response=%d %s", w.Code, w.Body.String())
	}
	current, err := service.Get(context.Background(), created.ID)
	if err != nil || len(current.Version.MCPTools) != 1 || !current.Version.MCPTools[0].Required {
		t.Fatalf("current flow=%+v err=%v", current, err)
	}

	// Bad selections are the caller's mistake (400), never "flow not found" or 500.
	for name, sel := range map[string]string{
		"unknown server": `{"mcp_server_id":"` + storage.NewID() + `","remote_name":"lookup","schema_hash":"` + discovery.Tools[0].SchemaHash + `"}`,
		"unknown tool":   `{"mcp_server_id":"` + server.ID + `","remote_name":"nope","schema_hash":"` + discovery.Tools[0].SchemaHash + `"}`,
		"stale schema":   `{"mcp_server_id":"` + server.ID + `","remote_name":"lookup","schema_hash":"stale"}`,
	} {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/flows/"+created.ID+"/mcp-tools", strings.NewReader(`{"tools":[`+sel+`]}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		NewFlowsHandler(service).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_tool_selection") {
			t.Fatalf("%s: status=%d body=%s, want 400 invalid_tool_selection", name, w.Code, w.Body)
		}
	}
}

func TestFlowManagementRejectsUnknownInputAndMissingFlow(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "flow-api.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := NewFlowsHandler(flow.NewService(store, allAvailable{}))
	for _, tc := range []struct {
		path string
		body string
		want int
	}{
		{"/api/v1/flows", `{"name":"X","config":{},"secret":"no"}`, http.StatusBadRequest},
		{"/api/v1/flows/not-a-real-id", "", http.StatusNotFound},
	} {
		method := http.MethodPost
		if tc.path == "/api/v1/flows/not-a-real-id" {
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

// allAvailable is an environment where every provider service can run.
type allAvailable struct{}

func (allAvailable) Check(string, string) flow.Unavailable { return flow.Unavailable{} }

func TestFlowFallbackChainRoundTrip(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "chain.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := NewFlowsHandler(flow.NewService(store, allAvailable{}))
	body := func(fallbacks string) string {
		return `{"name":"Chain","config":{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"m"` + fallbacks + `},"tts":{"provider":"google","voice":"v"},"failover":{"on":["timeout","server_error"]}}}`
	}
	post := func(b string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/flows", strings.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := post(body(`,"fallbacks":[{"provider":"google","model":"m"}]`)); w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate chain status=%d body=%s", w.Code, w.Body)
	}
	created := post(body(`,"fallbacks":[{"provider":"google","model":"backup"}]`))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body)
	}
	var env struct {
		Data flow.Flow `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/flows/"+env.Data.ID, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model":"backup"`) || !strings.Contains(w.Body.String(), `"server_error"`) {
		t.Fatalf("get status=%d body=%s", w.Code, w.Body)
	}
}

func TestFlowUpdateCarriesMCPToolsAndPublishesOneVersion(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/api-upd-mcp.db"})
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
	service := flow.NewService(store, allAvailable{})
	cfg := `{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`
	created, err := service.Create(context.Background(), flow.CreateRequest{Name: "api", Config: json.RawMessage(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/flows/"+created.ID, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		NewFlowsHandler(service).ServeHTTP(w, r)
		return w
	}
	sel := `{"mcp_server_id":"` + server.ID + `","remote_name":"lookup","schema_hash":"` + discovery.Tools[0].SchemaHash + `","required":true}`
	w := put(`{"config":` + cfg + `,"mcp_tools":[` + sel + `]}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"number":2`) || !strings.Contains(w.Body.String(), `"required":true`) {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	// Absent and null both keep the selection.
	for _, body := range []string{`{"config":` + cfg + `}`, `{"config":` + cfg + `,"mcp_tools":null}`} {
		if w := put(body); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), discovery.Tools[0].SchemaHash) {
			t.Fatalf("keep %s: %d %s", body, w.Code, w.Body)
		}
	}
	// A stale hash is the caller's mistake.
	stale := `{"mcp_server_id":"` + server.ID + `","remote_name":"lookup","schema_hash":"stale"}`
	if w := put(`{"config":` + cfg + `,"mcp_tools":[` + stale + `]}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_tool_selection") {
		t.Fatalf("stale: %d %s", w.Code, w.Body)
	}
}
