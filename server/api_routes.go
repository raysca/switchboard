package server

import (
	"cmp"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"govoice/server/api"
	"govoice/server/flow"
	"govoice/server/storage"
	"govoice/transport/token"
)

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("Cache-Control", "no-store")
	if !s.management.Enabled() {
		api.WriteError(w, http.StatusServiceUnavailable, "management_unavailable", "management API is not configured", requestID)
		return
	}
	// The limiter runs after Authenticate: it only converts repeated 401s into 429s
	// and counts a metric; it does not stop guessing. A correct key is never
	// throttled; the 32+ byte key makes guessing infeasible.
	if !s.management.Authenticate(r) {
		s.metrics.ManagementAuthFailuresTotal.Add(1)
		if !s.authFails.AllowN(s.authNow(), 1) {
			s.metrics.ManagementAuthThrottledTotal.Add(1)
			w.Header().Set("Retry-After", "6")
			api.WriteError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed attempts; try again shortly", requestID)
			return
		}
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
			ClientIP:                     s.clientIP(r), Secure: s.secure(r),
			Audio: api.ServerAudio{Backgrounds: append([]string{}, policyFrom(s.cfg).sounds...), Cues: append([]string{}, policyFrom(s.cfg).cues...), AllowFlowBackground: s.cfg.Audio.AllowFlowBackground, BackgroundDefault: s.cfg.Audio.BackgroundDefault},
			Idle:  api.ServerIdle{UserTimeoutMS: int(s.cfg.Idle.UserTimeout.Milliseconds()), MaxPrompts: s.cfg.Idle.MaxPrompts, PromptMode: cmp.Or(s.cfg.Idle.PromptMode, "llm")},
			Turn: api.ServerTurn{Manager: s.cfg.Turn.Manager, MinWords: s.cfg.Turn.MinWords, Mute: append([]string{}, s.cfg.Turn.Mute...),
				MaxMuteMS: int(s.cfg.Turn.effectiveMaxMute().Milliseconds()), SpeechTimeoutMS: int(s.cfg.Turn.effectiveSpeechTimeout().Milliseconds()),
				EndOfTurn: s.cfg.Turn.endOfTurn(), SmartTurnAvailable: s.cfg.LoadsSmartTurn(), SmartHoldMS: int(s.cfg.Turn.Analyzer.Hold.Milliseconds())},
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
		if input.Principal == "" || len(input.Principal) > 128 || input.FlowID == "" || len(input.FlowID) > 128 {
			api.WriteError(w, http.StatusBadRequest, "invalid_request", "principal and flow_id must be 1 to 128 characters", requestID)
			return
		}
		if s.flows == nil {
			api.WriteError(w, http.StatusServiceUnavailable, "flow_management_unavailable", "flow storage is unavailable", requestID)
			return
		}
		target, err := s.flows.Get(r.Context(), input.FlowID)
		if errors.Is(err, sql.ErrNoRows) {
			api.WriteError(w, http.StatusNotFound, "not_found", "flow not found", requestID)
			return
		}
		if err != nil {
			api.WriteError(w, http.StatusInternalServerError, "internal_error", "flow could not be loaded", requestID)
			return
		}
		if !flowCallable(target.Readiness.State) {
			api.WriteJSON(w, http.StatusConflict, api.ErrorResponse{Error: api.Error{Code: "flow_not_ready", Message: "flow is not ready to take calls", RequestID: requestID, Details: target.Readiness}})
			return
		}
		now := time.Now()
		lifetime := s.effectiveVoiceTokenLifetime()
		tokenID := storage.NewID() // becomes the session id
		if input.Draft != nil {
			snapshot, readiness, err := s.flows.ResolveDraft(r.Context(), target.ID, input.Draft.Config, input.Draft.MCPTools)
			if err != nil {
				s.writeDraftError(w, err, requestID)
				return
			}
			if !flowCallable(readiness.State) {
				api.WriteJSON(w, http.StatusConflict, api.ErrorResponse{Error: api.Error{Code: "flow_not_ready", Message: "the draft is not ready to take calls", RequestID: requestID, Details: readiness}})
				return
			}
			if err := s.drafts.put(tokenID, draftCall{snapshot: snapshot, expires: now.Add(lifetime)}, now); err != nil {
				api.WriteError(w, http.StatusTooManyRequests, "too_many_drafts", err.Error(), requestID)
				return
			}
		}
		claims := token.Claims{RegisteredClaims: jwt.RegisteredClaims{
			Issuer: s.cfg.Token.Issuer, Audience: jwt.ClaimStrings{s.cfg.Token.Audience},
			Subject: input.Principal, ID: tokenID,
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(lifetime)),
		}, Principal: input.Principal, FlowID: target.ID, Scope: "voice:connect", IssuerEpoch: s.voiceEpoch}
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
		// The live-call routes sit under /api/v1/sessions, so they and demo are matched before the prefix table.
		if r.URL.Path == "/api/v1/demo" || strings.HasPrefix(r.URL.Path, "/api/v1/demo/") {
			s.handleDemo(w, r, requestID)
			return
		}
		if id, action, ok := liveCallRoute(r.URL.Path); ok {
			s.handleLiveCall(w, r, requestID, id, action)
			return
		}
		// A table, not http.ServeMux: ServeMux would add path cleaning and redirects this API never had.
		for _, p := range [...]struct {
			prefix, code, message string
			handler               http.Handler
		}{
			{"/api/v1/flows", "flow_management_unavailable", "flow management storage is unavailable", s.flowManagementAPI},
			{"/api/v1/providers", "provider_management_unavailable", "provider management storage is unavailable", s.providerManagementAPI},
			{"/api/v1/mcp-servers", "mcp_management_unavailable", "MCP management storage is unavailable", s.mcpManagementAPI},
			{"/api/v1/sessions", "session_management_unavailable", "session management storage is unavailable", s.sessionManagementAPI},
		} {
			if r.URL.Path != p.prefix && !strings.HasPrefix(r.URL.Path, p.prefix+"/") {
				continue
			}
			if p.handler == nil {
				api.WriteError(w, http.StatusServiceUnavailable, p.code, p.message, requestID)
				return
			}
			p.handler.ServeHTTP(w, r.WithContext(api.WithRequestID(r.Context(), requestID)))
			return
		}
		api.WriteError(w, http.StatusNotFound, "not_found", "route not found", requestID)
	}
}

