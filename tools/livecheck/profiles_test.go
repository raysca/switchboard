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
	"govoice/server/provider"
	"govoice/server/storage"
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
	if why := by["google"].gate(envOf(map[string]string{"GOOGLE_API_KEY": "k", "GOOGLE_APPLICATION_CREDENTIALS": "/nonexistent.json"})); !strings.Contains(why, "not readable") {
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

// A mixed-vendor profile stores one credential per provider and references them per stage.
func TestCredPlanMixedVendors(t *testing.T) {
	ps, _ := loadProfiles("")
	var p Profile
	for _, x := range ps {
		if x.Name == "cartesia-nvidia" {
			p = x
		}
	}
	secrets, stageKey, err := credPlan(p, p.lookup(envOf(map[string]string{"CARTESIA_API_KEY": "ck", "NVIDIA_API_KEY": "nk"})))
	if err != nil {
		t.Fatal(err)
	}
	if len(secrets) != 3 { // cartesia, nvidia llm key, nvidia riva json
		t.Fatalf("secrets = %d: %v", len(secrets), stageKey)
	}
	if stageKey["stt"] != "cartesia/key" || stageKey["llm"] != "nvidia/llm" || stageKey["tts"] != "nvidia/riva" {
		t.Errorf("stageKey = %v", stageKey)
	}
	if secrets["nvidia/llm"] != "nk" || !strings.Contains(secrets["nvidia/riva"], "877104f7-e885-42b9-8de8-f6e4c6303969") {
		t.Error("nvidia secrets wrong")
	}
	cfg := agentConfig(p.resolve(envOf(nil)), map[string]string{"stt": "c1", "llm": "c2", "tts": "c3"})
	llm := cfg["llm"].(map[string]any)
	if llm["credential_id"] != "c2" || llm["system_prompt"] != systemPrompt {
		t.Errorf("llm stage = %v", llm)
	}
}

func TestStandardAgentIsIdenticalAcrossProfiles(t *testing.T) {
	ps, _ := loadProfiles("")
	for _, p := range ps {
		if agentConfig(p, nil)["llm"].(map[string]any)["system_prompt"] != systemPrompt {
			t.Errorf("%s differs", p.Name)
		}
	}
}

// Every profile's management payload passes the REAL server validation offline: strict decode,
// provider ids and stage capabilities (server/agent), the server's own factories, and
// per-stage credential references to a credential of the right provider.
func TestProfilesPassServerValidation(t *testing.T) {
	ps, _ := loadProfiles("")
	for _, p := range ps {
		if p.Fake {
			continue
		}
		env := map[string]string{}
		for _, v := range p.Requires {
			env[v] = "fake-" + strings.ToLower(v)
		}
		env["GOOGLE_APPLICATION_CREDENTIALS"] = writeFakeSA(t)
		lk := p.lookup(envOf(env))
		secrets, stageKey, err := credPlan(p, lk)
		if err != nil {
			t.Errorf("%s: %v", p.Name, err)
			continue
		}
		var creds []storage.Credential
		ids := map[string]string{}
		for k := range secrets {
			prov, _, _ := strings.Cut(k, "/")
			ids[k] = "cred_" + strings.ReplaceAll(k, "/", "_")
			creds = append(creds, storage.Credential{ID: ids[k], Provider: prov, ValidationState: "valid"})
		}
		cred := map[string]string{}
		for st, k := range stageKey {
			cred[st] = ids[k]
		}
		raw, _ := json.Marshal(agentConfig(p.resolve(envOf(env)), cred))
		c, err := agent.DecodeConfig(raw)
		if err != nil {
			t.Errorf("%s: %v", p.Name, err)
			continue
		}
		defs := provider.Definitions()
		if slices.Contains(p.tags(), "xaispeech") { // what -tags xaispeech compiles in (server/provider/xaispeech.go)
			for i := range defs {
				if defs[i].ID == "xai" {
					defs[i].Capabilities = []provider.Capability{provider.CapabilityLLM, provider.CapabilitySTT, provider.CapabilityTTS}
				}
			}
		}
		if probs := agent.AttachmentProblems(c, provider.NewRegistry(defs), creds); len(probs) > 0 {
			t.Errorf("%s: %v", p.Name, probs)
		}
		if s := secrets["nvidia/riva"]; s != "" {
			var nv struct{ APIKey, FunctionID, TTSFunctionID string }
			_ = json.Unmarshal([]byte(strings.NewReplacer("api_key", "APIKey", "tts_function_id", "TTSFunctionID", "function_id", "FunctionID").Replace(s)), &nv)
			if nv.APIKey == "" || (p.Stages.TTS.Provider == "nvidia" && nv.TTSFunctionID == "") || (p.Stages.STT.Provider == "nvidia" && nv.FunctionID == "") {
				t.Errorf("%s: riva credential lacks key or function ids", p.Name)
			}
		}
	}
	// without the tag the server rejects xai speech: the harness must build with it
	p := Profile{}
	p.Stages.STT, p.Stages.LLM, p.Stages.TTS = Stage{Provider: "xai"}, Stage{Provider: "xai", Model: "m"}, Stage{Provider: "xai"}
	raw, _ := json.Marshal(agentConfig(p, map[string]string{"stt": "c", "llm": "c", "tts": "c"}))
	c, _ := agent.DecodeConfig(raw)
	if probs := agent.AttachmentProblems(c, provider.NewRegistry(provider.Definitions()), []storage.Credential{{ID: "c", Provider: "xai"}}); len(probs) == 0 {
		t.Error("untagged registry accepted xai speech; drop the implied xaispeech tag")
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
