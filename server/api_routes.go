package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"govoice/server/agent"
	"govoice/server/api"
	"govoice/server/storage"
	"govoice/transport/token"
)

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()
	w.Header().Set("X-Request-ID", requestID)
	if !s.management.Enabled() {
		api.WriteError(w, http.StatusServiceUnavailable, "management_unavailable", "management API is not configured", requestID)
		return
	}
	if !s.management.Authenticate(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="management"`)
		api.WriteError(w, http.StatusUnauthorized, "unauthorized", "valid management credential required", requestID)
		return
	}
	switch r.URL.Path {
	case "/api/v1/server-config":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			api.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
			return
		}
		api.WriteJSON(w, http.StatusOK, api.SuccessResponse[api.ServerConfig]{Data: api.ServerConfig{
			Profile: string(s.cfg.Profile), MaxSessions: s.cfg.Limits.MaxSessions,
			MaxCallDurationSeconds:       int(s.cfg.Limits.MaxCallDuration.Seconds()),
			VoiceTokenMaxLifetimeSeconds: int(s.effectiveVoiceTokenLifetime().Seconds()),
		}})
	case "/api/v1/operations/status":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			api.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
			return
		}
		status := s.getOperationsStatus(r.Context())
		api.WriteJSON(w, http.StatusOK, api.SuccessResponse[api.OperationsStatus]{Data: status})
	case "/api/v1/voice-tokens":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			api.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
			return
		}
		var input api.VoiceTokenRequest
		if err := api.DecodeJSON(r, &input); err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
			return
		}
		if input.Principal == "" || len(input.Principal) > 128 || input.AgentID == "" || len(input.AgentID) > 128 {
			api.WriteError(w, http.StatusBadRequest, "invalid_request", "principal and agent_id must be 1 to 128 characters", requestID)
			return
		}
		if s.agents == nil {
			api.WriteError(w, http.StatusServiceUnavailable, "agent_management_unavailable", "agent storage is unavailable", requestID)
			return
		}
		target, err := s.agents.Get(r.Context(), input.AgentID)
		if errors.Is(err, sql.ErrNoRows) {
			api.WriteError(w, http.StatusNotFound, "not_found", "agent not found", requestID)
			return
		}
		if err != nil {
			api.WriteError(w, http.StatusInternalServerError, "internal_error", "agent could not be loaded", requestID)
			return
		}
		if !agentCallable(target.Readiness.State) {
			api.WriteJSON(w, http.StatusConflict, api.ErrorResponse{Error: api.Error{Code: "agent_not_ready", Message: "agent is not ready to take calls", RequestID: requestID, Details: target.Readiness}})
			return
		}
		now := time.Now()
		lifetime := s.effectiveVoiceTokenLifetime()
		claims := token.Claims{RegisteredClaims: jwt.RegisteredClaims{
			Issuer: s.cfg.Token.Issuer, Audience: jwt.ClaimStrings{s.cfg.Token.Audience},
			Subject: input.Principal, ID: storage.NewID(), // becomes the session id
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(lifetime)),
		}, Principal: input.Principal, AgentID: target.ID, Scope: "voice:connect", IssuerEpoch: s.voiceEpoch}
		jwtToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		if s.voiceKeyID != "" {
			jwtToken.Header["kid"] = s.voiceKeyID
		}
		signed, err := jwtToken.SignedString(s.voiceSignKey)
		if err != nil {
			api.WriteError(w, http.StatusInternalServerError, "internal_error", "token could not be issued", requestID)
			return
		}
		api.WriteJSON(w, http.StatusCreated, api.SuccessResponse[api.VoiceTokenResponse]{Data: api.VoiceTokenResponse{
			Token: signed, ExpiresAt: claims.ExpiresAt.Time, Scope: "voice:connect",
		}})
	default:
		if r.URL.Path == "/api/v1/agents" || strings.HasPrefix(r.URL.Path, "/api/v1/agents/") {
			if s.agentManagementAPI == nil {
				api.WriteError(w, http.StatusServiceUnavailable, "agent_management_unavailable", "agent management storage is unavailable", requestID)
				return
			}
			r = r.WithContext(api.WithRequestID(r.Context(), requestID))
			s.agentManagementAPI.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/v1/providers" || strings.HasPrefix(r.URL.Path, "/api/v1/providers/") {
			if s.providerManagementAPI == nil {
				api.WriteError(w, http.StatusServiceUnavailable, "provider_management_unavailable", "provider management storage is unavailable", requestID)
				return
			}
			r = r.WithContext(api.WithRequestID(r.Context(), requestID))
			s.providerManagementAPI.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/v1/mcp-servers" || strings.HasPrefix(r.URL.Path, "/api/v1/mcp-servers/") {
			if s.mcpManagementAPI == nil {
				api.WriteError(w, http.StatusServiceUnavailable, "mcp_management_unavailable", "MCP management storage is unavailable", requestID)
				return
			}
			r = r.WithContext(api.WithRequestID(r.Context(), requestID))
			s.mcpManagementAPI.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/v1/sessions" || strings.HasPrefix(r.URL.Path, "/api/v1/sessions/") {
			if s.sessionManagementAPI == nil {
				api.WriteError(w, http.StatusServiceUnavailable, "session_management_unavailable", "session management storage is unavailable", requestID)
				return
			}
			r = r.WithContext(api.WithRequestID(r.Context(), requestID))
			s.sessionManagementAPI.ServeHTTP(w, r)
			return
		}
		api.WriteError(w, http.StatusNotFound, "not_found", "route not found", requestID)
	}
}

func (s *Server) effectiveVoiceTokenLifetime() time.Duration {
	lifetime := s.cfg.Token.MaxLifetime
	if lifetime <= 0 {
		lifetime = 5 * time.Minute
	}
	return min(time.Minute, lifetime).Truncate(time.Second)
}

// agentCallable reports whether an agent in this readiness state may run calls.
func agentCallable(state string) bool {
	return state == agent.ReadinessReady || state == agent.ReadinessWithToolWarning
}
