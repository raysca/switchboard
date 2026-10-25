package server

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
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
			MCPSyncIntervalSeconds:       int(s.cfg.MCP.SyncInterval.Seconds()),
			ClientIP:                     s.clientIP(r), Secure: s.secure(r),
			Audio: api.ServerAudio{Backgrounds: append([]string{}, policyFrom(s.cfg).sounds...), Cues: append([]string{}, policyFrom(s.cfg).cues...), AllowAgentBackground: s.cfg.Audio.AllowAgentBackground, BackgroundDefault: s.cfg.Audio.BackgroundDefault},
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
		if input.Principal == "" || len(input.Principal) > 128 || input.AgentID == "" || len(input.AgentID) > 128 {
			api.WriteError(w, http.StatusBadRequest, "invalid_request", "principal and agent_id must be 1 to 128 characters", requestID)
			return
		}
		resp, _, err := s.issueVoiceToken(r.Context(), input, false)
		var (
			notReady *notReadyError
			draftErr *draftCheckError
		)
		switch {
		case err == nil:
			api.WriteJSON(w, http.StatusCreated, api.SuccessResponse[api.VoiceTokenResponse]{Data: resp})
		case errors.Is(err, errAgentStoreUnavailable):
			api.WriteError(w, http.StatusServiceUnavailable, "agent_management_unavailable", "agent storage is unavailable", requestID)
		case errors.As(err, &notReady):
			msg := "agent is not ready to take calls"
			if notReady.Draft {
				msg = "the draft is not ready to take calls"
			}
			api.WriteJSON(w, http.StatusConflict, api.ErrorResponse{Error: api.Error{Code: "agent_not_ready", Message: msg, RequestID: requestID, Details: notReady.Readiness}})
		case errors.Is(err, errTooManyDrafts):
			api.WriteError(w, http.StatusTooManyRequests, "too_many_drafts", err.Error(), requestID)
		case errors.As(err, &draftErr):
			s.writeDraftError(w, draftErr.err, requestID)
		case errors.Is(err, sql.ErrNoRows):
			api.WriteError(w, http.StatusNotFound, "not_found", "agent not found", requestID)
		case errors.Is(err, errTokenSign):
			api.WriteError(w, http.StatusInternalServerError, "internal_error", "token could not be issued", requestID)
		default:
			api.WriteError(w, http.StatusInternalServerError, "internal_error", "agent could not be loaded", requestID)
		}
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
		if evalPath(r.URL.Path) { // under /api/v1/agents, so matched before the prefix table
			if s.evalManagementAPI == nil {
				api.WriteError(w, http.StatusServiceUnavailable, "eval_management_unavailable", "eval management storage is unavailable", requestID)
				return
			}
			s.evalManagementAPI.ServeHTTP(w, r.WithContext(api.WithRequestID(r.Context(), requestID)))
			return
		}
		// A table, not http.ServeMux: ServeMux would add path cleaning and redirects this API never had.
		for _, p := range [...]struct {
			prefix, code, message string
			handler               http.Handler
		}{
			{"/api/v1/agents", "agent_management_unavailable", "agent management storage is unavailable", s.agentManagementAPI},
			{"/api/v1/providers", "provider_management_unavailable", "provider management storage is unavailable", s.providerManagementAPI},
			{"/api/v1/mcp-servers", "mcp_management_unavailable", "MCP management storage is unavailable", s.mcpManagementAPI},
			{"/api/v1/http-tools", "http_tools_management_unavailable", "HTTP tools management storage is unavailable", s.httpToolsManagementAPI},
			{"/api/v1/retention", "retention_management_unavailable", "retention status is unavailable", s.retentionManagementAPI},
			{"/api/v1/secrets", "secrets_management_unavailable", "secrets management storage is unavailable", s.secretsManagementAPI},
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

var (
	// errAgentStoreUnavailable: the server runs without agent management, so no token can name an agent.
	errAgentStoreUnavailable = errors.New("agent storage is unavailable")
	errTokenSign             = errors.New("token could not be signed")
)

// draftCheckError carries a draft's validation failure unchanged (same text, same Is/As chain) so the
// handler can tell it from the agent lookup failures and map it as a publish does.
type draftCheckError struct{ err error }

func (e *draftCheckError) Error() string { return e.err.Error() }
func (e *draftCheckError) Unwrap() error { return e.err }

// notReadyError: the agent (or, when Draft, the draft) is not in a state that takes calls.
type notReadyError struct {
	Readiness agent.Readiness
	Draft     bool
}

func (e *notReadyError) Error() string {
	if e.Draft {
		return "the draft is not ready to take calls"
	}
	return "agent is not ready to take calls"
}

// issueVoiceToken signs a voice token for in.Principal on agent in.AgentID and returns it with the
// session id it carries (the token's jti). With in.Draft it also resolves the draft and holds it for
// the call. The HTTP handler and the in-process eval target (no HTTP hop) both issue through it.
// skipVersionReady leaves out the readiness check of the agent's current version, for a draft whose own
// readiness is what counts (an eval draft rewritten to echo runs on an agent whose real providers lack keys).
func (s *Server) issueVoiceToken(ctx context.Context, in api.VoiceTokenRequest, skipVersionReady bool) (api.VoiceTokenResponse, string, error) {
	if s.agents == nil {
		return api.VoiceTokenResponse{}, "", errAgentStoreUnavailable
	}
	target, err := s.agents.Get(ctx, in.AgentID)
	if err != nil {
		return api.VoiceTokenResponse{}, "", err
	}
	if !(skipVersionReady && in.Draft != nil) && !agent.Callable(target.Readiness.State) {
		return api.VoiceTokenResponse{}, "", &notReadyError{Readiness: target.Readiness}
	}
	now := time.Now()
	lifetime := s.effectiveVoiceTokenLifetime()
	tokenID := storage.NewID() // becomes the session id
	if in.Draft != nil {
		snapshot, readiness, err := s.agents.ResolveDraft(ctx, target.ID, in.Draft.Config, in.Draft.MCPTools)
		if err != nil {
			return api.VoiceTokenResponse{}, "", &draftCheckError{err}
		}
		if !agent.Callable(readiness.State) {
			return api.VoiceTokenResponse{}, "", &notReadyError{Readiness: readiness, Draft: true}
		}
		if err := s.drafts.put(tokenID, draftCall{snapshot: snapshot, expires: now.Add(lifetime)}, now); err != nil {
			return api.VoiceTokenResponse{}, "", err
		}
	}
	claims := token.Claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: s.cfg.Token.Issuer, Audience: jwt.ClaimStrings{s.cfg.Token.Audience},
		Subject: in.Principal, ID: tokenID,
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(lifetime)),
	}, Principal: in.Principal, AgentID: target.ID, Scope: "voice:connect", IssuerEpoch: s.voiceEpoch}
	jwtToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	if s.voiceKeyID != "" {
		jwtToken.Header["kid"] = s.voiceKeyID
	}
	signed, err := jwtToken.SignedString(s.voiceSignKey)
	if err != nil {
		return api.VoiceTokenResponse{}, "", fmt.Errorf("%w: %w", errTokenSign, err)
	}
	return api.VoiceTokenResponse{Token: signed, ExpiresAt: claims.ExpiresAt.Time, Scope: "voice:connect"}, tokenID, nil
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

// writeDraftError maps a draft's validation failure the way a publish maps its own.
func (s *Server) writeDraftError(w http.ResponseWriter, err error, requestID string) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		api.WriteError(w, http.StatusNotFound, "not_found", "agent not found", requestID)
	case errors.Is(err, agent.ErrNotAttached):
		api.WriteError(w, http.StatusBadRequest, "agent_not_attached", err.Error(), requestID)
	case errors.Is(err, agent.ErrInvalidConfig), errors.Is(err, storage.ErrInvalidToolSelection):
		api.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
	default:
		api.WriteError(w, http.StatusInternalServerError, "internal_error", "the draft could not be checked", requestID)
	}
}
