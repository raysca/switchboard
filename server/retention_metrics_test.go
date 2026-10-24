package server

import (
	"strings"
	"testing"
	"time"

	"govoice/server/retention"
)

func TestObserveRetentionCountsPartialProgressOfAFailedRun(t *testing.T) {
	m := &Metrics{StartTime: time.Now()}
	m.RetentionEnabled.Store(true)
	m.RetentionIntervalSeconds.Store(3600)
	m.ObserveRetention(retention.RunRecord{Result: retention.RunResult{TurnsPurged: 5, SessionsDeleted: 2}, Err: "timeout"})
	if m.RetentionRunsFailed.Load() != 1 || m.RetentionRunsOK.Load() != 0 {
		t.Fatalf("failed=%d ok=%d", m.RetentionRunsFailed.Load(), m.RetentionRunsOK.Load())
	}
	if m.RetentionTurnsPurgedTotal.Load() != 5 || m.RetentionSessionsDeleted.Load() != 2 {
		t.Fatalf("rows removed before the failure were not counted: %d turns, %d sessions", m.RetentionTurnsPurgedTotal.Load(), m.RetentionSessionsDeleted.Load())
	}
	if m.retentionLastOK.Load() != nil {
		t.Fatal("a failed run counted as a success")
	}
	m.ObserveRetention(retention.RunRecord{Result: retention.RunResult{SessionsDeleted: 1}})
	if m.RetentionRunsOK.Load() != 1 || m.RetentionSessionsDeleted.Load() != 3 || m.retentionLastOK.Load() == nil {
		t.Fatalf("ok run: ok=%d sessions=%d", m.RetentionRunsOK.Load(), m.RetentionSessionsDeleted.Load())
	}
	var b strings.Builder
	m.WritePrometheus(&b)
	for _, want := range []string{"voice_retention_interval_seconds 3600", "voice_retention_sessions_deleted_total 3"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("metrics lack %q", want)
		}
	}
}
