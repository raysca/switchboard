package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
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
	if err := checkPlan([]Case{persona}, 1, 40); err == nil || !strings.Contains(err.Error(), "persona") {
		t.Errorf("persona caller accepted before it exists: %v", err)
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
		{"-max-turns", "0"}, {"-version", "-1"}, {"-turn-timeout", "0s"}, {"-bogus"}} {
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
	if got := errb.String(); !strings.Contains(got, "-max-turns") || !strings.Contains(got, "-providers") {
		t.Errorf("-h usage:\n%s", got)
	}
}
