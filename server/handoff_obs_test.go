package server

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"govoice/core"
)

func TestTimelineStampsActiveNode(t *testing.T) {
	tl := NewSessionTimeline(1, 10, time.Minute)
	tl.Record(TimelineEvent{Type: EventLLMStart})
	tl.SetNode("sales")
	tl.Record(TimelineEvent{Type: EventLLMStart})
	tl.Record(TimelineEvent{Type: EventTTSAudio, Node: "explicit"})
	ev := tl.Events()
	if ev[0].Node != "" || ev[1].Node != "sales" || ev[2].Node != "explicit" {
		t.Fatalf("nodes = %q %q %q", ev[0].Node, ev[1].Node, ev[2].Node)
	}
}

func TestHandoffMetricsCountAndTime(t *testing.T) {
	var m handoffMetrics
	m.observe(core.HandoffEvent{Kind: core.HandoffActive, Elapsed: 400 * time.Millisecond})
	m.observe(core.HandoffEvent{Kind: core.HandoffFailed, Err: errors.New("x")})
	m.observe(core.HandoffEvent{Kind: core.HandoffPreparing}) // not counted
	var b bytes.Buffer
	m.write(&b)
	out := b.String()
	for _, want := range []string{"voice_handoffs_total 1", "voice_handoff_failures_total 1", `voice_handoff_seconds_bucket{le="0.5"} 1`, "voice_handoff_seconds_count 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestOnHandoffRecordsTimelineAndTraceSpans(t *testing.T) {
	s := apiTestServer(t)
	enableTestManagement(t, s)
	agentID := readyTestAgent(t, s)
	pin, err := s.agents.StartSession(t.Context(), agentID, "")
	if err != nil {
		t.Fatal(err)
	}
	tl := NewSessionTimeline(5, 50, time.Minute)
	on := s.onHandoff(tl, pin.ID)
	on(core.HandoffEvent{Kind: core.HandoffPreparing, From: "front", To: "sales"})
	on(core.HandoffEvent{Kind: core.HandoffActive, From: "front", To: "sales", Elapsed: 300 * time.Millisecond})
	on(core.HandoffEvent{Kind: core.HandoffFailed, From: "sales", To: "front", Err: errors.New("provider down")})

	got := map[TimelineEventType]TimelineEvent{}
	for _, e := range tl.Events() {
		got[e.Type] = e
	}
	if got[EventNodePreparing].Details != "sales" || got[EventHandoff].Details != "front->sales" || got[EventNodeActive].Node != "sales" {
		t.Fatalf("timeline = %+v", got)
	}
	if got[EventHandoffFailed].Error == "" {
		t.Fatal("a failed handoff must carry its error")
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
	if a := attrs["handoff.active"]; !strings.Contains(a, `"from_node":"front"`) || !strings.Contains(a, `"to_node":"sales"`) {
		t.Fatalf("handoff.active attributes = %s, want from_node and to_node", attrs["handoff.active"])
	}
	for _, want := range []string{"handoff.preparing", "handoff.active", "handoff.failed"} {
		if !names[want] {
			t.Fatalf("missing span %q, have %v", want, names)
		}
	}
}
