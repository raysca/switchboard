package mcp

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"govoice/server/api"
	"govoice/server/storage"
)

// Handler exposes the MCP management API. It must only be mounted behind the
// server's /api/v1 boundary, which authenticates every request first.
func Handler(service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/mcp-servers", func(w http.ResponseWriter, r *http.Request) {
		id := requestID(w, r)
		switch r.Method {
		case http.MethodGet:
			servers, err := service.store.MCP().ListServers(r.Context())
			if err != nil {
				api.WriteError(w, 500, "internal_error", "MCP servers unavailable", id)
				return
			}
			views := make([]ServerView, 0, len(servers))
			for _, m := range servers {
				views = append(views, view(m, service.tokens))
			}
			api.WriteJSON(w, 200, api.SuccessResponse[[]ServerView]{Data: views})
		case http.MethodPost:
			var input ServerConfig
			if err := api.DecodeJSON(r, &input); err != nil {
				api.WriteError(w, 400, "invalid_request", err.Error(), id)
				return
			}
			m, err := service.Create(r.Context(), input)
			if err != nil {
				api.WriteError(w, 400, "invalid_mcp_server", "MCP server configuration is invalid", id)
				return
			}
			api.WriteJSON(w, http.StatusCreated, api.SuccessResponse[ServerView]{Data: view(m, service.tokens)})
		default:
			w.Header().Set("Allow", "GET, POST")
			api.WriteError(w, 405, "method_not_allowed", "method is not supported", id)
		}
	})
	mux.HandleFunc("/api/v1/mcp-servers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := requestID(w, r)
		if r.Method == http.MethodDelete {
			if err := service.Delete(r.Context(), r.PathValue("id")); err != nil {
				if errors.Is(err, storage.ErrMCPReferenced) {
					refs, _ := service.store.MCP().CurrentReferences(r.Context(), r.PathValue("id"))
					api.WriteJSON(w, http.StatusConflict, api.ErrorResponse{Error: api.Error{Code: "mcp_server_in_use", Message: "MCP server is referenced by flows or active sessions", RequestID: id, Details: refs}})
					return
				}
				writeLookupError(w, err, id)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET, DELETE")
			api.WriteError(w, 405, "method_not_allowed", "method is not supported", id)
			return
		}
		m, err := service.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			writeLookupError(w, err, id)
			return
		}
		response := ServerDetail{ServerView: view(m, service.tokens)}
		if m.LatestDiscoveryID != "" {
			d, err := service.store.MCP().Discovery(r.Context(), m.LatestDiscoveryID)
			if err == nil {
				response.Discovery = &DiscoveryView{ID: d.ID, Hash: d.DiscoveryHash, CreatedAt: d.CreatedAt, Tools: toolViews(d.Tools)}
			}
		}
		api.WriteJSON(w, 200, api.SuccessResponse[ServerDetail]{Data: response})
	})
	mux.HandleFunc("POST /api/v1/mcp-servers/{id}/disable", func(w http.ResponseWriter, r *http.Request) {
		id := requestID(w, r)
		serverID := r.PathValue("id")
		if err := service.Disable(r.Context(), serverID); err != nil {
			api.WriteError(w, 404, "not_found", "MCP server not found", id)
			return
		}
		m, err := service.Get(r.Context(), serverID)
		if err != nil {
			api.WriteError(w, 404, "not_found", "MCP server not found", id)
			return
		}
		api.WriteJSON(w, 200, api.SuccessResponse[ServerView]{Data: view(m, service.tokens)})
	})
	mux.HandleFunc("POST /api/v1/mcp-servers/{id}/enable", func(w http.ResponseWriter, r *http.Request) {
		id := requestID(w, r)
		serverID := r.PathValue("id")
		if err := service.Enable(r.Context(), serverID); err != nil {
			api.WriteError(w, 409, "mcp_server_unavailable", "MCP server cannot be enabled while deletion is pending", id)
			return
		}
		m, err := service.Get(r.Context(), serverID)
		if err != nil {
			api.WriteError(w, 404, "not_found", "MCP server not found", id)
			return
		}
		api.WriteJSON(w, 200, api.SuccessResponse[ServerView]{Data: view(m, service.tokens)})
	})
	mux.HandleFunc("POST /api/v1/mcp-servers/{id}/detach-delete", func(w http.ResponseWriter, r *http.Request) {
		id := requestID(w, r)
		var input struct {
			Confirm bool `json:"confirm"`
		}
		if err := api.DecodeJSON(r, &input); err != nil || !input.Confirm {
			api.WriteError(w, 400, "confirmation_required", "explicit confirmation is required to detach and delete this MCP server", id)
			return
		}
		serverID := r.PathValue("id")
		if err := service.DetachAndDelete(r.Context(), serverID); err != nil {
			writeLookupError(w, err, id)
			return
		}
		m, err := service.Get(r.Context(), serverID)
		if errors.Is(err, ErrNotFound) { // detached and removed
			api.WriteJSON(w, 200, api.SuccessResponse[DetachResult]{Data: DetachResult{Deleted: true}})
			return
		}
		if err != nil {
			writeLookupError(w, err, id)
			return
		}
		v := view(m, service.tokens)
		api.WriteJSON(w, 202, api.SuccessResponse[DetachResult]{Data: DetachResult{Pending: true, Server: &v}})
	})
	mux.HandleFunc("POST /api/v1/mcp-servers/{id}/validate", func(w http.ResponseWriter, r *http.Request) {
		id := requestID(w, r)
		m, err := service.Validate(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrNotFound) {
			api.WriteError(w, 404, "not_found", "MCP server not found", id)
			return
		}
		// Validation failures are reported in the view, not as an HTTP error.
		api.WriteJSON(w, 200, api.SuccessResponse[ServerView]{Data: view(m, service.tokens)})
	})
	mux.HandleFunc("POST /api/v1/mcp-servers/{id}/tools/{tool}/test", func(w http.ResponseWriter, r *http.Request) {
		id := requestID(w, r)
		var input struct {
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := api.DecodeJSON(r, &input); err != nil {
			api.WriteError(w, 400, "invalid_request", err.Error(), id)
			return
		}
		result, err := service.Execute(r.Context(), r.PathValue("id"), r.PathValue("tool"), input.Arguments)
		if err != nil {
			status := 400
			if errors.Is(err, ErrNotFound) {
				status = 404
			}
			if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRemoteFailure) {
				status = 502
			}
			api.WriteError(w, status, "mcp_tool_failed", err.Error(), id)
			return
		}
		api.WriteJSON(w, 200, api.SuccessResponse[ExecutionView]{Data: executionView(result)})
	})
	return mux
}

