package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrSessionHeld: the call is on hold; release it first.
	ErrSessionHeld = errors.New("session is on hold")
	// ErrSessionActive: the call has not ended.
	ErrSessionActive = errors.New("session is still active")
)

// Retention audit actions.
const (
	AuditHold        = "hold"
	AuditRelease     = "release"
	AuditEraseCall   = "erase_call"
	AuditEraseCaller = "erase_caller"
)

// MaxHoldReason bounds the operator's note on a hold; MaxCallerLabel is the longest caller label a call stores.
const (
	MaxHoldReason  = 200
	MaxCallerLabel = 128
	// EraseCallerLimit is how many calls one erase-by-caller request removes.
	EraseCallerLimit    = 500
	eraseBatch          = 25
	MaxRetentionActions = 100
)

// notHeld is the one predicate every purge and the dry run use to leave held calls alone.
const notHeld = "NOT held"

// RetentionAction is one audited hold, release or erasure.
type RetentionAction struct {
	At                               time.Time
	Action, Subject, Detail, Request string
}

// SessionErasure counts what erasing one call removed.
type SessionErasure struct {
	Turns, ToolCalls, Spans, Events int64
}

func (e SessionErasure) detail() string {
	raw, _ := json.Marshal(map[string]int64{"turns": e.Turns, "tool_calls": e.ToolCalls, "spans": e.Spans, "events": e.Events})
	return string(raw)
}

// deleteSession removes one ended, non-held call and everything of its inside tx. The first statement is a no-op update
// guarded on the same condition: it takes the row's write lock and says whether the call is still erasable, so a hold
// placed (or a purge that ran) since the caller looked cannot leave a call half deleted. deleted is false when the call
// is gone, held or not ended; nothing was changed then.
//
// Every foreign key restricts, so rows reach the call by its id, by a span of it, or by a turn of it: events and spans
// are removed by all three links, not by session_id alone.
func (r RetentionRepository) deleteSession(ctx context.Context, tx *sql.Tx, id string) (stats SessionErasure, deleted bool, err error) {
	guard, err := tx.ExecContext(ctx, r.s.bind("UPDATE sessions SET ended_at=ended_at WHERE id=? AND status='ended' AND "+notHeld), id)
	if err != nil {
		return stats, false, err
	}
	if n, _ := guard.RowsAffected(); n == 0 {
		return stats, false, nil
	}
	const spansOfCall = "(SELECT id FROM trace_spans WHERE session_id=? OR turn_id IN (SELECT id FROM turns WHERE session_id=?))"
	const turnsOfCall = "(SELECT id FROM turns WHERE session_id=?)"
	exec := func(query string, args ...any) (int64, error) {
		res, err := tx.ExecContext(ctx, r.s.bind(query), args...)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return n, nil
	}
	if stats.Events, err = exec("DELETE FROM trace_events WHERE session_id=? OR span_id IN "+spansOfCall+" OR turn_id IN "+turnsOfCall, id, id, id, id); err != nil {
		return stats, false, err
	}
	if _, err = exec("UPDATE trace_spans SET parent_id=NULL WHERE session_id=? OR turn_id IN "+turnsOfCall, id, id); err != nil {
		return stats, false, err
	}
	if stats.Spans, err = exec("DELETE FROM trace_spans WHERE session_id=? OR turn_id IN "+turnsOfCall, id, id); err != nil {
		return stats, false, err
	}
	if stats.ToolCalls, err = exec("DELETE FROM tool_calls WHERE session_id=?", id); err != nil {
		return stats, false, err
	}
	if stats.Turns, err = exec("DELETE FROM turns WHERE session_id=?", id); err != nil {
		return stats, false, err
	}
	if _, err = exec("DELETE FROM session_mcp_tools WHERE session_id=?", id); err != nil {
		return stats, false, err
	}
	// An eval result keeps its score and outcome (the run's record), but its reason can quote the call (a never_say
	// match, a judge's words), so the erasure blanks it. No foreign key links the result to the call.
	if _, err = exec("UPDATE eval_results SET reason='' WHERE session_id=?", id); err != nil {
		return stats, false, err
	}
	n, err := exec("DELETE FROM sessions WHERE id=? AND status='ended' AND "+notHeld, id)
	if err != nil {
		return stats, false, err
	}
	return stats, n == 1, nil
}

