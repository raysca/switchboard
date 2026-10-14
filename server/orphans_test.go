package server

import (
	"context"
	"path/filepath"
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
	// A missing limit falls back to the server's default longest call.
	if got, want := orphanCutoff("postgres", 0, now), now.Add(-defaultMaxCallDuration-orphanGrace); !got.Equal(want) {
		t.Fatalf("postgres default cutoff %v, want %v", got, want)
	}
}

// seedActiveSession leaves one active session of a fresh flow in the store, as a crash would.
func seedActiveSession(t *testing.T, store *storage.Store, started time.Time) string {
	t.Helper()
	ctx := context.Background()
	f := storage.Flow{ID: storage.NewID(), Name: "F", Enabled: true, CreatedAt: time.Now()}
	if err := store.Flows().Create(ctx, f); err != nil {
		t.Fatal(err)
	}
	v, err := store.Flows().PublishVersion(ctx, f.ID, 1, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := storage.NewID()
	if err := store.Sessions().Create(ctx, storage.SessionRecord{ID: id, FlowID: f.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: started, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	return id
}

// A crash leaves its calls active. The next start, on the same store, ends them; a call started
// after the restart is not touched.
func TestStartupEndsTheCallsACrashLeftBehind(t *testing.T) {
	t.Setenv("GOVOICE_DATABASE_DSN", filepath.Join(t.TempDir(), "mgmt.db"))
	ctx := context.Background()

	// Before the crash: a call left active in the store.
	before := apiTestServer(t)
	before.cfg.Storage.Dialect = "sqlite"
	if err := before.EnableManagement(ctx); err != nil {
		t.Fatal(err)
	}
	orphan := seedActiveSession(t, before.store, time.Now().Add(-time.Hour))
	before.closeManagement() // the process dies: nothing ends the call

	// After the restart.
	s := apiTestServer(t)
	s.cfg.Storage.Dialect = "sqlite"
	if err := s.EnableManagement(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeManagement)
	got, err := s.store.Sessions().Get(ctx, orphan)
	if err != nil || got.Status != "ended" {
		t.Fatalf("orphan after restart: %+v err %v", got, err)
	}
	fresh := seedActiveSession(t, s.store, time.Now())
	if r, _ := s.store.Sessions().Get(ctx, fresh); r.Status != "active" {
		t.Fatalf("a call started after the restart was ended: %+v", r)
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
