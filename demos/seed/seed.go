package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"govoice/demos/spec"
)

const (
	// agentPageSize is the management API's maximum agent page size.
	agentPageSize = 100
	// defaultPollInterval is how often a pending MCP detach-delete is checked.
	defaultPollInterval = 250 * time.Millisecond
	// defaultRequestTimeout bounds one management request. MCP validation can
	// spend the server's discovery timeout, so this stays well above it.
	defaultRequestTimeout = 60 * time.Second
	// transportStreamableHTTP is the management API's MCP transport name.
	transportStreamableHTTP = "streamable-http"
	// httpToolRefPrefix is the pseudo MCP server id an HTTP tool selection uses.
	httpToolRefPrefix = "http:"
)

// options is one resolved seed run.
type options struct {
	serverURL string
	apiKey    string
	keyEnv    string
	demo      spec.Demo
	// mcpBases maps a demo MCPServerSpec.Logical to the base URL the fixture
	// answers on; an MCP server's endpoint is that base plus its EndpointPath.
	mcpBases map[string]string
	// httpBase is the base URL for the demo's HTTP tool paths.
	httpBase       string
	wipeOnly       bool
	dryRun         bool
	yes            bool
	jsonOut        bool
	wait           time.Duration
	pollInterval   time.Duration
	requestTimeout time.Duration
	getenv         func(string) string
	log            io.Writer
}

// report is the --json document and the record the CLI prints.
type report struct {
	Demo    string       `json:"demo"`
	Server  string       `json:"server"`
	DryRun  bool         `json:"dry_run"`
	Wiped   wipeReport   `json:"wiped"`
	Created createReport `json:"created"`
	Actions []string     `json:"actions"`
}

type wipeReport struct {
	AgentsArchived    int `json:"agents_archived"`
	MCPServersDeleted int `json:"mcp_servers_deleted"`
	SecretsDeleted    int `json:"secrets_deleted"`
}

type createReport struct {
	MCPServers map[string]string `json:"mcp_servers,omitempty"`
	HTTPTools  map[string]string `json:"http_tools,omitempty"`
	AgentID    string            `json:"agent_id,omitempty"`
	Readiness  string            `json:"readiness,omitempty"`
}

// target is one demo MCP server with its resolved endpoint.
type target struct {
	spec     spec.MCPServerSpec
	base     string
	endpoint string
}

// snapshot is the server state a run reads before it changes anything.
type snapshot struct {
	agents    []agentRow
	servers   []mcpServerRow
	httpTools []httpToolView
}

// runner performs one seed run.
type runner struct {
	opts   options
	c      *client
	report report
}

// run wipes and creates one demo. It writes nothing before it has checked the
// management key, the demo's tokens and the server's secret store.
func run(ctx context.Context, o options) (report, error) {
	if err := o.normalise(); err != nil {
		return report{}, err
	}
	var targets []target
	var httpBase string
	var err error
	if o.wipeOnly {
		// Wiping needs only the demo's resource names: no MCP service has to be running.
		targets = specTargets(o.demo)
	} else {
		targets, httpBase, err = resolveTargets(o.demo, o.mcpBases, o.httpBase)
		if err != nil {
			return report{}, err
		}
	}
	meta, err := demoAgentMeta(o.demo)
	if err != nil {
		return report{}, err
	}
	r := &runner{opts: o, c: newClient(o.serverURL, o.apiKey, o.requestTimeout)}
	r.report = report{
		Demo:    o.demo.Name(),
		Server:  o.serverURL,
		DryRun:  o.dryRun,
		Actions: []string{},
		Created: createReport{MCPServers: map[string]string{}, HTTPTools: map[string]string{}},
	}

	snap, err := r.readSnapshot(ctx)
	if err != nil {
		return r.report, err
	}
	liveAgents := matchingLiveAgents(snap.agents, meta.Name)
	servers := matchingServers(snap.servers, o.demo.MCPServers())
	if o.wipeOnly && !o.yes && !o.dryRun {
		return r.report, fmt.Errorf("--wipe-only is destructive: pass --yes (or --dry-run to preview it)")
	}
	if len(liveAgents)+len(servers) > 0 && !o.yes && !o.dryRun {
		return r.report, fmt.Errorf("demo %q already exists on %s: pass --yes to wipe and recreate it (--dry-run would only list the actions)", o.demo.Name(), o.serverURL)
	}

	if o.dryRun {
		// The plan still runs the read-only preflight so it cannot promise a run
		// that a missing token, a shared secret name or a disabled store would refuse.
		if o.wipeOnly {
			if err := r.preflightWipe(snap, targets); err != nil {
				return r.report, err
			}
		} else if err := r.preflightCreate(ctx, snap, targets); err != nil {
			return r.report, err
		}
		return r.plan(ctx, targets, httpBase, meta.Name, liveAgents, servers)
	}
	if !o.wipeOnly {
		if err := r.preflightCreate(ctx, snap, targets); err != nil {
			return r.report, err
		}
	} else if err := r.preflightWipe(snap, targets); err != nil {
		return r.report, err
	}
	if err := r.wipe(ctx, targets, liveAgents, servers); err != nil {
		return r.report, err
	}
	if o.wipeOnly {
		return r.report, nil
	}
	if err := r.create(ctx, targets, httpBase); err != nil {
		return r.report, err
	}
	r.act("seeded demo %q: agent %s is %s", o.demo.Name(), r.report.Created.AgentID, r.report.Created.Readiness)
	return r.report, nil
}

