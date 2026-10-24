package eval

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func results(t *testing.T, in Input, judges JudgeFor, th map[string]float64) map[string]Result {
	t.Helper()
	out := map[string]Result{}
	for _, r := range RunScorers(context.Background(), in, judges, th) {
		out[r.Scorer] = r
	}
	if len(out) != len(Builtins()) {
		t.Fatalf("%d results for %d scorers", len(out), len(Builtins()))
	}
	return out
}

func fixedJudge(reply string) JudgeFor {
	return func(string) *Judge {
		return &Judge{Model: "m", Ask: func(context.Context, string, string) (string, error) { return reply, nil }}
	}
}

func TestRunScorersSkipsJudgedWithoutJudge(t *testing.T) {
	in := inputFor(storedCall(true, "succeeded"))
	c := baseCase()
	in.Case = &c
	rs := RunScorers(context.Background(), in, nil, nil)
	for _, r := range rs {
		switch r.Scorer {
		case "correctness", "answer_relevancy", "toxicity":
			if r.Outcome != Skipped || r.Reason != "no judge configured" {
				t.Errorf("%s = %+v, want skipped with %q", r.Scorer, r, "no judge configured")
			}
		}
	}
	if !CaseRunPassed(rs) {
		t.Errorf("a call that meets its expectations did not pass: %+v", rs)
	}
	// a judge that answers nil for one scorer skips just that one
	j := func(s string) *Judge {
		if s == "toxicity" {
			return &Judge{Model: "m", Ask: func(context.Context, string, string) (string, error) { return `{"score":0}`, nil }}
		}
		return nil
	}
	got := results(t, in, j, nil)
	if got["correctness"].Outcome != Skipped || got["toxicity"].Outcome != Pass || got["toxicity"].Score != 1 {
		t.Errorf("per-scorer judge: %+v", got)
	}
}

func TestRunScorersNoStoredHistory(t *testing.T) {
	in := Input{Path: []string{"front"}}
	got := results(t, in, fixedJudge(`{"score":1}`), nil)
	for _, n := range []string{"correctness", "answer_relevancy", "toxicity"} {
		if got[n].Outcome != Skipped || got[n].Reason != "no stored history" {
			t.Errorf("%s = %+v", n, got[n])
		}
	}
	if got["expectations"].Outcome != Pass {
		t.Errorf("expectations = %+v", got["expectations"])
	}
}

func TestJudgeParsesScoreAndRejectsGarbage(t *testing.T) {
	in := inputFor(storedCall(true, "succeeded"))
	for _, tc := range []struct {
		reply   string
		outcome Outcome
		score   float64
	}{
		{`{"score":0.2,"reason":"x"}`, Fail, 0.2},
		{`Sure! {"score":0.9,"reason":"fine"} done`, Errored, 0}, // prose around the object is not a verdict
		{`{"score":0.7}`, Pass, 0.7},
		{"great!", Errored, 0},
		{`{"score":1.5}`, Errored, 0},
		{`{"score":-0.1}`, Errored, 0},
		{`{"reason":"no score"}`, Errored, 0},
	} {
		r := results(t, in, fixedJudge(tc.reply), nil)["correctness"]
		if r.Outcome != tc.outcome || (tc.outcome != Errored && r.Score != tc.score) {
			t.Errorf("%q: %+v, want %s %v", tc.reply, r, tc.outcome, tc.score)
		}
		if r.Outcome != Skipped && r.JudgeModel != "m" {
			t.Errorf("%q: judge model %q", tc.reply, r.JudgeModel)
		}
	}
	fail := func(string) *Judge {
		return &Judge{Ask: func(context.Context, string, string) (string, error) { return "", errors.New("boom") }}
	}
	if r := results(t, in, fail, nil)["correctness"]; r.Outcome != Errored || !strings.Contains(r.Reason, "boom") {
		t.Errorf("judge transport error = %+v", r)
	}
}

func TestToxicityIsInverted(t *testing.T) {
	in := inputFor(storedCall(true, "succeeded"))
	r := results(t, in, fixedJudge(`{"score":0.5,"reason":"rude"}`), nil)
	if tox := r["toxicity"]; tox.Outcome != Fail || tox.Score != 0.5 {
		t.Errorf("toxicity 0.5 = %+v", tox)
	}
	if tox := results(t, in, fixedJudge(`{"score":0.05}`), nil)["toxicity"]; tox.Outcome != Pass || tox.Score != 0.95 {
		t.Errorf("toxicity 0.05 = %+v", tox)
	}
	// a threshold override applies
	if c := results(t, in, fixedJudge(`{"score":0.5}`), map[string]float64{"correctness": 0.4})["correctness"]; c.Outcome != Pass {
		t.Errorf("threshold override ignored: %+v", c)
	}
}

func TestJudgeEvidenceIsBoundedAndNamesToolsAndPrompts(t *testing.T) {
	in := inputFor(storedCall(true, "succeeded"))
	in.Prompts = map[string]string{"front": strings.Repeat("P", 5000), "sales": "sell"}
	for range 400 {
		in.Turns = append(in.Turns, Line{"agent", strings.Repeat("long ", 40)})
	}
	var user string
	j := func(string) *Judge {
		return &Judge{Ask: func(_ context.Context, _, u string) (string, error) { user = u; return `{"score":1}`, nil }}
	}
	results(t, in, j, nil)
	if len(user) > 17<<10 || !strings.Contains(user, "transfer_to_sales") || !strings.Contains(user, "sell") || strings.Contains(user, strings.Repeat("P", 2049)) {
		t.Errorf("evidence: %d bytes, tools/prompts/cap wrong", len(user))
	}
}

