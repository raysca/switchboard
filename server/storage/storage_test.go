package storage

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
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
		if err := s.DB().QueryRow("SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&n); err != nil || n != 2 {
			t.Fatalf("version=%d err=%v", n, err)
		}
		s.Close()
	}
}

func TestRepositoryAgentVersionImmutabilityAndHistorySafety(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Agent{ID: NewID(), Name: "A", CreatedAt: time.Now()}
	if err := s.Agents().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := s.Agents().AddVersion(ctx, a.ID, 1, []byte(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.ConfigHash != "43258cff783fe7036d8a43033f830adfc60ec037382473548ac742b888292777" {
		t.Fatalf("hash %s", v.ConfigHash)
	}
	if _, err := s.Agents().AddVersion(ctx, a.ID, 1, []byte(`{"a":2}`)); err == nil {
		t.Fatal("duplicate version accepted")
	}
	got, err := s.Agents().Current(ctx, a.ID)
	if err != nil || got.CurrentVersionID != v.ID {
		t.Fatalf("current=%+v err=%v", got, err)
	}
	session := SessionRecord{ID: NewID(), AgentID: a.ID, AgentVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: time.Now(), Status: "active"}
	if err := s.Sessions().Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("DELETE FROM agent_versions WHERE id=?", v.ID); err == nil {
		t.Fatal("historical version deleted")
	}
	if _, err := s.DB().Exec("UPDATE agent_versions SET config_hash='changed' WHERE id=?", v.ID); err == nil {
		t.Fatal("immutable version updated")
	}
	if err := s.Agents().Archive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	got, err = s.Agents().Current(ctx, a.ID)
	if err != nil || !got.Archived {
		t.Fatalf("archive=%+v err=%v", got, err)
	}
}

func TestRepositoryTransactionRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := NewID()
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO agents(id,name,created_at,updated_at) VALUES(?,?,?,?)", id, "A", time.Now(), time.Now())
		if err != nil {
			return err
		}
		return sql.ErrTxDone
	})
	if err != sql.ErrTxDone {
		t.Fatalf("rollback err=%v", err)
	}
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM agents WHERE id=?", id).Scan(&n); err != nil || n != 0 {
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
		go func() { defer wg.Done(); errs <- s.Agents().Create(ctx, Agent{ID: NewID(), Name: "A"}) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.DB().QueryRow("SELECT count(*) FROM agents").Scan(&n); err != nil || n != 20 {
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
	a := Agent{ID: NewID(), Name: "contract"}
	if err := s.Agents().Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	v, err := s.Agents().AddVersion(context.Background(), a.ID, 1, []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Agents().Current(context.Background(), a.ID)
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
	if err := s.DB().QueryRow("SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&n); err != nil || n != 2 {
		t.Fatalf("upgrade version=%d err=%v", n, err)
	}
	// Goose runs each migration transactionally: failed DDL must not survive.
	files := fstest.MapFS{}
	for _, name := range []string{"001_initial.sql", "002_indexes.sql"} {
		body, err := fs.ReadFile(initial, name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = &fstest.MapFile{Data: body}
	}
	files["003_bad.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE rollback_probe (id INTEGER);\nINSERT INTO absent_table VALUES (1);\n")}
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

func TestRepositoryCredentialMCPAndHistory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Agent{ID: NewID(), Name: "A"}
	if err := s.Agents().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := s.Agents().AddVersion(ctx, a.ID, 1, []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	cred := Credential{ID: NewID(), Provider: "xai", MaskedSuffix: "1234", ValidationState: "valid", Ciphertext: []byte{1, 2, 3}, KeyID: "k1"}
	if err := s.Credentials().Put(ctx, cred); err != nil {
		t.Fatal(err)
	}
	gotCred, err := s.Credentials().Get(ctx, cred.ID)
	if err != nil || gotCred.KeyID != "k1" || len(gotCred.Ciphertext) != 3 {
		t.Fatalf("credential=%+v err=%v", gotCred, err)
	}
	m := MCPServer{ID: NewID(), Name: "M", Endpoint: "https://m.example", Transport: "streamable-http", AuthCredentialID: cred.ID, Enabled: true}
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
	if err := s.MCP().SelectTool(ctx, v.ID, m.ID, "a", gotD.Tools[0].SchemaHash, true); err != nil {
		t.Fatal(err)
	}
	sess := SessionRecord{ID: NewID(), AgentID: a.ID, AgentVersionID: v.ID, ConfigHash: v.ConfigHash, Status: "active"}
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
	if _, err := s.Agents().AddVersion(ctx, a.ID, 2, []byte(`{"x":2}`)); err != nil {
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
