// Package mcp implements the Server-owned MCP domain and client boundary.
// Framework packages intentionally do not depend on this package.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
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
	ErrNotFound         = errors.New("MCP resource not found")
	ErrUnavailable      = errors.New("MCP server unavailable")
	ErrInvalidSchema    = errors.New("MCP tool schema invalid")
	ErrInvalidArguments = errors.New("tool arguments do not match schema")
	ErrRemoteFailure    = errors.New("MCP tool execution failed")
	ErrResultTooLarge   = errors.New("MCP result exceeds size limit")
	ErrLocalDisabled    = errors.New("local MCP process transport is disabled")
)

type RemoteTool struct {
	Name, Description string
	InputSchema       json.RawMessage
}
type RemoteResult struct{ JSON json.RawMessage }

// RemoteClient is the narrow SDK adapter boundary. All MCP SDK types and
// protocol-version details stay in this Server-owned package.
type RemoteClient interface {
	ListTools(context.Context, string, string) ([]RemoteTool, error)
	CallTool(context.Context, string, string, string, json.RawMessage) (RemoteResult, error)
}
type CredentialResolver interface {
	Resolve(context.Context, string) (string, error)
}
type DestinationPolicy interface {
	Validate(context.Context, *url.URL) error
}

type ServiceConfig struct {
	Store                                            *storage.Store
	Remote                                           RemoteClient
	Credentials                                      CredentialResolver
	Destinations                                     DestinationPolicy
	Tracer                                           core.Tracer
	Timeout                                          time.Duration
	MaxSchemaBytes, MaxResultBytes, MaxArgumentBytes int
}
type Service struct {
	store                              *storage.Store
	remote                             RemoteClient
	credentials                        CredentialResolver
	destinations                       DestinationPolicy
	tracer                             core.Tracer
	timeout                            time.Duration
	maxSchema, maxResult, maxArguments int
}
type ServerConfig struct {
	Name, Description, Endpoint, Transport, CredentialID string
	Enabled                                              bool
}
type ToolResult struct {
	JSON     json.RawMessage
	Duration time.Duration
	Status   string
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
	return &Service{store: cfg.Store, remote: cfg.Remote, credentials: cfg.Credentials, destinations: cfg.Destinations, tracer: cfg.Tracer, timeout: cfg.Timeout, maxSchema: cfg.MaxSchemaBytes, maxResult: cfg.MaxResultBytes, maxArguments: cfg.MaxArgumentBytes}, nil
}

// HTTPSDestinationPolicy is the safe default: remote endpoints require HTTPS,
// a host, and no embedded user-info. Deployments can apply stricter egress rules.
type HTTPSDestinationPolicy struct{}

func (HTTPSDestinationPolicy) Validate(_ context.Context, u *url.URL) error {
	if u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("remote MCP endpoint must be an HTTPS URL without embedded credentials")
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
		return storage.MCPServer{}, errors.New("MCP endpoint query parameters are not accepted; use a credential reference")
	}
	if err = s.destinations.Validate(ctx, u); err != nil {
		return storage.MCPServer{}, errors.New("MCP destination denied")
	}
	if strings.TrimSpace(cfg.Name) == "" || len(cfg.Name) > 128 || len(cfg.Description) > 1024 {
		return storage.MCPServer{}, errors.New("invalid MCP server metadata")
	}
	if cfg.CredentialID != "" {
		if _, err := uuid.Parse(cfg.CredentialID); err != nil {
			return storage.MCPServer{}, errors.New("invalid MCP credential reference")
		}
	}
	m := storage.MCPServer{ID: storage.NewID(), Name: strings.TrimSpace(cfg.Name), Description: cfg.Description, Endpoint: u.String(), Transport: cfg.Transport, AuthCredentialID: cfg.CredentialID, Enabled: cfg.Enabled, ValidationState: StateNotValidated}
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

