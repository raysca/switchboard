package session_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"govoice/server/session"
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

func createTestAgentAndVersion(t *testing.T, s *storage.Store) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	agentID := storage.NewID()
	a := storage.Agent{ID: agentID, Name: "SessionAgent", Enabled: true, CreatedAt: time.Now()}
	if err := s.Agents().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := s.Agents().AddVersion(ctx, agentID, 1, []byte(`{"llm":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	return agentID, v.ID, v.ConfigHash
}

func TestSessionService(t *testing.T) {
	s := newTestStore(t)
	svc := session.NewService(s.Sessions())

	ctx := context.Background()
	agentID, vID, hash := createTestAgentAndVersion(t, s)
	sessID := storage.NewID()

	rec := storage.SessionRecord{
		ID:                  sessID,
		AgentID:             agentID,
		AgentVersionID:      vID,
		ConfigHash:          hash,
		Status:              "active",
		StartedAt:           time.Now().UTC(),
		ProviderStackJSON:   `{}`,
		RuntimeSnapshotJSON: `{}`,
	}
	if err := s.Sessions().Create(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// 1. Get
	got, err := svc.Get(ctx, sessID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != sessID {
		t.Fatalf("id mismatch: got %s, want %s", got.ID, sessID)
	}

	// 2. List
	items, err := svc.List(ctx, storage.SessionFilter{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}

	// 3. AddTurn and Turns
	turnID := storage.NewID()
	turn := storage.TurnRecord{
		ID:            turnID,
		SessionID:     sessID,
		Ordinal:       1,
		StartedAt:     time.Now().UTC(),
		Transcript:    "hello",
		AssistantText: "hi",
	}
	if err := svc.AddTurn(ctx, turn); err != nil {
		t.Fatalf("add turn: %v", err)
	}

	turns, err := svc.Turns(ctx, sessID)
	if err != nil {
		t.Fatalf("turns: %v", err)
	}
	if len(turns) != 1 || turns[0].Transcript != "hello" {
		t.Fatalf("unexpected turns: %+v", turns)
	}

	// 4. End
	if err := svc.End(ctx, sessID); err != nil {
		t.Fatalf("end: %v", err)
	}
	ended, err := svc.Get(ctx, sessID)
	if err != nil {
		t.Fatalf("get ended: %v", err)
	}
	if ended.Status != "ended" {
		t.Fatalf("status not ended: %s", ended.Status)
	}
}
