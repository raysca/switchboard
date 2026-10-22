package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"govoice/server/api"
	"govoice/server/storage"
)

// Handler exposes the MCP management API. It must only be mounted behind the
// server's /api/v1 boundary, which authenticates every request first.
func Handler(service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/mcp-servers", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
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
				writeInvalidServer(w, err, id)
				return
			}
			api.WriteJSON(w, http.StatusCreated, api.SuccessResponse[ServerView]{Data: view(m, service.tokens)})
		default:
			w.Header().Set("Allow", "GET, POST")
			api.WriteError(w, 405, "method_not_allowed", "method is not supported", id)
		}
	})
	mux.HandleFunc("/api/v1/mcp-servers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
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
		if r.Method == http.MethodPatch {
			var input ServerUpdate
			if err := api.DecodeJSON(r, &input); err != nil {
				api.WriteError(w, 400, "invalid_request", err.Error(), id)
				return
			}
			m, err := service.Update(r.Context(), r.PathValue("id"), input)
			switch {
			case errors.Is(err, ErrPendingDeletion):
				api.WriteError(w, 409, "mcp_server_unavailable", "MCP server cannot be edited while deletion is pending", id)
			case ReasonCode(err) != "":
				writeInvalidServer(w, err, id)
			case err != nil:
				writeLookupError(w, err, id)
			default:
				api.WriteJSON(w, 200, api.SuccessResponse[ServerView]{Data: view(m, service.tokens)})
			}
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET, PATCH, DELETE")
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
		id := api.RequestID(w, r)
		serverID := r.PathValue("id")
		if err := service.Disable(r.Context(), serverID); err != nil {
			writeLookupError(w, err, id)
			return
		}
		m, err := service.Get(r.Context(), serverID)
		if err != nil {
			writeLookupError(w, err, id)
			return
		}
		api.WriteJSON(w, 200, api.SuccessResponse[ServerView]{Data: view(m, service.tokens)})
	})
	mux.HandleFunc("POST /api/v1/mcp-servers/{id}/enable", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
		serverID := r.PathValue("id")
		if err := service.Enable(r.Context(), serverID); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				writeLookupError(w, err, id) // the store failed: 500
				return
			}
			// No row updated: the server is missing (404) or its deletion is pending (409).
			if _, gerr := service.Get(r.Context(), serverID); gerr != nil {
				writeLookupError(w, gerr, id)
				return
			}
			api.WriteError(w, 409, "mcp_server_unavailable", "MCP server cannot be enabled while deletion is pending", id)
			return
		}
		m, err := service.Get(r.Context(), serverID)
		if err != nil {
			writeLookupError(w, err, id)
			return
		}
		api.WriteJSON(w, 200, api.SuccessResponse[ServerView]{Data: view(m, service.tokens)})
	})
	mux.HandleFunc("POST /api/v1/mcp-servers/{id}/detach-delete", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
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
		id := api.RequestID(w, r)
		m, err := service.Validate(r.Context(), r.PathValue("id"))
		// Validation failures (unavailable, invalid schema, a cancelled context)
		// are reported in the view; a missing server is 404, a store failure 500.
		if err != nil && !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrInvalidSchema) &&
			!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			writeLookupError(w, err, id)
			return
		}
		api.WriteJSON(w, 200, api.SuccessResponse[ServerView]{Data: view(m, service.tokens)})
	})
	mux.HandleFunc("POST /api/v1/mcp-servers/{id}/tools/{tool}/test", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
		var input struct {
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := api.DecodeJSON(r, &input); err != nil {
			api.WriteError(w, 400, "invalid_request", err.Error(), id)
			return
		}
		result, err := service.Execute(r.Context(), r.PathValue("id"), r.PathValue("tool"), input.Arguments)
		if err != nil {
			category, message := ErrorCategory(err)
			details := map[string]string{"category": category}
			if message != "" {
				details["message"] = message
			}
			fail := func(status int) {
				api.WriteJSON(w, status, api.ErrorResponse{Error: api.Error{Code: "mcp_tool_failed", Message: err.Error(), Details: details, RequestID: id}})
			}
			switch {
			case errors.Is(err, ErrNotFound):
				api.WriteError(w, 404, "mcp_tool_failed", err.Error(), id)
			case category == "tool_error" || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrRemoteFailure):
				fail(502)
			case isMCPSentinel(err):
				fail(400)
			default: // a store failure or other internal error: no internal text
				writeLookupError(w, err, id)
			}
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

// writeInvalidServer answers a refused create or edit with its reason code.
func writeInvalidServer(w http.ResponseWriter, err error, requestID string) {
	api.WriteJSON(w, 400, api.ErrorResponse{Error: api.Error{Code: "invalid_mcp_server", Message: "MCP server configuration is invalid", RequestID: requestID, Details: map[string]string{"reason": ReasonCode(err)}}})
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

// isMCPSentinel reports whether err is one of the package's user-facing errors,
// whose fixed messages are safe to return.
func isMCPSentinel(err error) bool {
	for _, e := range []error{ErrInvalidSchema, ErrInvalidArguments, ErrResultTooLarge, ErrLocalDisabled, ErrAuthenticationFailed,
		ErrInitializeFailed, ErrDiscoveryFailed, ErrConnectionFailed, ErrExecutionFailed} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
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
