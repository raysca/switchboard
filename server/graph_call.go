package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"govoice/core"
	"govoice/server/agent"
	"govoice/server/runtime"
)

// NodeSpec is one graph node's LLM and TTS stages, resolved with secrets read
// from the environment when the node is activated.
type NodeSpec struct {
	Name         string
	LLM          StageSpec
	LLMFallbacks []StageSpec
	TTS          StageSpec
	TTSFallbacks []StageSpec
}

// wipe zeroes the secrets once the node's processors are built.
func (n *NodeSpec) wipe() {
	clear(n.LLM.Secret)
	clear(n.TTS.Secret)
	for _, f := range n.LLMFallbacks {
		clear(f.Secret)
	}
	for _, f := range n.TTSFallbacks {
		clear(f.Secret)
	}
}

// CallGraph is what a call needs to move between nodes. Commit is set by the
// pipeline builder once the handoff exists; transfer tools call it.
type CallGraph struct {
	Entry   string
	Resolve func(ctx context.Context, nodeID string) (NodeSpec, error)
	Commit  func(target string) error
	Cue     *agent.HandoffCue
}

// transferTools are the handoff tools of one node: one per outgoing edge, the
// edge condition being the model's cue to call it.
func transferTools(g agent.Graph, nodeID string) []core.ToolDefinition {
	edges := g.EdgesFrom(nodeID)
	defs := make([]core.ToolDefinition, 0, len(edges))
	for _, e := range edges {
		to, _ := g.Node(e.To)
		defs = append(defs, core.ToolDefinition{
			Name:        agent.TransferToolName(to.Name),
			Description: "Hand the caller to " + to.Name + ". Use when: " + e.Condition,
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Policy:      core.ToolPolicy{Timeout: 5 * time.Second},
		})
	}
	return defs
}

// transferTargets maps a node's transfer tool names to the target node ids.
func transferTargets(g agent.Graph, nodeID string) map[string]string {
	out := map[string]string{}
	for _, e := range g.EdgesFrom(nodeID) {
		to, _ := g.Node(e.To)
		out[agent.TransferToolName(to.Name)] = to.ID
	}
	return out
}

// nodeDefinitions keeps the session's captured MCP tools that a node lists
// ("<mcp_server_id>/<remote_name>"); a node that lists none exposes none.
func nodeDefinitions(all []core.ToolDefinition, refs []string) []core.ToolDefinition {
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

// nodeExecutor serves one node: its own MCP tools (allowed) and its transfer
// tools (transfer: tool name to target node id). Anything else is refused, so
// a hallucinated call cannot reach another node's tools.
func nodeExecutor(inner core.ToolExecutor, allowed map[string]bool, transfer map[string]string, commit func(target string) error) core.ToolExecutor {
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
