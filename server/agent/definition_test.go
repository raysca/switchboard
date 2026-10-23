package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const flatCfg = `{"stt":{"provider":"deepgram"},"llm":{"provider":"openai","model":"m","system_prompt":"hi"},"tts":{"provider":"cartesia","voice":"v"}}`

func step(id, name string, extra string) string {
	cfg := `{"llm":{"provider":"openai","model":"m","system_prompt":"` + name + `"},"tts":{"provider":"cartesia","voice":"v"}` + extra + `}`
	return `{"id":"` + id + `","name":"` + name + `","config":` + cfg + `}`
}

func TestDecodeAgentWrapsFlatConfig(t *testing.T) {
	g, err := Decode(json.RawMessage(flatCfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Steps) != 1 || g.Start != g.Steps[0].ID || len(g.Handoffs) != 0 {
		t.Fatalf("flat config must become a one-node graph, got %+v", g)
	}
	if g.StartStep().Config.LLM.Model != "m" {
		t.Fatal("entry config lost")
	}
}

func TestDecodeAgentTwoSteps(t *testing.T) {
	raw := `{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","config":` + flatCfg + `},` +
		step("sales", "Sales", "") + `],` +
		`"handoffs":[{"id":"e1","from":"front","to":"sales","condition":"wants to buy"}]}`
	g, err := Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := TransferToolName("Sales Team"); got != "transfer_to_sales_team" {
		t.Fatalf("tool name = %q", got)
	}
	if len(g.HandoffsFrom("front")) != 1 || len(g.HandoffsFrom("sales")) != 0 {
		t.Fatal("RoutesFrom wrong")
	}
}

// The pre-rename keys (entry/nodes/edges, and the pre-handoff list and cue keys) are read nowhere: nothing stored uses them.
func TestOldConfigKeysAreRejectedLikeAnyUnknownField(t *testing.T) {
	steps := `[{"id":"front","name":"Front desk","config":` + flatCfg + `},` + step("sales", "Sales", "") + `]`
	for name, raw := range map[string]string{
		"entry and nodes":  `{"entry":"front","nodes":` + steps + `,"edges":[]}`,
		"pre-handoff list": `{"start":"front","steps":` + steps + `,"` + "rou" + `tes":[]}`,
		"pre-handoff cue":  `{"start":"front","steps":` + steps + `,"handoffs":[],"` + "rou" + `te_cue":{"name":"chime"}}`,
	} {
		if _, err := Decode(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDecodeAgentRejects(t *testing.T) {
	front := `{"id":"front","name":"Front","config":` + flatCfg + `}`
	cases := map[string]string{
		"no entry":                `{"steps":[` + front + `],"handoffs":[]}`,
		"entry missing":           `{"start":"x","steps":[` + front + `],"handoffs":[]}`,
		"dup node id":             `{"start":"front","steps":[` + front + `,` + front + `],"handoffs":[]}`,
		"dup slug":                `{"start":"front","steps":[` + front + `,` + step("b", "front", "") + `],"handoffs":[]}`,
		"edge to nowhere":         `{"start":"front","steps":[` + front + `],"handoffs":[{"id":"e","from":"front","to":"zz","condition":"c"}]}`,
		"self edge":               `{"start":"front","steps":[` + front + `],"handoffs":[{"id":"e","from":"front","to":"front","condition":"c"}]}`,
		"empty condition":         `{"start":"front","steps":[` + front + `,` + step("b", "B", "") + `],"handoffs":[{"id":"e","from":"front","to":"b","condition":" "}]}`,
		"dup edge pair":           `{"start":"front","steps":[` + front + `,` + step("b", "B", "") + `],"handoffs":[{"id":"e1","from":"front","to":"b","condition":"c"},{"id":"e2","from":"front","to":"b","condition":"d"}]}`,
		"call-level on non-entry": `{"start":"front","steps":[` + front + `,` + step("b", "B", `,"vad_stop_ms":300`) + `],"handoffs":[]}`,
		"unknown field":           `{"start":"front","steps":[` + front + `],"handoffs":[],"bogus":1}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("want ErrInvalidConfig, got %v", err)
			}
		})
	}
}

func TestDecodeAgentBounds(t *testing.T) {
	var steps []string
	for i := 0; i <= MaxSteps; i++ {
		steps = append(steps, step("n"+strings.Repeat("x", i), "N"+strings.Repeat("x", i), ""))
	}
	raw := `{"start":"n","steps":[` + strings.Join(steps, ",") + `],"handoffs":[]}`
	if _, err := Decode(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("more than %d nodes must be rejected, got %v", MaxSteps, err)
	}
}

func TestSummarizationNeedsEverySummarizingLLM(t *testing.T) {
	entry := `{"stt":{"provider":"deepgram"},"llm":{"provider":"openai","model":"m"},"tts":{"provider":"cartesia"},"context":{"summarization":{"enabled":true}}}`
	other := `{"id":"b","name":"B","config":{"llm":{"provider":"cartesia","model":"m"},"tts":{"provider":"cartesia"}}}`
	raw := `{"start":"a","steps":[{"id":"a","name":"A","config":` + entry + `},` + other + `],"handoffs":[]}`
	if _, err := Decode(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
}

func TestDecodeAgentRejectsUnsafeIDsAndCue(t *testing.T) {
	front := func(id string) string { return `{"id":"` + id + `","name":"Front","config":` + flatCfg + `}` }
	graph := func(entry, step, extra string) string {
		return `{"start":"` + entry + `","steps":[` + step + `],"handoffs":[]` + extra + `}`
	}
	cases := map[string]string{
		"id with spaces":     graph("a b", front("a b"), ""),
		"id with arrow":      graph("a->b", front("a->b"), ""),
		"id too long":        graph(strings.Repeat("x", 33), front(strings.Repeat("x", 33)), ""),
		"cue name uppercase": graph("f", front("f"), `,"handoff_cue":{"name":"Chime"}`),
		"cue name too long":  graph("f", front("f"), `,"handoff_cue":{"name":"`+strings.Repeat("c", 33)+`"}`),
		"cue after negative": graph("f", front("f"), `,"handoff_cue":{"name":"chime","after_ms":-1}`),
		"cue after huge":     graph("f", front("f"), `,"handoff_cue":{"name":"chime","after_ms":600000}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("want ErrInvalidConfig, got %v", err)
			}
		})
	}
	ok := graph("f_1-a", front("f_1-a"), `,"handoff_cue":{"name":"chime","after_ms":300}`)
	if _, err := Decode(json.RawMessage(ok)); err != nil {
		t.Fatalf("valid ids and cue rejected: %v", err)
	}
}

func TestDecodeStepEndCall(t *testing.T) {
	step := `"config":{"llm":{"provider":"google","model":"m"},"tts":{"provider":"google"}}`
	front := `"config":{"stt":{"provider":"google"},"llm":{"provider":"google","model":"m"},"tts":{"provider":"google"}}`
	g, err := Decode(json.RawMessage(`{"start":"a","steps":[` +
		`{"id":"a","name":"A","end_call":true,` + front + `},{"id":"b","name":"B",` + step + `}],` +
		`"handoffs":[{"id":"r","from":"a","to":"b","condition":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := g.Step("a")
	b, _ := g.Step("b")
	if !a.EndCall || b.EndCall {
		t.Fatalf("EndCall a=%v b=%v, want true,false", a.EndCall, b.EndCall)
	}
}

func TestDecodeAcceptsEndCallOnAOneStepAgent(t *testing.T) {
	front := `"config":{"stt":{"provider":"google"},"llm":{"provider":"google","model":"m"},"tts":{"provider":"google"}}`
	g, err := Decode(json.RawMessage(`{"start":"a","steps":[{"id":"a","name":"A","end_call":true,` + front + `}],"handoffs":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := g.Step("a"); !a.EndCall {
		t.Fatal("EndCall lost")
	}
}

func TestDecodeRejectsACallLimitOnALaterStep(t *testing.T) {
	raw := twoStepCfg("")
	i := strings.Index(raw, `"id":"sales"`)
	raw = raw[:i] + strings.Replace(raw[i:], `"config":{`, `"config":{"max_call_duration_seconds":600,`, 1)
	_, err := Decode(json.RawMessage(raw))
	if err == nil || !strings.Contains(err.Error(), `step "sales"`) || !strings.Contains(err.Error(), "max_call_duration_seconds") {
		t.Fatalf("err = %v", err)
	}
}

// twoStepCfg is a routed flow whose second step carries the given extra config members.
func twoStepCfg(salesExtra string) string {
	sales := `{"id":"sales","name":"Sales","config":{"llm":{"provider":"openai","model":"m","system_prompt":"s"},"tts":{"provider":"cartesia","voice":"v"` + salesExtra + `}}}`
	return `{"start":"front","steps":[{"id":"front","name":"Front","config":` + flatCfg + `},` + sales + `],` +
		`"handoffs":[{"id":"e1","from":"front","to":"sales","condition":"wants to buy"}]}`
}

// K1: stored versions that carry a start-step-only setting on another step keep decoding.
func TestStoredAgentWithCallWideSettingsOnAStepStillDecodes(t *testing.T) {
	for _, raw := range []string{
		twoStepCfg(`,"clean_text":false`),
		strings.Replace(twoStepCfg(""), `"tts":{"provider":"cartesia","voice":"v"}}}`, `"tts":{"provider":"cartesia","voice":"v"},"failover":{"on":["timeout"]}}}`, 1),
	} {
		if _, err := Decode(json.RawMessage(raw)); err != nil {
			t.Fatalf("Decode(%s) = %v", raw, err)
		}
	}
}

// withSalesAudio is the routed flow whose second step carries the given audio object.
func withSalesAudio(audio string) json.RawMessage {
	const llm = `"llm":{"provider":"openai","model":"m","system_prompt":"s"}`
	return json.RawMessage(strings.Replace(twoStepCfg(""), llm, llm+`,"audio":`+audio, 1))
}

func TestDecodeAgentSamplingIsPerStep(t *testing.T) {
	d, err := Decode(json.RawMessage(`{"start":"a","steps":[
		{"id":"a","name":"A","config":{"llm":{"provider":"google","model":"m","temperature":0.3}}},
		{"id":"b","name":"B","config":{"llm":{"provider":"google","model":"m","top_p":0.5}}}],
		"handoffs":[{"id":"e1","from":"a","to":"b","condition":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	a, b := d.Steps[0].Config.LLM.LLMSampling, d.Steps[1].Config.LLM.LLMSampling
	if a.Temperature == nil || a.TopP != nil || b.Temperature != nil || b.TopP == nil {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
	if _, err := Decode(json.RawMessage(`{"start":"a","steps":[{"id":"a","name":"A","config":{"llm":{"provider":"google","temperature":3}}}]}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("step temperature 3: %v", err)
	}
}