func (r RetentionRepository) audit(ctx context.Context, tx *sql.Tx, action, subject, detail, requestID string) error {
	_, err := tx.ExecContext(ctx, r.s.bind("INSERT INTO retention_audit(id,at,action,subject,detail,request_id) VALUES(?,?,?,?,?,?)"), NewID(), utc(time.Now()), action, subject, detail, requestID)
	return err
}

// RecordAction audits something that happened outside the transactions here, such as a recording removal that was
// retried after an erasure.
func (r RetentionRepository) RecordAction(ctx context.Context, action, subject, detail, requestID string) error {
	return r.s.WithTx(ctx, func(tx *sql.Tx) error { return r.audit(ctx, tx, action, subject, detail, requestID) })
}

// RecentActions returns the audit trail, newest first, at most MaxRetentionActions.
func (r RetentionRepository) RecentActions(ctx context.Context, limit int) ([]RetentionAction, error) {
	if limit <= 0 || limit > MaxRetentionActions {
		limit = MaxRetentionActions
	}
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT at,action,subject,detail,request_id FROM retention_audit ORDER BY at DESC, id DESC LIMIT ?"), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetentionAction
	for rows.Next() {
		var a RetentionAction
		var at string
		if err := rows.Scan(&at, &a.Action, &a.Subject, &a.Detail, &a.Request); err != nil {
			return nil, err
		}
		if a.At, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetHold places or lifts a hold on a call and returns whether the flag changed. Placing a hold again only replaces
// the reason; only a change of the flag is audited. sql.ErrNoRows: no such call (an id that is not a stored session id
// counts as none).
func (r RetentionRepository) SetHold(ctx context.Context, id string, held bool, reason, requestID string) (changed bool, err error) {
	if validID(id) != nil {
		return false, sql.ErrNoRows
	}
	if len(reason) > MaxHoldReason {
		return false, errors.New("hold reason is too long")
	}
	if !held {
		reason = ""
	}
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		query := "SELECT held FROM sessions WHERE id=?"
		if r.s.dialect == Postgres {
			query += " FOR UPDATE"
		}
		var current bool
		if err := tx.QueryRowContext(ctx, r.s.bind(query), id).Scan(&current); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, r.s.bind("UPDATE sessions SET held=?,hold_reason=? WHERE id=?"), boolValue(r.s.dialect, held), reason, id); err != nil {
			return err
		}
		if current == held {
			return nil
		}
		changed = true
		action := AuditRelease
		if held {
			action = AuditHold
		}
		return r.audit(ctx, tx, action, id, "{}", requestID)
	})
	return changed, err
}

// IsHeld reports whether a call is on hold. A call that does not exist, or an id that is not a stored session id, is not.
func (r RetentionRepository) IsHeld(ctx context.Context, id string) (bool, error) {
	if validID(id) != nil {
		return false, nil
	}
	var held bool
	err := r.s.db.QueryRowContext(ctx, r.s.bind("SELECT held FROM sessions WHERE id=?"), id).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return held, err
}

// HeldCount is the number of calls on hold.
func (r RetentionRepository) HeldCount(ctx context.Context) (int64, error) {
	var n int64
	err := r.s.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE "+heldTrue(r.s.dialect)).Scan(&n)
	return n, err
}

func heldTrue(d Dialect) string {
	if d == Postgres {
		return "held"
	}
	return "held=1"
}

// EraseSession removes one ended, non-held call and its rows and records the erasure, in one transaction. The error
// says why a call was not erased: sql.ErrNoRows (no such call), ErrSessionHeld, ErrSessionActive. Recording files are
// the caller's to remove afterwards.
func (r RetentionRepository) EraseSession(ctx context.Context, id, requestID string) (SessionErasure, error) {
	var out SessionErasure
	if validID(id) != nil {
		return out, sql.ErrNoRows
	}
	err := r.s.WithTx(ctx, func(tx *sql.Tx) error {
		stats, deleted, err := r.deleteSession(ctx, tx, id)
		if err != nil {
			return err
		}
		if !deleted {
			return r.whyNotErasable(ctx, tx, id)
		}
		out = stats
		return r.audit(ctx, tx, AuditEraseCall, id, stats.detail(), requestID)
	})
	return out, err
}

