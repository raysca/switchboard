package retention_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/server/retention"
	"govoice/server/storage"
)

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Config{
		Dialect: storage.SQLite,
		DSN:     filepath.Join(t.TempDir(), "retention.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func createTestAgentAndVersion(t *testing.T, s *storage.Store) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	agentID := storage.NewID()
	a := storage.Agent{ID: agentID, Name: "RetentionAgent", Enabled: true, CreatedAt: time.Now().UTC()}
	if err := s.Agents().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := s.Agents().AddVersion(ctx, agentID, 1, []byte(`{"llm":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	return agentID, v.ID, v.ConfigHash
}

type sessionData struct {
	sessionID  string
	turnID     string
	toolCallID string
	spanID     string
	eventID    string
}

func createSessionWithData(t *testing.T, s *storage.Store, agentID, versionID, configHash, status string, startedAt, endedAt time.Time) sessionData {
	t.Helper()
	ctx := context.Background()
	sessID := storage.NewID()
	sess := storage.SessionRecord{
		ID:                  sessID,
		AgentID:             agentID,
		AgentVersionID:      versionID,
		ConfigHash:          configHash,
		Status:              status,
		StartedAt:           startedAt,
		EndedAt:             endedAt,
		ProviderStackJSON:   `{}`,
		RuntimeSnapshotJSON: `{}`,
	}
	if err := s.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	turnID := storage.NewID()
	turn := storage.TurnRecord{
		ID:            turnID,
		SessionID:     sessID,
		Ordinal:       1,
		StartedAt:     startedAt,
		EndedAt:       endedAt,
		Transcript:    "Hello world user question",
		AssistantText: "Hello there assistant response",
		LatenciesJSON: `{"llm_first_chunk_ms":120}`,
		ToolsJSON:     `[]`,
	}
	if err := s.Sessions().AddTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}

	toolCallID := storage.NewID()
	tc := storage.ToolCallRecord{
		ID:               toolCallID,
		SessionID:        sessID,
		TurnID:           turnID,
		RemoteName:       "weather_check",
		SchemaHash:       "hash123",
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		Status:           "completed",
		ArgumentsSummary: `{"city":"Berlin"}`,
		ResultSummary:    `{"temp":21}`,
	}
	if err := s.Traces().AddToolCall(ctx, tc); err != nil {
		t.Fatal(err)
	}

	spanID := storage.NewID()
	span := storage.TraceSpan{
		ID:             spanID,
		SessionID:      sessID,
		TurnID:         turnID,
		Name:           "turn.execute",
		StartedAt:      startedAt,
		EndedAt:        endedAt,
		AttributesJSON: `{"sample_rate":24000}`,
	}
	if err := s.Traces().AddSpan(ctx, span); err != nil {
		t.Fatal(err)
	}

	eventID := storage.NewID()
	ev := storage.TraceEvent{
		ID:             eventID,
		SpanID:         spanID,
		SessionID:      sessID,
		TurnID:         turnID,
		Name:           "speech.detected",
		OccurredAt:     startedAt.Add(10 * time.Millisecond),
		AttributesJSON: `{"duration_ms":500}`,
	}
	if err := s.Traces().AddEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}

	return sessionData{
		sessionID:  sessID,
		turnID:     turnID,
		toolCallID: toolCallID,
		spanID:     spanID,
		eventID:    eventID,
	}
}

func TestPolicyValidation(t *testing.T) {
	tests := []struct {
		name    string
		policy  retention.Policy
		wantErr bool
	}{
		{
			name: "valid sane policy",
			policy: retention.Policy{
				MetadataRetention: 30 * 24 * time.Hour,
				ContentRetention:  7 * 24 * time.Hour,
				BatchSize:         500,
			},
			wantErr: false,
		},
		{
			name: "valid equal policy",
			policy: retention.Policy{
				MetadataRetention: 30 * 24 * time.Hour,
				ContentRetention:  30 * 24 * time.Hour,
				BatchSize:         100,
			},
			wantErr: false,
		},
		{
			name: "valid metadata only policy",
			policy: retention.Policy{
				MetadataRetention: 30 * 24 * time.Hour,
				ContentRetention:  0,
				BatchSize:         100,
			},
			wantErr: false,
		},
		{
			name: "negative metadata retention",
			policy: retention.Policy{
				MetadataRetention: -1 * time.Hour,
				ContentRetention:  7 * 24 * time.Hour,
			},
			wantErr: true,
		},
		{
			name: "negative content retention",
			policy: retention.Policy{
				MetadataRetention: 30 * 24 * time.Hour,
				ContentRetention:  -1 * time.Hour,
			},
			wantErr: true,
		},
		{
			name: "content retention exceeds metadata retention",
			policy: retention.Policy{
				MetadataRetention: 7 * 24 * time.Hour,
				ContentRetention:  30 * 24 * time.Hour,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestContentPurge(t *testing.T) {
	s := newTestStore(t)
	agentID, versionID, configHash := createTestAgentAndVersion(t, s)
	ctx := context.Background()
	now := time.Now().UTC()

	// Session 1: ended 10 days ago (content expired under 7-day retention)
	sess1 := createSessionWithData(t, s, agentID, versionID, configHash, "ended",
		now.Add(-10*24*time.Hour), now.Add(-10*24*time.Hour+time.Minute))

	// Session 2: ended 2 days ago (content NOT expired)
	sess2 := createSessionWithData(t, s, agentID, versionID, configHash, "ended",
		now.Add(-2*24*time.Hour), now.Add(-2*24*time.Hour+time.Minute))

	// Session 3: active, started 20 days ago (active sessions must NOT have content purged)
	sess3 := createSessionWithData(t, s, agentID, versionID, configHash, "active",
		now.Add(-20*24*time.Hour), time.Time{})

	policy := retention.Policy{
		MetadataRetention: 30 * 24 * time.Hour,
		ContentRetention:  7 * 24 * time.Hour,
		BatchSize:         100,
	}

	job := retention.NewJob(s.Retention(), policy, retention.WithClock(func() time.Time { return now }))
	res, err := job.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}
	if res.TurnsPurged != 1 {
		t.Errorf("expected 1 turn purged, got %d", res.TurnsPurged)
	}
	if res.ToolCallsPurged != 1 {
		t.Errorf("expected 1 tool call purged, got %d", res.ToolCallsPurged)
	}

	// Verify session 1: content purged, metadata preserved
	turn1, err := s.Sessions().Turn(ctx, sess1.turnID)
	if err != nil {
		t.Fatalf("query turn 1: %v", err)
	}
	if turn1.Transcript != "" {
		t.Errorf("expected empty/purged transcript, got %q", turn1.Transcript)
	}
	if turn1.AssistantText != "" {
		t.Errorf("expected empty/purged assistant text, got %q", turn1.AssistantText)
	}
	if turn1.Ordinal != 1 || turn1.LatenciesJSON == "" {
		t.Errorf("expected turn metadata preserved, got ordinal=%d, latencies=%s", turn1.Ordinal, turn1.LatenciesJSON)
	}

	tcs1, err := s.Traces().ToolCalls(ctx, sess1.sessionID, "")
	if err != nil || len(tcs1) == 0 {
		t.Fatalf("tool calls query: %v", err)
	}
	if tcs1[0].ArgumentsSummary != "" || tcs1[0].ResultSummary != "" {
		t.Errorf("expected purged tool call arguments and results, got args=%q, res=%q",
			tcs1[0].ArgumentsSummary, tcs1[0].ResultSummary)
	}
	if tcs1[0].RemoteName != "weather_check" || tcs1[0].Status != "completed" {
		t.Errorf("expected tool call metadata preserved, got %+v", tcs1[0])
	}

	// Session 1 structure still exists
	if _, err := s.Sessions().Get(ctx, sess1.sessionID); err != nil {
		t.Errorf("expected session 1 to still exist: %v", err)
	}
	spans1, _ := s.Traces().Spans(ctx, sess1.sessionID, "")
	if len(spans1) != 1 {
		t.Errorf("expected session 1 spans preserved, got %d", len(spans1))
	}

	// Verify session 2: content intact
	turn2, err := s.Sessions().Turn(ctx, sess2.turnID)
	if err != nil {
		t.Fatalf("query turn 2: %v", err)
	}
	if turn2.Transcript == "" || turn2.AssistantText == "" {
		t.Errorf("expected turn 2 content intact, got transcript=%q, assistant=%q", turn2.Transcript, turn2.AssistantText)
	}

	// Verify session 3 (active): content intact
	turn3, err := s.Sessions().Turn(ctx, sess3.turnID)
	if err != nil {
		t.Fatalf("query turn 3: %v", err)
	}
	if turn3.Transcript == "" || turn3.AssistantText == "" {
		t.Errorf("expected active session turn 3 content intact, got transcript=%q, assistant=%q", turn3.Transcript, turn3.AssistantText)
	}
}

func TestMetadataPurgeAndInvariants(t *testing.T) {
	s := newTestStore(t)
	agentID, versionID, configHash := createTestAgentAndVersion(t, s)
	ctx := context.Background()
	now := time.Now().UTC()

	// Session 1: ended 40 days ago (metadata expired under 30-day retention)
	sess1 := createSessionWithData(t, s, agentID, versionID, configHash, "ended",
		now.Add(-40*24*time.Hour), now.Add(-40*24*time.Hour+time.Minute))

	// Session 2: ended 10 days ago (metadata NOT expired)
	sess2 := createSessionWithData(t, s, agentID, versionID, configHash, "ended",
		now.Add(-10*24*time.Hour), now.Add(-10*24*time.Hour+time.Minute))

	// Session 3: active, started 50 days ago (ACTIVE SESSION MUST NEVER BE DELETED!)
	sess3 := createSessionWithData(t, s, agentID, versionID, configHash, "active",
		now.Add(-50*24*time.Hour), time.Time{})

	// Session 4: draining, started 50 days ago (DRAINING SESSION MUST NEVER BE DELETED!)
	sess4 := createSessionWithData(t, s, agentID, versionID, configHash, "draining",
		now.Add(-50*24*time.Hour), time.Time{})

	// Standalone expired span & event (no session)
	standaloneSpanID := storage.NewID()
	if err := s.Traces().AddSpan(ctx, storage.TraceSpan{
		ID:             standaloneSpanID,
		Name:           "standalone.old",
		StartedAt:      now.Add(-45 * 24 * time.Hour),
		EndedAt:        now.Add(-45*24*time.Hour + time.Second),
		AttributesJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	standaloneEventID := storage.NewID()
	if err := s.Traces().AddEvent(ctx, storage.TraceEvent{
		ID:             standaloneEventID,
		Name:           "standalone.old.event",
		OccurredAt:     now.Add(-45 * 24 * time.Hour),
		AttributesJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}

	policy := retention.Policy{
		MetadataRetention: 30 * 24 * time.Hour,
		ContentRetention:  7 * 24 * time.Hour,
		BatchSize:         100,
	}

	job := retention.NewJob(s.Retention(), policy, retention.WithClock(func() time.Time { return now }))
	res, err := job.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	if res.SessionsDeleted != 1 {
		t.Errorf("expected 1 session deleted, got %d", res.SessionsDeleted)
	}
	if res.TurnsDeleted != 1 {
		t.Errorf("expected 1 turn deleted, got %d", res.TurnsDeleted)
	}

	// Verify session 1 is completely gone
	if _, err := s.Sessions().Get(ctx, sess1.sessionID); err == nil {
		t.Errorf("expected session 1 to be deleted")
	}
	if _, err := s.Sessions().Turn(ctx, sess1.turnID); err == nil {
		t.Errorf("expected turn 1 to be deleted")
	}
	spans1, _ := s.Traces().Spans(ctx, sess1.sessionID, "")
	if len(spans1) != 0 {
		t.Errorf("expected session 1 spans to be deleted, got %d", len(spans1))
	}
	events1, _ := s.Traces().Events(ctx, sess1.sessionID, "")
	if len(events1) != 0 {
		t.Errorf("expected session 1 events to be deleted, got %d", len(events1))
	}
	tcs1, _ := s.Traces().ToolCalls(ctx, sess1.sessionID, "")
	if len(tcs1) != 0 {
		t.Errorf("expected session 1 tool calls to be deleted, got %d", len(tcs1))
	}

	// Verify session 2 is intact
	if _, err := s.Sessions().Get(ctx, sess2.sessionID); err != nil {
		t.Errorf("expected session 2 to exist: %v", err)
	}

	// Verify INVARIANTS: Active & draining sessions MUST NEVER be deleted
	if _, err := s.Sessions().Get(ctx, sess3.sessionID); err != nil {
		t.Errorf("INVARIANT VIOLATION: active session 3 was deleted! err: %v", err)
	}
	if _, err := s.Sessions().Get(ctx, sess4.sessionID); err != nil {
		t.Errorf("INVARIANT VIOLATION: draining session 4 was deleted! err: %v", err)
	}

	// Verify INVARIANTS: Agent versions are NEVER deleted
	ag, err := s.Agents().Current(ctx, agentID)
	if err != nil {
		t.Fatalf("agent lookup: %v", err)
	}
	if ag.CurrentVersionID != versionID {
		t.Errorf("INVARIANT VIOLATION: agent version changed/missing")
	}
}

func TestBatchPurge(t *testing.T) {
	s := newTestStore(t)
	agentID, versionID, configHash := createTestAgentAndVersion(t, s)
	ctx := context.Background()
	now := time.Now().UTC()

	// Create 5 expired ended sessions
	for i := 0; i < 5; i++ {
		createSessionWithData(t, s, agentID, versionID, configHash, "ended",
			now.Add(-40*24*time.Hour), now.Add(-40*24*time.Hour+time.Minute))
	}

	// Run with BatchSize = 2 to force multiple iterations
	policy := retention.Policy{
		MetadataRetention: 30 * 24 * time.Hour,
		ContentRetention:  7 * 24 * time.Hour,
		BatchSize:         2,
	}

	job := retention.NewJob(s.Retention(), policy, retention.WithClock(func() time.Time { return now }))
	res, err := job.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}
	if res.SessionsDeleted != 5 {
		t.Errorf("expected 5 sessions deleted across batches, got %d", res.SessionsDeleted)
	}

	// Check that 0 sessions remain
	remaining, err := s.Sessions().List(ctx, storage.SessionFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected 0 remaining sessions, got %d", len(remaining))
	}
}

func TestPurgeContent_ToolResultAndTextSanitization(t *testing.T) {
	s := newTestStore(t)
	agentID, versionID, configHash := createTestAgentAndVersion(t, s)
	ctx := context.Background()
	now := time.Now().UTC()

	// Session 1: ended 10 days ago (content expired).
	// Turns have NO transcript/assistant text, and tool calls have NO arguments/result summary.
	// ONLY spans and events have "tool.result" and "text" attributes!
	sessID1 := storage.NewID()
	if err := s.Sessions().Create(ctx, storage.SessionRecord{
		ID:                  sessID1,
		AgentID:             agentID,
		AgentVersionID:      versionID,
		ConfigHash:          configHash,
		Status:              "ended",
		StartedAt:           now.Add(-10 * 24 * time.Hour),
		EndedAt:             now.Add(-10*24*time.Hour + time.Minute),
		ProviderStackJSON:   `{}`,
		RuntimeSnapshotJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}

	// Turn has empty transcript and assistant_text
	turnID1 := storage.NewID()
	if err := s.Sessions().AddTurn(ctx, storage.TurnRecord{
		ID:            turnID1,
		SessionID:     sessID1,
		Ordinal:       1,
		StartedAt:     now.Add(-10 * 24 * time.Hour),
		EndedAt:       now.Add(-10*24*time.Hour + time.Minute),
		Transcript:    "",
		AssistantText: "",
		LatenciesJSON: `{}`,
		ToolsJSON:     `[]`,
	}); err != nil {
		t.Fatal(err)
	}

	// Span has "tool.result", along with preserved metadata
	spanID1 := storage.NewID()
	if err := s.Traces().AddSpan(ctx, storage.TraceSpan{
		ID:             spanID1,
		SessionID:      sessID1,
		TurnID:         turnID1,
		Name:           "tool.execution",
		StartedAt:      now.Add(-10 * 24 * time.Hour),
		EndedAt:        now.Add(-10*24*time.Hour + time.Second),
		AttributesJSON: `{"tool.result":"sensitive-tool-output","sample_rate":24000}`,
	}); err != nil {
		t.Fatal(err)
	}

	// Event has "text" attribute, along with preserved metadata
	eventID1 := storage.NewID()
	if err := s.Traces().AddEvent(ctx, storage.TraceEvent{
		ID:             eventID1,
		SpanID:         spanID1,
		SessionID:      sessID1,
		TurnID:         turnID1,
		Name:           "user.speech",
		OccurredAt:     now.Add(-10 * 24 * time.Hour),
		AttributesJSON: `{"text":"sensitive-user-speech","duration_ms":300}`,
	}); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-7 * 24 * time.Hour)
	_, err := s.Retention().PurgeContent(ctx, cutoff, 100, storage.ContentCursor{})
	if err != nil {
		t.Fatalf("PurgeContent failed: %v", err)
	}

	// Verify span: "tool.result" must be purged, "sample_rate" must be preserved
	spans, err := s.Traces().Spans(ctx, sessID1, "")
	if err != nil || len(spans) == 0 {
		t.Fatalf("query spans: %v", err)
	}
	if strings.Contains(spans[0].AttributesJSON, "tool.result") || strings.Contains(spans[0].AttributesJSON, "sensitive-tool-output") {
		t.Errorf("expected tool.result to be purged from span attributes, got %s", spans[0].AttributesJSON)
	}
	if !strings.Contains(spans[0].AttributesJSON, "sample_rate") {
		t.Errorf("expected sample_rate to be preserved in span attributes, got %s", spans[0].AttributesJSON)
	}

	// Verify event: "text" must be purged, "duration_ms" must be preserved
	events, err := s.Traces().Events(ctx, sessID1, "")
	if err != nil || len(events) == 0 {
		t.Fatalf("query events: %v", err)
	}
	if strings.Contains(events[0].AttributesJSON, "text") || strings.Contains(events[0].AttributesJSON, "sensitive-user-speech") {
		t.Errorf("expected text to be purged from event attributes, got %s", events[0].AttributesJSON)
	}
	if !strings.Contains(events[0].AttributesJSON, "duration_ms") {
		t.Errorf("expected duration_ms to be preserved in event attributes, got %s", events[0].AttributesJSON)
	}
}

func TestConcurrentPurgeAndTraceInsertion(t *testing.T) {
	s := newTestStore(t)
	agentID, versionID, configHash := createTestAgentAndVersion(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	now := time.Now().UTC()

	// Seed 60 expired ended sessions
	for i := 0; i < 60; i++ {
		createSessionWithData(t, s, agentID, versionID, configHash, "ended",
			now.Add(-40*24*time.Hour), now.Add(-40*24*time.Hour+time.Minute))
	}

	// Active session for ongoing trace insertions
	activeSessID := storage.NewID()
	if err := s.Sessions().Create(ctx, storage.SessionRecord{
		ID:                  activeSessID,
		AgentID:             agentID,
		AgentVersionID:      versionID,
		ConfigHash:          configHash,
		Status:              "active",
		StartedAt:           now,
		ProviderStackJSON:   `{}`,
		RuntimeSnapshotJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 100)
	done := make(chan struct{})

	// Continuous trace insertions
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				spanID := storage.NewID()
				if err := s.Traces().AddSpan(ctx, storage.TraceSpan{
					ID:             spanID,
					SessionID:      activeSessID,
					Name:           "live.span",
					StartedAt:      time.Now().UTC(),
					EndedAt:        time.Now().UTC(),
					AttributesJSON: `{"status":"ok"}`,
				}); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if err := s.Traces().AddEvent(ctx, storage.TraceEvent{
					ID:             storage.NewID(),
					SpanID:         spanID,
					SessionID:      activeSessID,
					Name:           "live.event",
					OccurredAt:     time.Now().UTC(),
					AttributesJSON: `{"step":1}`,
				}); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()

	// Run content purge then metadata purge
	policy := retention.Policy{
		MetadataRetention: 30 * 24 * time.Hour,
		ContentRetention:  7 * 24 * time.Hour,
		BatchSize:         60,
	}
	job := retention.NewJob(s.Retention(), policy, retention.WithClock(func() time.Time { return now }))
	res, err := job.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}
	if res.SessionsDeleted != 60 {
		t.Errorf("expected 60 sessions deleted, got %d", res.SessionsDeleted)
	}

	close(done)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Errorf("concurrent trace insertion error: %v", err)
		}
	}
}

// Sessions whose only trace attributes merely mention a content word in a label
// (e.g. "frame":"transcript_candidate") must not be re-selected forever and
// starve the purge of newer sessions that still hold real content.
func TestPurgeContent_LabelMentionsDoNotStarveNewerContent(t *testing.T) {
	s := newTestStore(t)
	agentID, versionID, configHash := createTestAgentAndVersion(t, s)
	ctx := context.Background()
	now := time.Now().UTC()

	addSession := func(endedAgo time.Duration, transcript string) string {
		id := storage.NewID()
		ended := now.Add(-endedAgo)
		if err := s.Sessions().Create(ctx, storage.SessionRecord{ID: id, AgentID: agentID, AgentVersionID: versionID, ConfigHash: configHash,
			Status: "ended", StartedAt: ended.Add(-time.Minute), EndedAt: ended, ProviderStackJSON: `{}`, RuntimeSnapshotJSON: `{}`}); err != nil {
			t.Fatal(err)
		}
		turnID := storage.NewID()
		if err := s.Sessions().AddTurn(ctx, storage.TurnRecord{ID: turnID, SessionID: id, Ordinal: 1, StartedAt: ended.Add(-time.Minute), EndedAt: ended,
			Transcript: transcript, LatenciesJSON: `{}`, ToolsJSON: `[]`}); err != nil {
			t.Fatal(err)
		}
		if err := s.Traces().AddSpan(ctx, storage.TraceSpan{ID: storage.NewID(), SessionID: id, TurnID: turnID, Name: "processor.frame",
			StartedAt: ended.Add(-time.Minute), EndedAt: ended,
			AttributesJSON: `{"component":"*transcript.Processor","frame":"transcript_candidate","prompt_tokens":3}`}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i := 0; i < 3; i++ { // older than the batch size, already content-free
		addSession(time.Duration(20+i)*24*time.Hour, "")
	}
	fresh := addSession(10*24*time.Hour, "secret transcript")

	job := retention.NewJob(s.Retention(), retention.Policy{ContentRetention: 7 * 24 * time.Hour, MetadataRetention: 30 * 24 * time.Hour, BatchSize: 2},
		retention.WithClock(func() time.Time { return now }))
	if _, err := job.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	turns, err := s.Sessions().Turns(ctx, fresh)
	if err != nil || len(turns) != 1 {
		t.Fatalf("turns: %v %d", err, len(turns))
	}
	if turns[0].Transcript != "" {
		t.Fatalf("content past retention was not purged: %q", turns[0].Transcript)
	}
}

// Rows that match the content predicate but cannot be sanitized (here a string
// value containing an escaped "text": key) must be passed over, not re-selected
// forever ahead of newer sessions with real content.
func TestPurgeContent_UnsanitizableRowsDoNotStall(t *testing.T) {
	s := newTestStore(t)
	agentID, versionID, configHash := createTestAgentAndVersion(t, s)
	ctx := context.Background()
	now := time.Now().UTC()
	add := func(endedAgo time.Duration, transcript, attrs string) string {
		id := storage.NewID()
		ended := now.Add(-endedAgo)
		if err := s.Sessions().Create(ctx, storage.SessionRecord{ID: id, AgentID: agentID, AgentVersionID: versionID, ConfigHash: configHash,
			Status: "ended", StartedAt: ended.Add(-time.Minute), EndedAt: ended, ProviderStackJSON: `{}`, RuntimeSnapshotJSON: `{}`}); err != nil {
			t.Fatal(err)
		}
		turnID := storage.NewID()
		if err := s.Sessions().AddTurn(ctx, storage.TurnRecord{ID: turnID, SessionID: id, Ordinal: 1, StartedAt: ended.Add(-time.Minute), EndedAt: ended,
			Transcript: transcript, LatenciesJSON: `{}`, ToolsJSON: `[]`}); err != nil {
			t.Fatal(err)
		}
		if err := s.Traces().AddSpan(ctx, storage.TraceSpan{ID: storage.NewID(), SessionID: id, TurnID: turnID, Name: "x",
			StartedAt: ended.Add(-time.Minute), EndedAt: ended, AttributesJSON: attrs}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	for i := 0; i < 3; i++ {
		add(time.Duration(20+i)*24*time.Hour, "", `{"custom.note":"he said \"text\": hi"}`)
	}
	nested := add(15*24*time.Hour, "", `{"custom.payload":{"text":"nested secret"}}`)
	fresh := add(10*24*time.Hour, "secret transcript", `{}`)

	job := retention.NewJob(s.Retention(), retention.Policy{ContentRetention: 7 * 24 * time.Hour, MetadataRetention: 30 * 24 * time.Hour, BatchSize: 2},
		retention.WithClock(func() time.Time { return now }))
	if _, err := job.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if turns, _ := s.Sessions().Turns(ctx, fresh); len(turns) != 1 || turns[0].Transcript != "" {
		t.Fatalf("newer content stalled behind unsanitizable rows: %+v", turns)
	}
	if spans, _ := s.Traces().Spans(ctx, nested, ""); len(spans) != 1 || strings.Contains(spans[0].AttributesJSON, "nested secret") {
		t.Fatalf("nested content key not removed: %+v", spans)
	}
}

// blockingStore stalls a purge until its context ends.
type blockingStore struct{ entered chan struct{} }

func (b blockingStore) PurgeContent(ctx context.Context, _ time.Time, _ int, _ storage.ContentCursor) (storage.ContentPurgeResult, error) {
	close(b.entered)
	<-ctx.Done()
	return storage.ContentPurgeResult{}, ctx.Err()
}

func (b blockingStore) PurgeMetadata(ctx context.Context, _ time.Time, _ int) (storage.PurgeMetadataStats, error) {
	return storage.PurgeMetadataStats{}, ctx.Err()
}

// Stop must end an in-flight run instead of waiting up to its 15 minute budget.
func TestJobStopCancelsInFlightRun(t *testing.T) {
	store := blockingStore{entered: make(chan struct{})}
	job := retention.NewJob(store, retention.Policy{ContentRetention: time.Hour, MetadataRetention: 2 * time.Hour, BatchSize: 10}, retention.WithInterval(time.Millisecond))
	job.Start(context.Background())
	<-store.entered
	stopped := make(chan struct{})
	go func() { job.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop waited for the in-flight run")
	}
}