func (s *Service) credential(ctx context.Context, m storage.MCPServer) (string, error) {
	if m.AuthCredentialID == "" {
		return "", nil
	}
	if s.credentials == nil {
		return "", errors.New("credential resolver unavailable")
	}
	token, err := s.credentials.Resolve(ctx, m.AuthCredentialID)
	if err != nil || token == "" {
		return "", errors.New("MCP authentication unavailable")
	}
	return token, nil
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
func (s *Service) span(ctx context.Context, name string) core.Span {
	return core.StartSpanSafe(ctx, s.tracer, core.NewTraceEvent(name, time.Now(), "", "", 0, core.TracePolicy{Content: core.TraceMetadata}, nil))
}

func (s *Service) Refresh(ctx context.Context, id string) (storage.MCPDiscovery, error) {
	span := s.span(ctx, "mcp.discovery")
	ended := false
	end := func(err error) {
		if !ended {
			ended = true
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
	token, err := s.credential(ctx, m)
	if err != nil {
		_ = s.store.MCP().SetValidation(ctx, m.ID, StateAuthenticationFailed, time.Time{}, time.Time{})
		end(err)
		return storage.MCPDiscovery{}, ErrUnavailable
	}
	callCtx, cancel := s.timed(ctx)
	defer cancel()
	started := time.Now()
	tools, err := s.remote.ListTools(callCtx, u.String(), token)
	if err != nil {
		state := StateConnectionFailed
		if strings.Contains(strings.ToLower(err.Error()), "auth") || strings.Contains(strings.ToLower(err.Error()), "401") || strings.Contains(strings.ToLower(err.Error()), "403") {
			state = StateAuthenticationFailed
		}
		_ = s.store.MCP().SetValidation(context.WithoutCancel(ctx), m.ID, state, time.Now(), time.Time{})
		end(safeCategory(state))
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
		if _, err := validateSchema(tool.InputSchema); err != nil {
			_ = s.store.MCP().SetValidation(context.WithoutCancel(ctx), m.ID, StateSchemaInvalid, time.Now(), time.Time{})
			span.End(ErrInvalidSchema)
			return storage.MCPDiscovery{}, ErrInvalidSchema
		}
		items = append(items, storage.MCPTool{RemoteName: tool.Name, Description: tool.Description, SchemaJSON: tool.InputSchema})
	}
	if callCtx.Err() != nil {
		_ = s.store.MCP().SetValidation(context.WithoutCancel(ctx), m.ID, StateConnectionFailed, time.Now(), time.Time{})
		end(callCtx.Err())
		return storage.MCPDiscovery{}, ErrUnavailable
	}
	d, err := s.store.MCP().AddDiscovery(ctx, m.ID, items)
	if err != nil {
		_ = s.store.MCP().SetValidation(context.WithoutCancel(ctx), m.ID, StateDiscoveryFailed, time.Now(), time.Time{})
		end(ErrInvalidSchema)
		return storage.MCPDiscovery{}, ErrInvalidSchema
	}
	_ = s.store.MCP().SetValidation(ctx, m.ID, StateHealthy, time.Now(), time.Now())
	span.SetAttribute("mcp.tool_count", len(items))
	span.SetAttribute("mcp.duration_ms", time.Since(started).Milliseconds())
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
	started := time.Now()
	span := s.span(ctx, "mcp.tool.execute")
	ended := false
	end := func(err error) {
		if !ended {
			ended = true
			span.End(err)
		}
	}
	defer func() { span.SetAttribute("mcp.duration_ms", time.Since(started).Milliseconds()); end(nil) }()
	m, err := s.Get(ctx, id)
	if err != nil {
		end(ErrNotFound)
		return ToolResult{}, ErrNotFound
	}
	if !m.Enabled {
		end(ErrUnavailable)
		return ToolResult{}, ErrUnavailable
	}
	d, err := s.store.MCP().LatestDiscovery(ctx, id)
	if err != nil {
		end(ErrNotFound)
		return ToolResult{}, ErrNotFound
	}
	var schema json.RawMessage
	for _, tool := range d.Tools {
		if tool.RemoteName == name {
			schema = tool.SchemaJSON
			break
		}
	}
	if len(schema) == 0 {
		end(ErrNotFound)
		return ToolResult{}, ErrNotFound
	}
	if len(args) > s.maxArguments {
		end(ErrInvalidArguments)
		return ToolResult{}, ErrInvalidArguments
	}
	instance, err := decodeJSON(args)
	if err != nil {
		end(ErrInvalidArguments)
		return ToolResult{}, ErrInvalidArguments
	}
	resolved, err := validateSchema(schema)
	if err != nil || resolved.Validate(instance) != nil {
		end(ErrInvalidArguments)
		return ToolResult{}, ErrInvalidArguments
	}
	u, err := s.endpoint(ctx, m)
	if err != nil {
		end(ErrUnavailable)
		return ToolResult{}, ErrUnavailable
	}
	token, err := s.credential(ctx, m)
	if err != nil {
		end(ErrUnavailable)
		return ToolResult{}, ErrUnavailable
	}
	callCtx, cancel := s.timed(ctx)
	defer cancel()
	result, err := s.remote.CallTool(callCtx, u.String(), token, name, args)
	if err != nil {
		end(ErrRemoteFailure)
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
	clean := redactResult(result.JSON, token)
	end(nil)
	return ToolResult{JSON: clean, Duration: time.Since(started), Status: "success"}, nil
}

func safeCategory(state string) error {
	switch state {
	case StateAuthenticationFailed:
		return errors.New("authentication_failed")
	case StateSchemaInvalid:
		return ErrInvalidSchema
	default:
		return ErrUnavailable
	}
}
func validateSchema(raw []byte) (*jsonschema.Resolved, error) {
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	if schema.Type != "object" && len(schema.Types) == 0 {
		return nil, errors.New("root schema must declare object type")
	}
	if schema.Type != "object" && len(schema.Types) > 0 {
		found := false
		for _, typ := range schema.Types {
			if typ == "object" {
				found = true
			}
		}
		if !found {
			return nil, errors.New("root schema must allow object type")
		}
	}
	return schema.Resolve(nil)
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

var secretPattern = regexp.MustCompile(`(?i)("?(?:password|secret|token|api[_-]?key|authorization)"?\s*[:=]\s*")([^"\r\n]{1,512})(")`)

func redactResult(raw []byte, token string) json.RawMessage {
	clean := string(raw)
	if token != "" {
		clean = strings.ReplaceAll(clean, token, "[redacted]")
	}
	clean = secretPattern.ReplaceAllString(clean, "$1[redacted]$3")
	return json.RawMessage(clean)
}

// Validates a destination before each operation. The SDK adapter also pins
// checked public IP addresses at dial time to resist DNS rebinding.
func validatePublicIPs(ctx context.Context, host string) error {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return errors.New("MCP destination resolution failed")
	}
	for _, addr := range ips {
		ip := addr.IP
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return errors.New("MCP destination denied")
		}
	}
	return nil
}
