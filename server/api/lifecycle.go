package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"govoice/server/storage"
)

// ErrRecordingsNotRemoved: the stored rows of a call are gone but a recording file could not be removed. Erasing the
// same id again retries the files.
var ErrRecordingsNotRemoved = errors.New("recording files could not be removed")

// Erasure is what erasing one call removed.
type Erasure struct {
	Turns             int64 `json:"turns"`
	ToolCalls         int64 `json:"tool_calls"`
	Spans             int64 `json:"spans"`
	Events            int64 `json:"events"`
	RecordingsRemoved int   `json:"recordings_removed"`
}

// CallerErasure is the outcome of one erase-by-caller request. A client repeats the request until Erased is 0.
type CallerErasure struct {
	Erased    int `json:"erased"`
	Held      int `json:"held"`
	Active    int `json:"active"`
	Remaining int `json:"remaining"`
	Erasure
	// RecordingsFailed lists the erased calls whose recording files could not be removed; erase each id again to retry.
	RecordingsFailed []string `json:"recordings_failed,omitempty"`
}

// SessionLifecycle holds, releases and erases stored calls. The handler maps its errors: sql.ErrNoRows is 404,
// storage.ErrSessionHeld and storage.ErrSessionActive are 409, ErrRecordingsNotRemoved is 500.
type SessionLifecycle interface {
	Hold(ctx context.Context, id, reason, requestID string) error
	Release(ctx context.Context, id, requestID string) error
	Erase(ctx context.Context, id, requestID string) (Erasure, error)
	EraseCaller(ctx context.Context, label, requestID string, dryRun bool) (CallerErasure, error)
}

type holdBody struct {
	Reason string `json:"reason"`
}

type eraseCallerBody struct {
	Caller        string `json:"caller"`
	ConfirmCaller string `json:"confirm_caller"`
	DryRun        bool   `json:"dry_run"`
}

func (h tracesHandler) sessionNotFound(w http.ResponseWriter, err error, requestID string) bool {
	if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "UUIDv7") {
		WriteError(w, http.StatusNotFound, "not_found", "session not found", requestID)
		return true
	}
	return false
}

// hold serves PUT and DELETE /sessions/{id}/hold. Both are idempotent.
func (h tracesHandler) hold(w http.ResponseWriter, r *http.Request, id, requestID string) {
	var err error
	switch r.Method {
	case http.MethodPut:
		var body holdBody
		if r.ContentLength != 0 {
			if err := DecodeJSON(r, &body); err != nil {
				WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
				return
			}
		}
		body.Reason = strings.TrimSpace(body.Reason)
		if len(body.Reason) > storage.MaxHoldReason {
			WriteError(w, http.StatusBadRequest, "invalid_request", "reason is too long", requestID)
			return
		}
		err = h.lifecycle.Hold(r.Context(), id, body.Reason, requestID)
	case http.MethodDelete:
		err = h.lifecycle.Release(r.Context(), id, requestID)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
		return
	}
	if err != nil {
		if !h.sessionNotFound(w, err, requestID) {
			WriteError(w, http.StatusInternalServerError, "internal_error", "failed to change the hold", requestID)
		}
		return
	}
	rec, err := h.sessions.Get(r.Context(), id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to get session", requestID)
		return
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[SessionDTO]{Data: toSessionDTO(rec)})
}

// erase serves DELETE /sessions/{id}.
func (h tracesHandler) erase(w http.ResponseWriter, r *http.Request, id, requestID string) {
	out, err := h.lifecycle.Erase(r.Context(), id, requestID)
	switch {
	case err == nil:
		WriteJSON(w, http.StatusOK, SuccessResponse[Erasure]{Data: out})
	case errors.Is(err, storage.ErrSessionHeld):
		WriteError(w, http.StatusConflict, "session_held", "the call is on hold: release it before erasing it", requestID)
	case errors.Is(err, storage.ErrSessionActive):
		WriteError(w, http.StatusConflict, "session_active", "the call has not ended", requestID)
	case errors.Is(err, ErrRecordingsNotRemoved):
		WriteError(w, http.StatusInternalServerError, "recordings_not_removed", "the call's rows are erased but a recording file could not be removed: erase the same id again to retry", requestID)
	default:
		if !h.sessionNotFound(w, err, requestID) {
			WriteError(w, http.StatusInternalServerError, "internal_error", "failed to erase the call", requestID)
		}
	}
}

// eraseCaller serves POST /sessions/erase.
func (h tracesHandler) eraseCaller(w http.ResponseWriter, r *http.Request, requestID string) {
	var body eraseCallerBody
	if err := DecodeJSON(r, &body); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
		return
	}
	switch {
	case strings.TrimSpace(body.Caller) == "":
		WriteError(w, http.StatusBadRequest, "invalid_request", "caller is required", requestID)
	case len(body.Caller) > storage.MaxCallerLabel:
		WriteError(w, http.StatusBadRequest, "invalid_request", "caller is too long", requestID)
	case body.ConfirmCaller != body.Caller:
		WriteError(w, http.StatusBadRequest, "invalid_request", "confirm_caller must repeat caller exactly", requestID)
	default:
		out, err := h.lifecycle.EraseCaller(r.Context(), body.Caller, requestID, body.DryRun)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", "failed to erase the caller's calls", requestID)
			return
		}
		WriteJSON(w, http.StatusOK, SuccessResponse[CallerErasure]{Data: out})
	}
}
