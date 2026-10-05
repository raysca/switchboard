package runtime

import (
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/server/agent"
	"govoice/server/mcp"
	"govoice/server/storage"
)

type runtimeRemote struct {
	mu      sync.Mutex
	schema  json.RawMessage
	calls   int
	block   bool
	entered chan struct{}
	release chan struct{}
}

func (r *runtimeRemote) ListTools(context.Context, string, string) ([]mcp.RemoteTool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return []mcp.RemoteTool{{Name: "lookup", Description: "lookup order", InputSchema: append(json.RawMessage(nil), r.schema...)}}, nil
}
func (r *runtimeRemote) CallTool(_ context.Context, _ string, _ string, name string, args json.RawMessage) (mcp.RemoteResult, error) {
	r.mu.Lock()
	r.calls++
	block, entered, release := r.block, r.entered, r.release
	r.mu.Unlock()
	if block {
		close(entered)
		<-release
	}
	if name != "lookup" {
		return mcp.RemoteResult{}, mcp.ErrNotFound
	}
	return mcp.RemoteResult{JSON: json.RawMessage(`{"ok":true}`)}, nil
}
func (r *runtimeRemote) setSchema(schema string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.schema = json.RawMessage(schema)
}

type allowAllDestinations struct{}

func (allowAllDestinations) Validate(context.Context, *url.URL) error { return nil }

func TestSessionUsesCapturedMCPWrapperAfterDisableAndRefresh(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/runtime.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	remote := &runtimeRemote{schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)}
	mcpSvc, err := mcp.NewService(mcp.ServiceConfig{Store: store, Remote: remote, Destinations: allowAllDestinations{}})
	if err != nil {
		t.Fatal(err)
	}
	server, err := mcpSvc.Create(ctx, mcp.ServerConfig{Name: "orders", Endpoint: "https://mcp.example/rpc", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := mcpSvc.Refresh(ctx, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	agents := agent.NewService(store)
	created, err := agents.Create(ctx, agent.CreateRequest{Name: "support", Config: json.RawMessage(`{"stt":"s","llm":"l","tts":"t"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agents.SetMCPTools(ctx, created.ID, []agent.ToolSelection{{ServerID: server.ID, RemoteName: "lookup", SchemaHash: discovery.Tools[0].SchemaHash, Required: true}}); err != nil {
		t.Fatal(err)
	}
	session, err := StartSession(ctx, agents, mcpSvc, created.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Definitions) != 1 || len(session.Snapshot.MCPTools) != 1 {
		t.Fatalf("runtime snapshot=%+v defs=%+v", session.Snapshot, session.Definitions)
	}
	remote.setSchema(`{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`)
	if _, err := mcpSvc.Refresh(ctx, server.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MCP().DisableServer(ctx, server.ID); err != nil {
		t.Fatal(err)
	}
	definition := session.Definitions[0]
	call := core.ToolCall{ID: "call-1", Name: definition.Name, SessionID: core.SessionID(session.Pin.ID), TurnID: core.TurnID("turn-1"), Generation: 1, Arguments: json.RawMessage(`{"id":"old-schema"}`)}
	result, err := session.Executor.Execute(ctx, call)
	if err != nil || result.Status != core.ToolSucceeded || string(result.Result) != `{"ok":true}` {
		t.Fatalf("captured execution=%+v err=%v", result, err)
	}
	if err := store.MCP().DetachAndDelete(ctx, server.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MCP().Server(ctx, server.ID); err != nil {
		t.Fatalf("active captured tool lost server during drain: %v", err)
	}
	remote.mu.Lock()
	remote.block = true
	remote.entered = make(chan struct{})
	remote.release = make(chan struct{})
	entered, release := remote.entered, remote.release
	remote.mu.Unlock()
	secondCall := call
	secondCall.ID = "call-2"
	callDone := make(chan error, 1)
	go func() { _, err := session.Executor.Execute(ctx, secondCall); callDone <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("second captured execution did not start")
	}
	closeCtx, cancelClose := context.WithTimeout(ctx, 10*time.Millisecond)
	if err := session.Close(closeCtx); err == nil {
		t.Fatal("session close did not wait for active tool call")
	}
	cancelClose()
	if _, err := store.MCP().Server(ctx, server.ID); err != nil {
		t.Fatalf("server deleted while tool call was active: %v", err)
	}
	close(release)
	if err := <-callDone; err != nil {
		t.Fatal(err)
	}
	if err := session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MCP().Server(ctx, server.ID); err == nil {
		t.Fatal("pending delete did not complete at session close")
	}
	pinned, err := agents.ResolveSession(ctx, session.Pin.ID)
	if err != nil || len(pinned.MCPTools) != 1 || pinned.MCPTools[0].SchemaHash != discovery.Tools[0].SchemaHash || string(pinned.MCPTools[0].SchemaJSON) != string(definition.InputSchema) {
		t.Fatalf("historical snapshot=%+v err=%v", pinned, err)
	}
	remote.mu.Lock()
	calls := remote.calls
	remote.mu.Unlock()
	if calls != 2 {
		t.Fatalf("MCP calls=%d", calls)
	}
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := session.Close(closeCtx); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
}
