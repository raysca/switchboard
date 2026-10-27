package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"govoice/server/eval"
	"govoice/tools/internal/stack"
)

func TestPlanAndGate(t *testing.T) {
	script := eval.Case{Name: "s", Caller: eval.Caller{Script: []string{"a", "b"}}}
	persona := eval.Case{Name: "p", Caller: eval.Caller{Persona: &eval.Persona{Who: "w", Goal: "g", MaxTurns: 8}}}
	if got := plannedTurns([]eval.Case{script}, 3); got != 6 {
		t.Errorf("plannedTurns = %d", got)
	}
	if err := checkPlan([]eval.Case{script}, 3, 6); err != nil {
		t.Errorf("exact cap refused: %v", err)
	}
	if err := checkPlan([]eval.Case{script}, 3, 5); err == nil || !strings.Contains(err.Error(), "-max-turns") {
		t.Errorf("over the cap accepted: %v", err)
	}
	if err := checkPlan([]eval.Case{persona}, 1, 40); err != nil {
		t.Errorf("persona case refused: %v", err)
	}
	if err := checkPlan([]eval.Case{persona}, 2, 15); err == nil || !strings.Contains(err.Error(), "-max-turns") {
		t.Errorf("persona max_turns x runs over the cap accepted: %v", err)
	}
	if err := liveGate(true, ""); err == nil || !strings.Contains(err.Error(), "GOVOICE_LIVE=1") {
		t.Errorf("paid run allowed without the gate: %v", err)
	}
	if liveGate(true, "1") != nil || liveGate(false, "") != nil {
		t.Error("gate refused an allowed run")
	}
}

func TestParseFlagsRefusals(t *testing.T) {
	for _, args := range [][]string{{"-providers", "live"}, {"-runs", "0"}, {"-runs", "2", "-min-pass", "3"}, {"-min-pass", "0"},
		{"-max-turns", "0"}, {"-caller-llm", "gpt"}, {"-caller-llm", "xai", "-caller-url", "https://evil.example/v1"}, {"-version", "-1"}, {"-turn-timeout", "0s"}, {"-bogus"}} {
		if _, err := parseFlags(args, io.Discard); err == nil {
			t.Errorf("parseFlags(%v) accepted", args)
		}
	}
	cfg, err := parseFlags(nil, io.Discard)
	if err != nil || cfg.Mode != eval.ModeKeep || cfg.Runs != 1 || cfg.MinPass != 1 || cfg.MaxTurns != 40 {
		t.Errorf("defaults: %+v, %v", cfg, err)
	}
}

func TestHelpPrintsUsageAndSucceeds(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := realMain([]string{"-h"}, &out, &errb); rc != 0 {
		t.Errorf("-h exited %d", rc)
	}
	if got := errb.String(); !strings.Contains(got, "-max-turns") || !strings.Contains(got, "-providers") || !strings.Contains(got, "MCP tools run for real") {
		t.Errorf("-h usage:\n%s", got)
	}
}

// prepare names every missing agent in one error, so one run shows the whole fix.
func TestPrepareReportsAllMissingAgents(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer api.Close()
	cases := []eval.Case{{Name: "one", Agent: "Alpha"}, {Name: "two", Agent: "Beta"}, {Name: "three", Agent: "Alpha"}}
	_, _, err := prepare(&stack.Server{Admin: api.URL}, cases, config{Mode: eval.ModeOffline})
	if err == nil {
		t.Fatal("missing agents accepted")
	}
	for _, want := range []string{`case one: agent "Alpha"`, `case two: agent "Beta"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

// The example cases that ship with the tool (make eval-agents) must load.
func TestExampleCasesLoad(t *testing.T) {
	for dir, want := range map[string][]string{
		"cases":                      {"dealership-buyer-reaches-sales", "dev-assistant-answers"},
		"../../../demos/dental/eval": {"dental-new-patient-books"},
	} {
		cs, err := eval.LoadCases(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range cs {
			names = append(names, c.Name)
		}
		if !slices.Equal(names, want) {
			t.Errorf("%s = %v, want %v", dir, names, want)
		}
	}
}

func TestScoreCallFailureLines(t *testing.T) {
	c := eval.Case{Name: "c", Expect: eval.Expect{Say: []string{"zzz"}, ToolsCalled: []string{"book"}}}
	in := eval.Input{Case: &c, Turns: []eval.Line{{Who: "caller", Text: "hi"}, {Who: "agent", Text: "hello"}}}
	scores, fails := scoreCall(context.Background(), in, nil)
	if len(scores) != 5 {
		t.Fatalf("scores = %+v", scores)
	}
	want := []string{"say: want a match for \"zzz\"", "tools_called: want \"book:succeeded\", got []"}
	if !slices.Equal(fails, want) {
		t.Errorf("failures = %q, want %q", fails, want)
	}
	for _, s := range scores {
		if s.Scorer == "correctness" && s.Outcome != "skipped" {
			t.Errorf("no judge: %+v", s)
		}
	}
	bad := &eval.Judge{Model: "m", Ask: func(context.Context, string, string) (string, error) {
		return `{"score":0.1,"reason":"off topic"}`, nil
	}}
	_, fails = scoreCall(context.Background(), in, bad)
	if !slices.Contains(fails, "correctness: off topic") || !slices.Contains(fails, "answer_relevancy: off topic") {
		t.Errorf("judged failures = %q", fails)
	}
}

// A reason holding "; " is one failure line; a skipped scorer has a null score, a graded one a number.
func TestScoreCallKeepsReasonsWhole(t *testing.T) {
	in := eval.Input{Turns: []eval.Line{{Who: "caller", Text: "hi"}, {Who: "agent", Text: "hello"}}}
	judge := &eval.Judge{Model: "m", Ask: func(context.Context, string, string) (string, error) {
		return `{"score":0.1,"reason":"off topic; rambling"}`, nil
	}}
	scores, fails := scoreCall(context.Background(), in, judge)
	if !slices.Contains(fails, "correctness: off topic; rambling") {
		t.Errorf("failures = %q", fails)
	}
	broken := &eval.Judge{Model: "m", Ask: func(context.Context, string, string) (string, error) { return "", errors.New("boom") }}
	if _, fails = scoreCall(context.Background(), in, broken); !slices.Contains(fails, "correctness: judge: boom") {
		t.Errorf("errored judge failures = %q", fails)
	}
	skipped, _ := scoreCall(context.Background(), in, nil)
	for _, s := range append(scores, skipped...) {
		b, _ := json.Marshal(s)
		wantNull := s.Outcome == "skipped" || s.Outcome == "error"
		if got := strings.Contains(string(b), `"score":null`); got != wantNull {
			t.Errorf("%s: %s", s.Outcome, b)
		}
	}
}

func TestJudgeFlag(t *testing.T) {
	if _, err := parseFlags([]string{"-judge", "gpt"}, io.Discard); err == nil {
		t.Error("-judge gpt accepted")
	}
	if c, err := parseFlags([]string{"-judge", "offline"}, io.Discard); err != nil || c.Judge != judgeOffline {
		t.Errorf("-judge offline: %+v, %v", c, err)
	}
	if c, _ := parseFlags(nil, io.Discard); c.Judge != judgeNone {
		t.Errorf("default judge = %q", c.Judge)
	}
}
