package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"govoice/server/storage"
)

// SessionRepository defines the session operations needed by traces/sessions HTTP APIs.
type SessionRepository interface {
	Get(ctx context.Context, id string) (storage.SessionRecord, error)
	List(ctx context.Context, filter storage.SessionFilter) ([]storage.SessionRecord, error)
	Turns(ctx context.Context, sessionID string) ([]storage.TurnRecord, error)
	TurnLatencies(ctx context.Context, ids []string) (map[string][]string, error)
}

// TraceRepository defines the trace queries needed by traces/sessions HTTP APIs.
type TraceRepository interface {
	Spans(ctx context.Context, sessionID, turnID string) ([]storage.TraceSpan, error)
	Events(ctx context.Context, sessionID, turnID string) ([]storage.TraceEvent, error)
	ToolCalls(ctx context.Context, sessionID, turnID string) ([]storage.ToolCallRecord, error)
}

// SessionDTO represents a persisted voice session for the management API.
type SessionDTO struct {
	ID            string     `json:"id"`
	FlowID        string     `json:"flow_id"`
	FlowVersionID string     `json:"flow_version_id"`
	ConfigHash    string     `json:"config_hash"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	Status        string     `json:"status"`
	TurnCount     int        `json:"turn_count"`
	SafeSummary   string     `json:"safe_summary"`
	Draft         bool       `json:"draft"` // a test call on an unpublished config
	// Caller is the label the integration chose for the call (the voice token's principal).
	Caller string `json:"caller"`
	// EndReason is why the call ended (empty while it is live): caller_hung_up, silent_caller,
	// agent_stuck, ended_by_flow, time_limit, connection_lost, server_shutdown, error, setup_failed
	// or server_restart.
	EndReason string `json:"end_reason"`
	// MedianReplyMS is the median reply time over the call's measured turns; absent when none was measured.
	MedianReplyMS   *int             `json:"median_reply_ms,omitempty"`
	ProviderStack   json.RawMessage  `json:"provider_stack,omitempty"`
	RuntimeSnapshot json.RawMessage  `json:"runtime_snapshot,omitempty"`
	RuntimeTools    []RuntimeToolDTO `json:"runtime_tools,omitempty"`
}

// RuntimeToolDTO is an MCP tool captured for a session, in the API's shape.
type RuntimeToolDTO struct {
	MCPServerID string `json:"mcp_server_id"`
	RemoteName  string `json:"remote_name"`
	SchemaHash  string `json:"schema_hash"`
	Required    bool   `json:"required"`
}

// TurnDTO represents a session turn.
type TurnDTO struct {
	ID            string          `json:"id"`
	SessionID     string          `json:"session_id"`
	Ordinal       int             `json:"ordinal"`
	StartedAt     time.Time       `json:"started_at"`
	EndedAt       *time.Time      `json:"ended_at,omitempty"`
	Transcript    string          `json:"transcript,omitempty"`
	AssistantText string          `json:"assistant_text,omitempty"`
	Interrupted   bool            `json:"interrupted"`
	Latencies     json.RawMessage `json:"latencies,omitempty"`
	Tools         json.RawMessage `json:"tools,omitempty"`
}

// SessionTracesDTO aggregates trace spans, events, and tool calls for timeline rendering.
type SessionTracesDTO struct {
	SessionID string          `json:"session_id"`
	Spans     []TraceSpanDTO  `json:"spans"`
	Events    []TraceEventDTO `json:"events"`
	ToolCalls []ToolCallDTO   `json:"tool_calls"`
}

// TraceSpanDTO represents a persisted trace span.
type TraceSpanDTO struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"session_id,omitempty"`
	TurnID     string          `json:"turn_id,omitempty"`
	ParentID   string          `json:"parent_id,omitempty"`
	Name       string          `json:"name"`
	StartedAt  time.Time       `json:"started_at"`
	EndedAt    *time.Time      `json:"ended_at,omitempty"`
	Attributes json.RawMessage `json:"attributes,omitempty"`
}

// TraceEventDTO represents an individual trace event.
type TraceEventDTO struct {
	ID         string          `json:"id"`
	SpanID     string          `json:"span_id,omitempty"`
	SessionID  string          `json:"session_id,omitempty"`
	TurnID     string          `json:"turn_id,omitempty"`
	Name       string          `json:"name"`
	OccurredAt time.Time       `json:"occurred_at"`
	Attributes json.RawMessage `json:"attributes,omitempty"`
}

