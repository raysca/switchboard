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
