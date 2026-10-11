package server

import (
	"log/slog"
	"net/http"
	"strings"

	"govoice/server/api"
)

// liveCallRoute matches /api/v1/sessions/{id}/context and /api/v1/sessions/{id}/run.
func liveCallRoute(path string) (id, action string, ok bool) {
	rest, found := strings.CutPrefix(path, "/api/v1/sessions/")
	if !found {
		return "", "", false
	}
	id, action, found = strings.Cut(rest, "/")
	if !found || id == "" || (action != "context" && action != "run") {
		return "", "", false
	}
	return id, action, true
}

type acceptedResponse struct {
	Accepted int `json:"accepted"`
}

// handleLiveCall serves operator control of a call running on this instance. The caller has
// already been authenticated as the management client.
func (s *Server) handleLiveCall(w http.ResponseWriter, r *http.Request, requestID, id, action string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		api.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", requestID)
		return
	}
	var req contextRequest
	if action == "context" {
		if err := api.DecodeJSON(r, &req); err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
			return
		}
		if err := req.validate(); err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
			return
		}
	}
	inj, status := s.live.Take(id)
	switch status {
	case liveMissing:
		api.WriteError(w, http.StatusNotFound, "not_found", "no live call with this id on this instance", requestID)
		return
	case liveLimited:
		w.Header().Set("Retry-After", "1")
		api.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests for this call", requestID)
		return
	}
	if e, ok := inj.(interface{ Ended() bool }); ok && e.Ended() {
		api.WriteError(w, http.StatusNotFound, "not_found", "the call has ended", requestID)
		return
	}
	if !inj.Ready() {
		api.WriteError(w, http.StatusConflict, "call_not_ready", "the call is still starting; retry shortly", requestID)
		return
	}
	accepted := 1
	if action == "context" {
		for _, n := range req.Messages {
			inj.Note(n.Content, req.Run)
		}
		accepted = len(req.Messages)
	} else {
		inj.Run()
	}
	s.logger.Info("live call control", slog.String("session", id), slog.String("action", action), slog.Int("count", accepted), slog.Bool("run", req.Run || action == "run"))
	api.WriteJSON(w, http.StatusAccepted, api.SuccessResponse[acceptedResponse]{Data: acceptedResponse{Accepted: accepted}})
}
