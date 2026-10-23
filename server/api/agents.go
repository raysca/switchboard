package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"govoice/server/agent"
	"govoice/server/storage"
)

type AgentService interface {
	Create(context.Context, agent.CreateRequest) (agent.Agent, error)
	Get(context.Context, string) (agent.Agent, error)
	List(context.Context, int, int) ([]agent.Agent, error)
	Update(context.Context, string, agent.UpdateRequest) (agent.Agent, error)
	Archive(context.Context, string) error
	Duplicate(context.Context, string, string) (agent.Agent, error)
	VersionByNumber(context.Context, string, int) (agent.Version, error)
}

// NewFlowsHandler constructs the authenticated management route handler for
// persistent flows. Mount it under /api/v1/ on the private admin listener.
func NewAgentsHandler(service AgentService) http.Handler {
	return agentsHandler{service: service}
}

type agentsHandler struct{ service AgentService }

func (h agentsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := RequestID(w, r)
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/agents"), "/")
	if r.URL.Path != "/api/v1/agents" && !strings.HasPrefix(r.URL.Path, "/api/v1/agents/") {
		WriteError(w, http.StatusNotFound, "not_found", "route not found", requestID)
		return
	}
	if path == "" {
		h.collection(w, r, requestID)
		return
	}
	parts := strings.Split(path, "/")

	if len(parts) == 1 {
		h.item(w, r, parts[0], requestID)
		return
	}
	if len(parts) == 2 && parts[1] == "archive" {
		if r.Method != http.MethodPost {
			agentMethodNotAllowed(w, requestID, "POST")
			return
		}
		if err := h.service.Archive(r.Context(), parts[0]); err != nil {
			h.writeServiceError(w, err, requestID)
			return
		}
		WriteJSON(w, http.StatusOK, SuccessResponse[map[string]bool]{Data: map[string]bool{"archived": true}})
		return
	}
	if len(parts) == 3 && parts[1] == "versions" {
		h.version(w, r, parts[0], parts[2], requestID)
		return
	}
	if len(parts) == 2 && parts[1] == "duplicate" {
		h.duplicate(w, r, parts[0], requestID)
		return
	}
	WriteError(w, http.StatusNotFound, "not_found", "route not found", requestID)
}

func (h agentsHandler) collection(w http.ResponseWriter, r *http.Request, requestID string) {
	switch r.Method {
	case http.MethodGet:
		page, err := ParsePagination(r)
		if err != nil {
			WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
			return
		}
		items, err := h.service.List(r.Context(), page.Limit, page.Offset)
		if err != nil {
			h.writeServiceError(w, err, requestID)
			return
		}
		WriteJSON(w, http.StatusOK, SuccessResponse[[]agent.Agent]{Data: items})
	case http.MethodPost:
		var req agent.CreateRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
			return
		}
		created, err := h.service.Create(r.Context(), req)
		if err != nil {
			h.writeServiceError(w, err, requestID)
			return
		}
		WriteJSON(w, http.StatusCreated, SuccessResponse[agent.Agent]{Data: created})
	default:
		agentMethodNotAllowed(w, requestID, "GET, POST")
	}
}

// version serves GET /api/v1/agents/{id}/versions/{number}: one earlier publish, read-only.
func (h agentsHandler) version(w http.ResponseWriter, r *http.Request, id, number, requestID string) {
	if r.Method != http.MethodGet {
		agentMethodNotAllowed(w, requestID, "GET")
		return
	}
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 {
		WriteError(w, http.StatusBadRequest, "invalid_request", "version must be a positive number", requestID)
		return
	}
	v, err := h.service.VersionByNumber(r.Context(), id, n)
	if err != nil {
		h.writeServiceError(w, err, requestID)
		return
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[agent.Version]{Data: v})
}

func (h agentsHandler) item(w http.ResponseWriter, r *http.Request, id, requestID string) {
	switch r.Method {
	case http.MethodGet:
		item, err := h.service.Get(r.Context(), id)
		if err != nil {
			h.writeServiceError(w, err, requestID)
			return
		}
		WriteJSON(w, http.StatusOK, SuccessResponse[agent.Agent]{Data: item})
	case http.MethodPut, http.MethodPatch:
		var req agent.UpdateRequest
		if err := DecodeJSON(r, &req); err != nil {
			WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
			return
		}
		item, err := h.service.Update(r.Context(), id, req)
		if err != nil {
			h.writeServiceError(w, err, requestID)
			return
		}
		WriteJSON(w, http.StatusOK, SuccessResponse[agent.Agent]{Data: item})
	default:
		agentMethodNotAllowed(w, requestID, "GET, PUT, PATCH")
	}
}

func (h agentsHandler) duplicate(w http.ResponseWriter, r *http.Request, id, requestID string) {
	if r.Method != http.MethodPost {
		agentMethodNotAllowed(w, requestID, "POST")
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if r.ContentLength != 0 {
		if err := DecodeJSON(r, &req); err != nil {
			WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
			return
		}
	}
	item, err := h.service.Duplicate(r.Context(), id, req.Name)
	if err != nil {
		h.writeServiceError(w, err, requestID)
		return
	}
	WriteJSON(w, http.StatusCreated, SuccessResponse[agent.Agent]{Data: item})
}

func agentMethodNotAllowed(w http.ResponseWriter, requestID, allow string) {
	w.Header().Set("Allow", allow)
	WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
}

func (h agentsHandler) writeServiceError(w http.ResponseWriter, err error, requestID string) {
	if errors.Is(err, storage.ErrInvalidToolSelection) {
		WriteError(w, http.StatusBadRequest, "invalid_tool_selection", err.Error(), requestID)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		WriteError(w, http.StatusNotFound, "not_found", "agent not found", requestID)
		return
	}
	if errors.Is(err, agent.ErrNotAttached) {
		WriteError(w, http.StatusBadRequest, "agent_not_attached", err.Error(), requestID)
		return
	}
	if errors.Is(err, agent.ErrInvalidConfig) || strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "unavailable") || strings.Contains(err.Error(), "must be") {
		WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
		return
	}
	WriteError(w, http.StatusInternalServerError, "internal_error", "agent operation failed", requestID)
}
