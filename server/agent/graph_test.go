package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const flatCfg = `{"stt":{"provider":"deepgram","credential_id":"c1"},"llm":{"provider":"openai","model":"m","credential_id":"c2","system_prompt":"hi"},"tts":{"provider":"cartesia","voice":"v","credential_id":"c3"}}`

func node(id, name string, extra string) string {
	cfg := `{"llm":{"provider":"openai","model":"m","credential_id":"c2","system_prompt":"` + name + `"},"tts":{"provider":"cartesia","voice":"v","credential_id":"c3"}` + extra + `}`
	return `{"id":"` + id + `","name":"` + name + `","config":` + cfg + `}`
}

func TestDecodeGraphWrapsFlatConfig(t *testing.T) {
	g, err := DecodeGraph(json.RawMessage(flatCfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 1 || g.Entry != g.Nodes[0].ID || len(g.Edges) != 0 {
		t.Fatalf("flat config must become a one-node graph, got %+v", g)
	}
	if g.EntryNode().Config.LLM.Model != "m" {
		t.Fatal("entry config lost")
	}
}

func TestDecodeGraphTwoNodes(t *testing.T) {
	raw := `{"entry":"front","nodes":[` +
		`{"id":"front","name":"Front desk","config":` + flatCfg + `},` +
		node("sales", "Sales", "") + `],` +
		`"edges":[{"id":"e1","from":"front","to":"sales","condition":"wants to buy"}]}`
	g, err := DecodeGraph(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := TransferToolName("Sales Team"); got != "transfer_to_sales_team" {
		t.Fatalf("tool name = %q", got)
	}
	if len(g.EdgesFrom("front")) != 1 || len(g.EdgesFrom("sales")) != 0 {
		t.Fatal("EdgesFrom wrong")
	}
}

func TestDecodeGraphRejects(t *testing.T) {
	front := `{"id":"front","name":"Front","config":` + flatCfg + `}`
	cases := map[string]string{
		"no entry":                `{"nodes":[` + front + `],"edges":[]}`,
		"entry missing":           `{"entry":"x","nodes":[` + front + `],"edges":[]}`,
		"dup node id":             `{"entry":"front","nodes":[` + front + `,` + front + `],"edges":[]}`,
		"dup slug":                `{"entry":"front","nodes":[` + front + `,` + node("b", "front", "") + `],"edges":[]}`,
		"edge to nowhere":         `{"entry":"front","nodes":[` + front + `],"edges":[{"id":"e","from":"front","to":"zz","condition":"c"}]}`,
		"self edge":               `{"entry":"front","nodes":[` + front + `],"edges":[{"id":"e","from":"front","to":"front","condition":"c"}]}`,
		"empty condition":         `{"entry":"front","nodes":[` + front + `,` + node("b", "B", "") + `],"edges":[{"id":"e","from":"front","to":"b","condition":" "}]}`,
		"dup edge pair":           `{"entry":"front","nodes":[` + front + `,` + node("b", "B", "") + `],"edges":[{"id":"e1","from":"front","to":"b","condition":"c"},{"id":"e2","from":"front","to":"b","condition":"d"}]}`,
		"call-level on non-entry": `{"entry":"front","nodes":[` + front + `,` + node("b", "B", `,"vad_stop_ms":300`) + `],"edges":[]}`,
		"unknown field":           `{"entry":"front","nodes":[` + front + `],"edges":[],"bogus":1}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeGraph(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("want ErrInvalidConfig, got %v", err)
			}
		})
	}
}

func TestDecodeGraphBounds(t *testing.T) {
	var nodes []string
	for i := 0; i <= MaxNodes; i++ {
		nodes = append(nodes, node("n"+strings.Repeat("x", i), "N"+strings.Repeat("x", i), ""))
	}
	raw := `{"entry":"n","nodes":[` + strings.Join(nodes, ",") + `],"edges":[]}`
	if _, err := DecodeGraph(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("more than %d nodes must be rejected, got %v", MaxNodes, err)
	}
}

func TestSummarizationNeedsEverySummarizingLLM(t *testing.T) {
	entry := `{"stt":{"provider":"deepgram","credential_id":"c1"},"llm":{"provider":"openai","model":"m","credential_id":"c2"},"tts":{"provider":"cartesia","credential_id":"c3"},"context":{"summarization":{"enabled":true}}}`
	other := `{"id":"b","name":"B","config":{"llm":{"provider":"cartesia","model":"m","credential_id":"c2"},"tts":{"provider":"cartesia","credential_id":"c3"}}}`
	raw := `{"entry":"a","nodes":[{"id":"a","name":"A","config":` + entry + `},` + other + `],"edges":[]}`
	if _, err := DecodeGraph(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("want ErrInvalidConfig, got %v", err)
	}
}

func TestDecodeGraphRejectsUnsafeIDsAndCue(t *testing.T) {
	front := func(id string) string { return `{"id":"` + id + `","name":"Front","config":` + flatCfg + `}` }
	graph := func(entry, node, extra string) string {
		return `{"entry":"` + entry + `","nodes":[` + node + `],"edges":[]` + extra + `}`
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
			if _, err := DecodeGraph(json.RawMessage(raw)); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("want ErrInvalidConfig, got %v", err)
			}
		})
	}
	ok := graph("f_1-a", front("f_1-a"), `,"handoff_cue":{"name":"chime","after_ms":300}`)
	if _, err := DecodeGraph(json.RawMessage(ok)); err != nil {
		t.Fatalf("valid ids and cue rejected: %v", err)
	}
}
