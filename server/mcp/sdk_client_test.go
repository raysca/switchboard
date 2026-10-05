package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSDKClientTalksToSimulatedStreamableHTTPServerWithServerSideBearer(t *testing.T) {
	var authorized atomic.Int32
	remote := mcp.NewServer(&mcp.Implementation{Name: "simulated", Version: "1"}, nil)
	mcp.AddTool[map[string]any, map[string]any](remote, &mcp.Tool{Name: "greet", Description: "greet", InputSchema: json.RawMessage(`{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`)}, func(_ context.Context, _ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		return nil, map[string]any{"greeting": "hello " + input["name"].(string)}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer server-secret-sentinel" {
			authorized.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	base := server.Client().Transport
	client := &SDKClient{client: &http.Client{Transport: boundedBodyTransport{next: bearerTransport{next: base}}}, allowPrivate: true}
	tools, err := client.ListTools(context.Background(), server.URL, "server-secret-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "greet" || tools[0].Description != "greet" {
		t.Fatalf("tools=%#v", tools)
	}
	result, err := client.CallTool(context.Background(), server.URL, "server-secret-sentinel", "greet", json.RawMessage(`{"name":"Ada"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(result.JSON) != `{"greeting":"hello Ada"}` {
		t.Fatalf("result=%s", result.JSON)
	}
	if authorized.Load() < 2 {
		t.Fatalf("authorization not attached to MCP requests: %d", authorized.Load())
	}
}
