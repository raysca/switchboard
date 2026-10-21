> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# End Orphaned Sessions at Startup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A server crash or kill leaves its calls `active` in storage forever. Make startup reconcile them, so the Calls "Live" lamp and counts are trustworthy, and MCP servers and tools pinned by a dead call can be deleted again.

**Architecture:** One storage method, `SessionRepository.EndOrphans(ctx, before)`, ends every `active` or `draining` session that started before `before`, with an honest end time (the last turn's end, else the start) and a note, then retries the MCP pending-delete finalization. `EnableManagement` calls it once after storage opens and before any call is admitted, with a cutoff chosen by the storage dialect: **SQLite** is a single-process file, so every active session is an orphan (cutoff = now); **Postgres** may be shared by several replicas, so only sessions older than the longest call any replica allows (`limits.max_call_duration` plus a grace) are orphans. No migration, no new config, no periodic job.

**Tech Stack:** Go (`server/storage`, `server`), SQLite and Postgres SQL, `go test -race`.

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` Section 5, item S14. Project rules: `AGENTS.md` (every feature consumed by the server; clean cross-dialect SQL; `go test -race`).

## Why this is safe, and what it does not do

- A call's pipeline, context and turn state live in one process's memory (`docs/operations_runbook.md`, "Affinity"); no other process can resume it. After a process dies, its `active` rows are dead for certain.
- SQLite (`storage.dialect: sqlite`, what the deployments run) cannot be shared by two live server processes, so at startup every `active` row belongs to a dead predecessor.
- Postgres can be shared. A replica starting up must not end another live replica's calls, so it only ends sessions that no live call can still be: older than `max_call_duration` plus a grace. A crashed call younger than that stays `active` until a later restart sweeps it: the plan documents this limit; a heartbeat or owner id would remove it and is out of scope.
- No end reason exists on sessions yet (S10), so the note goes in `safe_summary`, which the API already returns; it is set only when empty.

## Global Constraints

- Statuses are `active`, `draining`, `ended`; ending sets `status='ended'` and `ended_at`. A session already `ended` is never touched.
- The SQL must run unchanged on SQLite (timestamps stored as RFC3339 text via `utc()`) and Postgres (`TIMESTAMPTZ`): bind times with `utc(...)` and `r.s.bind(...)`, as the neighbouring code does; no dialect-specific functions.
- `ended_at` is the last turn's `ended_at` when there is one, else the session's `started_at`; never "now", which would show a long phantom call.
- Retention never touches `active`/`draining` sessions (`storage/retention.go`); once ended, an orphan becomes eligible like any other session.
- Startup must not fail because of the sweep: a storage error is logged at WARN and startup continues (calls still work; the rows stay stale until the next restart).
- Conventional commits ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`; `gofmt`; `go test -race ./server/...` clean.

## Review Focus

1. **A live call must never be ended.** Sessions at or after the cutoff are untouched; a session created right after startup is untouched. Pinned in Tasks 1 and 3.
2. **Honest end time.** An orphan with turns ends at its last finished turn; one with none ends at its start; never at "now". Pinned in Task 1.
3. **Already-ended and unrelated rows are untouched** (status, `ended_at`, `safe_summary`), and a pre-existing `safe_summary` is kept. Pinned in Task 1.
4. **MCP deletes unblock.** A server pinned only by an orphan can be deleted after the sweep. Pinned in Task 1.
5. **A failing sweep does not stop startup,** and the Postgres cutoff is computed from the configured `max_call_duration` (a zero or missing value falls back to the 4 h default). Pinned in Task 2.

---

### Task 1: `SessionRepository.EndOrphans`

**Files:**
- Modify: `server/storage/history.go` (after `End`, line 158)
- Test: `server/storage/orphans_test.go` (create)

**Interfaces:**
- Produces: `func (r SessionRepository) EndOrphans(ctx context.Context, before time.Time) (int, error)` — ends every `active` or `draining` session with `started_at < before`; returns how many; calls `r.s.MCP().FinalizePendingDeletes(ctx)` afterwards (also when none ended).
- Consumes: `utc`, `r.s.bind`, `r.s.WithTx`, `FinalizePendingDeletes` (existing).

- [ ] **Step 1: Write the failing tests** — `server/storage/orphans_test.go`:

```go
package storage

import (
	"context"
	"testing"
	"time"
)

// seed makes one session of a fresh flow, started at `started`, with the given status.
func seedSession(t *testing.T, s *Store, started time.Time, status string) (sessionID string) {
	t.Helper()
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "A", Enabled: true, CreatedAt: time.Now()}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := s.Flows().PublishVersion(ctx, a.ID, 1, []byte(`{}`), nil)
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
	s := testStore(t)
	ctx := context.Background()
	old := time.Now().Add(-2 * time.Hour)
	active, draining := seedSession(t, s, old, "active"), seedSession(t, s, old, "draining")
	n, err := s.Sessions().EndOrphans(ctx, time.Now())
	if err != nil || n != 2 {
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
}

func TestEndOrphansNeverTouchesALiveOrLaterSession(t *testing.T) {
	s := testStore(t)
	cutoff := time.Now().Add(-time.Hour)
	live := seedSession(t, s, time.Now(), "active")            // started after the cutoff
	boundary := seedSession(t, s, cutoff.Add(time.Second), "active") // just after it
	if n, err := s.Sessions().EndOrphans(context.Background(), cutoff); err != nil || n != 0 {
		t.Fatalf("ended %d, err %v", n, err)
	}
	for _, id := range []string{live, boundary} {
		if r := sessionOf(t, s, id); r.Status != "active" || !r.EndedAt.IsZero() {
			t.Fatalf("%s was changed: %+v", id, r)
		}
	}
}

func TestEndOrphansEndsAtTheLastTurnNotAtNow(t *testing.T) {
	s := testStore(t)
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
}

func TestEndOrphansKeepsEndedSessionsAndExistingNotes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	old := time.Now().Add(-2 * time.Hour)
	ended := seedSession(t, s, old, "active")
	if err := s.Sessions().End(ctx, ended); err != nil {
		t.Fatal(err)
	}
	before := sessionOf(t, s, ended)
	noted := seedSession(t, s, old, "active")
	if _, err := s.DB().ExecContext(ctx, s.bind("UPDATE sessions SET safe_summary=? WHERE id=?"), "caller asked about billing", noted); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Sessions().EndOrphans(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("ended %d, err %v", n, err)
	}
	if after := sessionOf(t, s, ended); after.Status != "ended" || !after.EndedAt.Equal(before.EndedAt) || after.SafeSummary != before.SafeSummary {
		t.Fatalf("an ended session changed: %+v -> %+v", before, after)
	}
	if r := sessionOf(t, s, noted); r.Status != "ended" || r.SafeSummary != "caller asked about billing" {
		t.Fatalf("an existing note was replaced: %+v", r)
	}
}

func TestEndOrphansUnblocksAnMCPServerPinnedOnlyByTheDeadCall(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Reuse the pattern of TestSessionEndRetriesPendingDeleteFinalization / the MCP delete tests in
	// this package: create an MCP server and a session whose runtime tools pin it, request the delete
	// (it is deferred while the session is active), then:
	id := seedPinnedSession(t, s, time.Now().Add(-2*time.Hour)) // helper written from those tests
	if _, err := s.Sessions().EndOrphans(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := sessionOf(t, s, id).Status; got != "ended" {
		t.Fatalf("status %s", got)
	}
	assertPendingDeleteFinalized(t, s) // the server row is gone once the pin is released
}
```

For the last test, read `TestConcurrentSessionCloseAndMCPDeleteAlwaysDrainsSafely` (`storage_test.go:832`) and `TestSessionEndRetriesPendingDeleteFinalization` (`:115`): they already create a pinned session and a deferred MCP delete. Write `seedPinnedSession` and `assertPendingDeleteFinalized` from exactly those calls (create MCP server via `s.MCP()`, `SessionRecord.RuntimeTools: []SelectedTool{...}`, `DetachAndDelete` which defers while pinned, then assert `s.MCP().Server(ctx, id)` returns `sql.ErrNoRows` after the sweep). Do not leave helper bodies unwritten: copy the lines.

- [ ] **Step 2: Run to see them fail**

Run: `cd server && go test -race -count=1 -run 'TestEndOrphans' ./storage`
Expected: FAIL — `EndOrphans` undefined.

- [ ] **Step 3: Implement** — in `server/storage/history.go` after `End`:

```go
// orphanNote is set on a session ended by EndOrphans, unless it already has a summary.
const orphanNote = "Ended when the server restarted."

// EndOrphans ends every active or draining session that started before `before`: its process is
// gone, so nothing will ever end it. The end time is the last finished turn, else the start (never
// "now": that would show a phantom call that lasted until the restart). It returns how many it
// ended, and retries the MCP deletes those sessions were holding back.
func (r SessionRepository) EndOrphans(ctx context.Context, before time.Time) (int, error) {
	var n int
	err := r.s.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, r.s.bind(`UPDATE sessions SET
			status='ended',
			ended_at=COALESCE((SELECT MAX(t.ended_at) FROM turns t WHERE t.session_id=sessions.id), started_at),
			safe_summary=CASE WHEN safe_summary='' THEN ? ELSE safe_summary END
			WHERE status IN ('active','draining') AND started_at < ?`), orphanNote, utc(before))
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		n = int(affected)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, r.s.MCP().FinalizePendingDeletes(ctx)
}
```

- [ ] **Step 4: Run to see them pass, then the package**

Run: `cd server && go test -race -count=1 ./storage 2>&1 | tail -5; gofmt -l storage`
Expected: PASS. If a test fails on the `MAX(t.ended_at)` text comparison under SQLite (fixed-width RFC3339 with microseconds from `utc()`, so lexical order is chronological), keep the SQL and fix the test data; if Postgres (`TIMESTAMPTZ`) needs a cast, add it for both dialects only if it is dialect-neutral.

- [ ] **Step 5: Commit**

```bash
git add server/storage/history.go server/storage/orphans_test.go
git commit -m "feat(storage): EndOrphans ends sessions whose process is gone, at their last turn

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Reconcile at startup, with a dialect-aware cutoff

**Files:**
- Modify: `server/management.go` (`EnableManagement`, after `s.SetStore(store)`), create `server/orphans.go`
- Test: `server/orphans_test.go` (create)

**Interfaces:**
- Consumes: `store.Sessions().EndOrphans`, `s.cfg.Storage.Dialect`, `s.cfg.Limits.MaxCallDuration` (a `time.Duration`; 0 means the server default), `s.logger`.
- Produces: `orphanCutoff(dialect string, maxCall time.Duration, now time.Time) time.Time` and `(s *Server) endOrphanedSessions(ctx context.Context, store *storage.Store)` (logs; never returns an error).

- [ ] **Step 1: Write the failing tests** — `server/orphans_test.go`:

```go
package server

import (
	"context"
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

// A crash leaves its calls active. The next start, on the same store, ends them; a call started
// after the restart is not touched.
func TestStartupEndsTheCallsACrashLeftBehind(t *testing.T) {
	t.Setenv("GOVOICE_DEV_DIR", t.TempDir())
	t.Setenv("GOVOICE_MANAGEMENT_API_KEY", e2eMgmtKey)
	ctx := context.Background()

	// "Before the crash": a session left active in the development store.
	first := apiTestServerWithoutManagement(t) // see Step 3 note
	dsn, err := first.developmentStore()
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, storage.Config{Dialect: storage.SQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	orphan := seedActiveSession(t, store, time.Now().Add(-time.Hour))
	store.Close()

	// "After the restart".
	s := apiTestServerWithoutManagement(t)
	if err := s.EnableManagement(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(ctx) })
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
	s := apiTestServerWithoutManagement(t)
	closed, err := storage.Open(context.Background(), storage.Config{Dialect: storage.SQLite, DSN: t.TempDir() + "/closed.db"})
	if err != nil {
		t.Fatal(err)
	}
	closed.Close() // every query on it fails
	s.endOrphanedSessions(context.Background(), closed) // must return, not panic
}
```

Helpers: `apiTestServer(t)` already exists (`server/api_*_test.go`); read its definition and `enableTestManagement`. If `apiTestServer` already calls `EnableManagement`, write `apiTestServerWithoutManagement` as the same constructor minus that call (copy its body), and write `seedActiveSession(t, store, started)` from the flow-create / `PublishVersion` / `Sessions().Create` calls in `storage/storage_test.go:86-110` (the storage package's `seedSession` is test-only there; copy its body, exporting nothing).

- [ ] **Step 2: Run to see them fail**

Run: `cd server && go test -race -count=1 -run 'TestOrphanCutoff|TestStartupEndsTheCalls|TestAFailingSweep' .`
Expected: FAIL — `orphanCutoff` undefined.

- [ ] **Step 3: Implement** — `server/orphans.go`:

```go
package server

import (
	"context"
	"log/slog"
	"time"

	"govoice/server/storage"
)

const (
	// defaultMaxCallDuration is the longest call when limits.max_call_duration is not set.
	defaultMaxCallDuration = 4 * time.Hour
	// orphanGrace is added to the longest call before a session counts as dead on a store that
	// other replicas may share.
	orphanGrace = 5 * time.Minute
)

// orphanCutoff is how old a session must be to be certainly dead at startup. SQLite is a
// single-process file: every active session is a predecessor's, so everything before now. Postgres
// may be shared by live replicas: only sessions older than the longest call any of them allows.
func orphanCutoff(dialect string, maxCall time.Duration, now time.Time) time.Time {
	if dialect != string(storage.Postgres) {
		return now
	}
	if maxCall <= 0 {
		maxCall = defaultMaxCallDuration
	}
	return now.Add(-maxCall - orphanGrace)
}

// endOrphanedSessions ends the calls a previous process left active, before this one admits any.
// A storage error is logged, not returned: calls still work, and the rows are swept at the next start.
func (s *Server) endOrphanedSessions(ctx context.Context, store *storage.Store) {
	cutoff := orphanCutoff(s.cfg.Storage.Dialect, s.cfg.Limits.MaxCallDuration, time.Now())
	n, err := store.Sessions().EndOrphans(ctx, cutoff)
	if err != nil {
		s.logger.Warn("could not end orphaned sessions", slog.Any("error", err))
		return
	}
	if n > 0 {
		s.logger.Warn("ended sessions left active by a previous process", slog.Int("sessions", n))
	}
}
```

(`storage.Postgres` is the dialect constant used with `storage.SQLite`; confirm the exact exported name in `storage/storage.go`.) In `EnableManagement`, immediately after `s.SetStore(store)`: `s.endOrphanedSessions(ctx, store)`.

- [ ] **Step 4: Run to see them pass, then the module**

Run: `cd server && go test -race -count=1 ./... 2>&1 | grep -E "^(FAIL|--- FAIL|ok)" | head -20; gofmt -l .`
Expected: PASS. If an existing test creates active sessions in a store and then calls `EnableManagement` on the same store expecting them to stay active, it encodes the old behaviour: fix the test to start its sessions after `EnableManagement`.

- [ ] **Step 5: Commit**

```bash
git add server/orphans.go server/orphans_test.go server/management.go
git commit -m "feat(server): end the calls a crashed process left active when management starts

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Seen through the API, documented, spec updated, reviewed

**Files:**
- Modify: `docs/operations_runbook.md` (a short "After a crash" paragraph), `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (S14 row)
- Test: `server/orphans_test.go` (add one case)

- [ ] **Step 1: Failing test, API view.** Add to `server/orphans_test.go` a case that, after the restart of `TestStartupEndsTheCallsACrashLeftBehind`, lists `GET /api/v1/sessions` (use `apiRequest(s, http.MethodGet, "/api/v1/sessions", e2eMgmtKey, "")` as the other API tests do) and asserts the orphan appears with `status: "ended"` and that `GET /api/v1/operations/status` reports `active_sessions` 0 (`op.active_sessions` counts live sessions in memory: assert it equals the number of sessions this process admitted, 0).
- [ ] **Step 2: Run it, expect PASS** (the behaviour exists after Task 2; if it fails, the API reads a different source than storage status and the finding belongs in the plan ledger before changing anything).
- [ ] **Step 3: Docs.** Runbook: "After a crash or kill, the next start ends the calls the old process left active (SQLite: all of them; Postgres: those older than `limits.max_call_duration` plus 5 minutes, because other replicas may still own younger ones). They appear as ended at their last finished turn, with the note 'Ended when the server restarted.'; an MCP server or tool only they were holding can then be deleted." Spec S14 row: mark done with that scope and the Postgres limit.
- [ ] **Step 4: Gate.** `cd server && go test -race -count=1 ./... && gofmt -l .` — Expected: green, clean.
- [ ] **Step 5: Independent review** (AGENTS.md): dispatch a fresh reviewer on `main..HEAD`: YAGNI (is the Postgres branch worth its keep without a live Postgres test? either test it against the CI Postgres job if one exists or keep it behind the unit test of `orphanCutoff` and say so), correctness of the cross-dialect SQL (`MAX` of text versus `TIMESTAMPTZ`, the correlated subquery on the table being updated, `safe_summary` default), the five Review Focus items, and that no code path ends a session whose call is live in this process. Fix confirmed findings, each with a test that fails first.
- [ ] **Step 6: Commit**

```bash
git add docs server
git commit -m "docs: S14 done — startup ends orphaned sessions; runbook note

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
