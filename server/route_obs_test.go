package server

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"govoice/core"
)

func TestTimelineStampsActiveStep(t *testing.T) {
	tl := NewSessionTimeline(1, 10, time.Minute)
	tl.Record(TimelineEvent{Type: EventLLMStart})
	tl.SetStep("sales")
	tl.Record(TimelineEvent{Type: EventLLMStart})
	tl.Record(TimelineEvent{Type: EventTTSAudio, Step: "explicit"})
	ev := tl.Events()
	if ev[0].Step != "" || ev[1].Step != "sales" || ev[2].Step != "explicit" {
		t.Fatalf("steps = %q %q %q", ev[0].Step, ev[1].Step, ev[2].Step)
	}
}

func TestRouteMetricsCountAndTime(t *testing.T) {
	var m routeMetrics
	m.observe(core.RouteEvent{Kind: core.RouteActive, Elapsed: 400 * time.Millisecond})
	m.observe(core.RouteEvent{Kind: core.RouteFailed, Err: errors.New("x")})
	m.observe(core.RouteEvent{Kind: core.RoutePreparing}) // not counted
	var b bytes.Buffer
	m.write(&b)
	out := b.String()
	for _, want := range []string{"voice_routes_total 1", "voice_route_failures_total 1", `voice_route_seconds_bucket{le="0.5"} 1`, "voice_route_seconds_count 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestOnRouteRecordsTimelineAndTraceSpans(t *testing.T) {
	s := apiTestServer(t)
	enableTestManagement(t, s)
	flowID := readyTestFlow(t, s)
	pin, err := s.flows.StartSession(t.Context(), flowID, "")
	if err != nil {
		t.Fatal(err)
	}
	tl := NewSessionTimeline(5, 50, time.Minute)
	on := s.onRoute(tl, pin.ID)
	on(core.RouteEvent{Kind: core.RoutePreparing, From: "front", To: "sales"})
	on(core.RouteEvent{Kind: core.RouteActive, From: "front", To: "sales", Elapsed: 300 * time.Millisecond})
	on(core.RouteEvent{Kind: core.RouteFailed, From: "sales", To: "front", Err: errors.New("provider down")})

	got := map[TimelineEventType]TimelineEvent{}
	for _, e := range tl.Events() {
		got[e.Type] = e
	}
	if got[EventStepPreparing].Details != "sales" || got[EventRoute].Details != "front->sales" || got[EventStepActive].Step != "sales" {
		t.Fatalf("timeline = %+v", got)
	}
	if got[EventRouteFailed].Error == "" {
		t.Fatal("a failed route must carry its error")
	}
	names := map[string]bool{}
	attrs := map[string]string{} // span name -> attributes JSON
	for end := time.Now().Add(3 * time.Second); len(names) < 3 && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		spans, err := s.mgmt.store.Traces().Spans(t.Context(), pin.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, sp := range spans {
			names[sp.Name] = true
			attrs[sp.Name] = sp.AttributesJSON
		}
	}
	// The designer replays the path from these two attributes: the trace adapter
	// drops any key outside its allowlist, so assert they actually persisted.
	if a := attrs["route.active"]; !strings.Contains(a, `"from_step":"front"`) || !strings.Contains(a, `"to_step":"sales"`) {
		t.Fatalf("route.active attributes = %s, want from_step and to_step", attrs["route.active"])
	}
	for _, want := range []string{"route.preparing", "route.active", "route.failed"} {
		if !names[want] {
			t.Fatalf("missing span %q, have %v", want, names)
		}
	}
}
