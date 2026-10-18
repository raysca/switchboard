package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"context"
	"govoice/server/flow"
	"govoice/server/storage"
)

func TestManagementHandlerProtectsMCPAndNeverReturnsSecretEndpointQuery(t *testing.T) {
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	svc, _ := newTestService(t, remote, testTokens{"MCP_TOKEN_X": "mcp-secret-sentinel"})
	handler := Handler(svc)
	request := httptest.NewRequest("POST", "/api/v1/mcp-servers", strings.NewReader(`{"name":"orders","endpoint":"https://mcp.example/rpc?token=url-secret-sentinel","transport":"streamable-http","enabled":true}`))
	request.Header.Set("Authorization", "Bearer management-test-secret-which-is-long-enough")
	request.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, request)
	if created.Code != http.StatusBadRequest {
		t.Fatalf("accepted endpoint query credential: create=%d %s", created.Code, created.Body.String())
	}
	if strings.Contains(created.Body.String(), "url-secret-sentinel") {
		t.Fatalf("returned endpoint query secret: %s", created.Body.String())
	}
	request = httptest.NewRequest("POST", "/api/v1/mcp-servers", strings.NewReader(`{"name":"orders","endpoint":"https://mcp.example/rpc","transport":"streamable-http","enabled":true}`))
	request.Header.Set("Authorization", "Bearer management-test-secret-which-is-long-enough")
	request.Header.Set("Content-Type", "application/json")
	created = httptest.NewRecorder()
	handler.ServeHTTP(created, request)
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	var result struct {
		Data struct {
			ID       string `json:"id"`
			Endpoint string `json:"endpoint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRequest("GET", "/api/v1/mcp-servers/"+result.Data.ID, nil)
	get.Header.Set("Authorization", "Bearer management-test-secret-which-is-long-enough")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, get)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "url-secret-sentinel") {
		t.Fatalf("get response=%d %s", response.Code, response.Body.String())
	}
	if got := safeEndpoint("https://mcp.example/rpc?token=url-secret-sentinel"); strings.Contains(got, "url-secret-sentinel") || !strings.Contains(got, "%5Bredacted%5D") {
		t.Fatalf("stored legacy endpoint was not safely masked: %s", got)
	}
	if strings.Contains(response.Body.String(), "mcp-secret-sentinel") {
		t.Fatalf("credential leaked: %s", response.Body.String())
	}
}

func TestMCPDisableDeleteConflictAndConfirmedDetachRoutes(t *testing.T) {
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	svc, _ := newTestService(t, remote, testTokens{})
	discovery, err := svc.Refresh(context.Background(), mustCreateMCP(t, svc))
	if err != nil {
		t.Fatal(err)
	}
	// Resolve the created server from its latest discovery owner.
	servers, err := svc.store.MCP().ListServers(context.Background())
	if err != nil || len(servers) != 1 {
		t.Fatalf("servers=%+v err=%v", servers, err)
	}
	flowService := flow.NewService(svc.store, allAvailable{})
	created, err := flowService.Create(context.Background(), flow.CreateRequest{Name: "uses mcp", Config: json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flowService.SetMCPTools(context.Background(), created.ID, []flow.ToolSelection{{ServerID: servers[0].ID, RemoteName: "lookup", SchemaHash: discovery.Tools[0].SchemaHash, Required: true}}); err != nil {
		t.Fatal(err)
	}
	h := Handler(svc)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer management-test-secret-which-is-long-enough")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	disabled := do("POST", "/api/v1/mcp-servers/"+servers[0].ID+"/disable", "")
	if disabled.Code != 200 || !strings.Contains(disabled.Body.String(), `"enabled":false`) {
		t.Fatalf("disable=%d %s", disabled.Code, disabled.Body.String())
	}
	blocked := do("DELETE", "/api/v1/mcp-servers/"+servers[0].ID, "")
	if blocked.Code != 409 || !strings.Contains(blocked.Body.String(), created.ID) || !strings.Contains(blocked.Body.String(), "lookup") {
		t.Fatalf("delete conflict=%d %s", blocked.Code, blocked.Body.String())
	}
	if _, err := svc.store.MCP().Server(context.Background(), servers[0].ID); err != nil {
		t.Fatalf("blocked delete removed server: %v", err)
	}
	missingConfirmation := do("POST", "/api/v1/mcp-servers/"+servers[0].ID+"/detach-delete", `{"confirm":false}`)
	if missingConfirmation.Code != 400 {
		t.Fatalf("confirmation status=%d body=%s", missingConfirmation.Code, missingConfirmation.Body.String())
	}
	detached := do("POST", "/api/v1/mcp-servers/"+servers[0].ID+"/detach-delete", `{"confirm":true}`)
	if detached.Code != 200 || !strings.Contains(detached.Body.String(), `"deleted":true`) {
		t.Fatalf("detach=%d %s", detached.Code, detached.Body.String())
	}
	updated, err := flowService.Get(context.Background(), created.ID)
	if err != nil || len(updated.Version.MCPTools) != 0 || updated.Version.Number != 3 {
		t.Fatalf("detached flow=%+v err=%v", updated, err)
	}
}

func mustCreateMCP(t *testing.T, svc *Service) string {
	t.Helper()
	server, err := svc.Create(context.Background(), ServerConfig{Name: "orders", Endpoint: "https://mcp.example/rpc", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return server.ID
}

func TestAuthenticatedMCPCreateValidateDetailAndExecuteJSONContract(t *testing.T) {
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup_order", Description: "lookup", InputSchema: json.RawMessage(`{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}`)}}}
	svc, _ := newTestService(t, remote, testTokens{})
	h := Handler(svc)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer management-test-secret-which-is-long-enough")
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	created := do("POST", "/api/v1/mcp-servers", `{"name":"orders","endpoint":"https://mcp.example/rpc","enabled":true}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	var createdDTO struct {
		Data struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Endpoint string `json:"endpoint"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdDTO); err != nil {
		t.Fatal(err)
	}
	if createdDTO.Data.ID == "" || createdDTO.Data.Name != "orders" || createdDTO.Data.Endpoint != "https://mcp.example/rpc" {
		t.Fatalf("non snake_case server DTO: %s", created.Body.String())
	}
	validated := do("POST", "/api/v1/mcp-servers/"+createdDTO.Data.ID+"/validate", "")
	if validated.Code != http.StatusOK {
		t.Fatalf("validate=%d %s", validated.Code, validated.Body.String())
	}
	detail := do("GET", "/api/v1/mcp-servers/"+createdDTO.Data.ID, "")
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"input_schema":{"properties"`) || !strings.Contains(detail.Body.String(), `"remote_name":"lookup_order"`) {
		t.Fatalf("detail schema DTO=%d %s", detail.Code, detail.Body.String())
	}
	executed := do("POST", "/api/v1/mcp-servers/"+createdDTO.Data.ID+"/tools/lookup_order/test", `{"arguments":{"id":"42"}}`)
	if executed.Code != http.StatusOK {
		t.Fatalf("execute=%d %s", executed.Code, executed.Body.String())
	}
	var output map[string]any
	if err := json.Unmarshal(executed.Body.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	data := output["data"].(map[string]any)
	for _, field := range []string{"execution_id", "mcp_server_id", "tool_name", "duration_ms", "result"} {
		if _, ok := data[field]; !ok {
			t.Errorf("missing %s in %s", field, executed.Body.String())
		}
	}
	if _, old := data["Duration"]; old || data["duration"] != nil {
		t.Fatalf("duration contract is ambiguous: %#v", data)
	}
}

// allAvailable is an environment where every provider service can run.
type allAvailable struct{}

func (allAvailable) Check(string, string) flow.Unavailable { return flow.Unavailable{} }

// A deleted detach result carries no server object (omitempty is a no-op on structs).
func TestDetachDeleteResultOmitsServerWhenDeleted(t *testing.T) {
	svc, _ := newTestService(t, &testRemote{}, testTokens{})
	id := mustCreateMCP(t, svc)
	r := httptest.NewRequest("POST", "/api/v1/mcp-servers/"+id+"/detach-delete", strings.NewReader(`{"confirm":true}`))
	r.Header.Set("Authorization", "Bearer management-test-secret-which-is-long-enough")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	Handler(svc).ServeHTTP(w, r)
	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
		t.Fatalf("status=%d err=%v body=%s", w.Code, err, w.Body.String())
	}
	if _, ok := body.Data["server"]; ok {
		t.Fatalf("deleted result has a server key: %s", w.Body.String())
	}
}

// Only a genuinely missing server is 404; a broken store is 500.
func TestMCPRoutesDistinguishNotFoundFromStoreFailure(t *testing.T) {
	svc, store := newTestService(t, &testRemote{}, testTokens{})
	h := Handler(svc)
	do := func(method, path, body string) int {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer management-test-secret-which-is-long-enough")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	routes := [][3]string{{"DELETE", "", ""}, {"GET", "", ""}, {"POST", "/detach-delete", `{"confirm":true}`},
		{"POST", "/disable", ""}, {"POST", "/enable", ""},
		{"POST", "/validate", ""}, {"POST", "/tools/lookup/test", `{"arguments":{}}`}}
	for _, rt := range routes {
		if code := do(rt[0], "/api/v1/mcp-servers/"+storage.NewID()+rt[1], rt[2]); code != 404 {
			t.Errorf("%s%s missing server: %d, want 404", rt[0], rt[1], code)
		}
	}
	id := mustCreateMCP(t, svc)
	_ = store.Close()
	for _, rt := range routes {
		if code := do(rt[0], "/api/v1/mcp-servers/"+id+rt[1], rt[2]); code != 500 {
			t.Errorf("%s%s closed store: %d, want 500", rt[0], rt[1], code)
		}
	}
}

// Enable on a server whose deletion is pending answers 409. (Disable on one
// answers 200: an accepted asymmetry, disabling is harmless.) This passes on
// the pre-4.6e code too (it answered 409 for every enable error); it pins the
// 409 branch against the new lookup mapping.
func TestMCPEnablePendingDeletionIsConflict(t *testing.T) {
	svc, store := newTestService(t, &testRemote{}, testTokens{})
	id := mustCreateMCP(t, svc)
	if _, err := store.DB().Exec("UPDATE mcp_servers SET pending_delete=1 WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/v1/mcp-servers/"+id+"/enable", nil)
	r.Header.Set("Authorization", "Bearer management-test-secret-which-is-long-enough")
	w := httptest.NewRecorder()
	Handler(svc).ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatalf("enable pending deletion: %d %s, want 409", w.Code, w.Body.String())
	}
}
