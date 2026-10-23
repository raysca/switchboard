// Package api defines versioned management wire values independent of storage.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"govoice/server/agent"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const MaxBodyBytes = 64 << 10

type requestIDContextKey struct{}

// WithRequestID binds a server-generated request identifier to an API call.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDContextKey{}, id)
}

// RequestIDFromContext retrieves the trusted request identifier installed by
// the outer management boundary. It intentionally never reads HTTP headers.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}

// RequestID is the call's trusted request identifier (a new UUID when the
// outer boundary installed none); it is also set as X-Request-ID.
func RequestID(w http.ResponseWriter, r *http.Request) string {
	id := RequestIDFromContext(r.Context())
	if id == "" {
		id = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", id)
	return id
}

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Details   any    `json:"details,omitempty"`
	RequestID string `json:"request_id"`
}

type ErrorResponse struct {
	Error Error `json:"error"`
}
type SuccessResponse[T any] struct {
	Data T `json:"data"`
}

type Pagination struct {
	Limit  int
	Offset int
}

type VoiceTokenRequest struct {
	Principal string `json:"principal"`
	AgentID   string `json:"agent_id"`
	// Draft, when set, makes a token for a call that runs this config instead of
	// the agent's published version (a test call). It is validated like a publish.
	Draft *DraftRequest `json:"draft,omitempty"`
}

// DraftRequest is an unpublished agent config to test.
type DraftRequest struct {
	Config json.RawMessage `json:"config"`
	// MCPTools is the draft's own tool selection; absent, the agent's current selection applies.
	MCPTools *[]agent.ToolSelection `json:"mcp_tools,omitempty"`
}
type VoiceTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Scope     string    `json:"scope"`
}
type ServerConfig struct {
	Profile                      string `json:"profile"`
	MaxSessions                  int    `json:"max_sessions"`
	MaxCallDurationSeconds       int    `json:"max_call_duration_seconds"`
	VoiceTokenMaxLifetimeSeconds int    `json:"voice_token_max_lifetime_seconds"`
	MCPSyncIntervalSeconds       int    `json:"mcp_sync_interval_seconds"` // 0: automatic MCP syncs are off
	ClientIP                     string `json:"client_ip"`
	Secure                       bool   `json:"secure"`
	// Audio and Idle tell the editor what an agent can choose from and what a call does when the agent says
	// nothing. Names and numbers only: never a file path or prompt text.
	Audio ServerAudio `json:"audio"`
	Idle  ServerIdle  `json:"idle"`
	Turn  ServerTurn  `json:"turn"`
}

// ServerTurn is the server's effective turn-taking: what a call does when its agent says nothing. Numbers
// are never a raw 0 for "default"; Mute is always a list.
type ServerTurn struct {
	Manager         bool     `json:"manager"`
	MinWords        int      `json:"min_words"`
	Mute            []string `json:"mute"`
	MaxMuteMS       int      `json:"max_mute_ms"`
	SpeechTimeoutMS int      `json:"speech_timeout_ms"`
	// EndOfTurn is the server's detector: smart, eager or default. SmartTurnAvailable is whether an agent
	// may choose smart; SmartHoldMS is the hold a smart call uses.
	EndOfTurn          string `json:"end_of_turn"`
	SmartTurnAvailable bool   `json:"smart_turn_available"`
	SmartHoldMS        int    `json:"smart_hold_ms"`
}

type ServerAudio struct {
	Backgrounds          []string `json:"backgrounds"`
	Cues                 []string `json:"cues"` // sound names an agent's handoff_cue may use
	AllowAgentBackground bool     `json:"allow_agent_background"`
	// BackgroundDefault is the sound every call plays unless its agent picks another or none ("" = none).
	BackgroundDefault string `json:"background_default"`
}

// ServerIdle is the server's quiet-caller default (0 timeout: off).
type ServerIdle struct {
	UserTimeoutMS int    `json:"user_timeout_ms"`
	MaxPrompts    int    `json:"max_prompts"`
	PromptMode    string `json:"prompt_mode"`
}

func ParsePagination(r *http.Request) (Pagination, error) {
	p := Pagination{Limit: 50}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return p, errors.New("limit must be between 1 and 100")
		}
		p.Limit = n
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return p, errors.New("offset must be nonnegative")
		}
		p.Offset = n
	}
	return p, nil
}

func DecodeJSON(r *http.Request, dst any) error {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if mediaType != "application/json" {
		return errors.New("content type must be application/json")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return errors.New("invalid JSON body")
	}
	if len(body) > MaxBodyBytes {
		return errors.New("JSON body exceeds size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// encoding/json reports `json: unknown field "x"` as a plain error with no type to match.
		if q, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			if name, uerr := strconv.Unquote(q); uerr == nil {
				if len(name) > 64 {
					name = name[:64] + "..."
				}
				return fmt.Errorf("unknown field %q", name)
			}
		}
		return errors.New("invalid JSON body")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("body must contain one JSON value")
	}
	return nil
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func WriteError(w http.ResponseWriter, status int, code, message, requestID string) {
	WriteJSON(w, status, ErrorResponse{Error: Error{Code: code, Message: message, RequestID: requestID}})
}
