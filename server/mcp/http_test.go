package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"govoice/server/auth"
)

func TestManagementHandlerProtectsMCPAndNeverReturnsSecretEndpointQuery(t *testing.T) {
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	svc, _ := newTestService(t, remote, testCredential{token: "mcp-secret-sentinel"})
	management, err := auth.NewManagement("management-test-secret-which-is-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler(svc, management)
	unauth := httptest.NewRecorder()
	handler.ServeHTTP(unauth, httptest.NewRequest("GET", "/api/v1/mcp-servers", nil))
	if unauth.Code != http.StatusUnauthorized || strings.Contains(unauth.Body.String(), "mcp-secret-sentinel") {
		t.Fatalf("unauthorized response=%d %s", unauth.Code, unauth.Body.String())
	}
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

func TestManagementHandlerFailsClosedWithoutAuth(t *testing.T) {
	svc, _ := newTestService(t, &testRemote{}, testCredential{})
	for _, management := range []*auth.Management{nil, &auth.Management{}} {
		r := httptest.NewRecorder()
		Handler(svc, management).ServeHTTP(r, httptest.NewRequest("GET", "/api/v1/mcp-servers", nil))
		if r.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
		}
	}
}

func TestAuthenticatedMCPCreateValidateDetailAndExecuteJSONContract(t *testing.T) {
	remote := &testRemote{tools: []RemoteTool{{Name: "lookup_order", Description: "lookup", InputSchema: json.RawMessage(`{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}`)}}}
	svc, _ := newTestService(t, remote, testCredential{})
	management, _ := auth.NewManagement("management-test-secret-which-is-long-enough")
	h := Handler(svc, management)
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
