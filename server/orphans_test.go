package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"govoice/server/storage"
)

func TestOrphanCutoffByDialect(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	if got := orphanCutoff("sqlite", 4*time.Hour, now); !got.Equal(now) {
		t.Fatalf("sqlite: every active session is an orphan, got cutoff %v", got)
	}
	// Postgres may be shared: only sessions no live call can still be.
	if got, want := orphanCutoff("postgres", 4*time.Hour, now), now.Add(-4*time.Hour-orphanGrace); !got.Equal(want) {
		t.Fatalf("postgres cutoff %v, want %v", got, want)
	}
}

// seedActiveSession leaves one active session of a fresh flow in the store, as a crash would.
func seedActiveSession(t *testing.T, store *storage.Store, started time.Time) string {
	t.Helper()
	ctx := context.Background()
	f := storage.Agent{ID: storage.NewID(), Name: "F", Enabled: true, CreatedAt: time.Now()}
	if err := store.Agents().Create(ctx, f); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, store, f.ID, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := storage.NewID()
	if err := store.Sessions().Create(ctx, storage.SessionRecord{ID: id, AgentID: f.ID, AgentVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: started, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	return id
}

// crashAndRestart leaves a call active in a store, kills the "process", and starts a new server on
// the same store. It returns the new server and the orphan's id.
func crashAndRestart(t *testing.T) (*Server, string) {
	t.Helper()
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "mgmt.db"))
	ctx := context.Background()
	before := apiTestServer(t)
	before.cfg.Storage.Dialect = "sqlite"
	if err := before.EnableManagement(ctx); err != nil {
		t.Fatal(err)
	}
	orphan := seedActiveSession(t, before.store, time.Now().Add(-time.Hour))
	before.closeManagement() // the process dies: nothing ends the call

	s := apiTestServer(t)
	s.cfg.Storage.Dialect = "sqlite"
	if err := s.EnableManagement(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeManagement)
	return s, orphan
}

// A crash leaves its calls active. The next start, on the same store, ends them. (That a call
// started after the cutoff is never ended is pinned in the storage tests.)
func TestStartupEndsTheCallsACrashLeftBehind(t *testing.T) {
	s, orphan := crashAndRestart(t)
	ctx := context.Background()
	got, err := s.store.Sessions().Get(ctx, orphan)
	if err != nil || got.Status != "ended" {
		t.Fatalf("orphan after restart: %+v err %v", got, err)
	}
}

// What the console reads: the orphan is listed as ended, with the note, and no longer as active.
func TestTheAPIShowsTheOrphanAsEnded(t *testing.T) {
	s, orphan := crashAndRestart(t)
	list := func(query string) string {
		w := apiRequest(s, http.MethodGet, "/api/v1/sessions"+query, e2eMgmtKey, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET sessions%s: %d %s", query, w.Code, w.Body)
		}
		return w.Body.String()
	}
	if body := list("?status=active"); strings.Contains(body, orphan) {
		t.Fatalf("the orphan is still listed as active: %s", body)
	}
	body := list("?status=ended")
	if !strings.Contains(body, orphan) || !strings.Contains(body, "Ended when the server restarted.") {
		t.Fatalf("the orphan is not listed as ended with its note: %s", body)
	}
}

func TestAFailingSweepDoesNotStopStartup(t *testing.T) {
	// endOrphanedSessions logs and returns on a storage error; it has no error result.
	s := apiTestServer(t)
	closed, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: filepath.Join(t.TempDir(), "closed.db")})
	if err != nil {
		t.Fatal(err)
	}
	closed.Close() // every query on it fails
	s.endOrphanedSessions(context.Background(), closed)
}

// publishTestVersion publishes config (with the given MCP selection) as the flow's next version.
func publishTestVersion(ctx context.Context, s *storage.Store, agentID string, config []byte, tools []storage.SelectedTool) (storage.AgentVersion, error) {
	v, _, err := s.Agents().MutateAndPublish(ctx, agentID, storage.AgentMutation{Tools: &tools}, config)
	return v, err
}