func TestToolCallAccuracyOrder(t *testing.T) {
	in := Input{Tools: []ToolRow{{Name: "lookup", Status: "succeeded"}, {Name: "book", Status: "succeeded"}}}
	run := func(e Expect) Result {
		c := Case{Expect: e}
		in.Case = &c
		return results(t, in, nil, nil)["tool_call_accuracy"]
	}
	if r := run(Expect{ToolsCalled: []string{"lookup", "book"}, ToolsOrdered: true}); r.Outcome != Pass {
		t.Errorf("in order: %+v", r)
	}
	r := run(Expect{ToolsCalled: []string{"book", "lookup"}, ToolsOrdered: true})
	if r.Outcome != Fail || !strings.HasPrefix(r.Reason, "tools_order:") {
		t.Errorf("out of order: %+v", r)
	}
	if r := run(Expect{ToolsCalled: []string{"book", "lookup"}}); r.Outcome != Pass {
		t.Errorf("order not requested: %+v", r)
	}
	if r := run(Expect{ToolsCalled: []string{"lookup", "missing"}, ToolsOrdered: true}); r.Outcome != Fail || strings.Contains(r.Reason, "tools_order:") {
		t.Errorf("missing tool is reported as missing, not as order: %+v", r)
	}
	if r := run(Expect{ToolsNotCalled: []string{"book"}}); r.Outcome != Fail || !strings.HasPrefix(r.Reason, "tools_not_called:") {
		t.Errorf("forbidden: %+v", r)
	}
	if r := run(Expect{}); r.Outcome != Skipped || r.Reason != "no tool expectations" {
		t.Errorf("nothing to check: %+v", r)
	}
	in.Case = nil
	if r := results(t, in, nil, nil)["tool_call_accuracy"]; r.Outcome != Skipped {
		t.Errorf("no case: %+v", r)
	}
}

// Each assertion kind keeps the prefix the CLI report and its self-test mutations rely on.
func TestExpectationsReasonsKeepCLIPrefixes(t *testing.T) {
	good := inputFor(storedCall(true, "succeeded"))
	for _, tc := range []struct {
		prefix string
		mutate func(*Case, *Input)
	}{
		{"path:", func(c *Case, _ *Input) { c.Expect.Path = []string{"front"} }},
		{"say:", func(c *Case, _ *Input) { c.Expect.Say = []string{"zzz"} }},
		{"never_say:", func(c *Case, _ *Input) { c.Expect.NeverSay = []string{"(?i)many"} }},
		{"end_reason:", func(c *Case, _ *Input) { c.Expect.EndReason = "ended_by_agent" }},
		{"max_turns:", func(c *Case, _ *Input) { c.Expect.MaxTurns = 1 }},
		{"median_reply_ms_max:", func(c *Case, _ *Input) { c.Expect.MedianReplyMSMax = 1 }},
		{"errors:", func(_ *Case, in *Input) { in.ClientErrors = 2 }},
		{"handoff:", func(_ *Case, in *Input) { in.HandoffFailures = 1 }},
		{"events:", func(_ *Case, in *Input) { in.Dropped = 1 }},
	} {
		c, in := baseCase(), good
		tc.mutate(&c, &in)
		in.Case = &c
		r := results(t, in, nil, nil)["expectations"]
		if r.Outcome != Fail || !strings.HasPrefix(r.Reason, tc.prefix) {
			t.Errorf("%s = %+v", tc.prefix, r)
		}
	}
}

func TestExpectationsScoreIsPassFraction(t *testing.T) {
	c := baseCase()
	c.Expect.Say = []string{"zzz"}
	in := inputFor(storedCall(true, "succeeded"))
	in.Case = &c
	r := results(t, in, nil, nil)["expectations"]
	if r.Score <= 0 || r.Score >= 1 {
		t.Errorf("one failure among many checks scored %v", r.Score)
	}
}

type panicScorer struct{}

func (panicScorer) Name() string              { return "p" }
func (panicScorer) Version() int              { return 1 }
func (panicScorer) Judged() bool              { return false }
func (panicScorer) DefaultThreshold() float64 { return 1 }
func (panicScorer) Score(context.Context, Input, *Judge) (float64, string, error) {
	panic("boom")
}

func TestRunOneRecoversPanicsAndSkips(t *testing.T) {
	r := runOne(context.Background(), panicScorer{}, Input{}, nil, nil)
	if r.Outcome != Errored || r.Reason != "scorer panicked" {
		t.Errorf("panic = %+v", r)
	}
}

func TestLookupAndOrder(t *testing.T) {
	var names []string
	for _, s := range Builtins() {
		names = append(names, s.Name())
		if got, ok := Lookup(s.Name()); !ok || got.Name() != s.Name() {
			t.Errorf("Lookup(%q)", s.Name())
		}
	}
	if want := "expectations tool_call_accuracy correctness answer_relevancy toxicity"; strings.Join(names, " ") != want {
		t.Errorf("order = %v", names)
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("unknown scorer found")
	}
}
