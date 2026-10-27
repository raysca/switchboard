package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeAPI is an in-memory management API. It implements the endpoints seed uses,
// records every request in order, and lets a test pre-populate the state a run
// would find. It is deliberately strict where the real server is strict: a bad
// bearer key is refused, and only the documented statuses are returned.
type fakeAPI struct {
	t   *testing.T
	mu  sync.Mutex
	key string

	agents       []*fakeAgent
	servers      []*fakeServer
	tools        []*fakeHTTPTool
	secrets      map[string]string
	storeEnabled bool
	discovery    []string // remote tool names every validated server publishes
	pendingPolls int      // detach-delete returns 202 for this many polls (0 = immediate)
	agentState   string   // readiness of a created agent; empty means ready
	agentReasons []string
	nextID       int

	ops []string
}

type fakeAgent struct {
	ID          string
	Name        string
	Description string
	Archived    bool
	Pins        []toolSelection
	Config      json.RawMessage
	State       string
	Reasons     []string
}

type fakeServer struct {
	row         mcpServerRow
	discovery   []discoveryTool
	pendingLeft int
	gone        bool
}

type fakeHTTPTool struct {
	view     httpToolView
	versions int
}

func newFakeAPI(t *testing.T, key string) *fakeAPI {
	t.Helper()
	return &fakeAPI{t: t, key: key, storeEnabled: true, secrets: map[string]string{}, agentState: "ready"}
}

// serve starts the fake on a loopback listener.
func (f *fakeAPI) serve() *httptest.Server {
	server := httptest.NewServer(f)
	f.t.Cleanup(server.Close)
	return server
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+f.key {
		f.writeError(w, http.StatusUnauthorized, "unauthorized", "invalid management key")
		return
	}
	path := r.URL.Path
	switch {
	case path == "/api/v1/agents":
		f.agentsCollection(w, r)
	case strings.HasPrefix(path, "/api/v1/agents/"):
		f.agentItem(w, r, strings.TrimPrefix(path, "/api/v1/agents/"))
	case path == "/api/v1/mcp-servers":
		f.serversCollection(w, r)
	case strings.HasPrefix(path, "/api/v1/mcp-servers/"):
		f.serverItem(w, r, strings.TrimPrefix(path, "/api/v1/mcp-servers/"))
	case path == "/api/v1/http-tools":
		f.httpToolsCollection(w, r)
	case strings.HasPrefix(path, "/api/v1/http-tools/"):
		f.httpToolItem(w, r, strings.TrimPrefix(path, "/api/v1/http-tools/"))
	case path == "/api/v1/secrets":
		f.secretsCollection(w, r)
	case strings.HasPrefix(path, "/api/v1/secrets/"):
		f.secretItem(w, r, strings.TrimPrefix(path, "/api/v1/secrets/"))
	default:
		f.writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func (f *fakeAPI) agentsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, offset := 50, 0
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 100 {
				f.writeError(w, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
				return
			}
			limit = parsed
		}
		if raw := r.URL.Query().Get("offset"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 {
				f.writeError(w, http.StatusBadRequest, "invalid_request", "offset must be nonnegative")
				return
			}
			offset = parsed
		}
		rows := make([]agentRow, 0, len(f.agents))
		for i, agent := range f.agents {
			if i < offset {
				continue
			}
			if len(rows) == limit {
				break
			}
			rows = append(rows, agentRow{ID: agent.ID, Name: agent.Name, Archived: agent.Archived})
		}
		f.writeData(w, http.StatusOK, rows)
	case http.MethodPost:
		var request createAgentRequest
		if !f.decode(w, r, &request) {
			return
		}
		agent := &fakeAgent{ID: f.id("agent"), Name: request.Name, Description: request.Description, Pins: request.MCPTools, Config: request.Config, State: f.agentState, Reasons: f.agentReasons}
		if agent.State == "" {
			agent.State = "ready"
		}
		if agent.Reasons == nil {
			agent.Reasons = []string{}
		}
		f.agents = append(f.agents, agent)
		f.writeData(w, http.StatusCreated, agent.detail())
	default:
		f.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported")
	}
}

