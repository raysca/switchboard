// Package mcp implements the Server-owned MCP domain and client boundary.
// Framework packages intentionally do not depend on this package.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"govoice/core"
	"govoice/server/storage"
)

const (
	TransportStreamableHTTP   = "streamable-http"
	TransportLocalProcess     = "stdio"
	StateNotValidated         = "not_validated"
	StateHealthy              = "healthy"
	StateAuthenticationFailed = "authentication_failed"
	StateConnectionFailed     = "connection_failed"
	StateDiscoveryFailed      = "discovery_failed"
	StateSchemaInvalid        = "tool_schema_invalid"
	StateDisabled             = "disabled"
	defaultTimeout            = 15 * time.Second
	defaultMaxSchemaBytes     = 64 << 10
	defaultMaxResultBytes     = 256 << 10
	defaultMaxArguments       = 64 << 10
	maxToolsPerDiscovery      = 256
)

var (
	ErrNotFound             = errors.New("MCP resource not found")
	ErrUnavailable          = errors.New("MCP server unavailable")
	ErrInvalidSchema        = errors.New("MCP tool schema invalid")
	ErrInvalidArguments     = errors.New("tool arguments do not match schema")
	ErrRemoteFailure        = errors.New("MCP tool execution failed")
	ErrResultTooLarge       = errors.New("MCP result exceeds size limit")
	ErrLocalDisabled        = errors.New("local MCP process transport is disabled")
	ErrAuthenticationFailed = errors.New("MCP authentication failed")
	ErrInitializeFailed     = errors.New("MCP initialization failed")
	ErrDiscoveryFailed      = errors.New("MCP discovery failed")
	ErrConnectionFailed     = errors.New("MCP connection failed")
	ErrExecutionFailed      = errors.New("MCP remote execution failed")
)

type RemoteTool struct {
	Name, Description string
	InputSchema       json.RawMessage
}
type RemoteResult struct{ JSON json.RawMessage }

type RemoteError struct{ Category error }

func (e *RemoteError) Error() string {
	if e == nil || e.Category == nil {
		return "MCP remote operation failed"
	}
	return e.Category.Error()
}
func (e *RemoteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Category
}
func remoteError(category error) error { return &RemoteError{Category: category} }

// RemoteClient is the narrow SDK adapter boundary. All MCP SDK types and
// protocol-version details stay in this Server-owned package.
type RemoteClient interface {
	ListTools(context.Context, string, string) ([]RemoteTool, error)
	CallTool(context.Context, string, string, string, json.RawMessage) (RemoteResult, error)
}
type DestinationPolicy interface {
	Validate(context.Context, *url.URL) error
}

type ServiceConfig struct {
	Store                                            *storage.Store
	Remote                                           RemoteClient
	Tokens                                           TokenSource
	Destinations                                     DestinationPolicy
	Tracer                                           core.Tracer
	Timeout                                          time.Duration
	MaxSchemaBytes, MaxResultBytes, MaxArgumentBytes int
}
type Service struct {
	store                              *storage.Store
	remote                             RemoteClient
	tokens                             TokenSource
	destinations                       DestinationPolicy
	tracer                             core.Tracer
	timeout                            time.Duration
	maxSchema, maxResult, maxArguments int
}
type ServerConfig struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Endpoint    string `json:"endpoint"`
	Transport   string `json:"transport,omitempty"`
	AuthEnv     string `json:"auth_env,omitempty"`
	Enabled     bool   `json:"enabled"`
}
type ToolResult struct {
	ExecutionID string
	MCPServerID string
	ToolName    string
	JSON        json.RawMessage
	Duration    time.Duration
	Status      string
}

// CapturedTool is an in-memory execution lease for one immutable agent/session
// snapshot. It intentionally has no JSON representation and never exposes the
// resolved bearer token. Server disable and discovery refresh do not change it;
// pending deletion waits for the owning session to drain.
type CapturedTool struct {
	owner                                                                                    *Service
	serverID, serverName, name, description, schemaHash, endpoint, token, transport, authEnv string
	schema                                                                                   json.RawMessage
}

func (t *CapturedTool) String() string {
	if t == nil {
		return "MCP captured tool <nil>"
	}
	return "MCP captured tool " + traceSafeToolName(t.name) + " (server " + t.serverID + ")"
}
func (t *CapturedTool) GoString() string { return t.String() }

