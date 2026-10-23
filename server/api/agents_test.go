package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"govoice/server/agent"
	"govoice/server/storage"
)

func TestAgentManagementCRUDDuplicateArchiveAndV1Import(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "flow-api.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := NewAgentsHandler(agent.NewService(store, allAvailable{}))
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Request-ID", "request-test")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	created := request(http.MethodPost, "/api/v1/agents", `{"name":"Voice","config":{"stt":{"provider":"google","model":"g.stt"},"llm":{"provider":"google","model":"g.llm"},"tts":{"provider":"google","voice":"g.tts"}}}`)
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
	updated := request(http.MethodPut, "/api/v1/agents/"+id, `{"name":"Revised","config":{"stt":{"provider":"google","model":"g.stt"},"llm":{"provider":"google","model":"g.llm"},"tts":{"provider":"google","voice":"g.tts"}}}`)
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

}

func TestAgentManagementRejectsUnknownInputAndMissingAgent(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "flow-api.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := NewAgentsHandler(agent.NewService(store, allAvailable{}))
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

// allAvailable is an environment where every provider service can run.
type allAvailable struct{}

func (allAvailable) Check(string, string) agent.Unavailable { return agent.Unavailable{} }

func TestAgentFallbackChainRoundTrip(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "chain.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := NewAgentsHandler(agent.NewService(store, allAvailable{}))
	body := func(fallbacks string) string {
		return `{"name":"Chain","config":{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"m"` + fallbacks + `},"tts":{"provider":"google","voice":"v"},"failover":{"on":["timeout","server_error"]}}}`
	}
	post := func(b string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(b))
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
		Data agent.Agent `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+env.Data.ID, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model":"backup"`) || !strings.Contains(w.Body.String(), `"server_error"`) {
		t.Fatalf("get status=%d body=%s", w.Code, w.Body)
	}
}

func TestAgentUpdateCarriesMCPToolsAndPublishesOneVersion(t *testing.T) {
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
	service := agent.NewService(store, allAvailable{})
	cfg := `{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`
	created, err := service.Create(context.Background(), agent.CreateRequest{Name: "api", Config: json.RawMessage(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	put := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/agents/"+created.ID, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		NewAgentsHandler(service).ServeHTTP(w, r)
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

// createRig is a flow API over a store with one MCP server offering one tool.
type createRig struct {
	store   *storage.Store
	handler http.Handler
	server  storage.MCPServer
	hash    string
	cfg     string
}

func newCreateRig(t *testing.T) createRig {
	t.Helper()
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "create-mcp.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := storage.MCPServer{ID: storage.NewID(), Name: "orders", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true, ValidationState: "healthy"}
	if err := store.MCP().CreateServer(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	d, err := store.MCP().AddDiscovery(context.Background(), server.ID, []storage.MCPTool{{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return createRig{store: store, handler: NewAgentsHandler(agent.NewService(store, allAvailable{})), server: server, hash: d.Tools[0].SchemaHash,
		cfg: `{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`}
}

func (r createRig) sel(serverID, tool, hash string) string {
	return `{"mcp_server_id":"` + serverID + `","remote_name":"` + tool + `","schema_hash":"` + hash + `","required":true}`
}

func (r createRig) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.handler.ServeHTTP(w, req)
	return w
}

func (r createRig) create(selections string) *httptest.ResponseRecorder {
	return r.do(http.MethodPost, "/api/v1/agents", `{"name":"F","config":`+r.cfg+`,"mcp_tools":[`+selections+`]}`)
}

func wantInvalidSelection(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_tool_selection") {
		t.Fatalf("status=%d body=%s, want 400 invalid_tool_selection", w.Code, w.Body)
	}
}

func TestCreateAgent_DisabledMCPServer_Returns400(t *testing.T) {
	r := newCreateRig(t)
	if err := r.store.MCP().DisableServer(context.Background(), r.server.ID); err != nil {
		t.Fatal(err)
	}
	wantInvalidSelection(t, r.create(r.sel(r.server.ID, "lookup", r.hash)))
}

func TestCreateAgent_SchemaDrift_Returns400(t *testing.T) {
	r := newCreateRig(t)
	wantInvalidSelection(t, r.create(r.sel(r.server.ID, "lookup", "stale")))
}

func TestCreateAgent_TooManySelections_Returns400(t *testing.T) {
	r := newCreateRig(t)
	tools := make([]storage.MCPTool, storage.MaxSelectedTools+1)
	for i := range tools {
		tools[i] = storage.MCPTool{RemoteName: fmt.Sprintf("tool_%03d", i), SchemaJSON: []byte(`{"type":"object"}`)}
	}
	d, err := r.store.MCP().AddDiscovery(context.Background(), r.server.ID, tools)
	if err != nil {
		t.Fatal(err)
	}
	sels := make([]string, len(d.Tools))
	for i, tool := range d.Tools {
		sels[i] = r.sel(r.server.ID, tool.RemoteName, tool.SchemaHash)
	}
	wantInvalidSelection(t, r.create(strings.Join(sels, ",")))
}

func TestCreateAgent_UnknownServer_Returns400_Not404(t *testing.T) {
	r := newCreateRig(t)
	wantInvalidSelection(t, r.create(r.sel(storage.NewID(), "lookup", r.hash)))
}

func TestDuplicateAgent_SchemaDrift_Returns400(t *testing.T) {
	r := newCreateRig(t)
	w := r.create(r.sel(r.server.ID, "lookup", r.hash))
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var env struct {
		Data agent.Agent `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	// The server re-discovers with a changed schema after the flow was saved.
	if _, err := r.store.MCP().AddDiscovery(context.Background(), r.server.ID, []storage.MCPTool{{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`)}}); err != nil {
		t.Fatal(err)
	}
	wantInvalidSelection(t, r.do(http.MethodPost, "/api/v1/agents/"+env.Data.ID+"/duplicate", `{}`))
}

func TestAgentVersionEndpointIsReadOnlyAndNumbered(t *testing.T) {
	store, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "flow-versions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := NewAgentsHandler(agent.NewService(store, allAvailable{}))
	request := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	config := `{"stt":{"provider":"google","model":"g.stt"},"llm":{"provider":"google","model":"g.llm"},"tts":{"provider":"google","voice":"g.tts"}}`
	created := request(http.MethodPost, "/api/v1/agents", `{"name":"Voice","config":`+config+`}`)
	var envelope struct {
		Data agent.Agent `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &envelope); err != nil || envelope.Data.ID == "" {
		t.Fatalf("create response=%s err=%v", created.Body, err)
	}
	base := "/api/v1/agents/" + envelope.Data.ID + "/versions/"
	if got := request(http.MethodGet, base+"1", ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"number":1`) {
		t.Fatalf("version 1 status=%d body=%s", got.Code, got.Body)
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, base + "2", http.StatusNotFound},
		{http.MethodGet, base + "0", http.StatusBadRequest},
		{http.MethodGet, base + "abc", http.StatusBadRequest},
		{http.MethodPost, base + "1", http.StatusMethodNotAllowed},
		{http.MethodDelete, base + "1", http.StatusMethodNotAllowed},
	} {
		if got := request(tc.method, tc.path, ""); got.Code != tc.want {
			t.Errorf("%s %s = %d (%s), want %d", tc.method, tc.path, got.Code, got.Body, tc.want)
		}
	}
}
