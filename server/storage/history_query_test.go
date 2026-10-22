package storage_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"govoice/server/storage"
)

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Config{
		Dialect: storage.SQLite,
		DSN:     filepath.Join(t.TempDir(), "voice.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func createTestFlowAndVersion(t *testing.T, s *storage.Store) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	agentID := storage.NewID()
	a := storage.Agent{ID: agentID, Name: "TestAgent", Enabled: true, CreatedAt: time.Now()}
	if err := s.Agents().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, _, err := s.Agents().MutateAndPublish(ctx, agentID, storage.AgentMutation{}, []byte(`{"llm":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	return agentID, v.ID, v.ConfigHash
}

func TestSessionRepositoryPaginationAndFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	flow1, v1, hash1 := createTestFlowAndVersion(t, s)
	flow2, v2, hash2 := createTestFlowAndVersion(t, s)

	baseTime := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

	// Create 3 sessions for flow1 (2 active, 1 ended)
	for i := 0; i < 3; i++ {
		status := "active"
		if i == 2 {
			status = "ended"
		}
		sess := storage.SessionRecord{
			ID:                  storage.NewID(),
			AgentID:             flow1,
			AgentVersionID:      v1,
			ConfigHash:          hash1,
			Status:              status,
			StartedAt:           baseTime.Add(time.Duration(i) * time.Hour),
			TurnCount:           i,
			SafeSummary:         "summary",
			ProviderStackJSON:   `{"llm":"test"}`,
			RuntimeSnapshotJSON: `{}`,
		}
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
	}

	// Create 2 sessions for flow2 (both ended)
	for i := 0; i < 2; i++ {
		sess := storage.SessionRecord{
			ID:                  storage.NewID(),
			AgentID:             flow2,
			AgentVersionID:      v2,
			ConfigHash:          hash2,
			Status:              "ended",
			StartedAt:           baseTime.Add(time.Duration(i+10) * time.Hour),
			TurnCount:           i,
			SafeSummary:         "summary 2",
			ProviderStackJSON:   `{"llm":"test"}`,
			RuntimeSnapshotJSON: `{}`,
		}
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
	}

	// Test 1: List all with pagination
	page1, err := s.Sessions().List(ctx, storage.SessionFilter{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("expected 2 sessions on page 1, got %d", len(page1))
	}

	page2, err := s.Sessions().List(ctx, storage.SessionFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("expected 2 sessions on page 2, got %d", len(page2))
	}

	page3, err := s.Sessions().List(ctx, storage.SessionFilter{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("list page 3: %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("expected 1 session on page 3, got %d", len(page3))
	}

	// Test 2: Filter by AgentID
	flow1Sessions, err := s.Sessions().List(ctx, storage.SessionFilter{AgentID: flow1, Limit: 50})
	if err != nil {
		t.Fatalf("list flow1: %v", err)
	}
	if len(flow1Sessions) != 3 {
		t.Fatalf("expected 3 sessions for flow1, got %d", len(flow1Sessions))
	}

	// Test 3: Filter by Status
	activeSessions, err := s.Sessions().List(ctx, storage.SessionFilter{Status: "active", Limit: 50})
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(activeSessions) != 2 {
		t.Fatalf("expected 2 active sessions, got %d", len(activeSessions))
	}

	// Test 4: Filter by AgentID AND Status
	flow1Active, err := s.Sessions().List(ctx, storage.SessionFilter{AgentID: flow1, Status: "active", Limit: 50})
	if err != nil {
		t.Fatalf("list flow1 active: %v", err)
	}
	if len(flow1Active) != 2 {
		t.Fatalf("expected 2 active sessions for flow1, got %d", len(flow1Active))
	}
}

func TestSessionRepositoryGetDetailAndTurns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agentID, vID, hash := createTestFlowAndVersion(t, s)
	sessID := storage.NewID()
	now := time.Now().UTC().Truncate(time.Microsecond)

	sess := storage.SessionRecord{
		ID:                  sessID,
		AgentID:             agentID,
		AgentVersionID:      vID,
		ConfigHash:          hash,
		Status:              "active",
		StartedAt:           now,
		TurnCount:           2,
		SafeSummary:         "detailed session",
		ProviderStackJSON:   `{"llm":"test"}`,
		RuntimeSnapshotJSON: `{"tools":[]}`,
	}
	if err := s.Sessions().Create(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Get detail
	got, err := s.Sessions().Get(ctx, sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.ID != sessID || got.AgentID != agentID || got.Status != "active" {
		t.Fatalf("mismatched session record: %+v", got)
	}

	// Non-existent session
	_, err = s.Sessions().Get(ctx, storage.NewID())
	if err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}

	// Add turns out of order: ordinal 2, then ordinal 1
	turn2 := storage.TurnRecord{
		ID:            storage.NewID(),
		SessionID:     sessID,
		Ordinal:       2,
		StartedAt:     now.Add(10 * time.Second),
		Transcript:    "user two",
		AssistantText: "assistant two",
		Interrupted:   false,
	}
	turn1 := storage.TurnRecord{
		ID:            storage.NewID(),
		SessionID:     sessID,
		Ordinal:       1,
		StartedAt:     now.Add(2 * time.Second),
		Transcript:    "user one",
		AssistantText: "assistant one",
		Interrupted:   true,
	}
	if err := s.Sessions().AddTurn(ctx, turn2); err != nil {
		t.Fatalf("add turn 2: %v", err)
	}
	if err := s.Sessions().AddTurn(ctx, turn1); err != nil {
		t.Fatalf("add turn 1: %v", err)
	}

	// Query turns - must be ordered by ordinal ASC
	turns, err := s.Sessions().Turns(ctx, sessID)
	if err != nil {
		t.Fatalf("query turns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(turns))
	}
	if turns[0].Ordinal != 1 || turns[1].Ordinal != 2 {
		t.Fatalf("turns not ordered by ordinal: %d, %d", turns[0].Ordinal, turns[1].Ordinal)
	}
	if !turns[0].Interrupted || turns[1].Interrupted {
		t.Fatalf("turn interruption mismatch")
	}
	if got, err := s.Sessions().Get(ctx, sessID); err != nil || got.TurnCount != 4 { // created with 2, plus the two turns added
		t.Fatalf("session turn_count = %d (err %v), want 4", got.TurnCount, err)
	}
}

func TestTraceRepositorySpansEventsAndToolCalls(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	agentID, vID, hash := createTestFlowAndVersion(t, s)
	sessID := storage.NewID()
	now := time.Now().UTC().Truncate(time.Microsecond)

	sess := storage.SessionRecord{
		ID:                  sessID,
		AgentID:             agentID,
		AgentVersionID:      vID,
		ConfigHash:          hash,
		Status:              "active",
		StartedAt:           now,
		ProviderStackJSON:   `{}`,
		RuntimeSnapshotJSON: `{}`,
	}
	if err := s.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	turnID := storage.NewID()
	if err := s.Sessions().AddTurn(ctx, storage.TurnRecord{
		ID:        turnID,
		SessionID: sessID,
		Ordinal:   1,
		StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Spans: insert span1, then span2
	span1ID := storage.NewID()
	span2ID := storage.NewID()
	if err := s.Traces().AddSpan(ctx, storage.TraceSpan{
		ID:             span1ID,
		SessionID:      sessID,
		TurnID:         turnID,
		Name:           "span.parent",
		StartedAt:      now,
		AttributesJSON: `{"status":"started"}`,
	}); err != nil {
		t.Fatalf("add span 1: %v", err)
	}
	if err := s.Traces().AddSpan(ctx, storage.TraceSpan{
		ID:             span2ID,
		SessionID:      sessID,
		TurnID:         turnID,
		ParentID:       span1ID,
		Name:           "span.child",
		StartedAt:      now.Add(100 * time.Millisecond),
		AttributesJSON: `{}`,
	}); err != nil {
		t.Fatalf("add span 2: %v", err)
	}

	// Test EndSpan on span1
	endedAt := now.Add(500 * time.Millisecond)
	if err := s.Traces().EndSpan(ctx, span1ID, endedAt, `{"status":"completed"}`); err != nil {
		t.Fatalf("end span 1: %v", err)
	}

	// Query Spans
	spans, err := s.Traces().Spans(ctx, sessID, "")
	if err != nil {
		t.Fatalf("query spans: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}
	if spans[0].ID != span1ID || spans[1].ID != span2ID {
		t.Fatalf("spans not ordered chronologically: %s, %s", spans[0].ID, spans[1].ID)
	}
	if spans[0].EndedAt.IsZero() || spans[0].EndedAt.UnixNano() != endedAt.UnixNano() {
		t.Fatalf("span 1 ended_at not updated: %v", spans[0].EndedAt)
	}

	// 2. Events: insert event2 then event1 out of chronological order
	event1ID := storage.NewID()
	event2ID := storage.NewID()
	if err := s.Traces().AddEvent(ctx, storage.TraceEvent{
		ID:             event2ID,
		SpanID:         span1ID,
		SessionID:      sessID,
		TurnID:         turnID,
		Name:           "event.second",
		OccurredAt:     now.Add(200 * time.Millisecond),
		AttributesJSON: `{}`,
	}); err != nil {
		t.Fatalf("add event 2: %v", err)
	}
	if err := s.Traces().AddEvent(ctx, storage.TraceEvent{
		ID:             event1ID,
		SpanID:         span1ID,
		SessionID:      sessID,
		TurnID:         turnID,
		Name:           "event.first",
		OccurredAt:     now.Add(50 * time.Millisecond),
		AttributesJSON: `{}`,
	}); err != nil {
		t.Fatalf("add event 1: %v", err)
	}

	// Query Events - ordered chronologically
	events, err := s.Traces().Events(ctx, sessID, turnID)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].ID != event1ID || events[1].ID != event2ID {
		t.Fatalf("events not chronological: got %s, %s", events[0].Name, events[1].Name)
	}

	// 3. Tool Calls
	tcID := storage.NewID()
	if err := s.Traces().AddToolCall(ctx, storage.ToolCallRecord{
		ID:         tcID,
		SessionID:  sessID,
		TurnID:     turnID,
		RemoteName: "search",
		SchemaHash: "hash123",
		StartedAt:  now.Add(150 * time.Millisecond),
		Status:     "completed",
	}); err != nil {
		t.Fatalf("add tool call: %v", err)
	}

	toolCalls, err := s.Traces().ToolCalls(ctx, sessID, turnID)
	if err != nil {
		t.Fatalf("query tool calls: %v", err)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].ID != tcID || toolCalls[0].RemoteName != "search" {
		t.Fatalf("tool call mismatch: %+v", toolCalls[0])
	}
}