func (t *CapturedTool) Name() string {
	if t == nil {
		return ""
	}
	return t.name
}
func (t *CapturedTool) Description() string {
	if t == nil {
		return ""
	}
	return t.description
}
func (t *CapturedTool) Schema() json.RawMessage {
	if t == nil {
		return nil
	}
	return append(json.RawMessage(nil), t.schema...)
}
func (t *CapturedTool) ServerID() string {
	if t == nil {
		return ""
	}
	return t.serverID
}
func (t *CapturedTool) ServerName() string {
	if t == nil {
		return ""
	}
	return t.serverName
}
func (t *CapturedTool) SchemaHash() string {
	if t == nil {
		return ""
	}
	return t.schemaHash
}
func (t *CapturedTool) Endpoint() string {
	if t == nil {
		return ""
	}
	return t.endpoint
}
func (t *CapturedTool) Transport() string {
	if t == nil {
		return ""
	}
	return t.transport
}
func (t *CapturedTool) AuthEnv() string {
	if t == nil {
		return ""
	}
	return t.authEnv
}

func (s *Service) CaptureTool(ctx context.Context, id, name, schemaHash string) (*CapturedTool, error) {
	m, err := s.Get(ctx, id)
	if err != nil {
		return nil, ErrNotFound
	}
	if !m.Enabled || m.ValidationState != StateHealthy || m.LatestDiscoveryID == "" {
		return nil, ErrUnavailable
	}
	d, err := s.store.MCP().LatestDiscovery(ctx, id)
	if err != nil {
		return nil, ErrUnavailable
	}
	var selected storage.MCPTool
	for _, tool := range d.Tools {
		if tool.RemoteName == name {
			selected = tool
			break
		}
	}
	if selected.RemoteName == "" {
		return nil, ErrNotFound
	}
	if schemaHash != "" && selected.SchemaHash != schemaHash {
		return nil, ErrInvalidSchema
	}
	u, err := s.endpoint(ctx, m)
	if err != nil {
		return nil, ErrUnavailable
	}
	token, err := s.token(m)
	if err != nil {
		return nil, ErrUnavailable
	}
	return &CapturedTool{owner: s, serverID: m.ID, serverName: m.Name, name: selected.RemoteName, description: selected.Description, schemaHash: selected.SchemaHash, endpoint: u.String(), token: token, transport: m.Transport, authEnv: m.AuthEnv, schema: append(json.RawMessage(nil), selected.SchemaJSON...)}, nil
}

func (t *CapturedTool) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	if t == nil || t.owner == nil {
		return ToolResult{}, ErrUnavailable
	}
	return t.owner.executeCaptured(ctx, t, args, "", "", "", 0)
}

// ExecuteCall carries provider-neutral conversation identity into MCP tracing.
// Console and other standalone callers can continue to use Execute.
func (t *CapturedTool) ExecuteCall(ctx context.Context, args json.RawMessage, callID, sessionID, turnID string, generation uint64) (ToolResult, error) {
	if t == nil || t.owner == nil {
		return ToolResult{}, ErrUnavailable
	}
	return t.owner.executeCaptured(ctx, t, args, callID, sessionID, turnID, generation)
}

func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Store == nil || cfg.Remote == nil {
		return nil, errors.New("MCP storage and client required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Timeout > 2*time.Minute {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.MaxSchemaBytes <= 0 {
		cfg.MaxSchemaBytes = defaultMaxSchemaBytes
	}
	if cfg.MaxResultBytes <= 0 {
		cfg.MaxResultBytes = defaultMaxResultBytes
	}
	if cfg.MaxArgumentBytes <= 0 {
		cfg.MaxArgumentBytes = defaultMaxArguments
	}
	if cfg.Destinations == nil {
		cfg.Destinations = HTTPSDestinationPolicy{}
	}
	if cfg.Tracer == nil {
		cfg.Tracer = core.NoopTracer{}
	}
	return &Service{store: cfg.Store, remote: cfg.Remote, tokens: cfg.Tokens, destinations: cfg.Destinations, tracer: cfg.Tracer, timeout: cfg.Timeout, maxSchema: cfg.MaxSchemaBytes, maxResult: cfg.MaxResultBytes, maxArguments: cfg.MaxArgumentBytes}, nil
}