// normalise fills defaults and validates the run's fixed inputs.
func (o *options) normalise() error {
	if o.demo == nil {
		return errors.New("no demo definition")
	}
	o.apiKey = strings.TrimSpace(o.apiKey)
	if o.apiKey == "" {
		return fmt.Errorf("environment variable %s is not set: the management key is read from the environment, never a flag", o.keyEnv)
	}
	base, err := parseBaseURL(o.serverURL)
	if err != nil {
		return fmt.Errorf("--server: %w", err)
	}
	o.serverURL = base
	if o.wait <= 0 {
		return errors.New("--wait must be a positive duration")
	}
	if o.pollInterval <= 0 {
		o.pollInterval = defaultPollInterval
	}
	if o.requestTimeout <= 0 {
		o.requestTimeout = defaultRequestTimeout
	}
	if o.getenv == nil {
		return errors.New("no environment lookup")
	}
	return nil
}

// specTargets carries the demo's MCP specifications without endpoints, for a
// wipe-only run where no demo service needs to be reachable.
func specTargets(d spec.Demo) []target {
	var out []target
	for _, s := range d.MCPServers() {
		out = append(out, target{spec: s})
	}
	return out
}

// resolveTargets maps every demo MCP server to its endpoint and picks the HTTP
// tool base. It refuses a missing, duplicated or unknown --mcp mapping.
func resolveTargets(d spec.Demo, mcpBases map[string]string, httpBase string) ([]target, string, error) {
	servers := d.MCPServers()
	targets := make([]target, 0, len(servers))
	for _, s := range servers {
		base, ok := mcpBases[s.Logical]
		if !ok || strings.TrimSpace(base) == "" {
			return nil, "", fmt.Errorf("--mcp %s=<base URL> is required for the demo MCP server %q", s.Logical, s.Name)
		}
		normalised, err := parseBaseURL(base)
		if err != nil {
			return nil, "", fmt.Errorf("--mcp %s: %w", s.Logical, err)
		}
		endpoint, err := joinURL(normalised, s.EndpointPath)
		if err != nil {
			return nil, "", fmt.Errorf("MCP server %q endpoint: %w", s.Name, err)
		}
		targets = append(targets, target{spec: s, base: normalised, endpoint: endpoint})
	}
	// Extra --mcp values are ignored: the shared make target passes every demo's
	// base URL, and a typo on a needed server is already refused above in the loop.
	httpTools := d.HTTPTools()
	if httpBase == "" {
		if len(targets) > 0 {
			httpBase = targets[0].base
		} else if len(httpTools) > 0 {
			return nil, "", errors.New("--http-base is required for a demo whose HTTP tools have no MCP server to default to")
		}
	}
	if len(httpTools) > 0 {
		normalised, err := parseBaseURL(httpBase)
		if err != nil {
			return nil, "", fmt.Errorf("--http-base: %w", err)
		}
		httpBase = normalised
	}
	return targets, httpBase, nil
}

