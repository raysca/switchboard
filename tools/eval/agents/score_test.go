package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"govoice/server/api"
	"govoice/tools/internal/stack"
)

func fixture[T any](t *testing.T, name string) (v T) {
	t.Helper()
	b, err := os.ReadFile("testdata/api/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

var t0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

// two typed turns: 120 ms and 380 ms to the first reply text, after a greeting.
func callEvents() ([]stack.Event, []Sent) {
	ms := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	ev := []stack.Event{
		{At: ms(10), Kind: "bot-llm-started"}, {At: ms(20), Kind: "bot-llm-text", Text: "Welcome to the desk."},
		{At: ms(1120), Kind: "bot-llm-started"}, {At: ms(1120), Kind: "bot-llm-text", Text: "Sure, "}, {At: ms(1130), Kind: "bot-llm-text", Text: "connecting you."},
		{At: ms(1500), Kind: "bot-llm-started"}, {At: ms(1500), Kind: "bot-llm-text", Text: "We sell hatchbacks."},
		{At: ms(3380), Kind: "bot-llm-text", Text: " Many."},
	}
	return ev, []Sent{{At: ms(1000), Text: "I want a car"}, {At: ms(3000), Text: "What kinds?"}}
}

func observed(t *testing.T, traces string) Observed {
	t.Helper()
	ev, sent := callEvents()
	return Observe(fixture[api.SessionDTO](t, "session.json"), fixture[api.SessionTracesDTO](t, traces), "front", ev, sent)
}

func TestObserve(t *testing.T) {
	o := observed(t, "traces-ok.json")
	if o.SessionID != "s1" || o.AgentVersionID != "v7" || o.ConfigHash != "abc123" || o.EndReason != "caller_hung_up" || o.TurnCount != 3 {
		t.Errorf("session fields: %+v", o)
	}
	if want := []string{"front", "sales"}; !reflect.DeepEqual(o.Path, want) {
		t.Errorf("path = %v", o.Path)
	}
	if want := []string{"transfer_to_sales:succeeded"}; !reflect.DeepEqual(o.Tools, want) {
		t.Errorf("tools = %v", o.Tools)
	}
	if want := []int{120, 380}; !reflect.DeepEqual(o.ReplyMS, want) {
		t.Errorf("reply ms = %v, want %v", o.ReplyMS, want)
	}
	want := []Line{{"agent", "Welcome to the desk."}, {"caller", "I want a car"}, {"agent", "Sure, connecting you. We sell hatchbacks."},
		{"caller", "What kinds?"}, {"agent", "Many."}}
	if !reflect.DeepEqual(o.Transcript, want) {
		t.Errorf("transcript = %v", o.Transcript)
	}
	if o.Replies == "" || o.ErrorFrames != 0 || o.HandoffFailures != 0 {
		t.Errorf("replies/errors: %+v", o)
	}
	if nh := observed(t, "traces-nohandoff.json"); !reflect.DeepEqual(nh.Path, []string{"front"}) {
		t.Errorf("no handoff path = %v", nh.Path)
	}
}

func baseCase() Case {
	return Case{Name: "c", Agent: "Dealership desk", Caller: Caller{Script: []string{"hi"}}, Expect: Expect{
		Path: []string{"front", "sales"}, ToolsCalled: []string{"transfer_to_sales"}, ToolsNotCalled: []string{"end_call"},
		Say: []string{"(?i)hatchback"}, NeverSay: []string{"(?i)as an ai"}, EndReason: "caller_hung_up", MaxTurns: 6, MedianReplyMSMax: 500,
	}}
}

var names = map[string]string{"front": "Front desk", "sales": "Sales"}

func TestScore(t *testing.T) {
	if f := Score(baseCase(), observed(t, "traces-ok.json"), names); len(f) != 0 {
		t.Fatalf("passing call failed: %v", f)
	}
	for _, tc := range []struct {
		name   string
		traces string
		mutate func(*Case, *Observed)
		want   string
	}{
		{"no handoff", "traces-nohandoff.json", func(c *Case, _ *Observed) { c.Expect.ToolsCalled = nil }, "path: want [front sales], got [front]"},
		{"handoff.failed span", "traces-handoff-failed.json", func(c *Case, _ *Observed) { c.Expect = Expect{} }, "handoff: want 0 failed handoffs, got 1"},
		{"tool failed", "traces-tool-failed.json", func(*Case, *Observed) {}, `tools_called: want "transfer_to_sales:succeeded", got [transfer_to_sales:failed]`},
		{"rtvi error", "traces-ok.json", func(_ *Case, o *Observed) { o.ErrorFrames = 1 }, "errors: want 0 RTVI error messages, got 1"},
		{"forbidden phrase", "traces-ok.json", func(c *Case, _ *Observed) { c.Expect.NeverSay = []string{"(?i)many"} }, `never_say: want no match for "(?i)many", matched "Many"`},
		{"slow median", "traces-ok.json", func(c *Case, _ *Observed) { c.Expect.MedianReplyMSMax = 100 }, "median_reply_ms_max: want at most 100, got 380"},
		{"wrong end reason", "traces-ok.json", func(c *Case, _ *Observed) { c.Expect.EndReason = "ended_by_agent" }, `end_reason: want "ended_by_agent", got "caller_hung_up"`},
		{"dropped events", "traces-ok.json", func(_ *Case, o *Observed) { o.Dropped = 3 }, "events: 3 client events were dropped past the cap, the call was not fully seen"},
		{"too many turns", "traces-ok.json", func(c *Case, _ *Observed) { c.Expect.MaxTurns = 2 }, "max_turns: want at most 2, got 3"},
		{"missing phrase", "traces-ok.json", func(c *Case, _ *Observed) { c.Expect.Say = []string{"sedan"} }, `say: want a match for "sedan"`},
		{"forbidden tool", "traces-ok.json", func(c *Case, _ *Observed) { c.Expect.ToolsNotCalled = []string{"transfer_to_sales"} }, `tools_not_called: want "transfer_to_sales" not called, got [transfer_to_sales:succeeded]`},
	} {
		c, o := baseCase(), observed(t, tc.traces)
		tc.mutate(&c, &o)
		if got := Score(c, o, names); !reflect.DeepEqual(got, []string{tc.want}) {
			t.Errorf("%s: failures = %q, want [%q]", tc.name, got, tc.want)
		}
	}
	// step names are accepted in expect.path
	c := baseCase()
	c.Expect.Path = []string{"Front desk", "sales"}
	if f := Score(c, observed(t, "traces-ok.json"), names); len(f) != 0 {
		t.Errorf("names in path: %v", f)
	}
}

// The server closes a call with an RTVI error message carrying the end reason: that is not a fault.
func TestObserveIgnoresTheEndReasonMessage(t *testing.T) {
	sess := fixture[api.SessionDTO](t, "session.json")
	sess.EndReason = "ended_by_agent"
	tr := fixture[api.SessionTracesDTO](t, "traces-ok.json")
	ev, sent := callEvents()
	ev = append(ev, stack.Event{At: t0.Add(4 * time.Second), Kind: "error", Text: "ended_by_agent"})
	if o := Observe(sess, tr, "front", ev, sent); o.ErrorFrames != 0 {
		t.Errorf("end-reason message counted: %d", o.ErrorFrames)
	}
	ev = append(ev, stack.Event{At: t0.Add(5 * time.Second), Kind: "error", Text: "provider_failed"})
	if o := Observe(sess, tr, "front", ev, sent); o.ErrorFrames != 1 {
		t.Errorf("real error not counted: %d", o.ErrorFrames)
	}
}