// HTTPSDestinationPolicy is the safe default: remote endpoints require HTTPS,
// a host, and no embedded user-info. Deployments can apply stricter egress rules.
type HTTPSDestinationPolicy struct{}

func (HTTPSDestinationPolicy) Validate(ctx context.Context, u *url.URL) error {
	if u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("remote MCP endpoint must be an HTTPS URL without embedded credentials")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if !isPublicEgressIP(ip) {
			return errors.New("MCP destination denied")
		}
	} else if err := validatePublicIPs(ctx, u.Hostname()); err != nil {
		return errors.New("MCP destination denied")
	}
	return nil
}

func (s *Service) Create(ctx context.Context, cfg ServerConfig) (storage.MCPServer, error) {
	if cfg.Transport == "" {
		cfg.Transport = TransportStreamableHTTP
	}
	if cfg.Transport == TransportLocalProcess {
		return storage.MCPServer{}, ErrLocalDisabled
	}
	if cfg.Transport != TransportStreamableHTTP {
		return storage.MCPServer{}, errors.New("unsupported MCP transport")
	}
	u, err := url.ParseRequestURI(cfg.Endpoint)
	if err != nil {
		return storage.MCPServer{}, errors.New("invalid MCP endpoint")
	}
	if u.RawQuery != "" {
		return storage.MCPServer{}, errors.New("MCP endpoint query parameters are not accepted; use auth_env")
	}
	if err = s.destinations.Validate(ctx, u); err != nil {
		return storage.MCPServer{}, errors.New("MCP destination denied")
	}
	if strings.TrimSpace(cfg.Name) == "" || len(cfg.Name) > 128 || len(cfg.Description) > 1024 {
		return storage.MCPServer{}, errors.New("invalid MCP server metadata")
	}
	if cfg.AuthEnv != "" && !ValidAuthEnv(cfg.AuthEnv) {
		return storage.MCPServer{}, errors.New("invalid MCP auth_env")
	}
	m := storage.MCPServer{ID: storage.NewID(), Name: strings.TrimSpace(cfg.Name), Description: cfg.Description, Endpoint: u.String(), Transport: cfg.Transport, AuthEnv: cfg.AuthEnv, Enabled: cfg.Enabled, ValidationState: StateNotValidated}
	if !m.Enabled {
		m.ValidationState = StateDisabled
	}
	if err = s.store.MCP().CreateServer(ctx, m); err != nil {
		return storage.MCPServer{}, errors.New("MCP server could not be saved")
	}
	return m, nil
}

func (s *Service) Get(ctx context.Context, id string) (storage.MCPServer, error) {
	m, err := s.store.MCP().Server(ctx, id)
	if err != nil {
		return m, ErrNotFound
	}
	return m, nil
}

func (s *Service) Disable(ctx context.Context, id string) error {
	return s.store.MCP().DisableServer(ctx, id)
}
func (s *Service) Enable(ctx context.Context, id string) error {
	return s.store.MCP().EnableServer(ctx, id)
}
func (s *Service) Delete(ctx context.Context, id string) error {
	return s.store.MCP().DeleteServer(ctx, id)
}
func (s *Service) DetachAndDelete(ctx context.Context, id string) error {
	return s.store.MCP().DetachAndDelete(ctx, id)
}

func (s *Service) token(m storage.MCPServer) (string, error) {
	if m.AuthEnv == "" {
		return "", nil
	}
	if s.tokens != nil {
		if token, ok := s.tokens.Token(m.AuthEnv); ok {
			return token, nil
		}
	}
	return "", errors.New("MCP authentication unavailable")
}
func (s *Service) endpoint(ctx context.Context, m storage.MCPServer) (*url.URL, error) {
	if m.Transport != TransportStreamableHTTP {
		return nil, ErrLocalDisabled
	}
	u, err := url.ParseRequestURI(m.Endpoint)
	if err != nil {
		return nil, errors.New("invalid MCP endpoint")
	}
	if err = s.destinations.Validate(ctx, u); err != nil {
		return nil, errors.New("MCP destination denied")
	}
	return u, nil
}
func (s *Service) timed(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.timeout)
}

