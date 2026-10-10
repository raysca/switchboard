package main

import (
	"slices"
	"testing"
)

func TestFailoverConfigBreaksPrimaryAndFallsBackToRealKey(t *testing.T) {
	p := Profile{Name: "x"}
	p.Stages.STT = Stage{Provider: "xai"}
	p.Stages.LLM = Stage{Provider: "xai", Model: "grok-3-mini"}
	p.Stages.TTS = Stage{Provider: "xai", Voice: "eve"}
	cfg := failoverConfig(p, map[string]string{"stt": "c-stt", "llm": "c-llm", "tts": "c-tts"}, "c-bad")
	llm := cfg["llm"].(map[string]any)
	if llm["credential_id"] != "c-bad" {
		t.Fatalf("primary credential = %v, want the broken one", llm["credential_id"])
	}
	fb := llm["fallbacks"].([]map[string]any)
	if len(fb) != 1 || fb[0]["credential_id"] != "c-llm" || fb[0]["provider"] != "xai" || fb[0]["model"] != "grok-3-mini" {
		t.Fatalf("fallbacks = %v, want the real key on the same provider and model", fb)
	}
	on := cfg["failover"].(map[string]any)["on"].([]string)
	if !slices.Contains(on, "client_error") {
		t.Fatalf("failover classes %v must include client_error (a rejected key)", on)
	}
}

func TestBrokenSecretChangesTheKeyButKeepsItsShape(t *testing.T) {
	for _, s := range []string{"xai-abcdefghijklmnop", "AIzaSyExampleExampleExample12"} {
		b := brokenSecret(s)
		if b == s || len(b) != len(s) || b[:len(s)-4] != s[:len(s)-4] {
			t.Fatalf("brokenSecret(%q) = %q", s, b)
		}
	}
}

// Every scenario in the table reaches the plan: a scenario whose server class the plan
// does not iterate would be dropped silently (no result, no error).
func TestBuildPlanCoversEveryScenario(t *testing.T) {
	p := Profile{Name: "x", Fake: true}
	plan := buildPlan([]Profile{p}, []mode{modes["off"]}, scenarios, 1, &opts{})
	got := map[string]bool{}
	for _, it := range plan {
		got[it.sc.id] = true
	}
	for _, sc := range scenarios {
		if !got[sc.id] {
			t.Errorf("%s (class %s) is missing from the plan", sc.id, sc.class.name)
		}
	}
}
