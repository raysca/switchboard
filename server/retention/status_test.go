package retention_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/server/retention"
	"govoice/server/storage"
)

const day = 24 * time.Hour

// seed makes sessions of several ages: ended long ago, ended a while ago, ended just now, and one still active.
func seed(t *testing.T, s *storage.Store) {
	t.Helper()
	agentID, versionID, hash := createTestAgentAndVersion(t, s)
	now := time.Now().UTC()
	createSessionWithData(t, s, agentID, versionID, hash, "ended", now.Add(-60*day), now.Add(-60*day+time.Minute))
	createSessionWithData(t, s, agentID, versionID, hash, "ended", now.Add(-60*day), now.Add(-60*day+2*time.Minute))
	createSessionWithData(t, s, agentID, versionID, hash, "ended", now.Add(-10*day), now.Add(-10*day+time.Minute))
	createSessionWithData(t, s, agentID, versionID, hash, "ended", now.Add(-time.Hour), now.Add(-time.Hour+time.Minute))
	createSessionWithData(t, s, agentID, versionID, hash, "active", now.Add(-90*day), time.Time{})
}

// The dry run is exactly what the next real run does: same sessions, turns, tool calls, spans and events.
func TestPreviewMatchesWhatARunRemoves(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	policy := retention.Policy{ContentRetention: 7 * day, MetadataRetention: 30 * day, BatchSize: 1} // small batches: totals still add up
	job := retention.NewJob(s.Retention(), policy)
	ctx := context.Background()

	preview, err := job.Preview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 2 sessions are past metadata retention (and content retention); 1 more is past content retention only
	if preview.MetadataSessions != 2 || preview.ContentSessions != 3 || preview.ContentTurns != 3 {
		t.Fatalf("preview = %+v", preview)
	}
	if again, _ := job.Preview(ctx); again != preview {
		t.Fatalf("a dry run changed something: %+v then %+v", preview, again)
	}

	res, err := job.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := storage.RetentionPreview{ContentTurns: res.TurnsPurged, MetadataSessions: res.SessionsDeleted, MetadataTurns: res.TurnsDeleted, MetadataToolCalls: res.ToolCallsDeleted, MetadataSpans: res.SpansDeleted, MetadataEvents: res.EventsDeleted}
	want := storage.RetentionPreview{ContentTurns: preview.ContentTurns, MetadataSessions: preview.MetadataSessions, MetadataTurns: preview.MetadataTurns, MetadataToolCalls: preview.MetadataToolCalls, MetadataSpans: preview.MetadataSpans, MetadataEvents: preview.MetadataEvents}
	if got != want {
		t.Fatalf("a run removed %+v, the dry run said %+v", got, want)
	}
	after, _ := job.Preview(ctx)
	if after.MetadataSessions != 0 || after.ContentTurns != 0 {
		t.Fatalf("a second dry run still finds work: %+v", after)
	}
}

func TestPreviewSkipsAPassThatIsOff(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	job := retention.NewJob(s.Retention(), retention.Policy{MetadataRetention: 30 * day})
	p, err := job.Preview(context.Background())
	if err != nil || p.ContentSessions != 0 || p.ContentTurns != 0 || p.MetadataSessions != 2 {
		t.Fatalf("preview = %+v %v", p, err)
	}
}

func TestEveryRunIsRecordedAndObserved(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	var mu sync.Mutex
	var seen []retention.RunRecord
	job := retention.NewJob(s.Retention(), retention.Policy{ContentRetention: 7 * day, MetadataRetention: 30 * day}, retention.WithObserver(func(r retention.RunRecord) {
		mu.Lock()
		seen = append(seen, r)
		mu.Unlock()
	}))
	if _, ok := job.Last(); ok {
		t.Fatal("a run is reported before one happened")
	}
	if _, err := job.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	last, ok := job.Last()
	if !ok || last.Err != "" || last.Result.SessionsDeleted != 2 || last.StartedAt.IsZero() {
		t.Fatalf("last = %+v %v", last, ok)
	}
	if len(seen) != 1 || seen[0].Result != last.Result {
		t.Fatalf("observer saw %+v", seen)
	}

	// a failed run is recorded too, with its error
	bad := retention.NewJob(s.Retention(), retention.Policy{ContentRetention: 30 * day, MetadataRetention: 7 * day})
	if _, err := bad.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "720h0m0s") || !strings.Contains(err.Error(), "168h0m0s") {
		t.Fatalf("the refusal must name both values: %v", err)
	}
	if rec, ok := bad.Last(); !ok || rec.Err == "" {
		t.Fatalf("failed run not recorded: %+v %v", rec, ok)
	}
}

func TestStatusEndpoint(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	settings := retention.Settings{Enabled: true, Interval: time.Hour, Policy: retention.Policy{ContentRetention: 7 * day, MetadataRetention: 30 * day, BatchSize: 500}, RecordingRetentionDays: 3}
	job := retention.NewJob(s.Retention(), settings.Policy)
	h := retention.Handler(settings, job, s.Retention())
	get := func(h http.Handler, path string) (*httptest.ResponseRecorder, retention.Status) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		var env struct {
			Data retention.Status `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		return w, env.Data
	}

	w, st := get(h, "/api/v1/retention")
	if w.Code != 200 || !st.Enabled || st.ContentRetentionDays != 7 || st.MetadataRetentionDays != 30 || st.RecordingRetentionDays != 3 || st.IntervalSeconds != 3600 || st.LastRun != nil || st.DryRun != nil {
		t.Fatalf("before any run: %d %s", w.Code, w.Body)
	}
	w, st = get(h, "/api/v1/retention?dry_run=true")
	if st.DryRun == nil || st.DryRun.MetadataSessions != 2 {
		t.Fatalf("dry run: %s", w.Body)
	}
	if _, err := job.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	w, st = get(h, "/api/v1/retention")
	if st.LastRun == nil || st.LastRun.SessionsDeleted != 2 || st.LastRun.Error != "" {
		t.Fatalf("after a run: %s", w.Body)
	}

	// off: the configured values are shown, and there is nothing to preview
	off := retention.Handler(retention.Settings{Enabled: false, Policy: retention.Policy{ContentRetention: day}}, nil, s.Retention())
	if w, st := get(off, "/api/v1/retention"); w.Code != 200 || st.Enabled || st.ContentRetentionDays != 1 {
		t.Fatalf("off: %d %s", w.Code, w.Body)
	}
	if w, _ := get(off, "/api/v1/retention?dry_run=true"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "retention_disabled") {
		t.Fatalf("off dry run: %d %s", w.Code, w.Body)
	}
	post := httptest.NewRecorder()
	h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/v1/retention", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", post.Code)
	}
}