type longCallKey struct{}

// LongCall marks a tool call that runs in the background (an async tool): it
// is bounded by the caller's deadline (the tool loop's AsyncTimeout) instead of
// the service's short per-call timeout.
func LongCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, longCallKey{}, true)
}
func (s *Service) span(ctx context.Context, name string) core.Span {
	return core.StartSpanSafe(ctx, s.tracer, core.NewTraceEvent(name, time.Now(), "", "", 0, core.TracePolicy{Content: core.TraceMetadata}, nil))
}

func (s *Service) Refresh(ctx context.Context, id string) (storage.MCPDiscovery, error) {
	ctx, cancel := s.timed(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return storage.MCPDiscovery{}, err
	}
	started := time.Now()
	span := s.span(ctx, "mcp.discovery")
	ended := false
	end := func(err error) {
		if !ended {
			ended = true
			span.SetAttribute("mcp.duration_ms", time.Since(started).Milliseconds())
			span.End(err)
		}
	}
	defer end(nil)
	m, err := s.Get(ctx, id)
	if err != nil {
		end(ErrNotFound)
		return storage.MCPDiscovery{}, ErrNotFound
	}
	if !m.Enabled {
		_ = s.store.MCP().SetValidation(ctx, m.ID, StateDisabled, time.Time{}, time.Time{})
		end(ErrUnavailable)
		return storage.MCPDiscovery{}, ErrUnavailable
	}
	u, err := s.endpoint(ctx, m)
	if err != nil {
		_ = s.store.MCP().SetValidation(ctx, m.ID, StateConnectionFailed, time.Time{}, time.Time{})
		end(err)
		return storage.MCPDiscovery{}, ErrUnavailable
	}
	token, err := s.token(m)
	if err != nil {
		_ = s.store.MCP().SetValidation(ctx, m.ID, StateAuthenticationFailed, time.Time{}, time.Time{})
		end(err)
		return storage.MCPDiscovery{}, ErrUnavailable
	}
	tools, err := s.remote.ListTools(ctx, u.String(), token)
	if err != nil {
		state := validationStateForError(err, true)
		_ = s.store.MCP().SetValidation(context.WithoutCancel(ctx), m.ID, state, time.Time{}, time.Time{})
		end(safeCategory(err))
		return storage.MCPDiscovery{}, ErrUnavailable
	}
	if len(tools) > maxToolsPerDiscovery {
		_ = s.store.MCP().SetValidation(context.WithoutCancel(ctx), m.ID, StateDiscoveryFailed, time.Now(), time.Time{})
		end(ErrInvalidSchema)
		return storage.MCPDiscovery{}, ErrInvalidSchema
	}
	items := make([]storage.MCPTool, 0, len(tools))
	for _, tool := range tools {
		if len(tool.InputSchema) == 0 || len(tool.InputSchema) > s.maxSchema || strings.TrimSpace(tool.Name) == "" || len(tool.Name) > 256 || len(tool.Description) > 4096 {
			_ = s.store.MCP().SetValidation(context.WithoutCancel(ctx), m.ID, StateSchemaInvalid, time.Now(), time.Time{})
			end(ErrInvalidSchema)
			return storage.MCPDiscovery{}, ErrInvalidSchema
		}
		if _, err := validateSchemaContext(ctx, tool.InputSchema, s.maxSchema); err != nil {
			_ = s.store.MCP().SetValidation(context.WithoutCancel(ctx), m.ID, StateSchemaInvalid, time.Now(), time.Time{})
			end(ErrInvalidSchema)
			return storage.MCPDiscovery{}, ErrInvalidSchema
		}
		items = append(items, storage.MCPTool{RemoteName: tool.Name, Description: tool.Description, SchemaJSON: tool.InputSchema})
	}
	if ctx.Err() != nil {
		_ = s.store.MCP().SetValidation(context.WithoutCancel(ctx), m.ID, StateConnectionFailed, time.Now(), time.Time{})
		end(ctx.Err())
		return storage.MCPDiscovery{}, ctx.Err()
	}
	d, err := s.store.MCP().AddDiscovery(ctx, m.ID, items)
	if err != nil {
		_ = s.store.MCP().SetValidation(context.WithoutCancel(ctx), m.ID, StateDiscoveryFailed, time.Now(), time.Time{})
		end(ErrInvalidSchema)
		return storage.MCPDiscovery{}, ErrInvalidSchema
	}
	_ = s.store.MCP().SetValidation(ctx, m.ID, StateHealthy, time.Now(), time.Now())
	span.SetAttribute("mcp.tool_count", len(items))
	return d, nil
}

