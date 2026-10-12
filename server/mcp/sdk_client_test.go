package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	client := &SDKClient{client: &http.Client{Transport: boundedBodyTransport{next: bearerTransport{next: base}}}, testOnlyAllowPrivate: true}
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSDKClientReturnsTypedAuthenticationFailure(t *testing.T) {
	client := &SDKClient{client: &http.Client{Transport: classifiedStatusTransport{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("credential sentinel"))}, nil
	})}}, testOnlyAllowPrivate: true}
	_, err := client.ListTools(context.Background(), "https://mcp.example/rpc", "token")
	if !errors.Is(err, ErrAuthenticationFailed) {
		t.Fatalf("error=%T %v", err, err)
	}
	if strings.Contains(err.Error(), "credential sentinel") {
		t.Fatalf("remote body escaped: %v", err)
	}
}

func TestSDKSessionCleanupDELETEHasSmallBoundedBudget(t *testing.T) {
	remote := mcp.NewServer(&mcp.Implementation{Name: "close-test", Version: "1"}, nil)
	remote.AddTool(&mcp.Tool{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{})
	var deleteSeen atomic.Bool
	var deleteCanceled atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteSeen.Store(true)
			select {
			case <-r.Context().Done():
				deleteCanceled.Store(true)
			case <-time.After(2 * time.Second):
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	client := &SDKClient{client: &http.Client{Transport: closeBudgetTransport{next: boundedBodyTransport{next: bearerTransport{next: server.Client().Transport}}}}, testOnlyAllowPrivate: true}
	started := time.Now()
	_, err := client.ListTools(context.Background(), server.URL, "")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if !deleteSeen.Load() {
		t.Fatal("session Close did not issue DELETE")
	}
	if elapsed > time.Second {
		t.Fatalf("session close exceeded its small lifecycle budget: %v", elapsed)
	}
}

func TestSessionCloseTransportCancelsStalledDELETEWithinBudget(t *testing.T) {
	var canceled atomic.Bool
	transport := closeBudgetTransport{next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("DELETE request has no lifecycle deadline")
		}
		<-r.Context().Done()
		canceled.Store(true)
		return nil, r.Context().Err()
	})}
	request := httptest.NewRequest(http.MethodDelete, "https://mcp.example/rpc", nil)
	started := time.Now()
	_, err := transport.RoundTrip(request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DELETE error=%v", err)
	}
	if !canceled.Load() || time.Since(started) > time.Second {
		t.Fatalf("DELETE cancellation=%v elapsed=%v", canceled.Load(), time.Since(started))
	}
}

type captureBodyTransport struct {
	next   http.RoundTripper
	mu     sync.Mutex
	bodies [][]byte
}

func (t *captureBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		t.mu.Lock()
		t.bodies = append(t.bodies, append([]byte(nil), body...))
		t.mu.Unlock()
	}
	return t.next.RoundTrip(r)
}

func TestSDKClientPreservesIntegersBeyondJavaScriptExactRange(t *testing.T) {
	remote := mcp.NewServer(&mcp.Implementation{Name: "int-test", Version: "1"}, nil)
	mcp.AddTool[map[string]any, any](remote, &mcp.Tool{Name: "number", InputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`)}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	capture := &captureBodyTransport{next: bearerTransport{next: server.Client().Transport}}
	client := &SDKClient{client: &http.Client{Transport: boundedBodyTransport{next: capture}}, testOnlyAllowPrivate: true}
	if _, err := client.CallTool(context.Background(), server.URL, "", "number", json.RawMessage(`{"n":9007199254740993}`)); err != nil {
		t.Fatal(err)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	joined := string(bytes.Join(capture.bodies, []byte("\n")))
	if !strings.Contains(joined, `"n":9007199254740993`) {
		t.Fatalf("outgoing MCP params rounded integer: %s", joined)
	}
	if strings.Contains(joined, `9007199254740992`) {
		t.Fatalf("outgoing MCP params rounded integer: %s", joined)
	}
}

// A call that times out has a done context; the session DELETE must still be sent
// (bounded by its own small budget) so the server can close the session.
func TestSessionCloseDELETEOutlivesTheCallContext(t *testing.T) {
	var deleteCtxErr atomic.Value
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deleteCtxErr.Store(fmt.Sprint(r.Context().Err()))
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "http://127.0.0.1:1/mcp", nil)
	if _, err := (closeBudgetTransport{next: next}).RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if got := deleteCtxErr.Load(); got != "<nil>" {
		t.Fatalf("DELETE sent on a done context: %v", got)
	}
}

// After a timeout the SDK tells the server the call was cancelled, on a context of
// its own (seconds long); against a server that never answers that notification
// must not outlive the small teardown budget.
func TestCancelNotificationHasTheTeardownBudget(t *testing.T) {
	next := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:1/mcp", strings.NewReader(body))
	began := time.Now()
	_, _ = (closeBudgetTransport{next: next}).RoundTrip(req)
	if took := time.Since(began); took > sdkCloseBudget+200*time.Millisecond {
		t.Fatalf("cancel notification took %v, budget %v", took, sdkCloseBudget)
	}
	// An ordinary call is not cut short by the teardown budget.
	call, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:1/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`))
	quick, stop := context.WithTimeout(context.Background(), sdkCloseBudget+300*time.Millisecond)
	defer stop()
	call = call.WithContext(quick)
	began = time.Now()
	_, _ = (closeBudgetTransport{next: next}).RoundTrip(call)
	if took := time.Since(began); took < sdkCloseBudget+200*time.Millisecond {
		t.Fatalf("a tool call was cut to the teardown budget: %v", took)
	}
}
