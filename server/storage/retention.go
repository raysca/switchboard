package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type PurgeMetadataStats struct {
	SessionsDeleted  int64
	TurnsDeleted     int64
	SpansDeleted     int64
	EventsDeleted    int64
	ToolCallsDeleted int64
}

type RetentionRepository struct {
	s *Store
}

func (s *Store) Retention() RetentionRepository {
	return RetentionRepository{s: s}
}

// ContentPurgeResult reports one PurgeContent batch.
type ContentPurgeResult struct {
	TurnsPurged int64
	Examined    int // sessions selected in this batch
}

// PurgeContent nulls turns.transcript and turns.assistant_text for ended sessions whose ended_at is at or
// before cutoff, up to limit sessions. Structural metadata (sessions, turns, latencies, spans, events) is
// preserved. Active and draining sessions are never modified. A session leaves the selection once purged, so
// calling it again until Examined < limit purges everything.
func (r RetentionRepository) PurgeContent(ctx context.Context, cutoff time.Time, limit int) (res ContentPurgeResult, err error) {
	if limit <= 0 {
		limit = 500
	}

	args := []any{utc(cutoff), limit}
	query := `SELECT id, ended_at FROM sessions
WHERE status = 'ended'
  AND ended_at IS NOT NULL
  AND ended_at <= ?
  AND ` + notHeld + `
  AND (
    EXISTS (SELECT 1 FROM turns WHERE session_id = sessions.id AND (transcript IS NOT NULL OR assistant_text IS NOT NULL))
  )
ORDER BY ended_at ASC, id ASC
LIMIT ?`

	rows, err := r.s.db.QueryContext(ctx, r.s.bind(query), args...)
	if err != nil {
		return res, err
	}
	sessionIDs, err := collect(rows, nil, func(row rowScanner) (string, error) {
		var id, endedAt string
		err := row.Scan(&id, &endedAt)
		return id, err
	})
	if err != nil {
		return res, err
	}
	res.Examined = len(sessionIDs)
	if r.s.purgeContentHook != nil {
		r.s.purgeContentHook()
	}

	if len(sessionIDs) == 0 {
		return res, nil
	}

	const subBatchSize = 25
	for i := 0; i < len(sessionIDs); i += subBatchSize {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		end := i + subBatchSize
		if end > len(sessionIDs) {
			end = len(sessionIDs)
		}
		batch := sessionIDs[i:end]

		err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
			for _, sessID := range batch {
				// Redact turns: set transcript and assistant_text to NULL
				resTurns, err := tx.ExecContext(ctx, r.s.bind(
					// The session is checked again here, so a hold placed since it was selected protects it (on Postgres a hold committed during this very statement can still be missed).
					"UPDATE turns SET transcript = NULL, assistant_text = NULL WHERE session_id = ? AND (transcript IS NOT NULL OR assistant_text IS NOT NULL) AND session_id IN (SELECT id FROM sessions WHERE status = 'ended' AND "+notHeld+")",
				), sessID)
				if err != nil {
					return err
				}
				nTurns, err := resTurns.RowsAffected()
				if err != nil {
					return err
				}
				res.TurnsPurged += nTurns
			}
			return nil
		})
		if err != nil {
			return res, err
		}
	}

	return res, nil
}

