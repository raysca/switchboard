package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"govoice/server/agent"
	"govoice/server/catalog"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestEmbeddedProfilesParse(t *testing.T) {
	ps, err := loadProfiles("")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]Profile{}
	for _, p := range ps {
		names[p.Name] = p
	}
	for _, n := range []string{"google", "google-latest-long", "nvidia", "xai", "deepgram-openai-cartesia", "elevenlabs-google-elevenlabs", "cartesia-nvidia", "xai-llm", "fake"} {
		if _, ok := names[n]; !ok {
			t.Errorf("profile %s missing", n)
		}
	}
	if names["google"].Stages.STT.Model != "chirp_2" || names["google-latest-long"].Stages.STT.Model != "latest_long" {
		t.Error("google STT models changed")
	}
	if n := names["nvidia"]; n.Defaults["NVIDIA_STT_FUNCTION_ID"] != "1598d209-5e27-4d3c-8079-4751568b1081" || n.Defaults["NVIDIA_TTS_FUNCTION_ID"] != "877104f7-e885-42b9-8de8-f6e4c6303969" {
		t.Error("nvidia function ids differ from docs/benchmarks/nvidia-live.md")
	}
}

func TestParseValidation(t *testing.T) {
	good := `profiles:
  - name: a
    stages: {stt: {provider: deepgram}, llm: {provider: openai, model: m}, tts: {provider: cartesia}}
    requires: [DEEPGRAM_API_KEY, OPENAI_API_KEY, CARTESIA_API_KEY]
`
	if _, err := parseProfiles([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]string{
		"unknown key":      strings.Replace(good, "requires", "require", 1),
		"bad stage":        strings.Replace(good, "stt: {provider: deepgram}", "stt: {provider: openai}", 1),
		"unknown provider": strings.Replace(good, "provider: cartesia", "provider: nope", 1),
		"no llm model":     strings.Replace(good, "model: m", "model: ''", 1),
		"undeclared env":   strings.Replace(good, ", CARTESIA_API_KEY", "", 1),
		"bad name":         strings.Replace(good, "name: a", "name: A_b", 1),
		"duplicate":        good + strings.TrimPrefix(good, "profiles:\n"),
		"bad interims":     strings.Replace(good, "requires", "interims: maybe\n    requires", 1),
	} {
		if _, err := parseProfiles([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGateSkipsWithReason(t *testing.T) {
	ps, _ := loadProfiles("")
	by := map[string]Profile{}
	for _, p := range ps {
		by[p.Name] = p
	}
	if why := by["deepgram-openai-cartesia"].gate(envOf(map[string]string{"DEEPGRAM_API_KEY": "x"})); !strings.Contains(why, "OPENAI_API_KEY") || !strings.Contains(why, "CARTESIA_API_KEY") || strings.Contains(why, "DEEPGRAM") {
		t.Errorf("reason = %q", why)
	}
	// nvidia: function ids come from the profile defaults, only the key is needed
	if why := by["nvidia"].gate(envOf(map[string]string{"NVIDIA_API_KEY": "k"})); why != "" {
		t.Errorf("nvidia gated: %s", why)
	}
	if why := by["nvidia"].gate(envOf(nil)); why != "missing env: NVIDIA_API_KEY" {
		t.Errorf("reason = %q", why)
	}
	if by["fake"].gate(envOf(nil)) != "" {
		t.Error("fake profile must always run")
	}
}

func TestResolveExpandsEnvAndDefaults(t *testing.T) {
	ps, _ := loadProfiles("")
	by := map[string]Profile{}
	for _, p := range ps {
		by[p.Name] = p
	}
	r := by["xai"].resolve(envOf(map[string]string{"XAI_MODEL": "grok-x", "XAI_ENDPOINT": "https://e.example/v1/responses"}))
	if r.Stages.LLM.Model != "grok-x" {
		t.Errorf("resolve = %+v", r.Stages.LLM)
	}
	// XAI_ENDPOINT (examples/ points it at chat/completions) must never reach the server config.
	if _, ok := r.Infra["xai_llm"]["endpoint"]; ok {
		t.Errorf("xai profile passed XAI_ENDPOINT through: %+v", r.Infra)
	}
	r = by["xai"].resolve(envOf(nil))
	if r.Stages.LLM.Model != "grok-2-1212" {
		t.Errorf("default model = %q", r.Stages.LLM.Model)
	}
	if _, ok := r.Infra["xai_llm"]["endpoint"]; ok {
		t.Error("empty endpoint must be dropped")
	}
}

func TestTagUnion(t *testing.T) {
	ps, _ := loadProfiles("")
	by := map[string]Profile{}
	for _, p := range ps {
		by[p.Name] = p
	}
	if got := tagUnion([]Profile{by["google"], by["nvidia"]}); len(got) != 0 {
		t.Errorf("google+nvidia tags = %v", got)
	}
	if got := tagUnion([]Profile{by["google"], by["xai"]}); !reflect.DeepEqual(got, []string{"xaispeech"}) {
		t.Errorf("google+xai tags = %v", got)
	}
	// implied even when the profile forgets to declare it
	p := Profile{Name: "x"}
	p.Stages.STT, p.Stages.TTS = Stage{Provider: "xai"}, Stage{Provider: "xai"}
	if got := tagUnion([]Profile{p}); !reflect.DeepEqual(got, []string{"xaispeech"}) {
		t.Errorf("implied tags = %v", got)
	}
	if got := tagUnion([]Profile{by["fake"]}); !reflect.DeepEqual(got, []string{"fakeproviders"}) {
		t.Errorf("fake tags = %v", got)
	}
}

func TestSelectProfiles(t *testing.T) {
	ps, _ := loadProfiles("")
	if _, err := selectProfiles(ps, "google,nope"); err == nil {
		t.Error("unknown profile accepted")
	}
	if _, err := selectProfiles(ps, "fake,google"); err == nil {
		t.Error("fake mixed with real accepted")
	}
	all, err := selectProfiles(ps, "all")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if p.Fake {
			t.Error("all includes the fake profile")
		}
	}
	if got, _ := selectProfiles(ps, "google,google"); len(got) != 1 {
		t.Error("duplicates not collapsed")
	}
}

func TestStandardAgentIsIdenticalAcrossProfiles(t *testing.T) {
	ps, _ := loadProfiles("")
	for _, p := range ps {
		if agentConfig(p)["llm"].(map[string]any)["system_prompt"] != systemPrompt {
			t.Errorf("%s differs", p.Name)
		}
	}
}

// catalogAvailability answers like the server's own check, minus the build's adapter support.
type catalogAvailability struct{ res *catalog.Resolver }

func (a catalogAvailability) Check(prov, svc string) agent.Unavailable {
	if !serves(prov, svc) {
		return agent.Unavailable{Reason: "unsupported_provider"}
	}
	if st := a.res.Status(prov, svc); !st.Detected {
		return agent.Unavailable{Reason: "missing_credential", MissingEnv: st.MissingEnv}
	}
	return agent.Unavailable{}
}

// Every profile's management payload passes the REAL server validation offline (strict decode,
// stage capabilities) and the server's catalog detects every stage from exactly the variables
// the harness hands the child (providerEnv), with its requires set and no other variable.
func TestProfilesPassServerValidation(t *testing.T) {
	ps, _ := loadProfiles("")
	sa := writeFakeSA(t)
	for _, p := range ps {
		if p.Fake {
			continue
		}
		env := map[string]string{"PROVIDER_NOISE_API_KEY": "must-not-pass"}
		for _, v := range p.Requires {
			env[v] = "fake-" + strings.ToLower(v)
		}
		if slices.Contains(p.Requires, "GOOGLE_APPLICATION_CREDENTIALS") {
			env["GOOGLE_APPLICATION_CREDENTIALS"] = sa
		}
		child := map[string]string{}
		for _, kv := range providerEnv(p, envOf(env)) {
			k, v, _ := strings.Cut(kv, "=")
			child[k] = v
		}
		if _, leaked := child["PROVIDER_NOISE_API_KEY"]; leaked {
			t.Fatalf("%s: providerEnv passed a variable the profile does not name", p.Name)
		}
		raw, _ := json.Marshal(agentConfig(p.resolve(envOf(env))))
		c, err := agent.DecodeConfig(raw)
		if err != nil {
			t.Errorf("%s: %v", p.Name, err)
			continue
		}
		if probs, missing := agent.AttachmentProblems(c, catalogAvailability{catalog.NewResolver(providers, envOf(child))}); len(probs) > 0 {
			t.Errorf("%s: %v (missing %v)", p.Name, probs, missing)
		}
	}
}

func writeFakeSA(t *testing.T) string {
	f := filepath.Join(t.TempDir(), "sa.json")
	_ = os.WriteFile(f, []byte(`{"type":"service_account","client_email":"x@p.iam.gserviceaccount.com","private_key":"fake"}`), 0o600)
	return f
}

// The child server inherits an allowlist only: no provider keys, unrelated tokens or real store.
func TestChildEnvAllowlist(t *testing.T) {
	got := childEnv([]string{"PATH=/bin", "HOME=/h", "GOOGLE_API_KEY=k", "CLAUDE_CODE_MESSAGING_TOKEN=t", "GOVOICE_DATABASE_DSN=postgres://x",
		"GOVOICE_CREDENTIAL_KEYS=k", "AWS_SECRET_ACCESS_KEY=s", "GOVOICE_ORT_LIB=/lib/ort.dylib"})
	if !reflect.DeepEqual(got, []string{"PATH=/bin", "HOME=/h", "GOVOICE_ORT_LIB=/lib/ort.dylib"}) {
		t.Errorf("childEnv = %v", got)
	}
}

// -max-calls: items that would push the running total over the limit are skipped, never started.
func TestRunPlanStopsAtMaxCalls(t *testing.T) {
	ps, _ := loadProfiles("")
	var g Profile
	for _, p := range ps {
		if p.Name == "google" {
			g = p
		}
	}
	plan := buildPlan([]Profile{g}, []mode{modes["off"]}, []scenario{scenarios[0], scenarios[6]}, 1, &opts{s1Turns: 2})
	rep := &Report{}
	// 5 already spent on clips; S1 (7) would exceed 10, so nothing may start (no server is built here)
	runPlan(context.Background(), runCfg{plan: plan, o: &opts{}, maxCalls: 10, spentBase: 5}, rep)
	if len(rep.Results) != 2 {
		t.Fatalf("results = %d", len(rep.Results))
	}
	for _, r := range rep.Results {
		if r.Status != stSkip || !strings.Contains(r.Reason, "-max-calls") {
			t.Errorf("%s: %s %s", r.Scenario, r.Status, r.Reason)
		}
	}
}
