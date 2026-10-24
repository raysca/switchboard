package httptool

import (
	"encoding/json"
	"errors"
	"net/http"

	"govoice/server/api"
	"govoice/server/storage"
)

// Handler exposes the HTTP tools management API. It must only be mounted behind the server's /api/v1 boundary,
// which authenticates every request first.
func Handler(s *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/http-tools", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
		switch r.Method {
		case http.MethodGet:
			views, err := s.List(r.Context())
			if err != nil {
				api.WriteError(w, 500, "internal_error", "HTTP tools unavailable", id)
				return
			}
			api.WriteJSON(w, 200, api.SuccessResponse[[]View]{Data: views})
		case http.MethodPost:
			var d Definition
			if err := api.DecodeJSON(r, &d); err != nil {
				api.WriteError(w, 400, "invalid_request", err.Error(), id)
				return
			}
			v, err := s.Create(r.Context(), d)
			if err != nil {
				writeError(w, err, id)
				return
			}
			api.WriteJSON(w, http.StatusCreated, api.SuccessResponse[View]{Data: v})
		default:
			w.Header().Set("Allow", "GET, POST")
			api.WriteError(w, 405, "method_not_allowed", "method is not supported", id)
		}
	})
	mux.HandleFunc("/api/v1/http-tools/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
		toolID := r.PathValue("id")
		switch r.Method {
		case http.MethodGet:
			v, err := s.Get(r.Context(), toolID)
			respond(w, v, err, id)
		case http.MethodPut:
			var d Definition
			if err := api.DecodeJSON(r, &d); err != nil {
				api.WriteError(w, 400, "invalid_request", err.Error(), id)
				return
			}
			v, err := s.Update(r.Context(), toolID, d)
			respond(w, v, err, id)
		case http.MethodDelete:
			if err := s.Delete(r.Context(), toolID); err != nil {
				writeError(w, err, id)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			api.WriteError(w, 405, "method_not_allowed", "method is not supported", id)
		}
	})
	for action, enabled := range map[string]bool{"enable": true, "disable": false} {
		mux.HandleFunc("POST /api/v1/http-tools/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			v, err := s.SetEnabled(r.Context(), r.PathValue("id"), enabled)
			respond(w, v, err, api.RequestID(w, r))
		})
	}
	mux.HandleFunc("POST /api/v1/http-tools/{id}/test", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
		var body struct {
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := api.DecodeJSON(r, &body); err != nil {
			api.WriteError(w, 400, "invalid_request", err.Error(), id)
			return
		}
		res, err := s.Test(r.Context(), r.PathValue("id"), body.Arguments)
		if err != nil {
			writeError(w, err, id)
			return
		}
		api.WriteJSON(w, 200, api.SuccessResponse[TestResult]{Data: res})
	})
	return mux
}

func respond(w http.ResponseWriter, v View, err error, id string) {
	if err != nil {
		writeError(w, err, id)
		return
	}
	api.WriteJSON(w, 200, api.SuccessResponse[View]{Data: v})
}

func writeError(w http.ResponseWriter, err error, id string) {
	var referenced *ReferencedError
	switch {
	case errors.Is(err, ErrInvalid):
		api.WriteJSON(w, 400, api.ErrorResponse{Error: api.Error{Code: "invalid_http_tool", Message: "the tool definition is not valid", RequestID: id, Details: map[string]string{"reason": Reason(err)}}})
	case errors.Is(err, ErrNotFound):
		api.WriteError(w, 404, "not_found", "HTTP tool not found", id)
	case errors.Is(err, storage.ErrHTTPToolNameTaken):
		api.WriteError(w, 409, "http_tool_name_taken", "an HTTP tool with that name exists", id)
	case errors.As(err, &referenced):
		api.WriteJSON(w, 409, api.ErrorResponse{Error: api.Error{Code: "http_tool_in_use", Message: "the tool is pinned by current agents", RequestID: id, Details: referenced.Agents}})
	default:
		api.WriteError(w, 500, "internal_error", "HTTP tool request failed", id)
	}
}