// demoAgentMeta asks the demo for its agent definition with placeholder tool
// ids. Wipe needs the exact agent name before any tool exists, and the contract
// only exposes it through Agent; the placeholder ids are never written.
func demoAgentMeta(d spec.Demo) (spec.AgentSpec, error) {
	resolution := spec.Resolution{ServerIDs: map[string]string{}, HTTPPins: map[string]spec.ToolPin{}}
	for _, s := range d.MCPServers() {
		resolution.ServerIDs[s.Logical] = "pending"
	}
	for _, t := range d.HTTPTools() {
		resolution.HTTPPins[t.Name] = spec.ToolPin{ServerID: "pending", RemoteName: t.Name, Required: true}
	}
	agent, err := d.Agent(resolution)
	if err != nil {
		return spec.AgentSpec{}, fmt.Errorf("demo %q agent definition: %w", d.Name(), err)
	}
	if strings.TrimSpace(agent.Name) == "" {
		return spec.AgentSpec{}, fmt.Errorf("demo %q has no agent name", d.Name())
	}
	return agent, nil
}

// readSnapshot reads every agent page and the MCP server list.
func (r *runner) readSnapshot(ctx context.Context) (snapshot, error) {
	agents, err := r.listAgents(ctx)
	if err != nil {
		return snapshot{}, err
	}
	servers, err := r.listServers(ctx)
	if err != nil {
		return snapshot{}, err
	}
	httpTools, err := r.listHTTPTools(ctx)
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{agents: agents, servers: servers, httpTools: httpTools}, nil
}

func (r *runner) listAgents(ctx context.Context) ([]agentRow, error) {
	out := make([]agentRow, 0, agentPageSize)
	for offset := 0; ; offset += agentPageSize {
		path := fmt.Sprintf("/api/v1/agents?limit=%d&offset=%d", agentPageSize, offset)
		var page envelope[[]agentRow]
		if _, err := r.c.do(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, fmt.Errorf("list agents: %w", err)
		}
		out = append(out, page.Data...)
		if len(page.Data) < agentPageSize {
			return out, nil
		}
	}
}

func (r *runner) listServers(ctx context.Context) ([]mcpServerRow, error) {
	var response envelope[[]mcpServerRow]
	if _, err := r.c.do(ctx, http.MethodGet, "/api/v1/mcp-servers", nil, &response); err != nil {
		return nil, fmt.Errorf("list MCP servers: %w", err)
	}
	return response.Data, nil
}

func (r *runner) listHTTPTools(ctx context.Context) ([]httpToolView, error) {
	var response envelope[[]httpToolView]
	if _, err := r.c.do(ctx, http.MethodGet, "/api/v1/http-tools", nil, &response); err != nil {
		return nil, fmt.Errorf("list HTTP tools: %w", err)
	}
	return response.Data, nil
}

// matchingLiveAgents returns the live agents with the demo agent's exact name.
func matchingLiveAgents(agents []agentRow, name string) []agentRow {
	out := make([]agentRow, 0, 1)
	for _, a := range agents {
		if a.Name == name && !a.Archived {
			out = append(out, a)
		}
	}
	return out
}

// matchingServers returns every MCP server whose name equals a demo server name.
func matchingServers(servers []mcpServerRow, demo []spec.MCPServerSpec) []mcpServerRow {
	names := make(map[string]bool, len(demo))
	for _, s := range demo {
		names[s.Name] = true
	}
	out := make([]mcpServerRow, 0, len(demo))
	for _, m := range servers {
		if names[m.Name] {
			out = append(out, m)
		}
	}
	return out
}

// preflightCreate checks everything that can refuse the run before the wipe
// begins: the demo tokens, a secret name another server owns, and the store.
func (r *runner) preflightCreate(ctx context.Context, snap snapshot, targets []target) error {
	for _, t := range targets {
		if strings.TrimSpace(r.opts.getenv(t.spec.TokenEnv)) == "" {
			return fmt.Errorf("environment variable %s is not set: demo %q needs it for the %q MCP server (the token is read from the environment, never a flag)", t.spec.TokenEnv, r.opts.demo.Name(), t.spec.Name)
		}
	}
	demoServerNames := make(map[string]bool, len(targets))
	for _, t := range targets {
		demoServerNames[t.spec.Name] = true
	}
	for _, t := range targets {
		for _, m := range snap.servers {
			if m.AuthEnv == t.spec.SecretName && !demoServerNames[m.Name] {
				return fmt.Errorf("refusing to write token secret %q: MCP server %q is not part of demo %q and already uses it", t.spec.SecretName, m.Name, r.opts.demo.Name())
			}
		}
		if tool := httpToolUsingSecret(snap.httpTools, t.spec.SecretName, demoHTTPToolNames(r.opts.demo)); tool != "" {
			return fmt.Errorf("refusing to write token secret %q: HTTP tool %q is not part of demo %q and already uses it", t.spec.SecretName, tool, r.opts.demo.Name())
		}
	}
	var secrets envelope[secretListView]
	if _, err := r.c.do(ctx, http.MethodGet, "/api/v1/secrets", nil, &secrets); err != nil {
		return fmt.Errorf("check secret store: %w", err)
	}
	if !secrets.Data.StoreEnabled {
		return errors.New("the server's encrypted secret store is off: set SWITCHBOARD_SECRETS_KEY and restart the server before seeding")
	}
	return nil
}

