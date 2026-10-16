package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"
)

// eachStore runs fn on SQLite and, when TEST_POSTGRES_DSN is set, on Postgres, where the orphan
// SQL (TIMESTAMPTZ comparison, correlated subquery, CASE with a bound parameter) is the part that
// depends on the dialect. A shared Postgres database may hold other tests' sessions, so the tests
// assert on their own sessions and treat counts as lower bounds.
func eachStore(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) { fn(t, testStore(t)) })
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			s, err := Open(context.Background(), Config{Dialect: Postgres, DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			fn(t, s)
		})
	}
}

// seedSession makes one session of a fresh flow, started at `started`, with the given status.
func seedSession(t *testing.T, s *Store, started time.Time, status string) (sessionID string) {
	t.Helper()
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "A", Enabled: true, CreatedAt: time.Now()}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := NewID()
	if err := s.Sessions().Create(ctx, SessionRecord{ID: id, FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: started, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE sessions SET status=? WHERE id=?"), status, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func sessionOf(t *testing.T, s *Store, id string) SessionRecord {
	t.Helper()
	r, err := s.Sessions().Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEndOrphansEndsOldActiveAndDrainingSessions(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		old := time.Now().Add(-2 * time.Hour)
		active, draining := seedSession(t, s, old, "active"), seedSession(t, s, old, "draining")
		n, err := s.Sessions().EndOrphans(context.Background(), time.Now())
		if err != nil || n < 2 {
			t.Fatalf("ended %d, err %v", n, err)
		}
		for _, id := range []string{active, draining} {
			r := sessionOf(t, s, id)
			if r.Status != "ended" || r.EndedAt.IsZero() {
				t.Fatalf("%s: %+v", id, r)
			}
			if r.SafeSummary != "Ended when the server restarted." {
				t.Fatalf("note %q", r.SafeSummary)
			}
		}
	})
}

func TestEndOrphansNeverTouchesALiveOrLaterSession(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		cutoff := time.Now().Add(-time.Hour)
		live := seedSession(t, s, time.Now(), "active")                  // started after the cutoff
		boundary := seedSession(t, s, cutoff.Add(time.Second), "active") // just after it
		if _, err := s.Sessions().EndOrphans(context.Background(), cutoff); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{live, boundary} {
			if r := sessionOf(t, s, id); r.Status != "active" || !r.EndedAt.IsZero() {
				t.Fatalf("%s was changed: %+v", id, r)
			}
		}
	})
}

func TestEndOrphansEndsAtTheLastTurnNotAtNow(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		started := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Microsecond)
		withTurns, without := seedSession(t, s, started, "active"), seedSession(t, s, started, "active")
		lastEnd := started.Add(5 * time.Minute)
		for i, end := range []time.Time{started.Add(2 * time.Minute), lastEnd} {
			if err := s.Sessions().AddTurn(ctx, TurnRecord{ID: NewID(), SessionID: withTurns, Ordinal: i + 1, StartedAt: started.Add(time.Duration(i) * time.Minute), EndedAt: end}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Sessions().EndOrphans(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got := sessionOf(t, s, withTurns).EndedAt; !got.Equal(lastEnd) {
			t.Fatalf("ended at %v, want the last turn's end %v", got, lastEnd)
		}
		if got := sessionOf(t, s, without).EndedAt; !got.Equal(started) {
			t.Fatalf("ended at %v, want the start %v", got, started)
		}
	})
}

func TestEndOrphansKeepsEndedSessionsAndExistingNotes(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		old := time.Now().Add(-2 * time.Hour)
		ended := seedSession(t, s, old, "active")
		if err := s.Sessions().End(ctx, ended, EndCallerHungUp); err != nil {
			t.Fatal(err)
		}
		before := sessionOf(t, s, ended)
		noted := seedSession(t, s, old, "active")
		if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE sessions SET safe_summary=? WHERE id=?"), "caller asked about billing", noted); err != nil {
			t.Fatal(err)
		}
		if n, err := s.Sessions().EndOrphans(ctx, time.Now()); err != nil || n < 1 {
			t.Fatalf("ended %d, err %v", n, err)
		}
		if after := sessionOf(t, s, ended); after.Status != "ended" || !after.EndedAt.Equal(before.EndedAt) || after.SafeSummary != before.SafeSummary {
			t.Fatalf("an ended session changed: %+v -> %+v", before, after)
		}
		if r := sessionOf(t, s, noted); r.Status != "ended" || r.SafeSummary != "caller asked about billing" {
			t.Fatalf("an existing note was replaced: %+v", r)
		}
	})
}

// An MCP server whose delete is deferred because a call pins it is deleted once the dead call is ended.
func TestEndOrphansUnblocksAnMCPServerPinnedOnlyByTheDeadCall(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		m := MCPServer{ID: NewID(), Name: "pinned", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true, ValidationState: "healthy"}
		if err := s.MCP().CreateServer(ctx, m); err != nil {
			t.Fatal(err)
		}
		d, err := s.MCP().AddDiscovery(ctx, m.ID, []MCPTool{{RemoteName: "tool", SchemaJSON: []byte(`{"type":"object"}`)}})
		if err != nil {
			t.Fatal(err)
		}
		a := Flow{ID: NewID(), Name: "pin", Enabled: true}
		if err := s.Flows().Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		v, err := publishTestVersion(ctx, s, a.ID, []byte(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`), []SelectedTool{{ServerID: m.ID, RemoteName: "tool", SchemaHash: d.Tools[0].SchemaHash, Required: true}})
		if err != nil {
			t.Fatal(err)
		}
		sessionID := NewID()
		if err := s.Sessions().Create(ctx, SessionRecord{ID: sessionID, FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: time.Now().Add(-2 * time.Hour), Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if err := s.MCP().DetachAndDelete(ctx, m.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MCP().Server(ctx, m.ID); err != nil {
			t.Fatalf("the server must survive while the dead call still pins it: %v", err)
		}
		if _, err := s.Sessions().EndOrphans(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MCP().Server(ctx, m.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("the MCP server must be deleted once the orphan ended: %v", err)
		}
	})
}
