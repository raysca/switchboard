package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"govoice/tools/internal/stack"
)

func TestPlanAndGate(t *testing.T) {
	script := Case{Name: "s", Caller: Caller{Script: []string{"a", "b"}}}
	persona := Case{Name: "p", Caller: Caller{Persona: &Persona{Who: "w", Goal: "g", MaxTurns: 8}}}
	if got := plannedTurns([]Case{script}, 3); got != 6 {
		t.Errorf("plannedTurns = %d", got)
	}
	if err := checkPlan([]Case{script}, 3, 6); err != nil {
		t.Errorf("exact cap refused: %v", err)
	}
	if err := checkPlan([]Case{script}, 3, 5); err == nil || !strings.Contains(err.Error(), "-max-turns") {
		t.Errorf("over the cap accepted: %v", err)
	}
	if err := checkPlan([]Case{persona}, 1, 40); err != nil {
		t.Errorf("persona case refused: %v", err)
	}
	if err := checkPlan([]Case{persona}, 2, 15); err == nil || !strings.Contains(err.Error(), "-max-turns") {
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
	if err != nil || cfg.Mode != modeKeep || cfg.Runs != 1 || cfg.MinPass != 1 || cfg.MaxTurns != 40 {
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
	cases := []Case{{Name: "one", Agent: "Alpha"}, {Name: "two", Agent: "Beta"}, {Name: "three", Agent: "Alpha"}}
	_, _, err := prepare(&stack.Server{Admin: api.URL}, cases, config{Mode: modeOffline})
	if err == nil {
		t.Fatal("missing agents accepted")
	}
	for _, want := range []string{`case one: agent "Alpha"`, `case two: agent "Beta"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}