func (f *fakeAPI) agentItem(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	agent := f.findAgent(parts[0])
	if agent == nil {
		f.writeError(w, http.StatusNotFound, "not_found", "agent not found")
		return
	}
	if len(parts) == 2 && parts[1] == "archive" && r.Method == http.MethodPost {
		if agent.Archived {
			f.writeData(w, http.StatusOK, map[string]bool{"archived": true})
			return
		}
		agent.Archived = true
		f.writeData(w, http.StatusOK, map[string]bool{"archived": true})
		return
	}
	if len(parts) != 1 || r.Method != http.MethodGet {
		f.writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	f.writeData(w, http.StatusOK, agent.detail())
}

func (f *fakeAPI) serversCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows := make([]mcpServerRow, 0, len(f.servers))
		for _, server := range f.servers {
			if !server.gone {
				rows = append(rows, server.row)
			}
		}
		f.writeData(w, http.StatusOK, rows)
	case http.MethodPost:
		var request createServerRequest
		if !f.decode(w, r, &request) {
			return
		}
		row := mcpServerRow{ID: f.id("mcp"), Name: request.Name, Description: request.Description, Endpoint: request.Endpoint, Transport: request.Transport, AuthEnv: request.AuthEnv, Enabled: request.Enabled, ValidationState: "not_validated"}
		server := &fakeServer{row: row}
		f.servers = append(f.servers, server)
		f.writeData(w, http.StatusCreated, row)
	default:
		f.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported")
	}
}

func (f *fakeAPI) serverItem(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	server := f.findServer(parts[0])
	if server == nil || server.gone {
		f.writeError(w, http.StatusNotFound, "not_found", "MCP server not found")
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		if server.pendingLeft > 0 {
			server.pendingLeft--
			if server.pendingLeft == 0 {
				server.gone = true
				f.writeError(w, http.StatusNotFound, "not_found", "MCP server not found")
				return
			}
		}
		f.writeData(w, http.StatusOK, mcpServerDetail{mcpServerRow: server.row, Discovery: server.discoveryView()})
	case len(parts) == 2 && parts[1] == "detach-delete" && r.Method == http.MethodPost:
		var request struct {
			Confirm bool `json:"confirm"`
		}
		if !f.decode(w, r, &request) {
			return
		}
		if !request.Confirm {
			f.writeError(w, http.StatusBadRequest, "confirmation_required", "explicit confirmation is required")
			return
		}
		if f.pendingPolls > 0 {
			server.pendingLeft = f.pendingPolls
			f.writeData(w, http.StatusAccepted, map[string]any{"pending": true, "server": server.row})
			return
		}
		server.gone = true
		f.writeData(w, http.StatusOK, map[string]any{"deleted": true})
	case len(parts) == 2 && parts[1] == "validate" && r.Method == http.MethodPost:
		if f.discovery == nil {
			server.row.ValidationState = "discovery_failed"
			server.discovery = nil
		} else {
			server.row.ValidationState = "healthy"
			server.discovery = make([]discoveryTool, 0, len(f.discovery))
			for _, name := range f.discovery {
				server.discovery = append(server.discovery, discoveryTool{RemoteName: name, Description: "fake " + name, InputSchema: json.RawMessage(`{"type":"object"}`), SchemaHash: "schema-" + name})
			}
		}
		f.writeData(w, http.StatusOK, server.row)
	default:
		f.writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

func (f *fakeAPI) httpToolsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		views := make([]httpToolView, 0, len(f.tools))
		for _, tool := range f.tools {
			views = append(views, tool.view)
		}
		f.writeData(w, http.StatusOK, views)
	case http.MethodPost:
		var request httpDefinitionRequest
		if !f.decode(w, r, &request) {
			return
		}
		if f.findHTTPTool(request.Name) != nil {
			f.writeError(w, http.StatusConflict, "http_tool_name_taken", "an HTTP tool with that name exists")
			return
		}
		tool := &fakeHTTPTool{view: httpToolView{ID: f.id("tool"), Name: request.Name, Enabled: true}}
		tool.versions = 1
		tool.setDefinition(request)
		f.tools = append(f.tools, tool)
		f.writeData(w, http.StatusCreated, tool.view)
	default:
		f.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported")
	}
}

