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
  AND (
    EXISTS (SELECT 1 FROM turns WHERE session_id = sessions.id AND (transcript IS NOT NULL OR assistant_text IS NOT NULL))
  )
ORDER BY ended_at ASC, id ASC
LIMIT ?`

	rows, err := r.s.db.QueryContext(ctx, r.s.bind(query), args...)
	if err != nil {
		return res, err
	}
	defer rows.Close()

	var sessionIDs []string
	for rows.Next() {
		var id, endedAt string
		if err := rows.Scan(&id, &endedAt); err != nil {
			return res, err
		}
		sessionIDs = append(sessionIDs, id)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	rows.Close()
	res.Examined = len(sessionIDs)

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
					"UPDATE turns SET transcript = NULL, assistant_text = NULL WHERE session_id = ? AND (transcript IS NOT NULL OR assistant_text IS NOT NULL)",
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
ORDER BY ended_at ASC
LIMIT ?`

	rows, err := r.s.db.QueryContext(ctx, r.s.bind(query), utc(cutoff), limit)
	if err != nil {
		return stats, err
	}
	defer rows.Close()

	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return stats, err
		}
		sessionIDs = append(sessionIDs, id)
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}
	rows.Close()

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
				// 1. Delete trace events referencing this session
				resEvents, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM trace_events WHERE session_id = ?"), sessID)
				if err != nil {
					return err
				}
				nEvents, _ := resEvents.RowsAffected()
				stats.EventsDeleted += nEvents

				// 2. Disconnect self-referential parent_id on trace_spans before deleting spans
				if _, err := tx.ExecContext(ctx, r.s.bind("UPDATE trace_spans SET parent_id = NULL WHERE session_id = ?"), sessID); err != nil {
					return err
				}

				// 3. Delete trace spans referencing this session
				resSpans, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM trace_spans WHERE session_id = ?"), sessID)
				if err != nil {
					return err
				}
				nSpans, _ := resSpans.RowsAffected()
				stats.SpansDeleted += nSpans

				// 4. Delete tool calls referencing this session
				resTC, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM tool_calls WHERE session_id = ?"), sessID)
				if err != nil {
					return err
				}
				nTC, _ := resTC.RowsAffected()
				stats.ToolCallsDeleted += nTC

				// 5. Delete turns referencing this session
				resTurns, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM turns WHERE session_id = ?"), sessID)
				if err != nil {
					return err
				}
				nTurns, _ := resTurns.RowsAffected()
				stats.TurnsDeleted += nTurns

				// 6. Delete session_mcp_tools referencing this session
				if _, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM session_mcp_tools WHERE session_id = ?"), sessID); err != nil {
					return err
				}

				// 7. Delete the ended session itself
				resSess, err := tx.ExecContext(ctx, r.s.bind("DELETE FROM sessions WHERE id = ? AND status = 'ended'"), sessID)
				if err != nil {
					return err
				}
				nSess, _ := resSess.RowsAffected()
				stats.SessionsDeleted += nSess
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
	ContentSessions   int64
	ContentTurns      int64
	MetadataSessions  int64
	MetadataTurns     int64
	MetadataToolCalls int64
	MetadataSpans     int64
	MetadataEvents    int64
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
		if err := count(&p.ContentSessions, `SELECT count(*) FROM sessions WHERE status='ended' AND ended_at IS NOT NULL AND ended_at <= ?
AND EXISTS (SELECT 1 FROM turns WHERE session_id = sessions.id AND (transcript IS NOT NULL OR assistant_text IS NOT NULL))`, c); err != nil {
			return p, err
		}
		if err := count(&p.ContentTurns, `SELECT count(*) FROM turns t JOIN sessions s ON s.id = t.session_id
WHERE s.status='ended' AND s.ended_at IS NOT NULL AND s.ended_at <= ? AND (t.transcript IS NOT NULL OR t.assistant_text IS NOT NULL)`, c); err != nil {
			return p, err
		}
	}
	if !metadataCutoff.IsZero() {
		c := utc(metadataCutoff)
		const old = "(SELECT id FROM sessions WHERE status='ended' AND ended_at IS NOT NULL AND ended_at <= ?)"
		for _, q := range []struct {
			dst   *int64
			query string
			args  []any
		}{
			{&p.MetadataSessions, "SELECT count(*) FROM sessions WHERE status='ended' AND ended_at IS NOT NULL AND ended_at <= ?", []any{c}},
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

// PurgeEval deletes, at most limit rows per kind: finished (done or dead) jobs last updated at or before jobsCutoff
// (resultsCutoff when jobsCutoff is zero, so that finished jobs are purged even with no metadata lifetime), results
// created at or before resultsCutoff, and then done runs that ended at or before resultsCutoff and have no job or
// result left. A zero cutoff leaves that kind out (a zero resultsCutoff also leaves the runs). Pending and running
// jobs are never touched. A result is not tied to a session (no foreign key), so purging sessions and purging results
// are independent.
func (r RetentionRepository) PurgeEval(ctx context.Context, resultsCutoff, jobsCutoff time.Time, limit int) (PurgeEvalStats, error) {
	var st PurgeEvalStats
	del := func(dst *int64, query string, args ...any) error {
		res, err := r.s.db.ExecContext(ctx, r.s.bind(query), args...)
		if err != nil {
			return err
		}
		*dst, _ = res.RowsAffected()
		return nil
	}
	if jobsCutoff.IsZero() {
		jobsCutoff = resultsCutoff
	}
	if !jobsCutoff.IsZero() {
		if err := del(&st.Jobs, "DELETE FROM eval_jobs WHERE id IN (SELECT id FROM eval_jobs WHERE status IN ('done','dead') AND updated_at <= ? ORDER BY updated_at LIMIT ?)", utc(jobsCutoff), limit); err != nil {
			return st, fmt.Errorf("purge eval jobs: %w", err)
		}
	}
	if !resultsCutoff.IsZero() {
		if err := del(&st.Results, "DELETE FROM eval_results WHERE id IN (SELECT id FROM eval_results WHERE created_at <= ? ORDER BY created_at LIMIT ?)", utc(resultsCutoff), limit); err != nil {
			return st, fmt.Errorf("purge eval results: %w", err)
		}
		if err := del(&st.Runs, `DELETE FROM eval_runs WHERE id IN (SELECT r.id FROM eval_runs r WHERE r.status='done' AND r.ended_at <= ?
			AND NOT EXISTS (SELECT 1 FROM eval_jobs j WHERE j.run_id=r.id) AND NOT EXISTS (SELECT 1 FROM eval_results e WHERE e.run_id=r.id) ORDER BY r.ended_at LIMIT ?)`, utc(resultsCutoff), limit); err != nil {
			return st, fmt.Errorf("purge eval runs: %w", err)
		}
	}
	return st, nil
}