func (s *Service) Validate(ctx context.Context, id string) (storage.MCPServer, error) {
	if _, err := s.Refresh(ctx, id); err != nil {
		m, _ := s.Get(context.WithoutCancel(ctx), id)
		return m, err
	}
	return s.Get(ctx, id)
}

func (s *Service) Execute(ctx context.Context, id, name string, args json.RawMessage) (ToolResult, error) {
	m, err := s.Get(ctx, id)
	if err != nil {
		return ToolResult{}, ErrNotFound
	}
	d, err := s.store.MCP().LatestDiscovery(ctx, id)
	if err != nil {
		return ToolResult{}, ErrNotFound
	}
	var hash string
	for _, tool := range d.Tools {
		if tool.RemoteName == name {
			hash = tool.SchemaHash
			break
		}
	}
	if hash == "" {
		started := time.Now()
		event := core.NewTraceEvent("mcp.tool.execute", started, "", "", 0, core.TracePolicy{Content: core.TraceMetadata}, nil)
		event.ToolCallID = storage.NewID()
		span := core.StartSpanSafe(ctx, s.tracer, event)
		span.SetAttribute("mcp.server_id", m.ID)
		span.SetAttribute("mcp.tool_name", traceSafeToolName(name))
		span.SetAttribute("mcp.duration_ms", time.Since(started).Milliseconds())
		span.End(ErrNotFound)
		return ToolResult{}, ErrNotFound
	}
	captured, err := s.CaptureTool(ctx, m.ID, name, hash)
	if err != nil {
		return ToolResult{}, err
	}
	return captured.Execute(ctx, args)
}

func (s *Service) executeCaptured(ctx context.Context, captured *CapturedTool, args json.RawMessage, callID, sessionID, turnID string, generation uint64) (ToolResult, error) {
	cancel := context.CancelFunc(func() {})
	if _, bounded := ctx.Deadline(); ctx.Value(longCallKey{}) == nil || !bounded {
		ctx, cancel = s.timed(ctx)
	}
	defer cancel()
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	started := time.Now()
	executionID := callID
	if executionID == "" {
		executionID = storage.NewID()
	}
	event := core.NewTraceEvent("mcp.tool.execute", time.Now(), core.SessionID(sessionID), core.TurnID(turnID), core.Generation(generation), core.TracePolicy{Content: core.TraceMetadata}, nil)
	event.ToolCallID = executionID
	trace := core.TraceContextFromContext(ctx)
	tracer := s.tracer
	if _, noTraceContext := trace.Tracer.(core.NoopTracer); !noTraceContext {
		tracer = trace.Tracer
	}
	span := core.StartSpanSafe(ctx, tracer, event)
	ended := false
	end := func(err error) {
		if !ended {
			ended = true
			span.SetAttribute("mcp.duration_ms", time.Since(started).Milliseconds())
			span.End(err)
		}
	}
	defer end(nil)
	span.SetAttribute("mcp.server_id", captured.serverID)
	span.SetAttribute("mcp.tool_name", traceSafeToolName(captured.name))
	if len(args) > s.maxArguments {
		end(ErrInvalidArguments)
		return ToolResult{}, ErrInvalidArguments
	}
	instance, err := decodeJSON(args)
	if err != nil {
		end(ErrInvalidArguments)
		return ToolResult{}, ErrInvalidArguments
	}
	resolved, err := validateSchemaContext(ctx, captured.schema, s.maxSchema)
	if err != nil || resolved.Validate(instance) != nil {
		end(ErrInvalidArguments)
		return ToolResult{}, ErrInvalidArguments
	}
	result, err := s.remote.CallTool(ctx, captured.endpoint, captured.token, captured.name, args)
	if err != nil {
		end(safeCategory(err))
		return ToolResult{}, ErrRemoteFailure
	}
	if len(result.JSON) > s.maxResult {
		end(ErrResultTooLarge)
		return ToolResult{}, ErrResultTooLarge
	}
	if !json.Valid(result.JSON) {
		end(ErrRemoteFailure)
		return ToolResult{}, ErrRemoteFailure
	}
	clean, sanitizeErr := sanitizeResult(result.JSON, captured.token, s.maxResult)
	if sanitizeErr != nil {
		end(ErrResultTooLarge)
		return ToolResult{}, ErrResultTooLarge
	}
	end(nil)
	return ToolResult{ExecutionID: executionID, MCPServerID: captured.serverID, ToolName: captured.name, JSON: clean, Duration: time.Since(started), Status: "success"}, nil
}

