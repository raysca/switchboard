package storage

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"
	"govoice/server/migrations"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), Config{Dialect: SQLite, DSN: filepath.Join(t.TempDir(), "voice.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationEmptyAndIdempotentReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voice.db")
	for i := 0; i < 2; i++ {
		s, err := Open(context.Background(), Config{Dialect: SQLite, DSN: path})
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := s.DB().QueryRow("SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&n); err != nil || n != 8 {
			t.Fatalf("version=%d err=%v", n, err)
		}
		s.Close()
	}
}

func TestRepositoryFlowVersionImmutabilityAndHistorySafety(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "A", Enabled: true, CreatedAt: time.Now()}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, a.ID, []byte(`{"b":2,"a":1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.ConfigHash != "43258cff783fe7036d8a43033f830adfc60ec037382473548ac742b888292777" {
		t.Fatalf("hash %s", v.ConfigHash)
	}
	got, err := s.Flows().Current(ctx, a.ID)
	if err != nil || got.CurrentVersionID != v.ID {
		t.Fatalf("current=%+v err=%v", got, err)
	}
	session := SessionRecord{ID: NewID(), FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: time.Now(), Status: "active"}
	if err := s.Sessions().Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("DELETE FROM flow_versions WHERE id=?", v.ID); err == nil {
		t.Fatal("historical version deleted")
	}
	if _, err := s.DB().Exec("UPDATE flow_versions SET config_hash='changed' WHERE id=?", v.ID); err == nil {
		t.Fatal("immutable version updated")
	}
	if err := s.Flows().Archive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.Flows().Current(ctx, a.ID)
	if err != nil || !got.Archived {
		t.Fatalf("archive=%+v err=%v", got, err)
	}
}

func TestSessionAdmissionRejectsDisabledOrArchivedFlow(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, mode := range []string{"disabled", "archived"} {
		t.Run(mode, func(t *testing.T) {
			a := Flow{ID: NewID(), Name: "A", Enabled: true, CreatedAt: time.Now()}
			if err := s.Flows().Create(ctx, a); err != nil {
				t.Fatal(err)
			}
			v, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "archived" {
				if err := s.Flows().Archive(ctx, a.ID); err != nil {
					t.Fatal(err)
				}
			} else if _, err := s.DB().ExecContext(ctx, "UPDATE flows SET enabled=0 WHERE id=?", a.ID); err != nil {
				t.Fatal(err)
			}
			err = s.Sessions().Create(ctx, SessionRecord{ID: NewID(), FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: time.Now(), Status: "active"})
			if err == nil {
				t.Fatal("session admitted after flow became unavailable")
			}
		})
	}
}

func TestSessionEndRetriesPendingDeleteFinalization(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a := Flow{ID: NewID(), Name: "A", Enabled: true, CreatedAt: time.Now()}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := NewID()
	if err := s.Sessions().Create(ctx, SessionRecord{ID: sessionID, FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: time.Now(), Status: "active"}); err != nil {
		t.Fatal(err)
	}
	s.finalizePendingDeletesHook = func(context.Context) error { return errors.New("transient injected failure") }
	if err := s.Sessions().End(ctx, sessionID, EndCallerHungUp); err == nil {
		t.Fatal("expected injected finalization failure")
	}
	var status string
	if err := s.DB().QueryRowContext(ctx, "SELECT status FROM sessions WHERE id=?", sessionID).Scan(&status); err != nil || status != "ended" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	s.finalizePendingDeletesHook = nil
	if err := s.Sessions().End(ctx, sessionID, EndCallerHungUp); err != nil {
		t.Fatalf("retry End: %v", err)
	}
}

func TestRepositoryFlowVersionsAreMonotonicAndFlowListUpdateWork(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "before"}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v1, err := publishTestVersion(ctx, s, a.ID, []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := publishTestVersion(ctx, s, a.ID, []byte(`{"x":2}`), nil)
	if err != nil || v2.Version != v1.Version+1 {
		t.Fatalf("second version=%+v err=%v", v2, err)
	}
	name, desc := "after", "updated"
	if _, _, err := s.Flows().MutateAndPublish(ctx, a.ID, FlowMutation{Name: &name, Description: &desc}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Flows().List(ctx, 10, 0)
	if err != nil || len(got) != 1 || got[0].Name != "after" || got[0].CurrentVersionID != v2.ID {
		t.Fatalf("list=%+v err=%v", got, err)
	}
}

func TestRepositoryTransactionRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := NewID()
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO flows(id,name,created_at,updated_at) VALUES(?,?,?,?)", id, "A", time.Now(), time.Now())
		if err != nil {
			return err
		}
		return sql.ErrTxDone
	})
	if err != sql.ErrTxDone {
		t.Fatalf("rollback err=%v", err)
	}
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM flows WHERE id=?", id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}

func TestStorageConcurrentSQLiteWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Flows().Create(ctx, Flow{ID: NewID(), Name: "A"}) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM flows").Scan(&n); err != nil || n != 20 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}

func TestMigrationConcurrentSQLiteOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(context.Background(), Config{Dialect: SQLite, DSN: path})
			if err == nil {
				err = s.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStorageSQLiteReplacementKeepsPragmas(t *testing.T) {
	s := testStore(t)
	s.DB().SetConnMaxLifetime(time.Nanosecond)
	for i := 0; i < 3; i++ {
		var fk, busy int
		if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := s.DB().QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if fk != 1 || busy < 5000 {
			t.Fatalf("replacement pragmas fk=%d busy=%d", fk, busy)
		}
	}
}

func TestStorageSQLiteRejectsSettingOverrides(t *testing.T) {
	for _, query := range []string{"_fk=off", "_foreign_keys=off", "_timeout=0", "_busy_timeout=0", "_journal=DELETE", "_journal_mode=DELETE", "_pragma=busy_timeout(0)", "_pragma=foreign_keys(OFF)", "_txlock=deferred"} {
		t.Run(query, func(t *testing.T) {
			_, err := sqliteConfiguredDSN("file:test.db?" + query)
			if err == nil {
				t.Fatal("reserved SQLite setting accepted")
			}
		})
	}
	s, err := Open(context.Background(), Config{Dialect: SQLite, DSN: filepath.Join(t.TempDir(), "safe.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.DB().SetConnMaxLifetime(time.Nanosecond)
	var fk, busy int
	var journal string
	if err := s.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if fk != 1 || busy < 5000 || strings.ToLower(journal) != "wal" {
		t.Fatalf("effective settings fk=%d busy=%d journal=%s", fk, busy, journal)
	}
}

func TestMigrationConcurrentSQLiteURI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uri.db")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, dsn := range []string{"file:" + path + "?cache=shared", "file:" + rel + "?cache=shared"} {
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, err := Open(context.Background(), Config{Dialect: SQLite, DSN: dsn})
				if err == nil {
					err = s.Close()
				}
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	expected, err := filepath.Abs("voice.db")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := sqliteLockPath("file:voice.db?cache=shared"); err != nil || got != expected {
		t.Fatalf("opaque uri path=%q err=%v", got, err)
	}
}

func TestMigrationEncodedURILockPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data dir")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "voice.db")
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	canonicalPath := filepath.Join(canonicalDir, "voice.db")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil {
		t.Fatal(err)
	}
	encoded := "file:" + strings.ReplaceAll(rel, " ", "%20") + "?cache=shared"
	for _, dsn := range []string{encoded, "file:" + path + "?cache=shared", path} {
		got, err := sqliteLockPath(dsn)
		if err != nil || got != canonicalPath {
			t.Fatalf("dsn=%q lock=%q err=%v", dsn, got, err)
		}
	}
	if got, err := sqliteLockPath("file::memory:"); err != nil || got != "" {
		t.Fatalf("memory lock=%q err=%v", got, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dsn := encoded
			if i%2 == 0 {
				dsn = "file:" + path + "?cache=shared"
			}
			s, err := Open(context.Background(), Config{Dialect: SQLite, DSN: dsn})
			if err == nil {
				err = s.Close()
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStorageTimestampLexicalOrder(t *testing.T) {
	earlier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 1, 1, 0, 0, 0, 1000, time.UTC)
	if utc(earlier) >= utc(later) {
		t.Fatalf("time sort %q >= %q", utc(earlier), utc(later))
	}
}

func TestStorageTimestampMatchesPostgresPrecision(t *testing.T) {
	got := utc(time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC))
	if got != "2026-01-01T00:00:00.123456Z" {
		t.Fatalf("timestamp=%q", got)
	}
}

func TestStorageRejectsInvalidIDs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"", "bad", "00000000-0000-4000-8000-000000000000", "00000000-0000-7000-0000-000000000000"} {
		if err := s.Flows().Create(ctx, Flow{ID: id, Name: "bad"}); err == nil {
			t.Fatalf("accepted id %q", id)
		}
	}
}

func TestRepositoryRejectsInvalidDurableIDs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "A", Enabled: true}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := "00000000-0000-4000-8000-000000000000"
	cases := []struct {
		name string
		run  func() error
	}{
		{"mcp", func() error { return s.MCP().CreateServer(ctx, MCPServer{ID: bad}) }},
		{"session", func() error {
			return s.Sessions().Create(ctx, SessionRecord{ID: bad, FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash})
		}},
		{"turn", func() error { return s.Sessions().AddTurn(ctx, TurnRecord{ID: bad, SessionID: NewID()}) }},
		{"span", func() error { return s.Traces().AddSpan(ctx, TraceSpan{ID: bad}) }},
		{"event", func() error { return s.Traces().AddEvent(ctx, TraceEvent{ID: bad}) }},
		{"toolcall", func() error { return s.Traces().AddToolCall(ctx, ToolCallRecord{ID: bad, SessionID: NewID()}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil {
				t.Fatal("accepted non-v7 id")
			}
		})
	}
}

func TestRepositoryFlowCreateHonorsFlags(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "archived", Enabled: false, Archived: true}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.Flows().Current(ctx, a.ID)
	if err != nil || got.Enabled || !got.Archived {
		t.Fatalf("flags=%+v err=%v", got, err)
	}
}

func TestRepositoryRejectsCorruptTimestamp(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "A", Enabled: true}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("UPDATE flows SET created_at='not-a-time' WHERE id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Flows().Current(ctx, a.ID); err == nil {
		t.Fatal("corrupt timestamp accepted")
	}
}

func TestRepositoryNormalizesReadTimestampUTC(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "A"}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("UPDATE flows SET created_at='2026-10-04T21:00:00+01:00' WHERE id=?", a.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.Flows().Current(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Location() != time.UTC || got.CreatedAt.Hour() != 20 {
		t.Fatalf("time=%v", got.CreatedAt)
	}
}

func TestRepositoryConfigHashRoundTrip(t *testing.T) {
	run := func(t *testing.T, s *Store) {
		ctx := context.Background()
		a := Flow{ID: NewID(), Name: "hash"}
		if err := s.Flows().Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		v, err := publishTestVersion(ctx, s, a.ID, []byte(`{"z":1e1000,"text":"\u0000","a":1e2}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Flows().Version(ctx, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if string(got.ConfigJSON) != `{"a":1e2,"text":"\u0000","z":1e1000}` || got.ConfigHash != v.ConfigHash {
			t.Fatalf("roundtrip=%s hash=%s want=%s", got.ConfigJSON, got.ConfigHash, v.ConfigHash)
		}
	}
	t.Run("sqlite", func(t *testing.T) { run(t, testStore(t)) })
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			s, err := Open(context.Background(), Config{Dialect: Postgres, DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			run(t, s)
		})
	}
}

