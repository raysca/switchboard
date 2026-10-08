package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sampleReport() *Report {
	rep := &Report{Started: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), Commit: "abc1234567", GoVersion: "go1.27.1",
		Tags: []string{"xaispeech"}, Modes: []string{"off", "on"}, Scenarios: []string{"S1", "S3", "S4"}, Repeat: 2, Estimate: 42,
		Profiles: []ProfileReport{
			{Name: "google", STT: "google chirp_2 en-US", LLM: "google gemini-2.5-flash", TTS: "google en-US-Journey-F", Notes: "n"},
			{Name: "xai", Skipped: "missing env: XAI_API_KEY", STT: "xai", LLM: "xai grok", TTS: "xai"},
		}}
	add := func(mode, sc, st string, rep2 int, kv ...KV) {
		r := &Result{Profile: "google", Mode: mode, Scenario: sc, Repeat: rep2, Status: st, Metrics: kv}
		if st == stFail {
			r.Checks = []Check{{"one user turn", false, "got 2"}}
		}
		rep.Results = append(rep.Results, r)
	}
	add("off", "S1", stPass, 1, KV{"v2v_ms", int64(1800)}, KV{"stt_final_ms", int64(300)})
	add("off", "S1", stPass, 2, KV{"v2v_ms", int64(2000)})
	add("off", "S3", stNA, 1)
	add("on", "S4", stFail, 1, KV{"user_turns", 2})
	add("on", "S4", stPass, 2, KV{"user_turns", 1})
	return rep
}

func TestRenderMarkdown(t *testing.T) {
	md := renderMarkdown(sampleReport())
	for _, want := range []string{
		"## Mode `off`", "## Mode `on`", "| google | PASS 2/2 1.8s | N/A | - |", "FAIL 1/2",
		"SKIPPED: missing env: XAI_API_KEY", "server commit `abc1234567`", "`xaispeech`", "| google | off | 2000 ms | 300 ms | - | - |",
		"FAIL one user turn got 2",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q\n%s", want, md)
		}
	}
	if strings.Contains(md, "| xai |") && strings.Contains(strings.SplitN(md, "## Mode", 2)[1], "| xai |") {
		t.Error("skipped profile must not appear in the matrix")
	}
}

func TestCellAggregation(t *testing.T) {
	mk := func(st ...string) []*Result {
		var rs []*Result
		for _, s := range st {
			rs = append(rs, &Result{Scenario: "S8", Status: s})
		}
		return rs
	}
	for _, c := range []struct {
		in   []*Result
		want string
	}{{nil, "-"}, {mk(stPass), "PASS"}, {mk(stPass, stFail), "FAIL 1/2"}, {mk(stNA, stNA), "N/A"}, {mk(stErr, stPass), "ERROR"}, {mk(stSkip), "SKIP"}, {mk(stInfo), "info"}} {
		if got := cell(c.in); got != c.want {
			t.Errorf("cell(%v) = %q want %q", c.in, got, c.want)
		}
	}
}

// Every output path must hide env secrets, literal or by pattern.
func TestReportsNeverContainEnvSecrets(t *testing.T) {
	secrets := map[string]string{
		"GOOGLE_API_KEY": "AIzaSyD-literal-secret-value-0123456789", "NVIDIA_API_KEY": "nvapi-AbCdEf0123456789-xyz", "XAI_API_KEY": "xai-9f8e7d6c5b4a39281706",
		"OPENAI_API_KEY": "sk-proj-abcdefghijklmnop", "GROQ_KEY": "gsk_abcdefghijklmnopqrstuv", "CUSTOM_TOKEN": "plainvalue12345",
		"DEEPGRAM_API_KEY": "0123456789abcdef0123456789abcdef01234567",
	}
	var environ []string
	for k, v := range secrets {
		environ = append(environ, k+"="+v)
	}
	r := &redactor{}
	r.addEnv(environ)
	old := theRedactor
	theRedactor = r
	defer func() { theRedactor = old }()

	rep := sampleReport()
	for _, v := range secrets {
		rep.Warnings = append(rep.Warnings, "dial failed with "+v)
		rep.Results[0].Reason = "Authorization: Bearer " + v
		rep.Results[0].Metrics = append(rep.Results[0].Metrics, KV{"reply", "token=" + v})
	}
	dir := t.TempDir()
	md, err := writeReport(rep, dir)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(md)
	b2, _ := os.ReadFile(filepath.Join(dir, "results.json"))
	for _, out := range []string{string(b1), string(b2)} {
		for k, v := range secrets {
			if strings.Contains(out, v) {
				t.Errorf("%s value leaked", k)
			}
		}
	}
	if !json.Valid(b2) {
		t.Error("results.json is not valid JSON after redaction")
	}
	// UUIDs (function ids) and table rules must survive
	if got := r.String("function 877104f7-e885-42b9-8de8-f6e4c6303969 |" + strings.Repeat("-", 60) + "|"); strings.Contains(got, "redacted") {
		t.Errorf("over-redacted: %s", got)
	}
}

