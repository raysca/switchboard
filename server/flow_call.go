package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"govoice/core"
	"govoice/server/flow"
	"govoice/server/runtime"
)

// StepSpec is one flow step's LLM and TTS stages, resolved with secrets read
// from the environment when the step is activated.
type StepSpec struct {
	Name         string
	LLM          StageSpec
	LLMFallbacks []StageSpec
	TTS          StageSpec
	TTSFallbacks []StageSpec
}

// wipe zeroes the secrets once the step's processors are built.
func (n *StepSpec) wipe() {
	clear(n.LLM.Secret)
	clear(n.TTS.Secret)
	for _, f := range n.LLMFallbacks {
		clear(f.Secret)
	}
	for _, f := range n.TTSFallbacks {
		clear(f.Secret)
	}
}

// CallFlow is what a call needs to move between steps. Commit is set by the
// pipeline builder once the handoff exists; transfer tools call it.
type CallFlow struct {
	Start   string
	Resolve func(ctx context.Context, stepID string) (StepSpec, error)
	Commit  func(target string) error
	Cue     *flow.RouteCue
}

// transferTools are the transfer tools of one step: one per outgoing route, the
// route condition being the model's cue to call it.
func transferTools(g flow.Definition, stepID string) []core.ToolDefinition {
	routes := g.RoutesFrom(stepID)
	defs := make([]core.ToolDefinition, 0, len(routes))
	for _, e := range routes {
		to, _ := g.Step(e.To)
		defs = append(defs, core.ToolDefinition{
			Name:        flow.TransferToolName(to.Name),
			Description: "Hand the caller to " + to.Name + ". Use when: " + e.Condition,
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Policy:      core.ToolPolicy{Timeout: 5 * time.Second},
		})
	}
	return defs
}

// transferTargets maps a step's transfer tool names to the target step ids.
func transferTargets(g flow.Definition, stepID string) map[string]string {
	out := map[string]string{}
	for _, e := range g.RoutesFrom(stepID) {
		to, _ := g.Step(e.To)
		out[flow.TransferToolName(to.Name)] = to.ID
	}
	return out
}

// stepDefinitions keeps the session's captured MCP tools that a step lists
// ("<mcp_server_id>/<remote_name>"); a step that lists none exposes none.
func stepDefinitions(all []core.ToolDefinition, refs []string) []core.ToolDefinition {
	want := make(map[string]bool, len(refs))
	for _, ref := range refs {
		server, remote, ok := strings.Cut(ref, "/")
		if ok {
			want[runtime.ToolName(server, remote)] = true
		}
	}
	var out []core.ToolDefinition
	for _, d := range all {
		if want[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

const transferOK = `{"status":"transferring","instruction":"Tell the caller in one short sentence that you are connecting them, then stop talking."}`

// stepExecutor serves one step: its own MCP tools (allowed) and its transfer
// tools (transfer: tool name to target step id). Anything else is refused, so
// a hallucinated call cannot reach another step's tools.
func stepExecutor(inner core.ToolExecutor, allowed map[string]bool, transfer map[string]string, commit func(target string) error) core.ToolExecutor {
	return core.ToolExecutorFunc(func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
		now := time.Now()
		res := func(status core.ToolStatus, body string) core.ToolResult {
			return core.ToolResult{CallID: call.ID, Status: status, Result: json.RawMessage(body), StartedAt: now, EndedAt: time.Now()}
		}
		if target, ok := transfer[call.Name]; ok {
			if err := commit(target); err != nil {
				if errors.Is(err, core.ErrHandoffPending) || errors.Is(err, core.ErrHandoffLimit) {
					return res(core.ToolFailed, `{"error":"transfer_unavailable"}`), nil
				}
				return res(core.ToolFailed, `{"error":"transfer_failed"}`), nil
			}
			return res(core.ToolSucceeded, transferOK), nil
		}
		if !allowed[call.Name] || inner == nil {
			return res(core.ToolFailed, `{"error":"unknown_tool"}`), nil
		}
		return inner.Execute(ctx, call)
	})
}
