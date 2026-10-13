// Package api defines versioned management wire values independent of storage.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

type VoiceTokenRequest struct {
	Principal string `json:"principal"`
	FlowID    string `json:"flow_id"`
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
	ClientIP                     string `json:"client_ip"`
	Secure                       bool   `json:"secure"`
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