func TestRedactorWriterAndService(t *testing.T) {
	r := &redactor{}
	r.add("topsecretvalue")
	var sb strings.Builder
	w := &redactWriter{w: &sb, r: r}
	w.Write([]byte("a topsecret"))
	w.Write([]byte("value b\nclient x@proj.iam.gserviceaccount.com\ntail topsecretvalue"))
	w.Flush()
	if strings.Contains(sb.String(), "topsecretvalue") || strings.Contains(sb.String(), "gserviceaccount") || !strings.Contains(sb.String(), "[redacted]") {
		t.Errorf("out = %q", sb.String())
	}
}

func TestEstimateMath(t *testing.T) {
	o := &opts{s1Turns: 2, mcpURL: ""}
	ps, _ := loadProfiles("")
	var g, f Profile
	for _, p := range ps {
		switch p.Name {
		case "google":
			g = p
		case "fake":
			f = p
		}
	}
	ms := []mode{modes["off"], modes["on"]}
	scs := []scenario{scenarios[0], scenarios[2], scenarios[5], scenarios[7]} // S1 S3 S6 S8
	plan := buildPlan([]Profile{g}, ms, scs, 2, o)
	if len(plan) != 2*4*2 {
		t.Fatalf("plan = %d", len(plan))
	}
	// google (4 TTS requests per reply): S1, 2 turns = stt 1+2*2 + llm 2 + tts 8 = 15; S3 is N/A
	// (chirp_2 has no interims); S6 N/A without -mcp-url; S8 = the call-start STT stream = 1
	want := 2 /*modes*/ * 2 /*repeat*/ * (15 + 1)
	if got := estimate(plan, 4); got != want+4 {
		t.Errorf("estimate = %d want %d", got, want+4)
	}
	o.mcpURL = "https://x.example/mcp"
	if got := estimate(buildPlan([]Profile{g}, ms, scs[2:3], 1, o), 0); got != 2*9 { // 1 turn: stt 3 + llm 2 (tool round) + tts 4
		t.Errorf("S6 estimate = %d", got)
	}
	if got := estimate(buildPlan([]Profile{f}, ms, scenarios, 1, o), 0); got != 0 {
		t.Errorf("fake profile must estimate 0, got %d", got)
	}
}

func TestWER(t *testing.T) {
	for _, c := range []struct {
		ref, hyp string
		want     float64
	}{{"What is two plus two?", "what is 2 + 2", 0}, {"What is two plus two?", "", 1}, {"a b c d", "a x c d", 0.25}, {"", "", 0},
		{"What is two plus two?", "What’s 2+2?", 0}, {"I would like to know", "I'd like to know.", 0}, {"Uh-huh, yeah.", "uh huh yeah", 0},
		{"what the capital of France is.", "'what the capital of france is'", 0}} {
		if got := wer(c.ref, c.hyp); got != c.want {
			t.Errorf("wer(%q,%q) = %v want %v", c.ref, c.hyp, got, c.want)
		}
	}
}

func TestServerYAML(t *testing.T) {
	y := serverYAML(serverSpec{mode: modes["on"], class: classPing, infra: map[string]map[string]string{"xai_llm": {"endpoint": "https://e.example/v1"}}, logLevel: "info"}, 1, 2)
	for _, want := range []string{"ping_interval: 1s", "min_words: 3", `xai_llm: {endpoint: "https://e.example/v1"}`, "google_tts: {sample_rate: 24000}", "idle_timeout: 3s"} {
		if !strings.Contains(y, want) {
			t.Errorf("yaml lacks %q", want)
		}
	}
	if y := serverYAML(serverSpec{mode: modes["off"], class: classIdle}, 1, 2); !strings.Contains(y, "user_timeout: 4s") || strings.Contains(y, "ping_interval") {
		t.Error("idle class yaml wrong")
	}
}