// effectiveVoiceTokenLifetime is how long an issued voice token is valid:
// token.max_lifetime (default 1 m), in whole seconds so exp-iat stays within
// the validator's ceiling, which is the same setting.
func (s *Server) effectiveVoiceTokenLifetime() time.Duration {
	return voiceTokenLifetime(s.cfg)
}

// voiceTokenLifetime is the one place a zero token.max_lifetime becomes the default,
// shared by the minting side and the validator.
func voiceTokenLifetime(cfg *Config) time.Duration {
	lifetime := cfg.Token.MaxLifetime
	if lifetime <= 0 {
		lifetime = time.Minute
	}
	return lifetime.Truncate(time.Second)
}

// flowCallable reports whether a flow in this readiness state may run calls.
func flowCallable(state string) bool {
	return state == flow.ReadinessReady || state == flow.ReadinessWithToolWarning
}

// writeDraftError maps a draft's validation failure the way a publish maps its own.
func (s *Server) writeDraftError(w http.ResponseWriter, err error, requestID string) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		api.WriteError(w, http.StatusNotFound, "not_found", "flow not found", requestID)
	case errors.Is(err, flow.ErrNotAttached):
		api.WriteError(w, http.StatusBadRequest, "flow_not_attached", err.Error(), requestID)
	case errors.Is(err, flow.ErrInvalidConfig), errors.Is(err, storage.ErrInvalidToolSelection):
		api.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
	default:
		api.WriteError(w, http.StatusInternalServerError, "internal_error", "the draft could not be checked", requestID)
	}
}
