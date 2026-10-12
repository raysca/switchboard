package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"govoice/core"
	"govoice/server/flow"
	"govoice/server/runtime"
)

func runtimeName(serverID, remote string) string { return runtime.ToolName(serverID, remote) }

func twoStepFlow(t *testing.T) flow.Definition {
	t.Helper()
	flat := `{"stt":{"provider":"google"},"llm":{"provider":"google","model":"m"},"tts":{"provider":"google"}}`
	step := `{"llm":{"provider":"google","model":"m"},"tts":{"provider":"google"}}`
	g, err := flow.Decode(json.RawMessage(`{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","config":` + flat + `},{"id":"sales","name":"Sales","config":` + step + `}],` +
		`"routes":[{"id":"e1","from":"front","to":"sales","condition":"wants to buy"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestTransferToolsFromRoutes(t *testing.T) {
	g := twoStepFlow(t)
	defs := transferTools(g, "front")
	if len(defs) != 1 || defs[0].Name != "transfer_to_sales" || !strings.Contains(defs[0].Description, "wants to buy") {
		t.Fatalf("%+v", defs)
	}
	if len(transferTools(g, "sales")) != 0 {
		t.Fatal("sales has no outgoing edges")
	}
}

func TestStepExecutorRejectsForeignTool(t *testing.T) {
	var ran bool
	inner := core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) {
		ran = true
		return core.ToolResult{Status: core.ToolSucceeded}, nil
	})
	ex := stepExecutor(inner, map[string]bool{"mcp_ok": true}, nil, nil)
	res, _ := ex.Execute(context.Background(), core.ToolCall{ID: "1", Name: "mcp_other"})
	if ran || res.Status != core.ToolFailed {
		t.Fatalf("a tool outside the node's list must not run: ran=%v status=%s", ran, res.Status)
	}
	res, _ = ex.Execute(context.Background(), core.ToolCall{ID: "2", Name: "mcp_ok"})
	if !ran || res.Status != core.ToolSucceeded {
		t.Fatalf("an allowed tool must run: ran=%v status=%s", ran, res.Status)
	}
}

func TestStepExecutorTransferCommits(t *testing.T) {
	var got string
	ex := stepExecutor(nil, nil, map[string]string{"transfer_to_sales": "sales"}, func(target string) error { got = target; return nil })
	res, _ := ex.Execute(context.Background(), core.ToolCall{ID: "1", Name: "transfer_to_sales"})
	if got != "sales" || res.Status != core.ToolSucceeded || !strings.Contains(string(res.Result), "one short sentence") {
		t.Fatalf("got=%q res=%+v", got, res)
	}
}

func TestStepExecutorTransferRefused(t *testing.T) {
	ex := stepExecutor(nil, nil, map[string]string{"transfer_to_sales": "sales"}, func(string) error { return core.ErrHandoffPending })
	res, _ := ex.Execute(context.Background(), core.ToolCall{ID: "1", Name: "transfer_to_sales"})
	if res.Status != core.ToolFailed {
		t.Fatalf("a refused transfer must tell the model it failed, got %s", res.Status)
	}
}

func TestStepDefinitionsKeepsOnlyListedTools(t *testing.T) {
	all := []core.ToolDefinition{{Name: runtimeName("srv1", "lookup")}, {Name: runtimeName("srv1", "other")}}
	got := stepDefinitions(all, []string{"srv1/lookup"})
	if len(got) != 1 || got[0].Name != runtimeName("srv1", "lookup") {
		t.Fatalf("%+v", got)
	}
	if len(stepDefinitions(all, nil)) != 0 {
		t.Fatal("a node that lists no tools exposes none")
	}
}

// A non-start step's secret is read when the step is activated, not at call
// start: a variable changed mid-call reaches the step's spec.
func TestFlowStepActivationRereadsTheEnvironment(t *testing.T) {
	s := apiTestServer(t)
	env := newLiveEnv(testProviderVars(t))
	s.env = env.get
	flat := `{"stt":{"provider":"google"},"llm":{"provider":"google","model":"m"},"tts":{"provider":"google"}}`
	step := `{"llm":{"provider":"xai","model":"grok"},"tts":{"provider":"google"}}`
	raw := json.RawMessage(`{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","config":` + flat + `},{"id":"sales","name":"Sales","config":` + step + `}],` +
		`"routes":[{"id":"e1","from":"front","to":"sales","condition":"wants to buy"}]}`)
	rs := &runtime.Session{Snapshot: flow.Snapshot{Version: flow.Version{Config: raw}}}
	call, err := s.callSpec(t.Context(), rs)
	if err != nil {
		t.Fatal(err)
	}
	env.set("XAI_API_KEY", "rotated-xai-key")
	spec, err := call.Graph.Resolve(t.Context(), "sales")
	if err != nil {
		t.Fatal(err)
	}
	if spec.LLM.Provider != "xai" || string(spec.LLM.Secret) != "rotated-xai-key" {
		t.Fatalf("sales llm = %s %q, want the rotated xai key", spec.LLM.Provider, spec.LLM.Secret)
	}
}
