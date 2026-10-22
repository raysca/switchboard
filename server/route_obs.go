package server

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"govoice/core"
)

var routeBuckets = [...]float64{0.25, 0.5, 1, 2, 5, 10}

// routeMetrics: unlabelled on purpose (step ids are tenant data, so labels
// would be unbounded); per-step detail lives in timeline events and trace spans.
type routeMetrics struct {
	total, failures atomic.Uint64
	buckets         [len(routeBuckets) + 1]atomic.Uint64 // non-cumulative; last is +Inf
	sumNs           atomic.Uint64
}

func (m *routeMetrics) observe(e core.RouteEvent) {
	switch e.Kind {
	case core.RouteFailed:
		m.failures.Add(1)
	case core.RouteActive:
		m.total.Add(1)
		m.sumNs.Add(uint64(e.Elapsed))
		b := 0
		for b < len(routeBuckets) && e.Elapsed.Seconds() > routeBuckets[b] {
			b++
		}
		m.buckets[b].Add(1)
	}
}

func (m *routeMetrics) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP voice_routes_total Completed routes between steps.\n# TYPE voice_routes_total counter\nvoice_routes_total %d\n", m.total.Load())
	fmt.Fprintf(w, "# HELP voice_route_failures_total Routes that could not complete (the caller stayed on the old step).\n# TYPE voice_route_failures_total counter\nvoice_route_failures_total %d\n", m.failures.Load())
	fmt.Fprint(w, "# HELP voice_route_seconds Time from the transfer to the new step being active.\n# TYPE voice_route_seconds histogram\n")
	var cum uint64
	for i, le := range routeBuckets {
		cum += m.buckets[i].Load()
		fmt.Fprintf(w, "voice_route_seconds_bucket{le=\"%g\"} %d\n", le, cum)
	}
	cum += m.buckets[len(routeBuckets)].Load()
	fmt.Fprintf(w, "voice_route_seconds_bucket{le=\"+Inf\"} %d\nvoice_route_seconds_sum %g\nvoice_route_seconds_count %d\n", cum, float64(m.sumNs.Load())/1e9, cum)
}

// onRoute is one call's route observer: timeline events, metrics, and
// trace spans (persisted for session replay when management is enabled).
func (s *Server) onRoute(tl *SessionTimeline, storedSessionID string) func(core.RouteEvent) {
	// Taken now, not at each event: a retired step reports that it closed after the call, possibly after the
	// server has closed management and cleared the field.
	mgmt := s.mgmt.Load()
	return func(e core.RouteEvent) {
		s.metrics.route.observe(e)
		switch e.Kind {
		case core.RoutePreparing:
			tl.Record(TimelineEvent{Type: EventStepPreparing, Details: e.To})
		case core.RouteActive:
			tl.SetStep(e.To)
			tl.Record(TimelineEvent{Type: EventRoute, Details: e.From + "->" + e.To, DurationMs: e.Elapsed.Milliseconds()})
			tl.Record(TimelineEvent{Type: EventStepActive, Details: e.To})
		case core.RouteClosed:
			tl.Record(TimelineEvent{Type: EventStepClosed, Details: e.From})
		case core.RouteFailed:
			msg := ""
			if e.Err != nil {
				msg = s.redactor.Redact(e.Err.Error())
			}
			tl.Record(TimelineEvent{Type: EventRouteFailed, Details: e.To, Error: msg})
		default:
			return // committed: preparing carries the same information
		}
		if mgmt != nil && mgmt.traces != nil && storedSessionID != "" {
			ev := core.NewTraceEvent("route."+string(e.Kind), time.Now(), core.SessionID(storedSessionID), "", 0, core.TracePolicy{},
				map[string]any{"from_step": e.From, "to_step": e.To})
			core.StartSpanSafe(context.Background(), mgmt.traces, ev).End(e.Err)
		}
	}
}

// onEndCall records the call.end span for a call ended by a step's end_call tool.
func (s *Server) onEndCall(storedSessionID string) func(stepID string) {
	mgmt := s.mgmt.Load() // taken now: management teardown clears the field
	return func(stepID string) {
		if mgmt == nil || mgmt.traces == nil || storedSessionID == "" {
			return
		}
		ev := core.NewTraceEvent("call.end", time.Now(), core.SessionID(storedSessionID), "", 0, core.TracePolicy{},
			map[string]any{"reason": EndReasonFlow, "from_step": stepID})
		core.StartSpanSafe(context.Background(), mgmt.traces, ev).End(nil)
	}
}