func safeCategory(err error) error {
	switch {
	case errors.Is(err, ErrAuthenticationFailed):
		return ErrAuthenticationFailed
	case errors.Is(err, ErrInitializeFailed):
		return ErrInitializeFailed
	case errors.Is(err, ErrDiscoveryFailed):
		return ErrDiscoveryFailed
	case errors.Is(err, ErrExecutionFailed):
		return ErrExecutionFailed
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, ErrInvalidSchema):
		return ErrInvalidSchema
	default:
		return ErrUnavailable
	}
}
func validationStateForError(err error, duringList bool) string {
	if errors.Is(err, ErrAuthenticationFailed) {
		return StateAuthenticationFailed
	}
	if errors.Is(err, ErrConnectionFailed) || errors.Is(err, ErrInitializeFailed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return StateConnectionFailed
	}
	if duringList || errors.Is(err, ErrDiscoveryFailed) {
		return StateDiscoveryFailed
	}
	return StateConnectionFailed
}
func traceSafeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 96 {
			break
		}
	}
	return b.String()
}

const (
	maxSchemaDepth         = 64
	maxSchemaNodes         = 4096
	maxSchemaRefs          = 128
	maxExpandedSchemaNodes = 16384
	maxSanitizeDepth       = 64
	maxSanitizeNodes       = 16384
)

type refEdge struct{ from, to string }

