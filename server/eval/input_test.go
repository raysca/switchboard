package eval

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"govoice/server/storage"
	"govoice/transport/wsclient"
)

var t0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

// two typed turns: 120 ms and 380 ms to the first reply text, after a greeting.
func callEvents() ([]wsclient.Event, []sentTurn) {
	ms := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Millisecond) }
	ev := []wsclient.Event{
		{At: ms(10), Kind: "bot-llm-started"}, {At: ms(20), Kind: "bot-llm-text", Text: "Welcome to the desk."},
		{At: ms(1120), Kind: "bot-llm-started"}, {At: ms(1120), Kind: "bot-llm-text", Text: "Sure, "}, {At: ms(1130), Kind: "bot-llm-text", Text: "connecting you."},
		{At: ms(1500), Kind: "bot-llm-started"}, {At: ms(1500), Kind: "bot-llm-text", Text: "We sell hatchbacks."},
		{At: ms(3380), Kind: "bot-llm-text", Text: " Many."},
	}
	return ev, []sentTurn{{At: ms(1000), Text: "I want a car"}, {At: ms(3000), Text: "What kinds?"}}
}

func sec(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }

// storedCall is the stored view of a three-turn call that hands off front -> sales.
func storedCall(withHandoff bool, toolStatus string) Stored {
	st := Stored{
		Session: storage.SessionRecord{ID: "s1", AgentVersionID: "v7", ConfigHash: "abc123", Draft: true, Caller: Principal,
			EndReason: "caller_hung_up", TurnCount: 3, StartedAt: sec(0), EndedAt: sec(20)},
		Turns: []storage.TurnRecord{
			{Ordinal: 1, AssistantText: "Welcome to the desk."},
			{Ordinal: 2, Transcript: "I want a car", AssistantText: "Sure, connecting you. We sell hatchbacks."},
			{Ordinal: 3, Transcript: "What kinds?", AssistantText: "Many."},
		},
		Spans: []storage.TraceSpan{{ID: "p1", Name: "llm.turn", StartedAt: sec(1)}},
		Tools: []storage.ToolCallRecord{{ID: "t1", RemoteName: "transfer_to_sales", Status: toolStatus, StartedAt: sec(5)}},
	}
	if withHandoff {
		st.Spans = append(st.Spans,
			storage.TraceSpan{ID: "p2", Name: "handoff.committed", StartedAt: sec(5), AttributesJSON: `{"from_step":"front","to_step":"sales"}`},
			storage.TraceSpan{ID: "p3", Name: "handoff.active", StartedAt: sec(6), AttributesJSON: `{"from_step":"front","to_step":"sales"}`})
	}
	return st
}

var testAgent = Agent{ID: "a1", Name: "Dealership desk", Start: "front", StepNames: map[string]string{"front": "Front desk", "sales": "Sales"}}

func observed(st Stored) Observed {
	ev, sent := callEvents()
	o := Observed{SessionID: st.Session.ID, Stored: st}
	o.Transcript, o.ReplyMS = clientView(ev, sent)
	return o
}

func TestBuildInput(t *testing.T) {
	in := BuildInput(testAgent, observed(storedCall(true, "succeeded")), nil)
	if in.EndReason != "caller_hung_up" || in.TurnCount != 3 || in.HandoffFailures != 0 {
		t.Errorf("session fields: %+v", in)
	}
	if want := []string{"front", "sales"}; !reflect.DeepEqual(in.Path, want) {
		t.Errorf("path = %v", in.Path)
	}
	if want := []string{"transfer_to_sales:succeeded"}; !reflect.DeepEqual(in.ToolStrings(), want) {
		t.Errorf("tools = %v", in.ToolStrings())
	}
	if want := []int{120, 380}; !reflect.DeepEqual(in.ReplyMS, want) {
		t.Errorf("reply ms = %v, want %v", in.ReplyMS, want)
	}
	want := []Line{{"agent", "Welcome to the desk."}, {"caller", "I want a car"}, {"agent", "Sure, connecting you. We sell hatchbacks."},
		{"caller", "What kinds?"}, {"agent", "Many."}}
	if !reflect.DeepEqual(in.Turns, want) {
		t.Errorf("turns = %v", in.Turns)
	}
	if nh := BuildInput(testAgent, observed(storedCall(false, "succeeded")), nil); !reflect.DeepEqual(nh.Path, []string{"front"}) {
		t.Errorf("no handoff path = %v", nh.Path)
	}
	// The path starts at the agent's start step as given; a handoff from another step is not papered over.
	other := testAgent
	other.Start = "other"
	if got := BuildInput(other, observed(storedCall(true, "succeeded")), nil); !reflect.DeepEqual(got.Path, []string{"other", "sales"}) {
		t.Errorf("path = %v", got.Path)
	}
}

