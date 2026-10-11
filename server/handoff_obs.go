package server

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"govoice/core"
)

var handoffBuckets = [...]float64{0.25, 0.5, 1, 2, 5, 10}

// handoffMetrics: unlabelled on purpose (node ids are tenant data, so labels
// would be unbounded); per-node detail lives in timeline events and trace spans.
type handoffMetrics struct {
	total, failures atomic.Uint64
	buckets         [len(handoffBuckets) + 1]atomic.Uint64 // non-cumulative; last is +Inf
	sumNs           atomic.Uint64
}

func (m *handoffMetrics) observe(e core.HandoffEvent) {
	switch e.Kind {
	case core.HandoffFailed:
		m.failures.Add(1)
	case core.HandoffActive:
		m.total.Add(1)
		m.sumNs.Add(uint64(e.Elapsed))
		b := 0
		for b < len(handoffBuckets) && e.Elapsed.Seconds() > handoffBuckets[b] {
			b++
		}
		m.buckets[b].Add(1)
	}
}

func (m *handoffMetrics) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP voice_handoffs_total Completed agent handoffs.\n# TYPE voice_handoffs_total counter\nvoice_handoffs_total %d\n", m.total.Load())
	fmt.Fprintf(w, "# HELP voice_handoff_failures_total Handoffs that could not complete (the caller stayed with the old agent).\n# TYPE voice_handoff_failures_total counter\nvoice_handoff_failures_total %d\n", m.failures.Load())
	fmt.Fprint(w, "# HELP voice_handoff_seconds Time from the transfer to the new agent being active.\n# TYPE voice_handoff_seconds histogram\n")
	var cum uint64
	for i, le := range handoffBuckets {
		cum += m.buckets[i].Load()
		fmt.Fprintf(w, "voice_handoff_seconds_bucket{le=\"%g\"} %d\n", le, cum)
	}
	cum += m.buckets[len(handoffBuckets)].Load()
	fmt.Fprintf(w, "voice_handoff_seconds_bucket{le=\"+Inf\"} %d\nvoice_handoff_seconds_sum %g\nvoice_handoff_seconds_count %d\n", cum, float64(m.sumNs.Load())/1e9, cum)
}

// onHandoff is one call's handoff observer: timeline events, metrics, and
// trace spans (persisted for session replay when management is enabled).
func (s *Server) onHandoff(tl *SessionTimeline, storedSessionID string) func(core.HandoffEvent) {
	return func(e core.HandoffEvent) {
		s.metrics.handoff.observe(e)
		switch e.Kind {
		case core.HandoffPreparing:
			tl.Record(TimelineEvent{Type: EventNodePreparing, Details: e.To})
		case core.HandoffActive:
			tl.SetNode(e.To)
			tl.Record(TimelineEvent{Type: EventHandoff, Details: e.From + "->" + e.To, DurationMs: e.Elapsed.Milliseconds()})
			tl.Record(TimelineEvent{Type: EventNodeActive, Details: e.To})
		case core.HandoffClosed:
			tl.Record(TimelineEvent{Type: EventNodeClosed, Details: e.From})
		case core.HandoffFailed:
			msg := ""
			if e.Err != nil {
				msg = s.redactor.Redact(e.Err.Error())
			}
			tl.Record(TimelineEvent{Type: EventHandoffFailed, Details: e.To, Error: msg})
		default:
			return // committed: preparing carries the same information
		}
		if s.mgmt != nil && s.mgmt.traces != nil && storedSessionID != "" {
			ev := core.NewTraceEvent("handoff."+string(e.Kind), time.Now(), core.SessionID(storedSessionID), "", 0, core.TracePolicy{},
				map[string]any{"from_node": e.From, "to_node": e.To})
			core.StartSpanSafe(context.Background(), s.mgmt.traces, ev).End(e.Err)
		}
	}
}