func validateSchemaContext(ctx context.Context, raw []byte, maxBytes int) (resolved *jsonschema.Resolved, err error) {
	defer func() {
		if recover() != nil {
			resolved = nil
			err = ErrInvalidSchema
		}
	}()
	if ctx == nil || ctx.Err() != nil {
		if ctx == nil {
			return nil, context.Canceled
		}
		return nil, ctx.Err()
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxSchemaBytes
	}
	if len(raw) == 0 || len(raw) > maxBytes {
		return nil, ErrInvalidSchema
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var root any
	if err = dec.Decode(&root); err != nil {
		return nil, ErrInvalidSchema
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, ErrInvalidSchema
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := preflightSchemaJSON(root); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// Decode exactly the tree preflight inspected. Decoding the original bytes
	// directly into Schema would merge duplicate object fields that the JSON
	// map decoder replaced, potentially retaining an unchecked reference.
	normalized, err := json.Marshal(root)
	if err != nil {
		return nil, ErrInvalidSchema
	}
	var schema jsonschema.Schema
	if err = json.Unmarshal(normalized, &schema); err != nil {
		return nil, ErrInvalidSchema
	}
	if schema.Type != "object" && len(schema.Types) == 0 {
		return nil, ErrInvalidSchema
	}
	if schema.Type != "object" && len(schema.Types) > 0 {
		found := false
		for _, typ := range schema.Types {
			if typ == "object" {
				found = true
			}
		}
		if !found {
			return nil, ErrInvalidSchema
		}
	}
	// Schema's struct decoder also accepts case-insensitive keyword aliases.
	// Recheck its canonical representation before Resolve so merged aliases
	// cannot hide cycles or expansion behind different JSON Pointer paths.
	canonical, err := json.Marshal(&schema)
	if err != nil {
		return nil, ErrInvalidSchema
	}
	canonicalRoot, err := decodeJSON(canonical)
	if err != nil {
		return nil, ErrInvalidSchema
	}
	if err := preflightSchemaJSON(canonicalRoot); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	resolved, err = schema.Resolve(nil)
	if err != nil {
		return nil, ErrInvalidSchema
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return resolved, nil
}

func preflightSchemaJSON(root any) error {
	rootMap, ok := root.(map[string]any)
	if !ok {
		return ErrInvalidSchema
	}
	if dialect, exists := rootMap["$schema"]; exists {
		value, ok := dialect.(string)
		if !ok || !supportedDialect(value) {
			return ErrInvalidSchema
		}
	}
	graphs := make(map[string][]string)
	nodes := make(map[string]any)
	refs := make([]refEdge, 0)
	budget := maxSchemaNodes
	if err := indexSchemaJSON(root, "", 0, &budget, nodes, graphs, &refs); err != nil {
		return err
	}
	if len(refs) > maxSchemaRefs {
		return ErrInvalidSchema
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref.to, "#") {
			return ErrInvalidSchema
		}
		target, err := resolveLocalRef(root, ref.to)
		if err != nil {
			return ErrInvalidSchema
		}
		switch target.(type) {
		case map[string]any, bool:
			// Boolean schemas are leaves; the validator also canonicalizes
			// empty schema objects to true and {"not": {}} to false.
		default:
			return ErrInvalidSchema
		}
		path, err := referencePath(ref.to)
		if err != nil {
			return ErrInvalidSchema
		}
		if _, ok := nodes[path]; !ok {
			return ErrInvalidSchema
		}
		graphs[ref.from] = append(graphs[ref.from], path)
	}
	if !schemaGraphWithinBounds(graphs) {
		return ErrInvalidSchema
	}
	return nil
}

func supportedDialect(dialect string) bool {
	// Match jsonschema-go v0.4.3's validation dialects exactly. Resolve alone
	// also accepts versions that Validate subsequently rejects.
	return dialect == "https://json-schema.org/draft/2020-12/schema" || dialect == "http://json-schema.org/draft-07/schema#" || dialect == "https://json-schema.org/draft-07/schema#"
}

func indexSchemaJSON(value any, path string, depth int, budget *int, nodes map[string]any, graph map[string][]string, refs *[]refEdge) error {
	if depth > maxSchemaDepth {
		return ErrInvalidSchema
	}
	*budget--
	if *budget < 0 {
		return ErrInvalidSchema
	}
	nodes[path] = value
	switch item := value.(type) {
	case map[string]any:
		if raw, ok := item["$ref"]; ok {
			target, ok := raw.(string)
			if !ok {
				return ErrInvalidSchema
			}
			*refs = append(*refs, refEdge{from: path, to: target})
		}
		for key, child := range item {
			// Only root-local JSON Pointer references are supported. Anchors,
			// resource IDs, dynamic/recursive refs and alternate dollar-key
			// spellings must never change the graph the validator will follow.
			if strings.HasPrefix(key, "$") && key != "$ref" && key != "$schema" && key != "$defs" && key != "$comment" {
				return ErrInvalidSchema
			}
			childPath := path + "/" + pointerEscape(key)
			if err := indexSchemaJSON(child, childPath, depth+1, budget, nodes, graph, refs); err != nil {
				return err
			}
			graph[path] = append(graph[path], childPath)
		}
	case []any:
		for i, child := range item {
			childPath := path + "/" + strconv.Itoa(i)
			if err := indexSchemaJSON(child, childPath, depth+1, budget, nodes, graph, refs); err != nil {
				return err
			}
			graph[path] = append(graph[path], childPath)
		}
	}
	return nil
}

func pointerEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
func referencePath(ref string) (string, error) {
	if ref == "#" {
		return "", nil
	}
	if !strings.HasPrefix(ref, "#/") {
		return "", ErrInvalidSchema
	}
	decoded, err := url.PathUnescape(ref[1:])
	if err != nil {
		return "", err
	}
	return decoded, nil
}
func resolveLocalRef(root any, ref string) (any, error) {
	path, err := referencePath(ref)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return root, nil
	}
	var current any = root
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				return nil, ErrInvalidSchema
			}
			current = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(node) {
				return nil, ErrInvalidSchema
			}
			current = node[i]
		default:
			return nil, ErrInvalidSchema
		}
	}
	return current, nil
}

