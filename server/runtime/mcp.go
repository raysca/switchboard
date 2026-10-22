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
	"slices"
	"strings"
	"sync"
	"time"

	"govoice/core"
	"govoice/server/agent"
	"govoice/server/mcp"
	"govoice/server/storage"
)

type Session struct {
	Pin         agent.SessionPin
	Snapshot    agent.Snapshot
	Definitions []core.ToolDefinition
	Executor    core.ToolExecutor
	end         func(ctx context.Context, sessionID, reason string) error
	// EndReason names why the call ended when it is closed (storage.EndReasons); nil or empty means the
	// caller hung up. The server sets it once it knows.
	EndReason func() string
	closeMu   sync.Mutex
	closed    bool
	runner    *sessionExecutor
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
		category, message := mcp.ErrorCategory(err)
		status := core.ToolFailed
		if category == "cancelled" {
			status = core.ToolCancelled
		}
		body, _ := json.Marshal(struct {
			Error   string `json:"error"`
			Message string `json:"message,omitempty"`
		}{category, message})
		return core.ToolResult{CallID: call.ID, Status: status, ErrorCategory: category, Result: body, StartedAt: started, EndedAt: ended}, nil
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
func StartSession(ctx context.Context, agents *agent.Service, mcpService *mcp.Service, agentID, sessionID, caller string) (*Session, error) {
	if agents == nil || mcpService == nil {
		return nil, errors.New("flow and MCP services are required")
	}
	snapshot, err := agents.ResolveCurrent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return StartSnapshotSession(ctx, agents, mcpService, snapshot, sessionID, caller)
}

// StartSnapshotSession starts a session from an already resolved snapshot: the
// current version, or a draft over it (flow.Service.ResolveDraft).
func StartSnapshotSession(ctx context.Context, agents *agent.Service, mcpService *mcp.Service, snapshot agent.Snapshot, sessionID, caller string) (*Session, error) {
	if agents == nil || mcpService == nil {
		return nil, errors.New("flow and MCP services are required")
	}
	items := make([]captured, 0, len(snapshot.MCPTools))
	agentCfg, _ := agent.StartConfig(snapshot.Version.Config) // already validated at publish; unusable config = no async tools
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
		definition := core.ToolDefinition{Name: name, RemoteName: tool.RemoteName, Description: capturedTool.Description(), InputSchema: capturedTool.Schema(), Policy: core.ToolPolicy{Required: tool.Required, Async: agentCfg.IsAsyncTool(tool.ServerID, tool.RemoteName)}}
		items = append(items, captured{definition: definition, handler: capturedTool})
	}
	definitions := make([]core.ToolDefinition, 0, len(items))
	metadata := make([]agent.MCPToolSnapshot, 0, len(items))
	references := make([]agent.ToolSelection, 0, len(items))
	for _, item := range items {
		definitions = append(definitions, item.definition)
		tool := item.handler
		metadata = append(metadata, agent.MCPToolSnapshot{ServerID: tool.ServerID(), ServerName: tool.ServerName(), Endpoint: tool.Endpoint(), Transport: tool.Transport(), AuthEnv: tool.AuthEnv(), RemoteName: tool.Name(), Description: tool.Description(), SchemaJSON: tool.Schema(), SchemaHash: tool.SchemaHash(), Required: item.definition.Policy.Required})
		references = append(references, agent.ToolSelection{ServerID: tool.ServerID(), RemoteName: tool.Name(), SchemaHash: tool.SchemaHash(), Required: item.definition.Policy.Required})
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
	pin, err := agents.StartCapturedSession(ctx, snapshot, sessionID, string(runtimeJSON), caller, references)
	if err != nil {
		return nil, err
	}
	runner.sessionID = pin.ID
	return &Session{Pin: pin, Snapshot: snapshot, Definitions: definitions, Executor: runner, end: agents.EndSession, runner: runner}, nil
}

// MCPTool is the captured MCP tool a model-facing tool name stands for; false
// for a built-in tool (a route's transfer, end_call, send_dtmf).
func (s *Session) MCPTool(name string) (agent.MCPToolSnapshot, bool) {
	for _, t := range s.Snapshot.MCPTools {
		if runtimeToolName(t.ServerID, t.RemoteName) == name {
			return t, true
		}
	}
	return agent.MCPToolSnapshot{}, false
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
				_ = s.end(endCtx, s.Pin.ID, s.reason())
			}()
			return err
		}
	}
	if err := s.end(ctx, s.Pin.ID, s.reason()); err != nil {
		return err
	}
	s.closed = true
	return nil
}

// reason is why the session ended, defaulting to the caller hanging up.
func (s *Session) reason() string {
	if s.EndReason != nil {
		if r := s.EndReason(); slices.Contains(storage.EndReasons, r) {
			return r // anything else would be refused by the store and leave the row active
		}
	}
	return storage.EndCallerHungUp
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
