package retention

import (
	"context"
	"net/http"
	"time"

	"govoice/server/api"
	"govoice/server/storage"
)

// Settings is the retention configuration the status endpoint reports, whether or not the job runs.
type Settings struct {
	Enabled                bool
	Interval               time.Duration
	Policy                 Policy
	RecordingRetentionDays int // 0 when recording is off
}

// Status is the policy in effect and the last run.
type Status struct {
	Enabled                bool                      `json:"enabled"`
	IntervalSeconds        int                       `json:"interval_seconds"`
	ContentRetentionDays   int                       `json:"content_retention_days"`
	MetadataRetentionDays  int                       `json:"metadata_retention_days"`
	BatchSize              int                       `json:"batch_size"`
	RecordingRetentionDays int                       `json:"recording_retention_days"`
	LastRun                *RunStatus                `json:"last_run"`
	DryRun                 *storage.RetentionPreview `json:"dry_run,omitempty"`
}

// RunStatus is a finished run.
type RunStatus struct {
	StartedAt        time.Time `json:"started_at"`
	DurationMS       int64     `json:"duration_ms"`
	Error            string    `json:"error,omitempty"`
	TurnsPurged      int64     `json:"turns_purged"`
	SessionsDeleted  int64     `json:"sessions_deleted"`
	TurnsDeleted     int64     `json:"turns_deleted"`
	ToolCallsDeleted int64     `json:"tool_calls_deleted"`
	SpansDeleted     int64     `json:"spans_deleted"`
	EventsDeleted    int64     `json:"events_deleted"`
}

const day = 24 * time.Hour

// dryRunTimeout bounds the counting queries of a dry run on a large database.
const dryRunTimeout = 30 * time.Second

// Handler serves GET /api/v1/retention: the policy in effect and the last run; with ?dry_run=true also what a run
// started now would remove. job is nil when the job is off. It must only be mounted behind the management boundary.
func Handler(settings Settings, job *Job) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/retention", func(w http.ResponseWriter, r *http.Request) {
		id := api.RequestID(w, r)
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			api.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", id)
			return
		}
		st := Status{
			Enabled:                settings.Enabled,
			IntervalSeconds:        int(settings.Interval / time.Second),
			ContentRetentionDays:   int(settings.Policy.ContentRetention / day),
			MetadataRetentionDays:  int(settings.Policy.MetadataRetention / day),
			BatchSize:              settings.Policy.BatchSize,
			RecordingRetentionDays: settings.RecordingRetentionDays,
		}
		if job != nil {
			if rec, ok := job.Last(); ok {
				st.LastRun = &RunStatus{StartedAt: rec.StartedAt, DurationMS: rec.Duration.Milliseconds(), Error: rec.Err, TurnsPurged: rec.Result.TurnsPurged, SessionsDeleted: rec.Result.SessionsDeleted, TurnsDeleted: rec.Result.TurnsDeleted, ToolCallsDeleted: rec.Result.ToolCallsDeleted, SpansDeleted: rec.Result.SpansDeleted, EventsDeleted: rec.Result.EventsDeleted}
			}
			if r.URL.Query().Get("dry_run") == "true" {
				ctx, cancel := context.WithTimeout(r.Context(), dryRunTimeout)
				defer cancel()
				preview, err := job.Preview(ctx)
				if err != nil {
					api.WriteError(w, http.StatusInternalServerError, "internal_error", "the dry run could not be counted", id)
					return
				}
				st.DryRun = &preview
			}
		} else if r.URL.Query().Get("dry_run") == "true" {
			api.WriteError(w, http.StatusConflict, "retention_disabled", "automatic retention is off (retention.enabled), so there is no run to preview", id)
			return
		}
		api.WriteJSON(w, http.StatusOK, api.SuccessResponse[Status]{Data: st})
	})
	return mux
}
