// Package runtime binds immutable flow snapshots to public core tool contracts.
// MCP SDK and server types stay on the Server side of the framework boundary.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"govoice/core"
	"govoice/server/flow"
	"govoice/server/mcp"
)

type Session struct {
	Pin         flow.SessionPin
	Snapshot    flow.Snapshot
	Definitions []core.ToolDefinition
	Executor    core.ToolExecutor
	end         func(context.Context, string) error
	closeMu     sync.Mutex
	closed      bool
	runner      *sessionExecutor
}

type captured struct {
	definition core.ToolDefinition
	handler    *mcp.CapturedTool
}

type sessionExecutor struct {
	mu       sync.Mutex
	closing  bool
	inFlight int
	drained  chan struct{}
	handlers map[string]*mcp.CapturedTool
	async    map[string]bool // async tools: bounded by the loop's AsyncTimeout, not the MCP call timeout
	// sessionID is the stored session's id. Calls carry the core session's
	// numeric id, which history and trace rows cannot reference.
	sessionID string
}

func (e *sessionExecutor) Execute(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
	e.mu.Lock()
	if e.closing {
		e.mu.Unlock()
		now := time.Now()
		return core.ToolResult{CallID: call.ID, Status: core.ToolCancelled, ErrorCategory: "session_closing", Result: json.RawMessage(`{"error":"session_closing"}`), StartedAt: now, EndedAt: now}, nil
	}
	handler, ok := e.handlers[call.Name]
	if ok {
		e.inFlight++
	}
	e.mu.Unlock()
	started := time.Now()
	defer func() {
		if ok {
			e.mu.Lock()
			e.inFlight--
			if e.closing && e.inFlight == 0 {
				close(e.drained)
			}
			e.mu.Unlock()
		}
	}()
	if !ok {
		return core.ToolResult{CallID: call.ID, Status: core.ToolFailed, ErrorCategory: "unknown_tool", Result: json.RawMessage(`{"error":"unknown_tool"}`), StartedAt: started, EndedAt: time.Now()}, nil
	}
	if e.async[call.Name] {
		ctx = mcp.LongCall(ctx)
	}
	result, err := handler.ExecuteCall(ctx, call.Arguments, call.ID, e.sessionID, string(call.TurnID), uint64(call.Generation))
	ended := time.Now()
	if err != nil {
		return core.ToolResult{CallID: call.ID, Status: core.ToolFailed, ErrorCategory: "mcp_tool_failed", Result: json.RawMessage(`{"error":"mcp_tool_failed"}`), StartedAt: started, EndedAt: ended}, nil
	}
	return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: append(json.RawMessage(nil), result.JSON...), StartedAt: started, EndedAt: ended}, nil
}