func (f *fakeAPI) httpToolItem(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "enable" && r.Method == http.MethodPost {
		tool := f.findHTTPToolByID(parts[0])
		if tool == nil {
			f.writeError(w, http.StatusNotFound, "not_found", "HTTP tool not found")
			return
		}
		tool.view.Enabled = true
		f.writeData(w, http.StatusOK, tool.view)
		return
	}
	if len(parts) != 1 {
		f.writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	tool := f.findHTTPToolByID(parts[0])
	if tool == nil {
		f.writeError(w, http.StatusNotFound, "not_found", "HTTP tool not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		f.writeData(w, http.StatusOK, tool.view)
	case http.MethodPut:
		var request httpDefinitionRequest
		if !f.decode(w, r, &request) {
			return
		}
		if request.Name != tool.view.Name {
			f.writeError(w, http.StatusBadRequest, "invalid_http_tool", "the name of a tool cannot be changed")
			return
		}
		tool.versions++
		tool.setDefinition(request)
		f.writeData(w, http.StatusOK, tool.view)
	case http.MethodDelete:
		// seed must never delete an HTTP tool; keep the call visible in ops.
		f.writeError(w, http.StatusConflict, "http_tool_in_use", "the tool is pinned by current agents")
	default:
		f.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported")
	}
}

func (f *fakeAPI) secretsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported")
		return
	}
	view := secretListView{StoreEnabled: f.storeEnabled}
	for name := range f.secrets {
		view.Secrets = append(view.Secrets, struct {
			Name string `json:"name"`
		}{Name: name})
	}
	f.writeData(w, http.StatusOK, view)
}

func (f *fakeAPI) secretItem(w http.ResponseWriter, r *http.Request, name string) {
	switch r.Method {
	case http.MethodPut:
		if !f.storeEnabled {
			f.writeError(w, http.StatusServiceUnavailable, "secrets_store_disabled", "the encrypted secret store is off")
			return
		}
		var request struct {
			Value string `json:"value"`
		}
		if !f.decode(w, r, &request) {
			return
		}
		if request.Value == "" {
			f.writeError(w, http.StatusBadRequest, "invalid_secret", "value must not be empty")
			return
		}
		f.secrets[name] = request.Value
		f.writeData(w, http.StatusOK, map[string]any{"name": name, "source": "store", "writable": true})
	case http.MethodDelete:
		if !f.storeEnabled {
			f.writeError(w, http.StatusNotFound, "not_found", "secret not found")
			return
		}
		if _, ok := f.secrets[name]; !ok {
			f.writeError(w, http.StatusNotFound, "not_found", "secret not found")
			return
		}
		delete(f.secrets, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported")
	}
}

func (a *fakeAgent) detail() agentDetail {
	return agentDetail{ID: a.ID, Name: a.Name, Archived: a.Archived, Readiness: readinessView{State: a.State, Reasons: a.Reasons}}
}

func (s *fakeServer) discoveryView() *discoveryView {
	if s.row.ValidationState != "healthy" || len(s.discovery) == 0 {
		return nil
	}
	return &discoveryView{Tools: s.discovery}
}

func (t *fakeHTTPTool) setDefinition(request httpDefinitionRequest) {
	t.view.Hash = fmt.Sprintf("hash-%s-%d", t.view.ID, t.versions)
	t.view.Definition = httpDefinitionView{
		Name:             request.Name,
		Description:      request.Description,
		Method:           request.Method,
		URL:              request.URL,
		InputSchema:      request.InputSchema,
		TimeoutMS:        10000,
		MaxResponseBytes: 65536,
	}
}

func (f *fakeAPI) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		f.writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return false
	}
	return true
}

