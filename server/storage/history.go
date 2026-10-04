package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type SessionRecord struct {
	ID, AgentID, AgentVersionID, ConfigHash, Status, ProviderStackJSON, SafeSummary string
	StartedAt, EndedAt                                                              time.Time
	TurnCount                                                                       int
}
type TurnRecord struct {
	ID, SessionID, Transcript, AssistantText, LatenciesJSON, ToolsJSON string
	Ordinal                                                            int
	StartedAt, EndedAt                                                 time.Time
	Interrupted                                                        bool
}
type SessionRepository struct{ s *Store }

func (s *Store) Sessions() SessionRepository { return SessionRepository{s} }
func (r SessionRepository) Create(ctx context.Context, v SessionRecord) error {
	for _, id := range []string{v.ID, v.AgentID, v.AgentVersionID} {
		if err := validID(id); err != nil {
			return err
		}
	}
	stack := v.ProviderStackJSON
	if stack == "" {
		stack = "{}"
	}
	canonical, err := CanonicalJSON([]byte(stack))
	if err != nil {
		return err
	}
	_, err = r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO sessions(id,agent_id,agent_version_id,config_hash,started_at,ended_at,status,provider_stack_json,turn_count,safe_summary) VALUES(?,?,?,?,?,?,?,?,?,?)"), v.ID, v.AgentID, v.AgentVersionID, v.ConfigHash, utc(v.StartedAt), nullTime(v.EndedAt), v.Status, string(canonical), v.TurnCount, v.SafeSummary)
	return err
}
func (r SessionRepository) AddTurn(ctx context.Context, v TurnRecord) error {
	for _, id := range []string{v.ID, v.SessionID} {
		if err := validID(id); err != nil {
			return err
		}
	}
	latency := v.LatenciesJSON
	if latency == "" {
		latency = "{}"
	}
	tools := v.ToolsJSON
	if tools == "" {
		tools = "[]"
	}
	latencyJSON, err := CanonicalJSON([]byte(latency))
	if err != nil {
		return err
	}
	toolsJSON, err := CanonicalJSON([]byte(tools))
	if err != nil {
		return err
	}
	_, err = r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO turns(id,session_id,ordinal,started_at,ended_at,transcript,assistant_text,latencies_json,interrupted,tools_json) VALUES(?,?,?,?,?,?,?,?,?,?)"), v.ID, v.SessionID, v.Ordinal, utc(v.StartedAt), nullTime(v.EndedAt), nullable(v.Transcript), nullable(v.AssistantText), string(latencyJSON), boolValue(r.s.dialect, v.Interrupted), string(toolsJSON))
	return err
}
func (r SessionRepository) Get(ctx context.Context, id string) (SessionRecord, error) {
	var v SessionRecord
	var started string
	var ended sql.NullString
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,agent_id,agent_version_id,config_hash,started_at,ended_at,status,provider_stack_json,turn_count,safe_summary FROM sessions WHERE id=?"), id).Scan(&v.ID, &v.AgentID, &v.AgentVersionID, &v.ConfigHash, &started, &ended, &v.Status, &v.ProviderStackJSON, &v.TurnCount, &v.SafeSummary)
	if err != nil {
		return v, err
	}
	if v.StartedAt, err = parseTime(started); err != nil {
		return v, err
	}
	if ended.Valid {
		if v.EndedAt, err = parseTime(ended.String); err != nil {
			return v, err
		}
	}
	return v, nil
}
func (r SessionRepository) Turn(ctx context.Context, id string) (TurnRecord, error) {
	var v TurnRecord
	var started string
	var ended, transcript, assistant sql.NullString
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT id,session_id,ordinal,started_at,ended_at,transcript,assistant_text,latencies_json,interrupted,tools_json FROM turns WHERE id=?"), id).Scan(&v.ID, &v.SessionID, &v.Ordinal, &started, &ended, &transcript, &assistant, &v.LatenciesJSON, &v.Interrupted, &v.ToolsJSON)
	if err != nil {
		return v, err
	}
	if v.StartedAt, err = parseTime(started); err != nil {
		return v, err
	}
	if ended.Valid {
		if v.EndedAt, err = parseTime(ended.String); err != nil {
			return v, err
		}
	}
	v.Transcript = transcript.String
	v.AssistantText = assistant.String
	return v, nil
}

type TraceSpan struct {
	ID, SessionID, TurnID, ParentID, Name, AttributesJSON string
	StartedAt, EndedAt                                    time.Time
}
type TraceEvent struct {
	ID, SpanID, SessionID, TurnID, Name, AttributesJSON string
	OccurredAt                                          time.Time
}
type ToolCallRecord struct {
	ID, SessionID, TurnID, MCPServerID, RemoteName, SchemaHash, Status, ArgumentsSummary, ResultSummary string
	StartedAt, EndedAt                                                                                  time.Time
}
type TraceRepository struct{ s *Store }

func (s *Store) Traces() TraceRepository { return TraceRepository{s} }
func (r TraceRepository) AddSpan(ctx context.Context, v TraceSpan) error {
	if err := validID(v.ID); err != nil {
		return err
	}
	for _, id := range []string{v.SessionID, v.TurnID, v.ParentID} {
		if err := optionalID(id); err != nil {
			return err
		}
	}
	attrs, err := metadata(v.AttributesJSON)
	if err != nil {
		return err
	}
	_, err = r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO trace_spans(id,session_id,turn_id,parent_id,name,started_at,ended_at,attributes_json) VALUES(?,?,?,?,?,?,?,?)"), v.ID, nullable(v.SessionID), nullable(v.TurnID), nullable(v.ParentID), v.Name, utc(v.StartedAt), nullTime(v.EndedAt), attrs)
	return err
}
func (r TraceRepository) AddEvent(ctx context.Context, v TraceEvent) error {
	if err := validID(v.ID); err != nil {
		return err
	}
	for _, id := range []string{v.SpanID, v.SessionID, v.TurnID} {
		if err := optionalID(id); err != nil {
			return err
		}
	}
	attrs, err := metadata(v.AttributesJSON)
	if err != nil {
		return err
	}
	_, err = r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO trace_events(id,span_id,session_id,turn_id,name,occurred_at,attributes_json) VALUES(?,?,?,?,?,?,?)"), v.ID, nullable(v.SpanID), nullable(v.SessionID), nullable(v.TurnID), v.Name, utc(v.OccurredAt), attrs)
	return err
}
func (r TraceRepository) AddToolCall(ctx context.Context, v ToolCallRecord) error {
	for _, id := range []string{v.ID, v.SessionID} {
		if err := validID(id); err != nil {
			return err
		}
	}
	for _, id := range []string{v.TurnID, v.MCPServerID} {
		if err := optionalID(id); err != nil {
			return err
		}
	}
	if v.SessionID == "" {
		return errors.New("tool call session required")
	}
	_, err := r.s.db.ExecContext(ctx, r.s.bind("INSERT INTO tool_calls(id,session_id,turn_id,mcp_server_id,remote_name,schema_hash,started_at,ended_at,status,arguments_summary,result_summary) VALUES(?,?,?,?,?,?,?,?,?,?,?)"), v.ID, v.SessionID, nullable(v.TurnID), nullable(v.MCPServerID), v.RemoteName, v.SchemaHash, utc(v.StartedAt), nullTime(v.EndedAt), v.Status, v.ArgumentsSummary, v.ResultSummary)
	return err
}
func metadata(s string) (string, error) {
	if s == "" {
		s = "{}"
	}
	v, err := CanonicalJSON([]byte(s))
	return string(v), err
}