func (e *sessionExecutor) close(ctx context.Context) error {
	e.mu.Lock()
	if !e.closing {
		e.closing = true
		e.drained = make(chan struct{})
		if e.inFlight == 0 {
			close(e.drained)
		}
	}
	drained := e.drained
	e.mu.Unlock()
	select {
	case <-drained:
		e.mu.Lock()
		e.handlers = nil
		e.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StartSession captures each selected MCP tool before persisting the session.
// Required capture failures reject admission; optional failures produce no
// wrapper. The MCP package holds the credential lease only in memory.
func StartSession(ctx context.Context, flows *flow.Service, mcpService *mcp.Service, flowID, sessionID string) (*Session, error) {
	if flows == nil || mcpService == nil {
		return nil, errors.New("agent and MCP services are required")
	}
	snapshot, err := flows.ResolveCurrent(ctx, flowID)
	if err != nil {
		return nil, err
	}
	items := make([]captured, 0, len(snapshot.MCPTools))
	flowCfg, _ := flow.StartConfig(snapshot.Version.Config) // already validated at publish; unusable config = no async tools
	for _, tool := range snapshot.MCPTools {
		capturedTool, captureErr := mcpService.CaptureTool(ctx, tool.ServerID, tool.RemoteName, tool.SchemaHash)
		if captureErr != nil {
			if tool.Required {
				return nil, errors.New("required MCP tool could not be captured")
			}
			continue
		}
		name := runtimeToolName(tool.ServerID, tool.RemoteName)
		if len(capturedTool.Schema()) > 32<<10 {
			if tool.Required {
				return nil, errors.New("required MCP schema exceeds framework tool limit")
			}
			continue
		}
		definition := core.ToolDefinition{Name: name, Description: capturedTool.Description(), InputSchema: capturedTool.Schema(), Policy: core.ToolPolicy{Required: tool.Required, Async: flowCfg.IsAsyncTool(tool.ServerID, tool.RemoteName)}}
		items = append(items, captured{definition: definition, handler: capturedTool})
	}
	definitions := make([]core.ToolDefinition, 0, len(items))
	metadata := make([]flow.MCPToolSnapshot, 0, len(items))
	references := make([]flow.ToolSelection, 0, len(items))
	for _, item := range items {
		definitions = append(definitions, item.definition)
		tool := item.handler
		metadata = append(metadata, flow.MCPToolSnapshot{ServerID: tool.ServerID(), ServerName: tool.ServerName(), Endpoint: tool.Endpoint(), Transport: tool.Transport(), AuthEnv: tool.AuthEnv(), RemoteName: tool.Name(), Description: tool.Description(), SchemaJSON: tool.Schema(), SchemaHash: tool.SchemaHash(), Required: item.definition.Policy.Required})
		references = append(references, flow.ToolSelection{ServerID: tool.ServerID(), RemoteName: tool.Name(), SchemaHash: tool.SchemaHash(), Required: item.definition.Policy.Required})
	}
	snapshot.MCPTools = metadata
	runtimeJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, errors.New("runtime MCP snapshot could not be encoded")
	}
	// The core executor closes over captured server-side handles; only definitions
	// and redacted snapshot metadata are persisted or exposed to the framework.
	handlers := make(map[string]*mcp.CapturedTool, len(items))
	async := map[string]bool{}
	for _, item := range items {
		handlers[item.definition.Name] = item.handler
		async[item.definition.Name] = item.definition.Policy.Async
	}
	runner := &sessionExecutor{handlers: handlers, async: async}
	pin, err := flows.StartCapturedSession(ctx, snapshot, sessionID, string(runtimeJSON), references)
	if err != nil {
		return nil, err
	}
	runner.sessionID = pin.ID
	return &Session{Pin: pin, Snapshot: snapshot, Definitions: definitions, Executor: runner, end: flows.EndSession, runner: runner}, nil
}

func (s *Session) Close(ctx context.Context) error {
	if s == nil || s.end == nil {
		return nil
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	if s.runner != nil {
		if err := s.runner.close(ctx); err != nil {
			// A tool call is still running, so the session keeps pinning its MCP
			// servers. End the row once the call finishes instead of never (a
			// retried Close may end it first; ending twice is harmless).
			go func() {
				_ = s.runner.close(context.Background())
				endCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = s.end(endCtx, s.Pin.ID)
			}()
			return err
		}
	}
	if err := s.end(ctx, s.Pin.ID); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// ToolName is the model-facing name of a captured MCP tool.
func ToolName(serverID, remote string) string { return runtimeToolName(serverID, remote) }

var invalidToolName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func runtimeToolName(serverID, remote string) string {
	server := strings.ReplaceAll(serverID, "-", "")
	if len(server) > 8 {
		server = server[:8]
	}
	clean := invalidToolName.ReplaceAllString(remote, "_")
	if clean == "" {
		clean = "tool"
	}
	h := sha256.Sum256([]byte(serverID + "\x00" + remote))
	suffix := hex.EncodeToString(h[:4])
	maxPrefix := 64 - len("mcp__") - len(server) - 1 - len(suffix)
	if len(clean) > maxPrefix {
		clean = clean[:maxPrefix]
	}
	return "mcp__" + server + "_" + clean + suffix
}
