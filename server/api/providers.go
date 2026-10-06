package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"govoice/server/credential"
	"govoice/server/provider"
	"govoice/server/storage"
)

type ProviderManagement struct {
	Credentials *credential.Service
	Registry    *provider.Registry
}

type createCredentialRequest struct {
	Provider string `json:"provider"`
	Secret   string `json:"secret"`
}
type updateCredentialRequest struct {
	Secret string `json:"secret"`
}

func (h *ProviderManagement) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := RequestIDFromContext(r.Context())
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", requestID)
	if h == nil || h.Credentials == nil || h.Registry == nil {
		WriteError(w, http.StatusServiceUnavailable, "provider_management_unavailable", "provider management storage is unavailable", requestID)
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/providers"), "/")
	if path == "" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, requestID, http.MethodGet)
			return
		}
		WriteJSON(w, http.StatusOK, SuccessResponse[[]provider.Definition]{Data: h.Registry.List()})
		return
	}
	if path == "credentials" {
		switch r.Method {
		case http.MethodGet:
			items, err := h.Credentials.List(r.Context())
			if err != nil {
				WriteError(w, 500, "internal_error", "credentials could not be listed", requestID)
				return
			}
			WriteJSON(w, 200, SuccessResponse[[]credential.CredentialDTO]{Data: items})
		case http.MethodPost:
			var input createCredentialRequest
			if err := DecodeJSON(r, &input); err != nil {
				WriteError(w, 400, "invalid_request", err.Error(), requestID)
				return
			}
			item, err := h.Credentials.Create(r.Context(), credential.CreateInput{Provider: input.Provider, Secret: input.Secret})
			if err != nil {
				credentialError(w, requestID, err)
				return
			}
			WriteJSON(w, http.StatusCreated, SuccessResponse[credential.CredentialDTO]{Data: item})
		default:
			methodNotAllowed(w, requestID, http.MethodGet+", POST")
		}
		return
	}
	parts := strings.Split(path, "/")
	if parts[0] != "credentials" || len(parts) < 2 || len(parts) > 3 {
		WriteError(w, 404, "not_found", "route not found", requestID)
		return
	}
	id := parts[1]
	if len(parts) == 3 && parts[2] == "rotate" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, requestID, http.MethodPost)
			return
		}
		if err := h.Credentials.Rotate(r.Context(), id); err != nil {
			credentialError(w, requestID, err)
			return
		}
		item, err := h.Credentials.Get(r.Context(), id)
		if err != nil {
			credentialError(w, requestID, err)
			return
		}
		WriteJSON(w, 200, SuccessResponse[credential.CredentialDTO]{Data: item})
		return
	}
	if len(parts) != 2 {
		WriteError(w, 404, "not_found", "route not found", requestID)
		return
	}
	switch r.Method {
	case http.MethodGet:
		item, err := h.Credentials.Get(r.Context(), id)
		if err != nil {
			credentialError(w, requestID, err)
			return
		}
		WriteJSON(w, 200, SuccessResponse[credential.CredentialDTO]{Data: item})
	case http.MethodPut:
		var input updateCredentialRequest
		if err := DecodeJSON(r, &input); err != nil {
			WriteError(w, 400, "invalid_request", err.Error(), requestID)
			return
		}
		item, err := h.Credentials.Update(r.Context(), id, credential.UpdateInput{Secret: input.Secret})
		if err != nil {
			credentialError(w, requestID, err)
			return
		}
		WriteJSON(w, 200, SuccessResponse[credential.CredentialDTO]{Data: item})
	case http.MethodDelete:
		if err := h.Credentials.Delete(r.Context(), id); err != nil {
			var inUse *storage.CredentialInUseError
			if errors.As(err, &inUse) {
				msg := "credential is attached to agents; attach those agents to another credential or archive them first"
				if len(inUse.MCPServers) > 0 {
					msg = "credential is used by MCP servers (" + strings.Join(inUse.MCPServers, ", ") + ") or agents; change or remove them first"
				}
				WriteJSON(w, http.StatusConflict, ErrorResponse{Error: Error{Code: "credential_in_use", Message: msg, RequestID: requestID, Details: inUse.Agents}})
				return
			}
			credentialError(w, requestID, err)
			return
		}
		WriteJSON(w, http.StatusOK, SuccessResponse[map[string]bool]{Data: map[string]bool{"deleted": true}})
	default:
		methodNotAllowed(w, requestID, "GET, PUT, DELETE")
	}
}

func credentialError(w http.ResponseWriter, requestID string, err error) {
	switch {
	case errors.Is(err, credential.ErrInvalidInput):
		WriteError(w, 400, "invalid_request", "credential input is invalid", requestID)
	case errors.Is(err, credential.ErrUnsupportedProvider):
		WriteError(w, 400, "unsupported_provider", "provider is not supported", requestID)
	case errors.Is(err, credential.ErrNotFound):
		WriteError(w, 404, "not_found", "credential not found", requestID)
	case errors.Is(err, credential.ErrValidationUnavailable):
		WriteError(w, 503, "validation_unavailable", "provider validation is unavailable", requestID)
	case errors.Is(err, credential.ErrUnknownKey), errors.Is(err, credential.ErrDecrypt):
		WriteError(w, 500, "credential_unavailable", "credential could not be decrypted", requestID)
	default:
		WriteError(w, 500, "internal_error", "credential operation failed", requestID)
	}
}

func methodNotAllowed(w http.ResponseWriter, requestID, allow string) {
	w.Header().Set("Allow", allow)
	WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
}
