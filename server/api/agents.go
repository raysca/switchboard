package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"govoice/server/agent"
)

type AgentService interface {
	Create(context.Context, agent.CreateRequest) (agent.Agent, error)
	Get(context.Context, string) (agent.Agent, error)
	List(context.Context, int, int) ([]agent.Agent, error)
	Update(context.Context, string, agent.UpdateRequest) (agent.Agent, error)
	Archive(context.Context, string) error
	Duplicate(context.Context, string, string) (agent.Agent, error)
	ImportV1(context.Context, string, string, string, agent.V1Pipeline) (agent.Agent, error)
}

// NewAgentsHandler constructs the authenticated management route handler for
// persistent agents. Mount it under /api/v1/ on the private admin listener.
func NewAgentsHandler(service AgentService) http.Handler {
	return agentsHandler{service: service}
}

type agentsHandler struct{ service AgentService }

func (h agentsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := RequestIDFromContext(r.Context())
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", requestID)
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
	if len(parts) == 1 && parts[0] == "import-v1" {
		h.importV1(w, r, requestID)
		return
	}
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

func (h agentsHandler) importV1(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method != http.MethodPost {
		agentMethodNotAllowed(w, requestID, "POST")
		return
	}
	var req struct {
		LegacyName  string           `json:"legacy_name"`
		Name        string           `json:"name"`
		Description string           `json:"description"`
		Pipeline    agent.V1Pipeline `json:"pipeline"`
	}
	if err := DecodeJSON(r, &req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
		return
	}
	item, err := h.service.ImportV1(r.Context(), req.LegacyName, req.Name, req.Description, req.Pipeline)
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
	if errors.Is(err, sql.ErrNoRows) {
		WriteError(w, http.StatusNotFound, "not_found", "agent not found", requestID)
		return
	}
	if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "unavailable") || strings.Contains(err.Error(), "must be") {
		WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
		return
	}
	WriteError(w, http.StatusInternalServerError, "internal_error", "agent operation failed", requestID)
}