type ServerView struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Description       string     `json:"description"`
	Endpoint          string     `json:"endpoint"`
	Transport         string     `json:"transport"`
	AuthEnv           string     `json:"auth_env,omitempty"`
	AuthEnvSet        bool       `json:"auth_env_set"`
	ValidationState   string     `json:"validation_state"`
	LatestDiscoveryID string     `json:"latest_discovery_id,omitempty"`
	Enabled           bool       `json:"enabled"`
	PendingDeletion   bool       `json:"pending_deletion"`
	LastConnectedAt   *time.Time `json:"last_connected_at,omitempty"`
	LastDiscoveredAt  *time.Time `json:"last_discovered_at,omitempty"`
}
type DetachResult struct {
	Pending bool        `json:"pending"`
	Deleted bool        `json:"deleted"`
	Server  *ServerView `json:"server,omitempty"`
}
type ServerDetail struct {
	ServerView
	Discovery *DiscoveryView `json:"discovery,omitempty"`
}
type DiscoveryView struct {
	ID        string     `json:"id"`
	Hash      string     `json:"discovery_hash"`
	CreatedAt time.Time  `json:"created_at"`
	Tools     []ToolView `json:"tools"`
}
type ToolView struct {
	RemoteName  string          `json:"remote_name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	SchemaHash  string          `json:"schema_hash"`
}
type ExecutionView struct {
	ExecutionID string          `json:"execution_id"`
	MCPServerID string          `json:"mcp_server_id"`
	ToolName    string          `json:"tool_name"`
	Status      string          `json:"status"`
	DurationMS  int64           `json:"duration_ms"`
	Result      json.RawMessage `json:"result"`
}

func view(m storage.MCPServer, tokens TokenSource) ServerView {
	authSet := false
	if m.AuthEnv != "" && tokens != nil {
		_, authSet = tokens.Token(m.AuthEnv)
	}
	var connected, discovered *time.Time
	if !m.LastConnectedAt.IsZero() {
		t := m.LastConnectedAt
		connected = &t
	}
	if !m.LastDiscoveredAt.IsZero() {
		t := m.LastDiscoveredAt
		discovered = &t
	}
	return ServerView{ID: m.ID, Name: m.Name, Description: m.Description, Endpoint: safeEndpoint(m.Endpoint), Transport: m.Transport, AuthEnv: m.AuthEnv, AuthEnvSet: authSet, ValidationState: m.ValidationState, LatestDiscoveryID: m.LatestDiscoveryID, Enabled: m.Enabled, PendingDeletion: m.PendingDelete, LastConnectedAt: connected, LastDiscoveredAt: discovered}
}
func safeEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	q := u.Query()
	for key := range q {
		q[key] = []string{"[redacted]"}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// writeLookupError maps a missing server to 404 and anything else (a broken
// store) to 500.
func writeLookupError(w http.ResponseWriter, err error, requestID string) {
	if errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		api.WriteError(w, 404, "not_found", "MCP server not found", requestID)
		return
	}
	api.WriteError(w, 500, "internal_error", "MCP server request failed", requestID)
}

func requestID(w http.ResponseWriter, r *http.Request) string {
	id := api.RequestIDFromContext(r.Context())
	if id == "" {
		id = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", id)
	return id
}

func toolViews(tools []storage.MCPTool) []ToolView {
	out := make([]ToolView, 0, len(tools))
	for _, tool := range tools {
		out = append(out, ToolView{RemoteName: tool.RemoteName, Description: tool.Description, InputSchema: json.RawMessage(tool.SchemaJSON), SchemaHash: tool.SchemaHash})
	}
	return out
}
func executionView(result ToolResult) ExecutionView {
	return ExecutionView{ExecutionID: result.ExecutionID, MCPServerID: result.MCPServerID, ToolName: result.ToolName, Status: result.Status, DurationMS: result.Duration.Milliseconds(), Result: result.JSON}
}