func TestBuildInputStepOfTool(t *testing.T) {
	st := Stored{
		Spans: []storage.TraceSpan{{Name: "handoff.active", StartedAt: sec(2), AttributesJSON: `{"from_step":"front","to_step":"sales"}`}},
		Tools: []storage.ToolCallRecord{
			{RemoteName: "lookup", Status: "succeeded", StartedAt: sec(3)},
			{RemoteName: "greet", Status: "succeeded", StartedAt: sec(1)},
		},
	}
	in := BuildInput(testAgent, Observed{Stored: st}, nil)
	var got []string
	for _, r := range in.Tools {
		got = append(got, r.Name+"@"+r.Step)
	}
	if want := []string{"greet@front", "lookup@sales"}; !slices.Equal(got, want) {
		t.Errorf("tool steps = %v, want %v", got, want)
	}
}

func baseCase() Case {
	return Case{Name: "c", Agent: "Dealership desk", Caller: Caller{Script: []string{"hi"}}, Expect: Expect{
		Path: []string{"front", "sales"}, ToolsCalled: []string{"transfer_to_sales"}, ToolsNotCalled: []string{"end_call"},
		Say: []string{"(?i)hatchback"}, NeverSay: []string{"(?i)as an ai"}, EndReason: "caller_hung_up", MaxTurns: 6, MedianReplyMSMax: 500,
	}}
}

// failLines runs the built-in scorers with no judge and returns the failure reasons, one per check.
func failLines(c Case, in Input) (out []string) {
	in.Case = &c
	for _, r := range RunScorers(context.Background(), in, nil, nil) {
		if r.Outcome == Fail || r.Outcome == Errored {
			out = append(out, strings.Split(r.Reason, "; ")...)
		}
	}
	return out
}

func inputFor(st Stored) Input { return BuildInput(testAgent, observed(st), nil) }

