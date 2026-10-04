package server

import (
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"govoice/server/api"
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
		if input.Principal == "" || len(input.Principal) > 128 || input.Pipeline == "" || len(input.Pipeline) > 128 {
			api.WriteError(w, http.StatusBadRequest, "invalid_request", "principal and pipeline must be 1 to 128 characters", requestID)
			return
		}
		if _, ok := s.cfg.Pipelines[input.Pipeline]; !ok {
			api.WriteError(w, http.StatusBadRequest, "invalid_request", "unknown pipeline", requestID)
			return
		}
		now := time.Now()
		lifetime := s.effectiveVoiceTokenLifetime()
		claims := token.Claims{RegisteredClaims: jwt.RegisteredClaims{
			Issuer: s.cfg.Token.Issuer, Audience: jwt.ClaimStrings{s.cfg.Token.Audience},
			Subject: input.Principal, ID: uuid.NewString(),
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(lifetime)),
		}, Principal: input.Principal, Pipeline: input.Pipeline, Scope: "voice:connect"}
		jwtToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		if s.cfg.Token.KeyID != "" {
			jwtToken.Header["kid"] = s.cfg.Token.KeyID
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
		api.WriteError(w, http.StatusNotFound, "not_found", "route not found", requestID)
	}
}

func (s *Server) effectiveVoiceTokenLifetime() time.Duration {
	lifetime := s.cfg.Token.MaxLifetime
	if lifetime <= 0 {
		lifetime = 5 * time.Minute
	}
	return min(time.Minute, lifetime)
}
