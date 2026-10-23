package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadBadCases(t *testing.T) {
	want := map[string]string{
		"unknown-key":     "pth",
		"both-callers":    "caller: set exactly one",
		"neither-caller":  "caller: set exactly one",
		"bad-regexp":      "expect.say[0]",
		"bad-end-reason":  "expect.end_reason",
		"bad-tool-status": "expect.tools_called[0]",
		"persona-turns":   "caller.persona.max_turns",
		"empty-line":      "caller.script[0]",
	}
	files, _ := filepath.Glob("testdata/cases-bad/*.yaml")
	if len(files) != len(want) {
		t.Fatalf("%d bad files, %d expectations", len(files), len(want))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		dir := t.TempDir()
		data, _ := os.ReadFile(f)
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(f)), data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadCases(dir, nil)
		if err == nil || !strings.Contains(err.Error(), filepath.Base(f)) || !strings.Contains(err.Error(), want[name]) {
			t.Errorf("%s: err = %v, want file name and %q", name, err, want[name])
		}
	}
}

func TestLoadDuplicateNames(t *testing.T) {
	_, err := LoadCases("testdata/cases-dup", nil)
	if err == nil || !strings.Contains(err.Error(), "b.yaml") || !strings.Contains(err.Error(), "already used by") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadGoodCases(t *testing.T) {
	got, err := LoadCases("testdata/cases-good", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Case{{
		Name: "dealership-buyer-reaches-sales", Agent: "Dealership desk", File: "testdata/cases-good/ok.yaml",
		Caller: Caller{Script: []string{"Hi, I'd like to buy a car.", "What kinds of cars do you sell?"}},
		Expect: Expect{Path: []string{"front", "sales"}, ToolsCalled: []string{"transfer_to_sales", "find_slots:failed"},
			ToolsNotCalled: []string{"end_call"}, Say: []string{"(?i)hatchback|suv"}, NeverSay: []string{"(?i)as an ai"},
			EndReason: "caller_hung_up", MaxTurns: 6, MedianReplyMSMax: 8000},
	}, {
		Name: "persona-case", Agent: "Smile Dental front desk", File: "testdata/cases-good/persona.yaml",
		Caller: Caller{Persona: &Persona{Who: "Jane", Goal: "book a cleaning", MaxTurns: 8}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	only, err := LoadCases("testdata/cases-good", []string{"persona-case"})
	if err != nil || len(only) != 1 || only[0].Name != "persona-case" {
		t.Fatalf("only = %+v, %v", only, err)
	}
	if _, err := LoadCases("testdata/cases-good", []string{"nope"}); err == nil {
		t.Fatal("unknown -case name accepted")
	}
}