func TestStorageUUIDv7(t *testing.T) {
	id := NewID()
	if len(id) != 36 || id[14] != '7' || id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b' {
		t.Fatalf("not UUIDv7: %q", id)
	}
}

func TestStorageCanonicalJSONRejectsTrailingInput(t *testing.T) {
	if _, err := CanonicalJSON([]byte(`{"a":1} garbage`)); err == nil {
		t.Fatal("trailing input accepted")
	}
}

func TestRepositoryPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	s, err := Open(context.Background(), Config{Dialect: Postgres, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := Flow{ID: NewID(), Name: "contract"}
	if err := s.Flows().Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(context.Background(), s, a.ID, []byte(`{"a":1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Flows().Current(context.Background(), a.ID)
	if err != nil || got.CurrentVersionID != v.ID {
		t.Fatalf("current=%+v err=%v", got, err)
	}
}

func TestMigrationUpgradeAndRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := fs.Sub(migrations.Files, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, initial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(context.Background(), Config{Dialect: SQLite, DSN: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB().QueryRow("SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&n); err != nil || n != 8 {
		t.Fatalf("upgrade version=%d err=%v", n, err)
	}
	// Goose runs each migration transactionally: failed DDL must not survive.
	files := fstest.MapFS{}
	for _, name := range []string{"001_initial.sql", "002_indexes.sql", "003_mcp_metadata.sql", "004_mcp_lifecycle.sql", "005_flow_mcp_review.sql", "006_session_draft.sql", "007_session_caller_end_reason.sql", "008_tool_call_error_category.sql"} {
		body, err := fs.ReadFile(initial, name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: body}
	}
	files["009_bad.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE rollback_probe (id INTEGER);\nINSERT INTO absent_table VALUES (1);\n")}
	bad, err := goose.NewProvider(goose.DialectSQLite3, s.DB(), files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Up(context.Background()); err == nil {
		t.Fatal("failed migration accepted")
	}
	if err := s.DB().QueryRow("SELECT count(*) FROM rollback_probe").Scan(&n); err == nil {
		t.Fatal("failed DDL was committed")
	}
}

func TestMigrationPostgresUpgradeRollback(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "storage_" + strings.ReplaceAll(NewID(), "-", "")
	created := false
	t.Cleanup(func() {
		if created {
			if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Errorf("drop test schema: %v", err)
			}
			var count int
			if err := admin.QueryRowContext(context.Background(), "SELECT count(*) FROM pg_namespace WHERE nspname=$1", schema).Scan(&count); err != nil || count != 0 {
				t.Errorf("test schema remains: count=%d err=%v", count, err)
			}
		}
		if err := admin.Close(); err != nil {
			t.Errorf("close test admin DB: %v", err)
		}
	})
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	created = true
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	scoped := parsed.String()
	db, err := sql.Open("pgx", scoped)
	if err != nil {
		t.Fatal(err)
	}
	migrationFS, err := fs.Sub(migrations.Files, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationFS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatalf("PostgreSQL 003->004 upgrade: %v", err)
	}
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("PostgreSQL 004->003 rollback: %v", err)
	}
	if _, err := provider.UpTo(ctx, 5); err != nil {
		t.Fatalf("PostgreSQL re-upgrade through 005: %v", err)
	}
	db.Close()
	s, err := Open(ctx, Config{Dialect: Postgres, DSN: scoped})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err := s.DB().QueryRowContext(ctx, "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&version); err != nil || version != 8 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	files := fstest.MapFS{}
	for _, name := range []string{"001_initial.sql", "002_indexes.sql", "003_mcp_metadata.sql", "004_mcp_lifecycle.sql", "005_flow_mcp_review.sql", "006_session_draft.sql", "007_session_caller_end_reason.sql", "008_tool_call_error_category.sql"} {
		body, err := fs.ReadFile(migrationFS, name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: body}
	}
	files["009_bad.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE rollback_probe (id INTEGER);\nINSERT INTO absent_table VALUES (1);\n")}
	bad, err := goose.NewProvider(goose.DialectPostgres, s.DB(), files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Up(ctx); err == nil {
		t.Fatal("bad migration passed")
	}
	var table sql.NullString
	if err := s.DB().QueryRowContext(ctx, "SELECT to_regclass('rollback_probe')").Scan(&table); err != nil || table.Valid {
		t.Fatalf("rolled-back table=%v err=%v", table, err)
	}
}

func TestRepositoryMCPAndHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "A", Enabled: true}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, a.ID, []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := MCPServer{ID: NewID(), Name: "M", Endpoint: "https://m.example", Transport: "streamable-http", AuthEnv: "MCP_TOKEN_X", Enabled: true}
	if err := s.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	d, err := s.MCP().AddDiscovery(ctx, m.ID, []MCPTool{{RemoteName: "z", SchemaJSON: []byte(`{"type":"object"}`)}, {RemoteName: "a", SchemaJSON: []byte(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	gotD, err := s.MCP().Discovery(ctx, d.ID)
	if err != nil || len(gotD.Tools) != 2 || gotD.Tools[0].RemoteName != "a" {
		t.Fatalf("discovery=%+v err=%v", gotD, err)
	}
	v, err = publishTestVersion(ctx, s, a.ID, []byte(`{"x":2}`), []SelectedTool{{ServerID: m.ID, RemoteName: "a", SchemaHash: gotD.Tools[0].SchemaHash, Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	sess := SessionRecord{ID: NewID(), FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, Status: "active"}
	if err := s.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	turn := TurnRecord{ID: NewID(), SessionID: sess.ID, Ordinal: 1}
	if err := s.Sessions().AddTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	if err := s.Traces().AddSpan(ctx, TraceSpan{ID: NewID(), SessionID: sess.ID, TurnID: turn.ID, Name: "turn"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Traces().AddEvent(ctx, TraceEvent{ID: NewID(), SessionID: sess.ID, TurnID: turn.ID, Name: "turn.completed"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Traces().AddToolCall(ctx, ToolCallRecord{ID: NewID(), SessionID: sess.ID, TurnID: turn.ID, MCPServerID: m.ID, RemoteName: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("DELETE FROM mcp_servers WHERE id=?", m.ID); err == nil {
		t.Fatal("live MCP reference deleted")
	}
	if _, err := publishTestVersion(ctx, s, a.ID, []byte(`{"x":3}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Sessions().End(ctx, sess.ID, EndCallerHungUp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("DELETE FROM mcp_servers WHERE id=?", m.ID); err != nil {
		t.Fatalf("detached MCP delete: %v", err)
	}
	preserved, err := s.MCP().Discovery(ctx, d.ID)
	if err != nil || preserved.DiscoveryHash != d.DiscoveryHash {
		t.Fatalf("historical discovery=%+v err=%v", preserved, err)
	}
	if _, err := s.DB().Exec("UPDATE mcp_tools SET schema_hash='changed' WHERE discovery_id=? AND remote_name='a'", d.ID); err == nil {
		t.Fatal("historical tool schema changed")
	}
	if _, err := s.DB().Exec("DELETE FROM mcp_tools WHERE discovery_id=? AND remote_name='a'", d.ID); err == nil {
		t.Fatal("historical tool removed")
	}
}

func TestMCPDisableAndDeleteRespectCurrentAndActiveVersionReferences(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := MCPServer{ID: NewID(), Name: "pinned", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true}
	if err := s.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	discovery, err := s.MCP().AddDiscovery(ctx, m.ID, []MCPTool{{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	a := Flow{ID: NewID(), Name: "uses-mcp", Enabled: true}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, a.ID, []byte(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`), []SelectedTool{{ServerID: m.ID, RemoteName: "lookup", SchemaHash: discovery.Tools[0].SchemaHash, Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := NewID()
	if err := s.Sessions().Create(ctx, SessionRecord{ID: sessionID, FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: time.Now(), Status: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MCP().DeleteServer(ctx, m.ID); err == nil {
		t.Fatal("delete of current association was allowed")
	}
	if err := s.MCP().DisableServer(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	disabled, err := s.MCP().Server(ctx, m.ID)
	if err != nil || disabled.Enabled || disabled.LatestDiscoveryID != discovery.ID {
		t.Fatalf("disable changed catalog: %+v %v", disabled, err)
	}
	if err := s.MCP().DeleteServer(ctx, m.ID); err == nil {
		t.Fatal("disabled server with current association was deleted")
	}
	if err := s.MCP().DetachAndDelete(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MCP().Server(ctx, m.ID); err != nil {
		t.Fatalf("server with an active historical snapshot should be pending: %v", err)
	}
	if err := s.MCP().DeleteServer(ctx, m.ID); err == nil {
		t.Fatal("direct delete bypassed active runtime reference")
	}
	if err := s.Sessions().End(ctx, sessionID, EndCallerHungUp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MCP().Server(ctx, m.ID); err == nil {
		t.Fatal("pending delete was not finalized after the last active session ended")
	}
	var count int
	if err := s.DB().QueryRow("SELECT count(*) FROM flow_mcp_tools WHERE flow_version_id=?", v.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("historical selection count=%d err=%v", count, err)
	}
}

func TestConcurrentSessionCloseAndMCPDeleteAlwaysDrainsSafely(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 10; i++ {
		m := MCPServer{ID: NewID(), Name: "race", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true, ValidationState: "healthy"}
		if err := s.MCP().CreateServer(ctx, m); err != nil {
			t.Fatal(err)
		}
		d, err := s.MCP().AddDiscovery(ctx, m.ID, []MCPTool{{RemoteName: "tool", SchemaJSON: []byte(`{"type":"object"}`)}})
		if err != nil {
			t.Fatal(err)
		}
		a := Flow{ID: NewID(), Name: "race", Enabled: true}
		if err := s.Flows().Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		v, err := publishTestVersion(ctx, s, a.ID, []byte(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`), []SelectedTool{{ServerID: m.ID, RemoteName: "tool", SchemaHash: d.Tools[0].SchemaHash, Required: true}})
		if err != nil {
			t.Fatal(err)
		}
		sessionID := NewID()
		if err := s.Sessions().Create(ctx, SessionRecord{ID: sessionID, FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, Status: "active"}); err != nil {
			t.Fatal(err)
		}
		if err := s.MCP().DetachAndDelete(ctx, m.ID); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		errs := make(chan error, 2)
		go func() { defer wg.Done(); errs <- s.Sessions().End(ctx, sessionID, EndCallerHungUp) }()
		go func() {
			defer wg.Done()
			err := s.MCP().DeleteServer(ctx, m.ID)
			if errors.Is(err, ErrMCPReferenced) || errors.Is(err, sql.ErrNoRows) {
				err = nil
			}
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("iteration %d concurrent close/delete: %v", i, err)
			}
		}
		if _, err := s.MCP().Server(ctx, m.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("iteration %d MCP row survived drain: %v", i, err)
		}
	}
}

func TestRepositoryDiscoveryRequiresLiveServer(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.MCP().AddDiscovery(ctx, NewID(), []MCPTool{{RemoteName: "a", SchemaJSON: []byte(`{}`)}}); err == nil {
		t.Fatal("orphan discovery accepted")
	}
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM mcp_discoveries").Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphan rows=%d err=%v", n, err)
	}
}

func TestRepositoryPublishRejectsUnknownToolAtomically(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "A"}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	m := MCPServer{ID: NewID(), Name: "M", Transport: "streamable-http", Enabled: true}
	if err := s.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	d, err := s.MCP().AddDiscovery(ctx, m.ID, []MCPTool{{RemoteName: "known", SchemaJSON: []byte(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, selection := range []SelectedTool{{ServerID: m.ID, RemoteName: "missing", SchemaHash: d.Tools[0].SchemaHash}, {ServerID: m.ID, RemoteName: "known", SchemaHash: "bad"}, {ServerID: NewID(), RemoteName: "known", SchemaHash: d.Tools[0].SchemaHash}} {
		if _, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), []SelectedTool{selection}); err == nil {
			t.Fatalf("accepted selection %+v", selection)
		}
	}
	got, err := s.Flows().Current(ctx, a.ID)
	if err != nil || got.CurrentVersionID != "" {
		t.Fatalf("pointer changed %+v err=%v", got, err)
	}
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM flow_versions WHERE flow_id=?", a.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("versions=%d err=%v", n, err)
	}
}

func TestRepositoryProvenanceAndPointers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "A"}
	b := Flow{ID: NewID(), Name: "B"}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Flows().Create(ctx, b); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []SessionRecord{{ID: NewID(), FlowID: b.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, Status: "active"}, {ID: NewID(), FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: "wrong", Status: "active"}} {
		if err := s.Sessions().Create(ctx, session); err == nil {
			t.Fatalf("accepted mismatched session %+v", session)
		}
	}
	if _, err := s.DB().Exec("UPDATE flows SET current_version_id=? WHERE id=?", v.ID, b.ID); err == nil {
		t.Fatal("cross-flow current version accepted")
	}
	if _, err := s.DB().Exec("INSERT INTO flows(id,name,current_version_id,created_at,updated_at) VALUES(?,?,?,?,?)", NewID(), "bad", v.ID, utc(time.Now()), utc(time.Now())); err == nil {
		t.Fatal("invalid insert current version accepted")
	}
	m := MCPServer{ID: NewID(), Name: "M"}
	other := MCPServer{ID: NewID(), Name: "O"}
	if err := s.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.MCP().CreateServer(ctx, other); err != nil {
		t.Fatal(err)
	}
	d, err := s.MCP().AddDiscovery(ctx, m.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("UPDATE mcp_servers SET latest_discovery_id=? WHERE id=?", d.ID, other.ID); err == nil {
		t.Fatal("cross-server discovery pointer accepted")
	}
	if _, err := s.DB().Exec("INSERT INTO mcp_servers(id,name,endpoint,transport,enabled,validation_state,latest_discovery_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)", NewID(), "bad", "", "", 0, "", d.ID, utc(time.Now()), utc(time.Now())); err == nil {
		t.Fatal("invalid insert discovery pointer accepted")
	}
}

func TestRepositoryContract(t *testing.T) {
	cases := []struct {
		name string
		open func(*testing.T) *Store
	}{{"sqlite", testStore}}
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		cases = append(cases, struct {
			name string
			open func(*testing.T) *Store
		}{"postgres", func(t *testing.T) *Store {
			s, err := Open(context.Background(), Config{Dialect: Postgres, DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			return s
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.open(t)
			ctx := context.Background()
			a := Flow{ID: NewID(), Name: "contract", Enabled: true}
			if err := s.Flows().Create(ctx, a); err != nil {
				t.Fatal(err)
			}
			m := MCPServer{ID: NewID(), Name: "m", Enabled: true}
			if err := s.MCP().CreateServer(ctx, m); err != nil {
				t.Fatal(err)
			}
			d, err := s.MCP().AddDiscovery(ctx, m.ID, []MCPTool{{RemoteName: "z", SchemaJSON: []byte(`{"type":"object"}`)}, {RemoteName: "a", SchemaJSON: []byte(`{"n":1e2}`)}})
			if err != nil {
				t.Fatal(err)
			}
			gotD, err := s.MCP().Discovery(ctx, d.ID)
			if err != nil || len(gotD.Tools) != 2 || gotD.Tools[0].RemoteName != "a" || string(gotD.Tools[0].SchemaJSON) != `{"n":1e2}` {
				t.Fatalf("discovery=%+v err=%v", gotD, err)
			}
			v, err := publishTestVersion(ctx, s, a.ID, []byte(`{"b":2,"a":1}`), []SelectedTool{{ServerID: m.ID, RemoteName: "a", SchemaHash: gotD.Tools[0].SchemaHash}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE flow_versions SET config_hash=? WHERE id=?"), "bad", v.ID); err == nil {
				t.Fatal("immutable version changed")
			}
			sess := SessionRecord{ID: NewID(), FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, Status: "active"}
			if err := s.Sessions().Create(ctx, sess); err != nil {
				t.Fatal(err)
			}
			active := sess.ID
			sess.ConfigHash = "bad"
			sess.ID = NewID()
			if err := s.Sessions().Create(ctx, sess); err == nil {
				t.Fatal("mismatched provenance")
			}
			turn := TurnRecord{ID: NewID(), SessionID: active, Ordinal: 1}
			if err := s.Sessions().AddTurn(ctx, turn); err != nil {
				t.Fatal(err)
			}
			if err := s.Traces().AddSpan(ctx, TraceSpan{ID: NewID(), SessionID: active, TurnID: turn.ID, Name: "turn"}); err != nil {
				t.Fatal(err)
			}
			if err := s.Traces().AddEvent(ctx, TraceEvent{ID: NewID(), SessionID: active, TurnID: turn.ID, Name: "done"}); err != nil {
				t.Fatal(err)
			}
			first := NewID()
			second := NewID()
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for _, event := range []TraceEvent{{ID: first, SessionID: active, Name: "boundary", OccurredAt: base}, {ID: second, SessionID: active, Name: "boundary", OccurredAt: base.Add(time.Microsecond)}} {
				if err := s.Traces().AddEvent(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := s.DB().QueryContext(ctx, s.bind("SELECT id FROM trace_events WHERE session_id=? AND name=? ORDER BY occurred_at"), active, "boundary")
			if err != nil {
				t.Fatal(err)
			}
			var ordered []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				ordered = append(ordered, id)
			}
			rows.Close()
			if len(ordered) != 2 || ordered[0] != first || ordered[1] != second {
				t.Fatalf("timestamp order=%v", ordered)
			}
			if err := s.Traces().AddToolCall(ctx, ToolCallRecord{ID: NewID(), SessionID: active, TurnID: turn.ID, MCPServerID: m.ID, RemoteName: "a"}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().ExecContext(ctx, s.bind("DELETE FROM mcp_servers WHERE id=?"), m.ID); err == nil {
				t.Fatal("current MCP deleted")
			}
			v2, err := publishTestVersion(ctx, s, a.ID, []byte(`{}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE sessions SET flow_version_id=?,config_hash=? WHERE id=?"), v2.ID, v2.ConfigHash, active); err == nil {
				t.Fatal("session provenance mutated")
			}
			if err := s.Sessions().End(ctx, active, EndCallerHungUp); err != nil {
				t.Fatal(err)
			}
			if err := s.Sessions().End(ctx, active, EndCallerHungUp); err != nil {
				t.Fatalf("idempotent session end: %v", err)
			}
			if _, err := s.DB().ExecContext(ctx, s.bind("DELETE FROM mcp_servers WHERE id=?"), m.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.MCP().Discovery(ctx, d.ID); err != nil {
				t.Fatal("historical discovery lost:", err)
			}
			var wg sync.WaitGroup
			writeErrors := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); writeErrors <- s.Flows().Create(ctx, Flow{ID: NewID(), Name: "concurrent"}) }()
			}
			wg.Wait()
			close(writeErrors)
			for err := range writeErrors {
				if err != nil {
					t.Fatal("concurrent write:", err)
				}
			}
		})
	}
}

// The credential table is gone; secrets live in the environment. MCP servers
// still name their bearer token variable.
func TestSchemaHasNoCredentialTable(t *testing.T) {
	check := func(t *testing.T, s *Store) {
		t.Helper()
		if _, err := s.DB().ExecContext(context.Background(), "SELECT 1 FROM provider_credentials"); err == nil {
			t.Fatal("provider_credentials still exists")
		}
		if _, err := s.DB().ExecContext(context.Background(), "SELECT auth_env FROM mcp_servers"); err != nil {
			t.Fatalf("mcp_servers.auth_env: %v", err)
		}
	}
	t.Run("sqlite", func(t *testing.T) { check(t, testStore(t)) })
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN not set")
		}
		s, err := Open(context.Background(), Config{Dialect: Postgres, DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		check(t, s)
	})
}

// A database from the stored-credentials era keeps provider_credentials (the
// schema changed in place, nothing is migrated): refuse it with a clear message.
func TestOpenRefusesPreEnvironmentCredentialsDatabase(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(context.Background(), Config{Dialect: SQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(context.Background(), "CREATE TABLE provider_credentials (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(context.Background(), Config{Dialect: SQLite, DSN: dsn}); !errors.Is(err, ErrPreEnvironmentCredentials) {
		t.Fatalf("err = %v", err)
	}
}

// publishTestVersion publishes config (with the given MCP selection) as the flow's next version.
func publishTestVersion(ctx context.Context, s *Store, flowID string, config []byte, tools []SelectedTool) (FlowVersion, error) {
	v, _, err := s.Flows().MutateAndPublish(ctx, flowID, FlowMutation{Tools: &tools}, config)
	return v, err
}
