package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"

	"govoice/server/api"
	"govoice/server/storage"
)

// Recordings removes the recording files of a call. record.LocalSink implements it.
type Recordings interface {
	Remove(id string) (int, error)
}

// Lifecycle holds, releases and erases stored calls, and removes their recording files. Its counts and logs never carry
// a transcript, a path or a caller label.
type Lifecycle struct {
	repo       storage.RetentionRepository
	recordings Recordings // nil when recording is off
	log        *slog.Logger
	erased     func(kind string, n int) // metric hook; may be nil
}

// NewLifecycle builds the service. recordings may be nil, and erased may be nil.
func NewLifecycle(repo storage.RetentionRepository, recordings Recordings, log *slog.Logger, erased func(kind string, n int)) *Lifecycle {
	return &Lifecycle{repo: repo, recordings: recordings, log: log, erased: erased}
}

func (l *Lifecycle) Hold(ctx context.Context, id, reason, requestID string) error {
	_, err := l.repo.SetHold(ctx, id, true, reason, requestID)
	return err
}

func (l *Lifecycle) Release(ctx context.Context, id, requestID string) error {
	_, err := l.repo.SetHold(ctx, id, false, "", requestID)
	return err
}

// removeFiles removes the recording of a stored call. Only a stored session id names a file here, never an older
// recording name, so an id from a request cannot reach a recording that is not that call's.
func (l *Lifecycle) removeFiles(id string) (int, error) {
	if l.recordings == nil || !storage.IsID(id) {
		return 0, nil
	}
	return l.recordings.Remove(id)
}

// Erase removes an ended, non-held call. The rows go first, in one transaction with the audit entry; the files after.
// A call with no rows is looked for as leftover recording files, so a failed removal can be retried with the same id.
func (l *Lifecycle) Erase(ctx context.Context, id, requestID string) (api.Erasure, error) {
	rows, err := l.repo.EraseSession(ctx, id, requestID)
	if errors.Is(err, sql.ErrNoRows) {
		return l.retryFiles(ctx, id, requestID)
	}
	if err != nil {
		return api.Erasure{}, err
	}
	out := api.Erasure{Turns: rows.Turns, ToolCalls: rows.ToolCalls, Spans: rows.Spans, Events: rows.Events}
	if l.erased != nil {
		l.erased("call", 1)
	}
	n, ferr := l.removeFiles(id)
	out.RecordingsRemoved = n
	if ferr != nil {
		l.noteFailure(ctx, id, requestID)
		return out, api.ErrRecordingsNotRemoved
	}
	return out, nil
}

func (l *Lifecycle) retryFiles(ctx context.Context, id, requestID string) (api.Erasure, error) {
	n, err := l.removeFiles(id)
	if err != nil {
		l.noteFailure(ctx, id, requestID)
		return api.Erasure{RecordingsRemoved: n}, api.ErrRecordingsNotRemoved
	}
	if n == 0 {
		return api.Erasure{}, sql.ErrNoRows
	}
	detail, _ := json.Marshal(map[string]int{"recordings_removed": n})
	if err := l.repo.RecordAction(ctx, storage.AuditEraseCall, id, string(detail), requestID); err != nil {
		l.log.Warn("retention: recording removal was not audited", slog.String("session", id), slog.String("err", err.Error()))
	}
	return api.Erasure{RecordingsRemoved: n}, nil
}

// noteFailure audits a recording that could not be removed. The error text may carry a path, so it is not recorded.
func (l *Lifecycle) noteFailure(ctx context.Context, id, requestID string) {
	l.log.Warn("retention: a recording file could not be removed", slog.String("session", id))
	if err := l.repo.RecordAction(ctx, storage.AuditEraseCall, id, `{"recordings_failed":true}`, requestID); err != nil {
		l.log.Warn("retention: the failed removal was not audited", slog.String("session", id), slog.String("err", err.Error()))
	}
}

// EraseCaller erases up to storage.EraseCallerLimit of a caller's ended, non-held calls and their recordings.
func (l *Lifecycle) EraseCaller(ctx context.Context, label, requestID string, dryRun bool) (api.CallerErasure, error) {
	res, err := l.repo.EraseCaller(ctx, label, requestID, dryRun)
	out := api.CallerErasure{
		Erased: res.Erased, Held: res.Held, Active: res.Active, Remaining: res.Remaining,
		Erasure: api.Erasure{Turns: res.Removed.Turns, ToolCalls: res.Removed.ToolCalls, Spans: res.Removed.Spans, Events: res.Removed.Events},
	}
	// Files of the calls that were erased, even when a later batch failed: their rows are gone.
	for _, id := range res.IDs {
		n, ferr := l.removeFiles(id)
		out.RecordingsRemoved += n
		if ferr != nil {
			out.RecordingsFailed = append(out.RecordingsFailed, id)
		}
	}
	if len(out.RecordingsFailed) > 0 {
		l.log.Warn("retention: recording files could not be removed", slog.Int("calls", len(out.RecordingsFailed)))
	}
	if l.erased != nil && res.Erased > 0 {
		l.erased("caller", res.Erased)
	}
	return out, err
}