// whyNotErasable classifies a call the guarded delete did not remove. Called inside the transaction that failed it.
func (r RetentionRepository) whyNotErasable(ctx context.Context, tx *sql.Tx, id string) error {
	var status string
	var held bool
	err := tx.QueryRowContext(ctx, r.s.bind("SELECT status,held FROM sessions WHERE id=?"), id).Scan(&status, &held)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return sql.ErrNoRows
	case err != nil:
		return err
	case held:
		return ErrSessionHeld
	case status != "ended":
		return ErrSessionActive
	}
	return errors.New("session could not be erased") // ended and not held, yet not removed: it changed while we looked
}

// CallerErasure is the outcome of one erase-by-caller request.
type CallerErasure struct {
	Erased    int
	Held      int      // matching calls skipped because they are on hold
	Active    int      // matching calls skipped because they have not ended
	Remaining int      // matching calls that are ended and not held and still to erase
	IDs       []string // the calls erased, for the caller to remove their recordings
	Removed   SessionErasure
}

// CallerHash is how a caller label appears in the audit trail: the first 16 hex characters of its SHA-256, so the
// trail never holds the label itself.
func CallerHash(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:8])
}

// EraseCaller erases up to EraseCallerLimit ended, non-held calls whose caller is exactly label, in batches of 25
// calls per transaction, and records one audit entry for the request when it erased any. With dryRun it only counts.
// The counts are taken before the erasure; Remaining is what is still to erase afterwards.
func (r RetentionRepository) EraseCaller(ctx context.Context, label, requestID string, dryRun bool) (CallerErasure, error) {
	var out CallerErasure
	count := func(query string, dst *int) error {
		var n int64
		if err := r.s.db.QueryRowContext(ctx, r.s.bind(query), label).Scan(&n); err != nil {
			return err
		}
		*dst = int(n)
		return nil
	}
	if err := count("SELECT count(*) FROM sessions WHERE caller=? AND "+heldTrue(r.s.dialect), &out.Held); err != nil {
		return out, err
	}
	if err := count("SELECT count(*) FROM sessions WHERE caller=? AND status<>'ended' AND "+notHeld, &out.Active); err != nil {
		return out, err
	}
	if err := count("SELECT count(*) FROM sessions WHERE caller=? AND status='ended' AND "+notHeld, &out.Remaining); err != nil {
		return out, err
	}
	if dryRun {
		return out, nil
	}
	rows, err := r.s.db.QueryContext(ctx, r.s.bind("SELECT id FROM sessions WHERE caller=? AND status='ended' AND "+notHeld+" ORDER BY ended_at, id LIMIT ?"), label, EraseCallerLimit)
	if err != nil {
		return out, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	finish := func(runErr error) (CallerErasure, error) {
		out.Remaining -= out.Erased
		if out.Erased > 0 {
			detail, _ := json.Marshal(map[string]int64{"erased": int64(out.Erased), "turns": out.Removed.Turns, "tool_calls": out.Removed.ToolCalls, "spans": out.Removed.Spans, "events": out.Removed.Events})
			// The calls are gone whatever happened to the request, so the audit is written even if the client left.
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			auditErr := r.s.WithTx(actx, func(tx *sql.Tx) error {
				return r.audit(actx, tx, AuditEraseCaller, CallerHash(label), string(detail), requestID)
			})
			if runErr == nil {
				runErr = auditErr
			}
		}
		return out, runErr
	}
	for i := 0; i < len(ids); i += eraseBatch {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		end := min(i+eraseBatch, len(ids))
		var batchIDs []string
		var batch SessionErasure
		err := r.s.WithTx(ctx, func(tx *sql.Tx) error {
			batchIDs, batch = nil, SessionErasure{} // a retried transaction starts clean
			for _, id := range ids[i:end] {
				stats, deleted, err := r.deleteSession(ctx, tx, id)
				if err != nil {
					return err
				}
				if deleted {
					batchIDs = append(batchIDs, id)
					batch.Turns += stats.Turns
					batch.ToolCalls += stats.ToolCalls
					batch.Spans += stats.Spans
					batch.Events += stats.Events
				}
			}
			return nil
		})
		if err != nil {
			return finish(err)
		}
		out.IDs = append(out.IDs, batchIDs...)
		out.Erased += len(batchIDs)
		out.Removed.Turns += batch.Turns
		out.Removed.ToolCalls += batch.ToolCalls
		out.Removed.Spans += batch.Spans
		out.Removed.Events += batch.Events
	}
	return finish(nil)
}
