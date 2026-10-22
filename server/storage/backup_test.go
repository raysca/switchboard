package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// TestSQLite_VacuumIntoBackupAndRestore drills an online backup using SQLite's VACUUM INTO
// while concurrent writes (session/turn creation) are occurring, then opens the backup as a
// new store to verify schema version, records, and foreign key integrity.
func TestSQLite_VacuumIntoBackupAndRestore(t *testing.T) {
	tmpDir := t.TempDir()
	primaryPath := filepath.Join(tmpDir, "primary.db")
	backupPath := filepath.Join(tmpDir, "backup.db")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := Open(ctx, Config{Dialect: SQLite, DSN: primaryPath})
	if err != nil {
		t.Fatalf("open primary: %v", err)
	}
	defer s.Close()

	// Seed initial agent and version
	fl := Agent{
		ID:        NewID(),
		Name:      "Backup-Agent",
		Enabled:   true,
		CreatedAt: time.Now(),
	}
	if err := s.Agents().Create(ctx, fl); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	ver, err := publishTestVersion(ctx, s, fl.ID, []byte(`{"voice":"default","prompt":"hello"}`), nil)
	if err != nil {
		t.Fatalf("add version: %v", err)
	}

	// Launch concurrent writers inserting sessions and turns
	var wg sync.WaitGroup
	stopWrites := make(chan struct{})
	writeErrCh := make(chan error, 10)

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			turnCount := 0
			for {
				select {
				case <-stopWrites:
					return
				default:
					turnCount++
					sess := SessionRecord{
						ID:             NewID(),
						AgentID:        fl.ID,
						AgentVersionID: ver.ID,
						ConfigHash:     ver.ConfigHash,
						StartedAt:      time.Now(),
						Status:         "active",
					}
					if err := s.Sessions().Create(ctx, sess); err != nil {
						select {
						case writeErrCh <- fmt.Errorf("worker %d create session: %w", workerID, err):
						default:
						}
						return
					}
					tRec := TurnRecord{
						ID:        NewID(),
						SessionID: sess.ID,
						Ordinal:   turnCount,
						StartedAt: time.Now(),
					}
					if err := s.Sessions().AddTurn(ctx, tRec); err != nil {
						select {
						case writeErrCh <- fmt.Errorf("worker %d add turn: %w", workerID, err):
						default:
						}
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		}(i)
	}

	// Let some writes take place
	time.Sleep(100 * time.Millisecond)

	// Execute online backup via VACUUM INTO
	vacuumQuery := fmt.Sprintf("VACUUM INTO '%s'", backupPath)
	if _, err := s.DB().ExecContext(ctx, vacuumQuery); err != nil {
		t.Fatalf("VACUUM INTO failed: %v", err)
	}

	// Stop writers and wait
	close(stopWrites)
	wg.Wait()
	close(writeErrCh)

	for wErr := range writeErrCh {
		t.Fatalf("concurrent writer failed: %v", wErr)
	}

	// Verify backup file exists and has non-zero size
	fi, err := os.Stat(backupPath)
	if err != nil {
		t.Fatalf("stat backup file: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("backup file size is 0 bytes")
	}

	// Open the restored backup with Open() (which validates lock, ping, and Goose schema migrations)
	backupStore, err := Open(ctx, Config{Dialect: SQLite, DSN: backupPath})
	if err != nil {
		t.Fatalf("open restored backup: %v", err)
	}
	defer backupStore.Close()

	// Verify applied migration version
	diag := backupStore.Diagnostics(ctx)
	if diag.MigrationVersion < 5 {
		t.Fatalf("expected migration version >= 5 in restored backup, got %d", diag.MigrationVersion)
	}

	// Verify agent exists
	restoredFlow, err := backupStore.Agents().Current(ctx, fl.ID)
	if err != nil {
		t.Fatalf("fetch agent in restored store: %v", err)
	}
	if restoredFlow.Name != fl.Name {
		t.Fatalf("restored agent name mismatch: got %s, want %s", restoredFlow.Name, fl.Name)
	}

	// Verify integrity check (PRAGMA foreign_key_check and quick_check)
	var fkCheck string
	row := backupStore.DB().QueryRowContext(ctx, "PRAGMA foreign_key_check")
	if err := row.Scan(&fkCheck); err != nil && err.Error() != "sql: no rows in result set" {
		t.Fatalf("foreign_key_check error: %v", err)
	}

	var quickCheck string
	if err := backupStore.DB().QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickCheck); err != nil {
		t.Fatalf("quick_check: %v", err)
	}
	if quickCheck != "ok" {
		t.Fatalf("quick_check failed: %s", quickCheck)
	}
}

// TestSQLite_MigrationLockRecovery verifies that if a migration lock file exists from a crashed
// or cleanly exited prior run (flock released but lock file remains on disk), a new Open succeeds.
func TestSQLite_MigrationLockRecovery(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "recoverable.db")
	lockPath := dbPath + ".migration.lock"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. First open creates database and migrations
	s1, err := Open(ctx, Config{Dialect: SQLite, DSN: dbPath})
	if err != nil {
		t.Fatalf("open s1: %v", err)
	}
	s1.Close()

	// Confirm the lock file remains on disk (per docs/storage.md)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected lock file %s to exist: %v", lockPath, err)
	}

	// 2. Second open should successfully acquire the lock and open without error
	s2, err := Open(ctx, Config{Dialect: SQLite, DSN: dbPath})
	if err != nil {
		t.Fatalf("open s2 after cleanly closed s1: %v", err)
	}
	s2.Close()

	// 3. Test active contention: simulate an ongoing process holding flock
	activeGuard := flock.New(lockPath)
	locked, err := activeGuard.TryLock()
	if err != nil {
		t.Fatalf("acquire activeGuard: %v", err)
	}
	if !locked {
		t.Fatal("failed to lock activeGuard")
	}

	// Attempting to Open while another process holds the lock should fail with lock unavailable
	tryCtx, tryCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer tryCancel()
	s3, err := Open(tryCtx, Config{Dialect: SQLite, DSN: dbPath})
	if err == nil {
		s3.Close()
		t.Fatal("expected Open to fail while active migration lock is held")
	}
	if !strings.Contains(err.Error(), "migration lock unavailable") && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Release lock
	_ = activeGuard.Unlock()

	// 4. Now Open succeeds again
	s4, err := Open(ctx, Config{Dialect: SQLite, DSN: dbPath})
	if err != nil {
		t.Fatalf("open s4 after active lock released: %v", err)
	}
	s4.Close()
}