func TestJ1ClipIndex(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "clips.json"), []byte(`{"clips":[{"name":"q","file":"q.wav","text":"What is two plus two?"},{"id":"m","text":"Tell me about the planet Mars."}]}`), 0o644)
	idx := j1Index(dir)
	if idx[normText("what is two plus two")] != filepath.Join(dir, "q.wav") || idx[normText("Tell me about the planet Mars.")] != filepath.Join(dir, "m.wav") {
		t.Errorf("idx = %v", idx)
	}
	os.WriteFile(filepath.Join(dir, "clips.json"), []byte(`{"q":"What is two plus two?"}`), 0o644)
	if idx := j1Index(dir); idx[normText("What is two plus two?")] != filepath.Join(dir, "q.wav") {
		t.Errorf("map shape idx = %v", idx)
	}
}

func TestMCPToolPayloads(t *testing.T) {
	// the agent config references the credential ids it is given and nothing else
	p := Profile{Name: "x"}
	p.Stages.STT, p.Stages.LLM, p.Stages.TTS = Stage{Provider: "deepgram", Language: "en"}, Stage{Provider: "openai", Model: "m"}, Stage{Provider: "cartesia"}
	b, _ := json.Marshal(agentConfig(p, map[string]string{"stt": "s", "llm": "l", "tts": "t"}))
	for _, want := range []string{`"credential_id":"s"`, `"credential_id":"l"`, `"credential_id":"t"`, `"model":"m"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("config lacks %s: %s", want, b)
		}
	}
	if strings.Contains(string(b), `"model":""`) {
		t.Error("empty fields must be omitted")
	}
}

// Streamed bot text keeps its own spacing; transcripts are trimmed and joined with one space.
func TestTextsSpacing(t *testing.T) {
	t0 := time.Now()
	c := &client{}
	for i, e := range []ev{{kind: "bot-llm-text", text: "Two"}, {kind: "bot-llm-text", text: " plus two"}, {kind: "bot-llm-text", text: " is four. "},
		{kind: "user-transcription", text: " What is ", final: true}, {kind: "user-transcription", text: "two plus", final: false},
		{kind: "user-transcription", text: "two plus two? ", final: true}} {
		e.t = t0.Add(time.Duration(i+1) * time.Millisecond)
		c.evs = append(c.evs, e)
	}
	if got := c.texts(t0, "bot-llm-text", false); got != "Two plus two is four." {
		t.Errorf("bot text = %q", got)
	}
	if got := c.texts(t0, "user-transcription", true); got != "What is two plus two?" {
		t.Errorf("finals = %q", got)
	}
	if c.interims(t0) != 1 {
		t.Error("interims")
	}
}

// -write-docs accumulates: a rerun replaces only its own (profile, mode, scenario) cells.
func TestMergeDocsAccumulates(t *testing.T) {
	dir := t.TempDir()
	first := sampleReport() // google off S1 x2, off S3, on S4 x2
	if err := mergeDocs(first, dir); err != nil {
		t.Fatal(err)
	}
	second := &Report{Started: first.Started.Add(time.Hour), Commit: "def", Modes: []string{"off"}, Scenarios: []string{"S7", "S1"}, Repeat: 1,
		Profiles: []ProfileReport{{Name: "nvidia", STT: "nvidia"}, {Name: "google", Skipped: "missing env: GOOGLE_API_KEY"}},
		Results: []*Result{{Profile: "nvidia", Mode: "off", Scenario: "S7", Status: stPass}, {Profile: "google", Mode: "off", Scenario: "S1", Status: stSkip},
			{Profile: "google", Mode: "off", Scenario: "S3", Status: stFail}}}
	if err := mergeDocs(second, dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "agent-matrix.json"))
	var got Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, r := range got.Results {
		count[r.Profile+" "+r.Mode+" "+r.Scenario+" "+r.Status]++
		if r.At == "" {
			t.Errorf("%s %s: no run stamp", r.Profile, r.Scenario)
		}
	}
	want := map[string]int{"google off S1 pass": 2, "google off S3 fail": 1, "google on S4 fail": 1, "google on S4 pass": 1, "nvidia off S7 pass": 1}
	if !reflect.DeepEqual(count, want) {
		t.Errorf("merged = %v", count)
	}
	if len(got.Profiles) != 3 || got.Profiles[1].Name != "google" || got.Profiles[1].Skipped != "" { // the older real config beats a later skip
		t.Errorf("profiles = %+v", got.Profiles)
	}
	md, _ := os.ReadFile(filepath.Join(dir, "agent-matrix.md"))
	for _, w := range []string{"Accumulated over runs", "| nvidia | - | - | - | PASS |", "| google | PASS 2/2 1.8s | FAIL | - | - |", "last run 2026-10-08 13:00Z def", "## Mode `on`"} {
		if !strings.Contains(string(md), w) {
			t.Errorf("md lacks %q\n%s", w, md)
		}
	}
}