func (f *fakeAPI) writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (f *fakeAPI) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func (f *fakeAPI) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeAPI) findAgent(id string) *fakeAgent {
	for _, agent := range f.agents {
		if agent.ID == id {
			return agent
		}
	}
	return nil
}

func (f *fakeAPI) findServer(id string) *fakeServer {
	for _, server := range f.servers {
		if server.row.ID == id {
			return server
		}
	}
	return nil
}

func (f *fakeAPI) findHTTPTool(name string) *fakeHTTPTool {
	for _, tool := range f.tools {
		if tool.view.Name == name {
			return tool
		}
	}
	return nil
}

func (f *fakeAPI) findHTTPToolByID(id string) *fakeHTTPTool {
	for _, tool := range f.tools {
		if tool.view.ID == id {
			return tool
		}
	}
	return nil
}

// addAgent pre-populates an agent row.
func (f *fakeAPI) addAgent(name string, archived bool) *fakeAgent {
	f.mu.Lock()
	defer f.mu.Unlock()
	agent := &fakeAgent{ID: f.id("agent"), Name: name, Archived: archived, State: "ready", Reasons: []string{}}
	f.agents = append(f.agents, agent)
	return agent
}

// addServer pre-populates an MCP server row with the given tool names.
func (f *fakeAPI) addServer(name, endpoint, authEnv string, tools []string) *fakeServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	server := &fakeServer{row: mcpServerRow{ID: f.id("mcp"), Name: name, Endpoint: endpoint, Transport: transportStreamableHTTP, AuthEnv: authEnv, Enabled: true, ValidationState: "healthy"}}
	for _, tool := range tools {
		server.discovery = append(server.discovery, discoveryTool{RemoteName: tool, Description: "fake " + tool, InputSchema: json.RawMessage(`{"type":"object"}`), SchemaHash: "schema-" + tool})
	}
	f.servers = append(f.servers, server)
	return server
}

// addHTTPTool pre-populates an HTTP tool with an arbitrary stored definition.
func (f *fakeAPI) addHTTPTool(request httpDefinitionRequest) *fakeHTTPTool {
	f.mu.Lock()
	defer f.mu.Unlock()
	tool := &fakeHTTPTool{view: httpToolView{ID: f.id("tool"), Name: request.Name, Enabled: true}}
	tool.versions = 1
	tool.setDefinition(request)
	f.tools = append(f.tools, tool)
	return tool
}

func (f *fakeAPI) setSecret(name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets[name] = value
}

// resetOps clears the request record so a test can assert on one phase only.
func (f *fakeAPI) resetOps() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = nil
}

// recorded returns a copy of the request record.
func (f *fakeAPI) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// writes returns the recorded requests that changed state.
func (f *fakeAPI) writes() []string {
	var out []string
	for _, op := range f.recorded() {
		if !strings.HasPrefix(op, http.MethodGet+" ") {
			out = append(out, op)
		}
	}
	return out
}

// indexOf returns the position of the first recorded request with this prefix,
// or -1.
func (f *fakeAPI) indexOf(prefix string) int {
	for i, op := range f.recorded() {
		if strings.HasPrefix(op, prefix) {
			return i
		}
	}
	return -1
}

func (f *fakeAPI) liveAgents() []*fakeAgent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeAgent
	for _, agent := range f.agents {
		if !agent.Archived {
			out = append(out, agent)
		}
	}
	return out
}

func (f *fakeAPI) liveServers() []*fakeServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeServer
	for _, server := range f.servers {
		if !server.gone {
			out = append(out, server)
		}
	}
	return out
}
