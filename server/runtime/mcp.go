// Package runtime binds immutable agent snapshots to public core tool contracts.
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
	"govoice/server/httptool"
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

// toolHandler is a captured tool of either kind (MCP or HTTP) as the executor runs it.
type toolHandler interface {
	ExecuteCall(ctx context.Context, args json.RawMessage, callID, sessionID, turnID string, generation uint64) (json.RawMessage, error)
}

type mcpHandler struct{ tool *mcp.CapturedTool }

func (h mcpHandler) ExecuteCall(ctx context.Context, args json.RawMessage, callID, sessionID, turnID string, generation uint64) (json.RawMessage, error) {
	result, err := h.tool.ExecuteCall(ctx, args, callID, sessionID, turnID, generation)
	return result.JSON, err
}

type captured struct {
	definition core.ToolDefinition
	handler    toolHandler
	snapshot   agent.MCPToolSnapshot
}

// Option adds a tool kind to a session start.
type Option func(*options)

type options struct{ httpTools *httptool.Service }

// WithHTTPTools lets the session capture the agent's HTTP tools; without it an agent that pins one cannot start.
func WithHTTPTools(s *httptool.Service) Option { return func(o *options) { o.httpTools = s } }

type sessionExecutor struct {
	mu       sync.Mutex
	closing  bool
	inFlight int
	drained  chan struct{}
	handlers map[string]toolHandler
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
	return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: append(json.RawMessage(nil), result...), StartedAt: started, EndedAt: ended}, nil
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
func StartSession(ctx context.Context, agents *agent.Service, mcpService *mcp.Service, agentID, sessionID, caller string, opts ...Option) (*Session, error) {
	if agents == nil || mcpService == nil {
		return nil, errors.New("agent and MCP services are required")
	}
	snapshot, err := agents.ResolveCurrent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return StartSnapshotSession(ctx, agents, mcpService, snapshot, sessionID, caller, opts...)
}

// StartSnapshotSession starts a session from an already resolved snapshot: the
// current version, or a draft over it (agent.Service.ResolveDraft).
func StartSnapshotSession(ctx context.Context, agents *agent.Service, mcpService *mcp.Service, snapshot agent.Snapshot, sessionID, caller string, opts ...Option) (*Session, error) {
	var o options
	for _, apply := range opts {
		apply(&o)
	}
	if agents == nil || mcpService == nil {
		return nil, errors.New("agent and MCP services are required")
	}
	items := make([]captured, 0, len(snapshot.MCPTools))
	agentCfg, _ := agent.StartConfig(snapshot.Version.Config) // already validated at publish; unusable config = no async tools
	for _, tool := range snapshot.MCPTools {
		item, err := captureTool(ctx, mcpService, o.httpTools, agentCfg, tool)
		if err != nil {
			if tool.Required {
				return nil, err
			}
			continue
		}
		items = append(items, item)
	}
	definitions := make([]core.ToolDefinition, 0, len(items))
	metadata := make([]agent.MCPToolSnapshot, 0, len(items))
	references := make([]agent.ToolSelection, 0, len(items))
	for _, item := range items {
		definitions = append(definitions, item.definition)
		metadata = append(metadata, item.snapshot)
		references = append(references, agent.ToolSelection{ServerID: item.snapshot.ServerID, RemoteName: item.snapshot.RemoteName, SchemaHash: item.snapshot.SchemaHash, Required: item.snapshot.Required})
	}
	snapshot.MCPTools = metadata
	runtimeJSON, err := json.Marshal(metadata)
	if err != nil {
		return nil, errors.New("runtime MCP snapshot could not be encoded")
	}
	// The core executor closes over captured server-side handles; only definitions
	// and redacted snapshot metadata are persisted or exposed to the framework.
	handlers := make(map[string]toolHandler, len(items))
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

// captureTool pins one selected tool for the call: an MCP tool through the MCP service, an HTTP tool through the HTTP
// tool service. The error names the kind and nothing else, so a required tool's failure is safe to return.
func captureTool(ctx context.Context, mcpService *mcp.Service, httpTools *httptool.Service, cfg agent.Config, tool agent.MCPToolSnapshot) (captured, error) {
	name := runtimeToolName(tool.ServerID, tool.RemoteName)
	if httptool.IsRef(tool.ServerID) {
		id, _ := httptool.IDFromRef(tool.ServerID)
		if httpTools == nil {
			return captured{}, errors.New("required HTTP tool could not be captured")
		}
		c, err := httpTools.Capture(ctx, id, tool.SchemaHash)
		if err != nil || len(c.Schema()) > 32<<10 {
			return captured{}, errors.New("required HTTP tool could not be captured")
		}
		definition := core.ToolDefinition{Name: name, RemoteName: tool.RemoteName, Description: c.Description(), InputSchema: c.Schema(), Policy: core.ToolPolicy{Required: tool.Required}}
		snap := agent.MCPToolSnapshot{ServerID: tool.ServerID, ServerName: c.Name(), Endpoint: c.Endpoint(), Transport: "http", AuthEnv: c.AuthSecret(), RemoteName: tool.RemoteName, Description: c.Description(), SchemaJSON: c.Schema(), SchemaHash: c.Hash(), Required: tool.Required}
		return captured{definition: definition, handler: c, snapshot: snap}, nil
	}
	c, err := mcpService.CaptureTool(ctx, tool.ServerID, tool.RemoteName, tool.SchemaHash)
	if err != nil {
		return captured{}, errors.New("required MCP tool could not be captured")
	}
	if len(c.Schema()) > 32<<10 {
		return captured{}, errors.New("required MCP schema exceeds framework tool limit")
	}
	definition := core.ToolDefinition{Name: name, RemoteName: tool.RemoteName, Description: c.Description(), InputSchema: c.Schema(), Policy: core.ToolPolicy{Required: tool.Required, Async: cfg.IsAsyncTool(tool.ServerID, tool.RemoteName)}}
	snap := agent.MCPToolSnapshot{ServerID: c.ServerID(), ServerName: c.ServerName(), Endpoint: c.Endpoint(), Transport: c.Transport(), AuthEnv: c.AuthEnv(), RemoteName: c.Name(), Description: c.Description(), SchemaJSON: c.Schema(), SchemaHash: c.SchemaHash(), Required: tool.Required}
	return captured{definition: definition, handler: mcpHandler{c}, snapshot: snap}, nil
}

// MCPTool is the captured MCP tool a model-facing tool name stands for; false
// for a built-in tool (a handoff's transfer, end_call, send_dtmf).
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
	if httptool.IsRef(serverID) {
		// Tool names are unique and 1-40 characters of [a-z0-9_]; the prefix keeps them apart from MCP names and built-ins.
		return "http_" + remote
	}
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