func TestFailLines(t *testing.T) {
	if f := failLines(baseCase(), inputFor(storedCall(true, "succeeded"))); len(f) != 0 {
		t.Fatalf("passing call failed: %v", f)
	}
	for _, tc := range []struct {
		name   string
		st     Stored
		mutate func(*Case, *Input)
		want   string
	}{
		{"no handoff", storedCall(false, "succeeded"), func(c *Case, _ *Input) { c.Expect.ToolsCalled = nil }, "path: want [front sales], got [front]"},
		{"handoff.failed span", func() Stored {
			st := storedCall(false, "succeeded")
			st.Spans = append(st.Spans, storage.TraceSpan{Name: "handoff.failed", StartedAt: sec(6)})
			return st
		}(), func(c *Case, _ *Input) { c.Expect = Expect{} }, "handoff: want 0 failed handoffs, got 1"},
		{"tool failed", storedCall(true, "failed"), func(*Case, *Input) {}, `tools_called: want "transfer_to_sales:succeeded", got [transfer_to_sales:failed]`},
		{"rtvi error", storedCall(true, "succeeded"), func(_ *Case, in *Input) { in.ClientErrors = 1 }, "errors: want 0 RTVI error messages, got 1"},
		{"forbidden phrase", storedCall(true, "succeeded"), func(c *Case, _ *Input) { c.Expect.NeverSay = []string{"(?i)many"} }, `never_say: want no match for "(?i)many", matched "Many"`},
		{"slow median", storedCall(true, "succeeded"), func(c *Case, _ *Input) { c.Expect.MedianReplyMSMax = 100 }, "median_reply_ms_max: want at most 100, got 380"},
		{"wrong end reason", storedCall(true, "succeeded"), func(c *Case, _ *Input) { c.Expect.EndReason = "ended_by_agent" }, `end_reason: want "ended_by_agent", got "caller_hung_up"`},
		{"dropped events", storedCall(true, "succeeded"), func(_ *Case, in *Input) { in.Dropped = 3 }, "events: 3 client events were dropped past the cap, the call was not fully seen"},
		{"too many turns", storedCall(true, "succeeded"), func(c *Case, _ *Input) { c.Expect.MaxTurns = 2 }, "max_turns: want at most 2, got 3"},
		{"missing phrase", storedCall(true, "succeeded"), func(c *Case, _ *Input) { c.Expect.Say = []string{"sedan"} }, `say: want a match for "sedan"`},
		{"forbidden tool", storedCall(true, "succeeded"), func(c *Case, _ *Input) { c.Expect.ToolsNotCalled = []string{"transfer_to_sales"} }, `tools_not_called: want "transfer_to_sales" not called, got [transfer_to_sales:succeeded]`},
	} {
		c, in := baseCase(), inputFor(tc.st)
		tc.mutate(&c, &in)
		if got := failLines(c, in); !reflect.DeepEqual(got, []string{tc.want}) {
			t.Errorf("%s: failures = %q, want [%q]", tc.name, got, tc.want)
		}
	}
	// step names are accepted in expect.path
	c := baseCase()
	c.Expect.Path = []string{"Front desk", "sales"}
	if f := failLines(c, inputFor(storedCall(true, "succeeded"))); len(f) != 0 {
		t.Errorf("names in path: %v", f)
	}
}

// The server closes a call with an RTVI error message carrying the end reason: that is not a fault.
func TestClientErrorsIgnoreTheEndReasonMessage(t *testing.T) {
	ev, _ := callEvents()
	ev = append(ev, wsclient.Event{At: t0.Add(4 * time.Second), Kind: "error", Text: "ended_by_agent"})
	if n := clientErrors("ended_by_agent", ev); n != 0 {
		t.Errorf("end-reason message counted: %d", n)
	}
	ev = append(ev, wsclient.Event{At: t0.Add(5 * time.Second), Kind: "error", Text: "provider_failed"})
	if n := clientErrors("ended_by_agent", ev); n != 1 {
		t.Errorf("real error not counted: %d", n)
	}
}

// Idle and time-limit hang-ups close with a different word than the stored end reason.
func TestClientErrorsIgnoreMappedEndMessages(t *testing.T) {
	for stored, msg := range endMessages {
		ev, _ := callEvents()
		final := append(slices.Clone(ev), wsclient.Event{At: t0.Add(4 * time.Second), Kind: "error", Text: msg})
		if n := clientErrors(stored, final); n != 0 {
			t.Errorf("%s: closing message %q counted", stored, msg)
		}
		// not the last event: a real fault that happens to use the word
		early := slices.Insert(slices.Clone(ev), 2, wsclient.Event{At: t0.Add(30 * time.Millisecond), Kind: "error", Text: msg})
		if n := clientErrors(stored, early); n != 1 {
			t.Errorf("%s: mid-call %q not counted", stored, msg)
		}
	}
	ev, _ := callEvents() // caller_hung_up: user_idle is a fault
	ev = append(ev, wsclient.Event{At: t0.Add(4 * time.Second), Kind: "error", Text: "user_idle"})
	if n := clientErrors("caller_hung_up", ev); n != 1 {
		t.Errorf("user_idle under caller_hung_up not counted")
	}
}