// schemaGraphWithinBounds charges every reference occurrence and combinator
// branch, including repeated visits to shared definitions. Unique-node limits
// and cycle detection alone do not bound the expansion of an acyclic DAG.
// Memoized costs make this check linear in the stored graph, and subtraction
// before addition caps the cost without integer overflow.
func schemaGraphWithinBounds(graph map[string][]string) bool {
	state := make(map[string]uint8, len(graph))
	costs := make(map[string]int, len(graph))
	depths := make(map[string]int, len(graph))
	var visit func(string) bool
	visit = func(node string) bool {
		if state[node] == 1 {
			return false
		}
		if state[node] == 2 {
			return true
		}
		state[node] = 1
		cost, depth := 1, 1
		for _, next := range graph[node] {
			if !visit(next) || costs[next] > maxExpandedSchemaNodes-cost {
				return false
			}
			cost += costs[next]
			depth = max(depth, 1+depths[next])
			if depth > maxSchemaDepth {
				return false
			}
		}
		costs[node], depths[node] = cost, depth
		state[node] = 2
		return true
	}
	// The containment edges connect every JSON value to the root, including
	// unused definitions and annotations, intentionally overestimating work.
	return visit("")
}
func decodeJSON(raw []byte) (any, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("tool arguments must be object")
	}
	return value, nil
}

func sanitizeResult(raw []byte, credential string, maxBytes int) (json.RawMessage, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxResultBytes
	}
	if len(raw) == 0 || len(raw) > maxBytes {
		return nil, ErrResultTooLarge
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil || dec.Decode(new(any)) != io.EOF {
		return nil, ErrRemoteFailure
	}
	budget := maxSanitizeNodes
	clean, err := sanitizeJSONValue(value, credential, 0, &budget)
	if err != nil {
		return nil, ErrResultTooLarge
	}
	encoded, err := json.Marshal(clean)
	if err != nil || len(encoded) > maxBytes {
		return nil, ErrResultTooLarge
	}
	return encoded, nil
}
func sanitizeJSONValue(value any, credential string, depth int, budget *int) (any, error) {
	if depth > maxSanitizeDepth {
		return nil, ErrResultTooLarge
	}
	*budget--
	if *budget < 0 {
		return nil, ErrResultTooLarge
	}
	switch item := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, child := range item {
			safeKey := redactCredential(key, credential)
			if sensitiveJSONKey(key) {
				out[safeKey] = "[redacted]"
				continue
			}
			sanitized, err := sanitizeJSONValue(child, credential, depth+1, budget)
			if err != nil {
				return nil, err
			}
			out[safeKey] = sanitized
		}
		return out, nil
	case []any:
		out := make([]any, len(item))
		for i, child := range item {
			sanitized, err := sanitizeJSONValue(child, credential, depth+1, budget)
			if err != nil {
				return nil, err
			}
			out[i] = sanitized
		}
		return out, nil
	case string:
		return redactCredential(item, credential), nil
	default:
		return value, nil
	}
}
func sensitiveJSONKey(key string) bool {
	k := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(key))
	for _, needle := range []string{"password", "secret", "token", "apikey", "authorization", "credential", "privatekey", "accesskey", "clientsecret"} {
		if strings.Contains(k, needle) {
			return true
		}
	}
	return false
}
func redactCredential(value, credential string) string {
	if credential != "" {
		return strings.ReplaceAll(value, credential, "[redacted]")
	}
	return value
}

// Validates a destination before each operation. The SDK adapter also pins
// checked public IP addresses at dial time to resist DNS rebinding.
func validatePublicIPs(ctx context.Context, host string) error {
	_, err := publicEgressIPs(ctx, host, net.DefaultResolver.LookupIPAddr)
	return err
}

func publicEgressIPs(ctx context.Context, host string, lookup func(context.Context, string) ([]net.IPAddr, error)) ([]net.IPAddr, error) {
	ips, err := lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("MCP destination resolution failed")
	}
	for _, addr := range ips {
		if !isPublicEgressIP(addr.IP) {
			return nil, errors.New("MCP destination denied")
		}
	}
	return ips, nil
}

var deniedEgressPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.31.196.0/24"), netip.MustParsePrefix("192.52.193.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("192.175.48.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"), netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("fec0::/10"), netip.MustParsePrefix("ff00::/8"),
}

func isPublicEgressIP(raw net.IP) bool {
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range deniedEgressPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}