// PurgeMetadata deletes ended sessions and all associated child records (trace_events, trace_spans,
// tool_calls, turns, session_mcp_tools) whose ended_at is at or before cutoff, up to limit sessions.
// It also cleans up standalone trace_events and trace_spans older than cutoff.
// Deletion invariants: active/draining sessions, agent versions, and discoveries are never deleted.
func (r RetentionRepository) PurgeMetadata(ctx context.Context, cutoff time.Time, limit int) (stats PurgeMetadataStats, err error) {
	if limit <= 0 {
		limit = 500
	}

	query := `SELECT id FROM sessions
WHERE status = 'ended'
  AND ended_at IS NOT NULL
  AND ended_at <= ?
  AND ` + notHeld + `
ORDER BY ended_at ASC
LIMIT ?`

	sessionIDs, err := queryIDs(ctx, r.s.db, r.s.bind(query), utc(cutoff), limit)
	if err != nil {
		return stats, err
	}

	const subBatchSize = 25
	for i := 0; i < len(sessionIDs); i += subBatchSize {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		end := i + subBatchSize
		if end > len(sessionIDs) {
			end = len(sessionIDs)
		}
		batch := sessionIDs[i:end]

		err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
			for _, sessID := range batch {
				one, deleted, err := r.deleteSession(ctx, tx, sessID)
				if err != nil {
					return err
				}
				if deleted { // a call held or gone since it was selected is left alone
					stats.SessionsDeleted++
					stats.TurnsDeleted += one.Turns
					stats.SpansDeleted += one.Spans
					stats.EventsDeleted += one.Events
					stats.ToolCallsDeleted += one.ToolCalls
				}
			}
			return nil
		})
		if err != nil {
			return stats, err
		}
	}

	// Also clean up standalone trace_events and trace_spans (where session_id IS NULL) older than cutoff
	err = r.s.WithTx(ctx, func(tx *sql.Tx) error {
		resStandaloneEvents, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM trace_events WHERE session_id IS NULL AND occurred_at <= ?"), utc(cutoff))
		if err == nil {
			n, _ := resStandaloneEvents.RowsAffected()
			stats.EventsDeleted += n
		}

		if _, err := tx.ExecContext(ctx, r.s.bind("UPDATE trace_spans SET parent_id = NULL WHERE session_id IS NULL AND started_at <= ?"), utc(cutoff)); err == nil {
			resStandaloneSpans, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM trace_spans WHERE session_id IS NULL AND started_at <= ?"), utc(cutoff))
			if err == nil {
				n, _ := resStandaloneSpans.RowsAffected()
				stats.SpansDeleted += n
			}
		}

		return nil
	})

	return stats, err
}

// RetentionPreview is what one retention run would remove right now, counted without removing anything. A zero cutoff
// leaves that pass out. The counts are the ones a run reports: ContentTurns is RunResult.TurnsPurged, and the Metadata
// fields are the rows PurgeMetadata deletes (sessions and everything of theirs, plus standalone spans and events).
type RetentionPreview struct {
	ContentSessions   int64 `json:"content_sessions"`
	ContentTurns      int64 `json:"content_turns"`
	MetadataSessions  int64 `json:"metadata_sessions"`
	MetadataTurns     int64 `json:"metadata_turns"`
	MetadataToolCalls int64 `json:"metadata_tool_calls"`
	MetadataSpans     int64 `json:"metadata_spans"`
	MetadataEvents    int64 `json:"metadata_events"`
}

