package httptool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"govoice/core"
	"govoice/server/egress"
	"govoice/server/mcp"
	"govoice/server/secrets"
	"govoice/server/storage"
)

var (
	// ErrNotFound: no such tool.
	ErrNotFound = errors.New("HTTP tool not found")
	// ErrDisabled: the tool exists but is switched off.
	ErrDisabled = errors.New("HTTP tool is disabled")
	// ErrDefinitionMissing: the pinned definition is no longer stored.
	ErrDefinitionMissing = errors.New("HTTP tool definition is missing")
)

// ReferencedError carries the agents that block a delete.
type ReferencedError struct{ Agents []storage.HTTPToolReference }

func (*ReferencedError) Error() string { return storage.ErrHTTPToolReferenced.Error() }
func (*ReferencedError) Unwrap() error { return storage.ErrHTTPToolReferenced }

type Config struct {
	Store          *storage.Store
	Secrets        secrets.Resolver
	AllowLocalHTTP bool // development and tests: http to loopback is allowed
	Tracer         core.Tracer
}

type Service struct {
	store   *storage.Store
	opts    Options
	secrets secrets.Resolver
	exec    *Executor
	tracer  core.Tracer
}

func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("HTTP tools need a store")
	}
	if cfg.Secrets == nil {
		cfg.Secrets = secrets.Chain{}
	}
	if cfg.Tracer == nil {
		cfg.Tracer = core.NoopTracer{}
	}
	s := &Service{store: cfg.Store, opts: Options{AllowLocalHTTP: cfg.AllowLocalHTTP}, secrets: cfg.Secrets, tracer: cfg.Tracer}
	s.exec = &Executor{Client: egress.NewClient(cfg.AllowLocalHTTP), Secret: s.secrets.Resolve}
	return s, nil
}

// View is a tool as the management API shows it: the definition and whether its secret resolves, never a value.
type View struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Enabled       bool       `json:"enabled"`
	Hash          string     `json:"hash"`
	Definition    Definition `json:"definition"`
	AuthSecretSet *bool      `json:"auth_secret_set,omitempty"` // present only when the tool has an auth secret
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func (s *Service) view(ctx context.Context, t storage.HTTPTool) (View, error) {
	def, err := s.definition(ctx, t.ID, t.CurrentHash)
	if err != nil {
		return View{}, err
	}
	v := View{ID: t.ID, Name: t.Name, Enabled: t.Enabled, Hash: t.CurrentHash, Definition: def, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt}
	if def.Auth != nil {
		set := s.SecretSet(ctx, def.Auth.Secret)
		v.AuthSecretSet = &set
	}
	return v, nil
}

// SecretSet reports whether a secret name resolves, without counting as a use of it.
func (s *Service) SecretSet(ctx context.Context, name string) bool {
	_, ok := s.secrets.Resolve(secrets.WithoutUseStamp(ctx), name)
	return ok
}

func (s *Service) definition(ctx context.Context, id, hash string) (Definition, error) {
	raw, err := s.store.HTTPTools().Definition(ctx, id, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return Definition{}, ErrDefinitionMissing
	}
	if err != nil {
		return Definition{}, err
	}
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return Definition{}, fmt.Errorf("stored HTTP tool definition: %w", err)
	}
	return d, nil
}

// prepare normalizes a definition, checks its destination and returns it with its hash and canonical JSON.
func (s *Service) prepare(ctx context.Context, d Definition) (Definition, string, []byte, error) {
	d, err := d.Normalize(ctx, s.opts)
	if err != nil {
		return d, "", nil, err
	}
	if err := s.checkDestination(ctx, d); err != nil {
		return d, "", nil, err
	}
	hash, canonical, err := d.Hash()
	return d, hash, canonical, err
}

// checkDestination refuses a host that is not reachable under the egress rules: public addresses only, plus loopback
// when the development switch is on. The dialer checks again at every call.
func (s *Service) checkDestination(ctx context.Context, d Definition) error {
	u, err := d.Host()
	if err != nil {
		return err
	}
	host := u.Hostname()
	loopback := s.opts.AllowLocalHTTP && egress.LoopbackOnly(ctx, host, net.DefaultResolver.LookupIPAddr)
	if loopback {
		return nil
	}
	if u.Scheme != "https" {
		return invalid("url must use https")
	}
	if err := egress.ValidatePublicHost(ctx, host); err != nil {
		return invalid("the host must be a public address")
	}
	return nil
}

func (s *Service) Create(ctx context.Context, d Definition) (View, error) {
	d, hash, canonical, err := s.prepare(ctx, d)
	if err != nil {
		return View{}, err
	}
	t := storage.HTTPTool{ID: storage.NewID(), Name: d.Name, CurrentHash: hash, Enabled: true}
	if err := s.store.HTTPTools().Create(ctx, t, canonical); err != nil {
		return View{}, err
	}
	return s.Get(ctx, t.ID)
}

// Update makes d the tool's new current definition. Agents that pinned an older one keep working until they are
// reviewed against the new one.
func (s *Service) Update(ctx context.Context, id string, d Definition) (View, error) {
	current, err := s.store.HTTPTools().Get(ctx, id)
	if err != nil {
		return View{}, mapNotFound(err)
	}
	// Agents and their steps refer to the tool by name, so a name is fixed once chosen.
	if strings.TrimSpace(d.Name) != current.Name {
		return View{}, invalid("the name of a tool cannot be changed; create a new tool instead")
	}
	d, hash, canonical, err := s.prepare(ctx, d)
	if err != nil {
		return View{}, err
	}
	if err := s.store.HTTPTools().NewDefinition(ctx, id, d.Name, hash, canonical); err != nil {
		return View{}, mapNotFound(err)
	}
	return s.Get(ctx, id)
}

func mapNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Service) Get(ctx context.Context, id string) (View, error) {
	t, err := s.store.HTTPTools().Get(ctx, id)
	if err != nil {
		return View{}, mapNotFound(err)
	}
	return s.view(ctx, t)
}

func (s *Service) List(ctx context.Context) ([]View, error) {
	tools, err := s.store.HTTPTools().List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(tools))
	for _, t := range tools {
		v, err := s.view(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) SetEnabled(ctx context.Context, id string, enabled bool) (View, error) {
	if err := s.store.HTTPTools().SetEnabled(ctx, id, enabled); err != nil {
		return View{}, mapNotFound(err)
	}
	return s.Get(ctx, id)
}

// Delete removes a tool no current agent pins.
func (s *Service) Delete(ctx context.Context, id string) error {
	err := s.store.HTTPTools().Delete(ctx, id)
	if errors.Is(err, storage.ErrHTTPToolReferenced) {
		refs, _ := s.store.HTTPTools().CurrentReferences(ctx, id)
		return &ReferencedError{Agents: refs}
	}
	return mapNotFound(err)
}

// TestResult is what a test call returns: the same result or error category a model would see.
type TestResult struct {
	Status        string          `json:"status"` // completed or failed
	DurationMS    int64           `json:"duration_ms"`
	Result        json.RawMessage `json:"result,omitempty"`
	ErrorCategory string          `json:"error_category,omitempty"`
	Message       string          `json:"message,omitempty"`
}

// Test calls the tool's current definition with sample arguments.
func (s *Service) Test(ctx context.Context, id string, args json.RawMessage) (TestResult, error) {
	t, err := s.store.HTTPTools().Get(ctx, id)
	if err != nil {
		return TestResult{}, mapNotFound(err)
	}
	def, err := s.definition(ctx, t.ID, t.CurrentHash)
	if err != nil {
		return TestResult{}, err
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	started := time.Now()
	out, err := s.exec.Execute(ctx, def, args)
	res := TestResult{DurationMS: time.Since(started).Milliseconds()}
	if err != nil {
		res.Status = "failed"
		res.ErrorCategory, res.Message = mcp.ErrorCategory(err)
		return res, nil
	}
	res.Status, res.Result = "completed", out
	return res, nil
}

// Captured is a tool pinned for one call: its definition at the pinned hash, ready to execute. It holds no secret
// value; the secret is resolved at each call.
type Captured struct {
	svc  *Service
	id   string
	hash string
	def  Definition
}

// Capture loads the pinned definition of an enabled tool.
func (s *Service) Capture(ctx context.Context, id, hash string) (*Captured, error) {
	t, err := s.store.HTTPTools().Get(ctx, id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if !t.Enabled {
		return nil, ErrDisabled
	}
	def, err := s.definition(ctx, id, hash)
	if err != nil {
		return nil, err
	}
	return &Captured{svc: s, id: id, hash: hash, def: def}, nil
}

func (c *Captured) Name() string            { return c.def.Name }
func (c *Captured) Description() string     { return c.def.Description }
func (c *Captured) Schema() json.RawMessage { return c.def.InputSchema }
func (c *Captured) Hash() string            { return c.hash }

// Endpoint is the URL template, which carries no credential (the definition forbids userinfo and query).
func (c *Captured) Endpoint() string { return c.def.URL }

// AuthSecret is the secret's name, or "".
func (c *Captured) AuthSecret() string {
	if c.def.Auth == nil {
		return ""
	}
	return c.def.Auth.Secret
}

// ExecuteCall runs the tool for one model call. It emits a metadata-only span: tool id and name, duration; never
// arguments, results, the URL or a secret.
func (c *Captured) ExecuteCall(ctx context.Context, args json.RawMessage, callID, sessionID, turnID string, generation uint64) (json.RawMessage, error) {
	started := time.Now()
	event := core.NewTraceEvent("http_tool.execute", started, core.SessionID(sessionID), core.TurnID(turnID), core.Generation(generation), core.TracePolicy{Content: core.TraceMetadata}, nil)
	event.ToolCallID = callID
	tracer := c.svc.tracer
	if trace := core.TraceContextFromContext(ctx); trace.Tracer != nil {
		if _, none := trace.Tracer.(core.NoopTracer); !none {
			tracer = trace.Tracer
		}
	}
	span := core.StartSpanSafe(ctx, tracer, event)
	span.SetAttribute("http_tool.id", c.id)
	span.SetAttribute("http_tool.name", c.def.Name)
	out, err := c.svc.exec.Execute(ctx, c.def, args)
	span.SetAttribute("http_tool.duration_ms", time.Since(started).Milliseconds())
	span.End(err)
	return out, err
}

// SecretUses lists the tools that use a secret by name in any stored definition, current or older: an agent may still
// pin an older one, and deleting the secret would break it.
func (s *Service) SecretUses(ctx context.Context, name string) ([]storage.SecretUse, error) {
	defs, err := s.store.HTTPTools().AllDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	var out []storage.SecretUse
	seen := map[string]bool{}
	for _, d := range defs {
		var def Definition
		if json.Unmarshal(d.JSON, &def) != nil || def.Auth == nil || def.Auth.Secret != name || seen[d.ToolID] {
			continue
		}
		seen[d.ToolID] = true
		out = append(out, storage.SecretUse{Kind: "http_tool", ID: d.ToolID, Name: d.Name})
	}
	return out, nil
}

// IsRef reports whether a pinned tool's server id is an HTTP tool.
func IsRef(serverID string) bool { return strings.HasPrefix(serverID, RefPrefix) }