// ToolCallDTO represents a recorded tool call.
type ToolCallDTO struct {
	ID               string     `json:"id"`
	SessionID        string     `json:"session_id"`
	TurnID           string     `json:"turn_id,omitempty"`
	MCPServerID      string     `json:"mcp_server_id,omitempty"`
	RemoteName       string     `json:"remote_name"`
	SchemaHash       string     `json:"schema_hash"`
	StartedAt        time.Time  `json:"started_at"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	Status           string     `json:"status"`
	ArgumentsSummary string     `json:"arguments_summary,omitempty"`
	ResultSummary    string     `json:"result_summary,omitempty"`
}

// NewTracesHandler constructs the HTTP handler for sessions and traces query endpoints.
func NewTracesHandler(sessions SessionRepository, traces TraceRepository) http.Handler {
	return tracesHandler{sessions: sessions, traces: traces}
}

type tracesHandler struct {
	sessions SessionRepository
	traces   TraceRepository
}

func (h tracesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := RequestIDFromContext(r.Context())
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", requestID)

	if r.URL.Path != "/api/v1/sessions" && !strings.HasPrefix(r.URL.Path, "/api/v1/sessions/") {
		WriteError(w, http.StatusNotFound, "not_found", "route not found", requestID)
		return
	}

	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/sessions"), "/")
	if path == "" {
		h.collection(w, r, requestID)
		return
	}

	parts := strings.Split(path, "/")
	if len(parts) == 1 {
		h.item(w, r, parts[0], requestID)
		return
	}
	if len(parts) == 2 && parts[1] == "turns" {
		h.turns(w, r, parts[0], requestID)
		return
	}
	if len(parts) == 2 && parts[1] == "traces" {
		h.tracesRoute(w, r, parts[0], requestID)
		return
	}

	WriteError(w, http.StatusNotFound, "not_found", "route not found", requestID)
}

func (h tracesHandler) collection(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
		return
	}

	page, err := ParsePagination(r)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
		return
	}

	filter := storage.SessionFilter{
		Limit:  page.Limit,
		Offset: page.Offset,
		Status: r.URL.Query().Get("status"),
		FlowID: r.URL.Query().Get("flow_id"),
	}

	records, err := h.sessions.List(r.Context(), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to list sessions", requestID)
		return
	}

	ids := make([]string, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.ID)
	}
	latencies, err := h.sessions.TurnLatencies(r.Context(), ids) // one query for the page
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to list sessions", requestID)
		return
	}
	dtos := make([]SessionDTO, 0, len(records))
	for _, rec := range records {
		dto := toSessionDTO(rec)
		dto.MedianReplyMS = medianReplyMS(latencies[rec.ID])
		dtos = append(dtos, dto)
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[[]SessionDTO]{Data: dtos})
}

func (h tracesHandler) item(w http.ResponseWriter, r *http.Request, id, requestID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
		return
	}

	rec, err := h.sessions.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "UUIDv7") {
			WriteError(w, http.StatusNotFound, "not_found", "session not found", requestID)
			return
		}
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to get session", requestID)
		return
	}

	dto := toSessionDTO(rec)
	if latencies, err := h.sessions.TurnLatencies(r.Context(), []string{id}); err == nil {
		dto.MedianReplyMS = medianReplyMS(latencies[id])
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[SessionDTO]{Data: dto})
}

func (h tracesHandler) turns(w http.ResponseWriter, r *http.Request, id, requestID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
		return
	}

	// Verify session exists
	if _, err := h.sessions.Get(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "UUIDv7") {
			WriteError(w, http.StatusNotFound, "not_found", "session not found", requestID)
			return
		}
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to get session", requestID)
		return
	}

	turns, err := h.sessions.Turns(r.Context(), id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to get turns", requestID)
		return
	}

	dtos := make([]TurnDTO, 0, len(turns))
	for _, turn := range turns {
		dtos = append(dtos, toTurnDTO(turn))
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[[]TurnDTO]{Data: dtos})
}

func (h tracesHandler) tracesRoute(w http.ResponseWriter, r *http.Request, id, requestID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
		return
	}

	// Verify session exists
	if _, err := h.sessions.Get(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "UUIDv7") {
			WriteError(w, http.StatusNotFound, "not_found", "session not found", requestID)
			return
		}
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to get session", requestID)
		return
	}

	turnID := r.URL.Query().Get("turn_id")

	spans, err := h.traces.Spans(r.Context(), id, turnID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to get spans", requestID)
		return
	}

	events, err := h.traces.Events(r.Context(), id, turnID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to get events", requestID)
		return
	}

	toolCalls, err := h.traces.ToolCalls(r.Context(), id, turnID)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to get tool calls", requestID)
		return
	}

	spanDTOs := make([]TraceSpanDTO, 0, len(spans))
	for _, sp := range spans {
		spanDTOs = append(spanDTOs, toTraceSpanDTO(sp))
	}

	eventDTOs := make([]TraceEventDTO, 0, len(events))
	for _, ev := range events {
		eventDTOs = append(eventDTOs, toTraceEventDTO(ev))
	}

	toolCallDTOs := make([]ToolCallDTO, 0, len(toolCalls))
	for _, tc := range toolCalls {
		toolCallDTOs = append(toolCallDTOs, toToolCallDTO(tc))
	}

	result := SessionTracesDTO{
		SessionID: id,
		Spans:     spanDTOs,
		Events:    eventDTOs,
		ToolCalls: toolCallDTOs,
	}
	WriteJSON(w, http.StatusOK, SuccessResponse[SessionTracesDTO]{Data: result})
}

func toSessionDTO(r storage.SessionRecord) SessionDTO {
	dto := SessionDTO{
		ID:            r.ID,
		FlowID:        r.FlowID,
		FlowVersionID: r.FlowVersionID,
		ConfigHash:    r.ConfigHash,
		StartedAt:     r.StartedAt,
		Status:        r.Status,
		TurnCount:     r.TurnCount,
		SafeSummary:   r.SafeSummary,
		Draft:         r.Draft,
		Caller:        r.Caller,
		EndReason:     r.EndReason,
	}
	for _, t := range r.RuntimeTools {
		dto.RuntimeTools = append(dto.RuntimeTools, RuntimeToolDTO{MCPServerID: t.ServerID, RemoteName: t.RemoteName, SchemaHash: t.SchemaHash, Required: t.Required})
	}
	if !r.EndedAt.IsZero() {
		dto.EndedAt = &r.EndedAt
	}
	if r.ProviderStackJSON != "" && json.Valid([]byte(r.ProviderStackJSON)) {
		dto.ProviderStack = json.RawMessage(r.ProviderStackJSON)
	}
	if r.RuntimeSnapshotJSON != "" && json.Valid([]byte(r.RuntimeSnapshotJSON)) {
		dto.RuntimeSnapshot = json.RawMessage(r.RuntimeSnapshotJSON)
	}
	return dto
}

func toTurnDTO(r storage.TurnRecord) TurnDTO {
	dto := TurnDTO{
		ID:            r.ID,
		SessionID:     r.SessionID,
		Ordinal:       r.Ordinal,
		StartedAt:     r.StartedAt,
		Transcript:    r.Transcript,
		AssistantText: r.AssistantText,
		Interrupted:   r.Interrupted,
	}
	if !r.EndedAt.IsZero() {
		dto.EndedAt = &r.EndedAt
	}
	if r.LatenciesJSON != "" && json.Valid([]byte(r.LatenciesJSON)) {
		dto.Latencies = json.RawMessage(r.LatenciesJSON)
	}
	if r.ToolsJSON != "" && json.Valid([]byte(r.ToolsJSON)) {
		dto.Tools = json.RawMessage(r.ToolsJSON)
	}
	return dto
}

func toTraceSpanDTO(r storage.TraceSpan) TraceSpanDTO {
	dto := TraceSpanDTO{
		ID:        r.ID,
		SessionID: r.SessionID,
		TurnID:    r.TurnID,
		ParentID:  r.ParentID,
		Name:      r.Name,
		StartedAt: r.StartedAt,
	}
	if !r.EndedAt.IsZero() {
		dto.EndedAt = &r.EndedAt
	}
	if r.AttributesJSON != "" && json.Valid([]byte(r.AttributesJSON)) {
		dto.Attributes = json.RawMessage(r.AttributesJSON)
	}
	return dto
}

func toTraceEventDTO(r storage.TraceEvent) TraceEventDTO {
	dto := TraceEventDTO{
		ID:         r.ID,
		SpanID:     r.SpanID,
		SessionID:  r.SessionID,
		TurnID:     r.TurnID,
		Name:       r.Name,
		OccurredAt: r.OccurredAt,
	}
	if r.AttributesJSON != "" && json.Valid([]byte(r.AttributesJSON)) {
		dto.Attributes = json.RawMessage(r.AttributesJSON)
	}
	return dto
}

func toToolCallDTO(r storage.ToolCallRecord) ToolCallDTO {
	dto := ToolCallDTO{
		ID:               r.ID,
		SessionID:        r.SessionID,
		TurnID:           r.TurnID,
		MCPServerID:      r.MCPServerID,
		RemoteName:       r.RemoteName,
		SchemaHash:       r.SchemaHash,
		StartedAt:        r.StartedAt,
		Status:           r.Status,
		ArgumentsSummary: r.ArgumentsSummary,
		ResultSummary:    r.ResultSummary,
	}
	if !r.EndedAt.IsZero() {
		dto.EndedAt = &r.EndedAt
	}
	return dto
}