// Preview counts what PurgeContent (contentCutoff) and PurgeMetadata (metadataCutoff) would remove. It reads in
// separate queries, so a call ending between them can shift a count by one session.
func (r RetentionRepository) Preview(ctx context.Context, contentCutoff, metadataCutoff time.Time) (RetentionPreview, error) {
	var p RetentionPreview
	count := func(dst *int64, query string, args ...any) error {
		return r.s.db.QueryRowContext(ctx, r.s.bind(query), args...).Scan(dst)
	}
	if !contentCutoff.IsZero() {
		c := utc(contentCutoff)
		if err := count(&p.ContentSessions, `SELECT count(*) FROM sessions WHERE status='ended' AND ended_at IS NOT NULL AND ended_at <= ? AND `+notHeld+`
AND EXISTS (SELECT 1 FROM turns WHERE session_id = sessions.id AND (transcript IS NOT NULL OR assistant_text IS NOT NULL))`, c); err != nil {
			return p, err
		}
		if err := count(&p.ContentTurns, `SELECT count(*) FROM turns t JOIN sessions s ON s.id = t.session_id
WHERE s.status='ended' AND s.ended_at IS NOT NULL AND s.ended_at <= ? AND NOT s.held AND (t.transcript IS NOT NULL OR t.assistant_text IS NOT NULL)`, c); err != nil {
			return p, err
		}
	}
	if !metadataCutoff.IsZero() {
		c := utc(metadataCutoff)
		const old = "(SELECT id FROM sessions WHERE status='ended' AND ended_at IS NOT NULL AND ended_at <= ? AND " + notHeld + ")"
		for _, q := range []struct {
			dst   *int64
			query string
			args  []any
		}{
			{&p.MetadataSessions, "SELECT count(*) FROM sessions WHERE status='ended' AND ended_at IS NOT NULL AND ended_at <= ? AND " + notHeld, []any{c}},
			{&p.MetadataTurns, "SELECT count(*) FROM turns WHERE session_id IN " + old, []any{c}},
			{&p.MetadataToolCalls, "SELECT count(*) FROM tool_calls WHERE session_id IN " + old, []any{c}},
			{&p.MetadataSpans, "SELECT (SELECT count(*) FROM trace_spans WHERE session_id IN " + old + ") + (SELECT count(*) FROM trace_spans WHERE session_id IS NULL AND started_at <= ?)", []any{c, c}},
			{&p.MetadataEvents, "SELECT (SELECT count(*) FROM trace_events WHERE session_id IN " + old + ") + (SELECT count(*) FROM trace_events WHERE session_id IS NULL AND occurred_at <= ?)", []any{c, c}},
		} {
			if err := count(q.dst, q.query, q.args...); err != nil {
				return p, err
			}
		}
	}
	return p, nil
}

// PurgeEvalStats counts the rows one PurgeEval call deleted.
type PurgeEvalStats struct{ Jobs, Results, Runs int64 }

// PurgeEval deletes, at most limit rows per kind, everything of an eval run that is at or before cutoff: finished
// (done or dead) jobs last updated then, results created then, and then done runs that ended then and have no job or
// result left. Jobs go with the run, not earlier, because the run detail is built from them. A zero cutoff does
// nothing. Pending and running jobs are never touched. A result is not tied to a session (no foreign key), so
// purging sessions and purging results are independent.
func (r RetentionRepository) PurgeEval(ctx context.Context, cutoff time.Time, limit int) (PurgeEvalStats, error) {
	var st PurgeEvalStats
	del := func(dst *int64, query string, args ...any) error {
		res, err := r.s.db.ExecContext(ctx, r.s.bind(query), args...)
		if err != nil {
			return err
		}
		*dst, _ = res.RowsAffected()
		return nil
	}
	if cutoff.IsZero() {
		return st, nil
	}
	if err := del(&st.Jobs, "DELETE FROM eval_jobs WHERE id IN (SELECT id FROM eval_jobs WHERE status IN ('done','dead') AND updated_at <= ? ORDER BY updated_at LIMIT ?)", utc(cutoff), limit); err != nil {
		return st, fmt.Errorf("purge eval jobs: %w", err)
	}
	if err := del(&st.Results, "DELETE FROM eval_results WHERE id IN (SELECT id FROM eval_results WHERE created_at <= ? ORDER BY created_at LIMIT ?)", utc(cutoff), limit); err != nil {
		return st, fmt.Errorf("purge eval results: %w", err)
	}
	if err := del(&st.Runs, `DELETE FROM eval_runs WHERE id IN (SELECT r.id FROM eval_runs r WHERE r.status='done' AND r.ended_at <= ?
		AND NOT EXISTS (SELECT 1 FROM eval_jobs j WHERE j.run_id=r.id) AND NOT EXISTS (SELECT 1 FROM eval_results e WHERE e.run_id=r.id) ORDER BY r.ended_at LIMIT ?)`, utc(cutoff), limit); err != nil {
		return st, fmt.Errorf("purge eval runs: %w", err)
	}
	return st, nil
}
