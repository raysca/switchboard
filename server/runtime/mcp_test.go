package runtime

import (
	"context"
	"encoding/json"
	"net/url"
	"sync"
	"testing"
	"time"

	"govoice/core"
	coretools "govoice/core/tools"
	"govoice/server/flow"
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

type runtimeTraceRecorder struct {
	events  chan core.TraceEvent
	parents chan bool
}

func (r *runtimeTraceRecorder) StartSpan(ctx context.Context, event core.TraceEvent) core.Span {
	r.events <- event
	r.parents <- core.SpanFromContext(ctx) != nil
	return runtimeTraceSpan{}
}

type runtimeTraceSpan struct{}

func (runtimeTraceSpan) SetAttribute(string, any) {}
func (runtimeTraceSpan) AddEvent(core.TraceEvent) {}
func (runtimeTraceSpan) End(error)                {}

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
	flows := flow.NewService(store, allAvailable{})
	created, err := flows.Create(ctx, flow.CreateRequest{Name: "support", Config: json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flows.SetMCPTools(ctx, created.ID, []flow.ToolSelection{{ServerID: server.ID, RemoteName: "lookup", SchemaHash: discovery.Tools[0].SchemaHash, Required: true}}); err != nil {
		t.Fatal(err)
	}
	session, err := StartSession(ctx, flows, mcpSvc, created.ID, "")
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
	if definition.RemoteName != session.Snapshot.MCPTools[0].RemoteName || definition.RemoteName == "" {
		t.Fatalf("definition %q must carry the tool's remote name %q", definition.Name, session.Snapshot.MCPTools[0].RemoteName)
	}
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
	pinned, err := flows.ResolveSession(ctx, session.Pin.ID)
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

func TestToolLoopPreservesMCPCallIdentityAndAsyncTraceParent(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/trace.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	remote := &runtimeRemote{schema: json.RawMessage(`{"type":"object"}`)}
	trace := &runtimeTraceRecorder{events: make(chan core.TraceEvent, 8), parents: make(chan bool, 8)}
	async := core.NewAsyncTracer(trace, 32)
	defer async.Close()
	mcpSvc, err := mcp.NewService(mcp.ServiceConfig{Store: store, Remote: remote, Destinations: allowAllDestinations{}, Tracer: async})
	if err != nil {
		t.Fatal(err)
	}
	server, err := mcpSvc.Create(ctx, mcp.ServerConfig{Name: "trace", Endpoint: "https://mcp.example/rpc", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := mcpSvc.Refresh(ctx, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	flows := flow.NewService(store, allAvailable{})
	created, err := flows.Create(ctx, flow.CreateRequest{Name: "trace", Config: json.RawMessage(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flows.SetMCPTools(ctx, created.ID, []flow.ToolSelection{{ServerID: server.ID, RemoteName: "lookup", SchemaHash: discovery.Tools[0].SchemaHash, Required: true}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-trace.events: // Discard discovery span recorded during setup.
	case <-time.After(time.Second):
		t.Fatal("discovery span was not recorded")
	}
	<-trace.parents
	session, err := StartSession(ctx, flows, mcpSvc, created.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	loop, err := coretools.NewToolLoop(session.Definitions, session.Executor, core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer loop.Close()
	loop = loop.WithTraceContext(core.ContextWithTraceContext(ctx, async, core.TracePolicy{}))
	call := core.ToolCall{ID: "mcp-call-42", Name: session.Definitions[0].Name, SessionID: core.SessionID(session.Pin.ID), TurnID: "turn-9", Generation: 7, Arguments: json.RawMessage(`{}`)}
	if _, err := loop.Run(ctx, []core.ToolCall{call}, nil); err != nil {
		t.Fatal(err)
	}
	var got []core.TraceEvent
	var parentFlags []bool
	deadline := time.After(time.Second)
	for len(got) < 2 {
		select {
		case event := <-trace.events:
			got = append(got, event)
		case <-deadline:
			t.Fatalf("trace events=%+v", got)
		}
	}
	for range got {
		parentFlags = append(parentFlags, <-trace.parents)
	}
	var loopEvent, mcpEvent *core.TraceEvent
	for i := range got {
		event := &got[i]
		if event.Name == "tool.execute" {
			loopEvent = event
		}
		if event.Name == "mcp.tool.execute" {
			mcpEvent = event
		}
	}
	if loopEvent == nil || mcpEvent == nil {
		t.Fatalf("unexpected trace events=%+v", got)
	}
	for _, event := range []*core.TraceEvent{loopEvent, mcpEvent} {
		if event.ToolCallID != call.ID || event.SessionID != call.SessionID || event.TurnID != call.TurnID || event.Generation != call.Generation {
			t.Fatalf("lost call correlation: %+v", *event)
		}
	}
	if !parentFlags[1] {
		t.Fatalf("MCP trace did not inherit ToolLoop span parent; parent flags=%v", parentFlags)
	}
}

// allAvailable is an environment where every provider service can run.
type allAvailable struct{}

func (allAvailable) Check(string, string) flow.Unavailable { return flow.Unavailable{} }
