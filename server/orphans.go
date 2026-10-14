package server

import (
	"context"
	"log/slog"
	"time"

	"govoice/server/storage"
)

// orphanGrace is added to the longest call before a session counts as dead on a store that other
// replicas may share.
const orphanGrace = 5 * time.Minute

// orphanCutoff is how old a session must be to be certainly dead at startup. SQLite is a
// single-process file: every active session is a predecessor's, so everything before now. Postgres
// may be shared by live replicas: only sessions older than the longest call any of them allows.
func orphanCutoff(dialect string, maxCall time.Duration, now time.Time) time.Time {
	if dialect != string(storage.Postgres) {
		return now
	}
	return now.Add(-maxCall - orphanGrace) // config validation guarantees maxCall > 0
}

// endOrphanedSessions ends the calls a previous process left active, before this one admits any.
// A storage error is logged, not returned: calls still work, and the rows are swept at the next start.
func (s *Server) endOrphanedSessions(ctx context.Context, store *storage.Store) {
	cutoff := orphanCutoff(s.cfg.Storage.Dialect, s.cfg.Limits.MaxCallDuration, time.Now())
	n, err := store.Sessions().EndOrphans(ctx, cutoff)
	if err != nil {
		s.logger.Warn("orphaned sessions sweep failed", slog.Int("ended", n), slog.Any("error", err))
	}
	if n > 0 {
		s.logger.Warn("ended sessions left active by a previous process", slog.Int("sessions", n))
	}
}