// preflightWipe refuses to start a teardown the secret API would block halfway:
// a non-demo MCP server already owns one of the demo's secret names.
func (r *runner) preflightWipe(snap snapshot, targets []target) error {
	demoServerNames := make(map[string]bool, len(targets))
	for _, t := range targets {
		demoServerNames[t.spec.Name] = true
	}
	for _, t := range targets {
		for _, m := range snap.servers {
			if m.AuthEnv == t.spec.SecretName && !demoServerNames[m.Name] {
				return fmt.Errorf("refusing to wipe token secret %q: MCP server %q is not part of demo %q and already uses it", t.spec.SecretName, m.Name, r.opts.demo.Name())
			}
		}
		if tool := httpToolUsingSecret(snap.httpTools, t.spec.SecretName, demoHTTPToolNames(r.opts.demo)); tool != "" {
			return fmt.Errorf("refusing to wipe token secret %q: HTTP tool %q is not part of demo %q and already uses it", t.spec.SecretName, tool, r.opts.demo.Name())
		}
	}
	return nil
}

// demoHTTPToolNames is the set of tools the demo owns by exact name.
func demoHTTPToolNames(d spec.Demo) map[string]bool {
	out := make(map[string]bool, len(d.HTTPTools()))
	for _, h := range d.HTTPTools() {
		out[h.Name] = true
	}
	return out
}

// httpToolUsingSecret names a non-demo HTTP tool whose definition references the
// secret. Historical definitions are not enumerable through the API; only the
// current definition can be checked here.
func httpToolUsingSecret(tools []httpToolView, secret string, owned map[string]bool) string {
	for _, tool := range tools {
		if owned[tool.Name] {
			continue
		}
		var auth struct {
			Secret string `json:"secret"`
		}
		if len(tool.Definition.Auth) > 0 && json.Unmarshal(tool.Definition.Auth, &auth) == nil && auth.Secret == secret {
			return tool.Name
		}
	}
	return ""
}

// act records a human-readable action. In --json mode only the report carries
// it, so stdout stays one JSON document.
func (r *runner) act(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.report.Actions = append(r.report.Actions, line)
	if !r.opts.jsonOut && r.opts.log != nil {
		_, _ = fmt.Fprintln(r.opts.log, line)
	}
}

// parseBaseURL validates an absolute http(s) URL without a query or fragment
// and returns it without a trailing slash.
func parseBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil {
		return "", errors.New("invalid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("URL %q must use http or https", safeURL(parsed))
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("URL %q has no host", safeURL(parsed))
	}
	if parsed.User != nil {
		return "", fmt.Errorf("URL %q must not carry credentials", safeURL(parsed))
	}
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/mcp") {
		return "", fmt.Errorf("URL %q is the MCP endpoint; use its base URL (without /mcp)", safeURL(parsed))
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("URL %q must not carry a query or fragment", safeURL(parsed))
	}
	return strings.TrimRight(raw, "/"), nil
}

// safeURL renders a URL for an error message: no credentials, query or fragment.
func safeURL(u *url.URL) string {
	if u == nil {
		return "invalid URL"
	}
	copied := *u
	copied.User = nil
	copied.RawQuery = ""
	copied.Fragment = ""
	return copied.String()
}

// joinURL appends a path to a base URL.
func joinURL(base, path string) (string, error) {
	if _, err := parseBaseURL(base); err != nil {
		return "", err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return strings.TrimRight(base, "/"), nil
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/"), nil
}
