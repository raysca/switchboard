package storage

import (
	"context"
	"database/sql"
	"encoding/json"
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

// PurgeContent redacts transcript, assistant_text, and tool call argument/result summaries
// for ended sessions whose ended_at is at or before cutoff, up to limit sessions.
// Structural metadata (sessions, turns, latencies, spans, events) are preserved.
// Active and draining sessions are never modified.
func (r RetentionRepository) PurgeContent(ctx context.Context, cutoff time.Time, limit int) (turnsPurged, toolCallsPurged int64, err error) {
	if limit <= 0 {
		limit = 500
	}

	query := `SELECT id FROM sessions
WHERE status = 'ended'
  AND ended_at IS NOT NULL
  AND ended_at <= ?
  AND (
    EXISTS (SELECT 1 FROM turns WHERE session_id = sessions.id AND (transcript IS NOT NULL OR assistant_text IS NOT NULL))
    OR EXISTS (SELECT 1 FROM tool_calls WHERE session_id = sessions.id AND (arguments_summary != '' OR result_summary != ''))
    OR EXISTS (SELECT 1 FROM trace_spans WHERE session_id = sessions.id AND (
        attributes_json LIKE '%"transcript"%' OR attributes_json LIKE '%transcript%'
        OR attributes_json LIKE '%"prompt"%' OR attributes_json LIKE '%prompt%'
        OR attributes_json LIKE '%"text"%'
        OR attributes_json LIKE '%"tool.arguments"%' OR attributes_json LIKE '%tool.arguments%'
        OR attributes_json LIKE '%"tool.result"%' OR attributes_json LIKE '%tool.result%'
    ))
    OR EXISTS (SELECT 1 FROM trace_events WHERE session_id = sessions.id AND (
        attributes_json LIKE '%"transcript"%' OR attributes_json LIKE '%transcript%'
        OR attributes_json LIKE '%"prompt"%' OR attributes_json LIKE '%prompt%'
        OR attributes_json LIKE '%"text"%'
        OR attributes_json LIKE '%"tool.arguments"%' OR attributes_json LIKE '%tool.arguments%'
        OR attributes_json LIKE '%"tool.result"%' OR attributes_json LIKE '%tool.result%'
    ))
  )
ORDER BY ended_at ASC
LIMIT ?`

	rows, err := r.s.db.QueryContext(ctx, r.s.bind(query), utc(cutoff), limit)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, 0, err
		}
		sessionIDs = append(sessionIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	rows.Close()

	if len(sessionIDs) == 0 {
		return 0, 0, nil
	}

	const subBatchSize = 25
	for i := 0; i < len(sessionIDs); i += subBatchSize {
		if err := ctx.Err(); err != nil {
			return turnsPurged, toolCallsPurged, err
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
				turnsPurged += nTurns

				// Redact tool calls: clear arguments_summary and result_summary
				resTC, err := tx.ExecContext(ctx, r.s.bind(
					"UPDATE tool_calls SET arguments_summary = '', result_summary = '' WHERE session_id = ? AND (arguments_summary != '' OR result_summary != '')",
				), sessID)
				if err != nil {
					return err
				}
				nTC, err := resTC.RowsAffected()
				if err != nil {
					return err
				}
				toolCallsPurged += nTC

				// Sanitize any text attributes in trace_spans
				spanRows, err := tx.QueryContext(ctx, r.s.bind(
					`SELECT id, attributes_json FROM trace_spans WHERE session_id = ? AND (
						attributes_json LIKE '%"transcript"%' OR attributes_json LIKE '%transcript%'
						OR attributes_json LIKE '%"prompt"%' OR attributes_json LIKE '%prompt%'
						OR attributes_json LIKE '%"text"%'
						OR attributes_json LIKE '%"tool.arguments"%' OR attributes_json LIKE '%tool.arguments%'
						OR attributes_json LIKE '%"tool.result"%' OR attributes_json LIKE '%tool.result%'
					)`,
				), sessID)
				if err == nil {
					var toUpdateSpans [][2]string
					for spanRows.Next() {
						var sID, attrs string
						if spanRows.Scan(&sID, &attrs) == nil {
							cleaned := sanitizeAttributesJSON(attrs)
							if cleaned != attrs {
								toUpdateSpans = append(toUpdateSpans, [2]string{sID, cleaned})
							}
						}
					}
					spanRows.Close()
					for _, pair := range toUpdateSpans {
						_, _ = tx.ExecContext(ctx, r.s.bind("UPDATE trace_spans SET attributes_json = ? WHERE id = ?"), pair[1], pair[0])
					}
				}

				// Sanitize any text attributes in trace_events
				eventRows, err := tx.QueryContext(ctx, r.s.bind(
					`SELECT id, attributes_json FROM trace_events WHERE session_id = ? AND (
						attributes_json LIKE '%"transcript"%' OR attributes_json LIKE '%transcript%'
						OR attributes_json LIKE '%"prompt"%' OR attributes_json LIKE '%prompt%'
						OR attributes_json LIKE '%"text"%'
						OR attributes_json LIKE '%"tool.arguments"%' OR attributes_json LIKE '%tool.arguments%'
						OR attributes_json LIKE '%"tool.result"%' OR attributes_json LIKE '%tool.result%'
					)`,
				), sessID)
				if err == nil {
					var toUpdateEvents [][2]string
					for eventRows.Next() {
						var eID, attrs string
						if eventRows.Scan(&eID, &attrs) == nil {
							cleaned := sanitizeAttributesJSON(attrs)
							if cleaned != attrs {
								toUpdateEvents = append(toUpdateEvents, [2]string{eID, cleaned})
							}
						}
					}
					eventRows.Close()
					for _, pair := range toUpdateEvents {
						_, _ = tx.ExecContext(ctx, r.s.bind("UPDATE trace_events SET attributes_json = ? WHERE id = ?"), pair[1], pair[0])
					}
				}
			}
			return nil
		})
		if err != nil {
			return turnsPurged, toolCallsPurged, err
		}
	}

	return turnsPurged, toolCallsPurged, nil
}

func sanitizeAttributesJSON(raw string) string {
	if raw == "" || raw == "{}" {
		return "{}"
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		return raw
	}
	changed := false
	for _, key := range []string{"transcript", "prompt", "text", "tool.arguments", "tool.result"} {
		if _, exists := attrs[key]; exists {
			delete(attrs, key)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(attrs)
	if err != nil {
		return raw
	}
	canonical, err := CanonicalJSON(out)
	if err != nil {
		return string(out)
	}
	return string(canonical)
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
