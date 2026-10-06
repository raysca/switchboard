package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"context"
	"govoice/server/agent"
	"govoice/server/storage"
)

func TestManagementHandlerProtectsMCPAndNeverReturnsSecretEndpointQuery(t *testing.T) {
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	svc, _ := newTestService(t, remote, testCredential{token: "mcp-secret-sentinel"})
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
	svc, _ := newTestService(t, remote, testCredential{})
	discovery, err := svc.Refresh(context.Background(), mustCreateMCP(t, svc))
	if err != nil {
		t.Fatal(err)
	}
	// Resolve the created server from its latest discovery owner.
	servers, err := svc.store.MCP().ListServers(context.Background())
	if err != nil || len(servers) != 1 {
		t.Fatalf("servers=%+v err=%v", servers, err)
	}
	if err := svc.store.Credentials().Put(context.Background(), storage.Credential{ID: "018f0000-0000-7000-8000-000000000001", Provider: "google", MaskedSuffix: "abcd",
		ValidationState: "unvalidated", CapabilitiesJSON: "[]", Ciphertext: []byte("x"), KeyID: "k"}); err != nil {
		t.Fatal(err)
	}
	agentService := agent.NewService(svc.store)
	created, err := agentService.Create(context.Background(), agent.CreateRequest{Name: "uses mcp", Config: json.RawMessage(`{"stt":{"provider":"google","model":"s","credential_id":"018f0000-0000-7000-8000-000000000001"},"llm":{"provider":"google","model":"l","credential_id":"018f0000-0000-7000-8000-000000000001"},"tts":{"provider":"google","voice":"t","credential_id":"018f0000-0000-7000-8000-000000000001"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentService.SetMCPTools(context.Background(), created.ID, []agent.ToolSelection{{ServerID: servers[0].ID, RemoteName: "lookup", SchemaHash: discovery.Tools[0].SchemaHash, Required: true}}); err != nil {
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
	updated, err := agentService.Get(context.Background(), created.ID)
	if err != nil || len(updated.Version.MCPTools) != 0 || updated.Version.Number != 3 {
		t.Fatalf("detached agent=%+v err=%v", updated, err)
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
	svc, _ := newTestService(t, remote, testCredential{})
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
