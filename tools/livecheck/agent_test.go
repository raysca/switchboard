package main

import (
	"slices"
	"strings"
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

func TestGraphConfigHasEntryWithSTTAndASalesNodeWithout(t *testing.T) {
	p := Profile{Name: "x"}
	p.Stages.STT = Stage{Provider: "xai"}
	p.Stages.LLM = Stage{Provider: "xai", Model: "grok-3-mini"}
	p.Stages.TTS = Stage{Provider: "xai", Voice: "eve"}
	cfg := graphConfig(p, map[string]string{"stt": "c-stt", "llm": "c-llm", "tts": "c-tts"})
	nodes := cfg["nodes"].([]map[string]any)
	if cfg["entry"] != "front" || len(nodes) != 2 {
		t.Fatalf("entry=%v nodes=%d", cfg["entry"], len(nodes))
	}
	front := nodes[0]["config"].(map[string]any)
	sales := nodes[1]["config"].(map[string]any)
	if _, ok := front["stt"]; !ok {
		t.Fatal("the entry node needs STT")
	}
	if _, ok := sales["stt"]; ok {
		t.Fatal("only the entry node may set STT")
	}
	if !strings.Contains(sales["llm"].(map[string]any)["system_prompt"].(string), graphMarker) {
		t.Fatal("the sales prompt must carry the marker word S14 looks for")
	}
	if front["llm"].(map[string]any)["system_prompt"] == sales["llm"].(map[string]any)["system_prompt"] {
		t.Fatal("each node needs its own prompt")
	}
	e := cfg["edges"].([]map[string]any)
	if len(e) != 1 || e[0]["from"] != "front" || e[0]["to"] != "sales" {
		t.Fatalf("edges = %v", e)
	}
}
