package main

import (
	"slices"
	"strings"
	"testing"
)

func TestFailoverFallsBackToAnotherProviderWithTheBrokenKeyInTheEnv(t *testing.T) {
	ps, _ := loadProfiles("")
	by := map[string]Profile{}
	for _, p := range ps {
		by[p.Name] = p
	}
	p := by["xai-llm"].resolve(envOf(nil))
	prov, model, na := failoverPeer(p)
	if na != "" || prov != "google" || model != "gemini-2.5-flash" {
		t.Fatalf("peer = %q %q (na %q), want google on its catalog default", prov, model, na)
	}
	cfg := failoverConfig(p, prov, model)
	llm := cfg["llm"].(map[string]any)
	if llm["provider"] != "xai" {
		t.Fatalf("primary = %v", llm["provider"])
	}
	fb := llm["fallbacks"].([]map[string]any)
	if len(fb) != 1 || fb[0]["provider"] != "google" || fb[0]["model"] != "gemini-2.5-flash" {
		t.Fatalf("fallbacks = %v", fb)
	}
	if on := cfg["failover"].(map[string]any)["on"].([]string); !slices.Contains(on, "client_error") {
		t.Fatalf("failover classes %v must include client_error (a rejected key)", on)
	}
	env := providerEnv(p, envOf(map[string]string{"XAI_API_KEY": "xai-realrealrealreal", "GOOGLE_APPLICATION_CREDENTIALS": "/sa.json"}))
	got := brokenEnv(p, env)
	if !slices.Contains(got, "XAI_API_KEY=xai-realrealrealzzzz") || !slices.Contains(got, "GOOGLE_APPLICATION_CREDENTIALS=/sa.json") || len(got) != len(env) {
		t.Fatalf("brokenEnv = %v", got)
	}
	// one provider for everything: the broken key would break speech too
	for _, n := range []string{"xai", "google", "nvidia", "deepgram-openai-cartesia"} {
		if _, _, na := failoverPeer(by[n]); na == "" {
			t.Errorf("%s: S13 must be N/A", n)
		}
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

func TestFlowConfigHasEntryWithSTTAndASalesStepWithout(t *testing.T) {
	p := Profile{Name: "x"}
	p.Stages.STT = Stage{Provider: "xai"}
	p.Stages.LLM = Stage{Provider: "xai", Model: "grok-3-mini"}
	p.Stages.TTS = Stage{Provider: "xai", Voice: "eve"}
	cfg := routedFlowConfig(p)
	steps := cfg["nodes"].([]map[string]any)
	if cfg["entry"] != "front" || len(steps) != 2 {
		t.Fatalf("entry=%v nodes=%d", cfg["entry"], len(steps))
	}
	front := steps[0]["config"].(map[string]any)
	sales := steps[1]["config"].(map[string]any)
	if _, ok := front["stt"]; !ok {
		t.Fatal("the entry node needs STT")
	}
	if _, ok := sales["stt"]; ok {
		t.Fatal("only the entry node may set STT")
	}
	if !strings.Contains(sales["llm"].(map[string]any)["system_prompt"].(string), flowMarker) {
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
