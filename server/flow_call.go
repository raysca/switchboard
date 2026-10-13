package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	// CanEnd: some step has end_call, so the call builds the idle controller.
	CanEnd bool
	// End is set by the pipeline builder; the end_call tool calls it.
	End func(stepID string) error

	noteMu   sync.Mutex
	note     string
	commitMu sync.Mutex // serialises commitWithNote's set, commit and restore
}

const (
	// EndReasonFlow is the hang-up reason, trace attribute and RTVI error code when
	// a step's end_call tool ends the call.
	EndReasonFlow = "ended_by_flow"
	endCallName   = "end_call"
	// endCallWait caps the wait for the goodbye to finish before the call ends anyway.
	endCallWait = 15 * time.Second
	endCallOK   = `{"status":"ending","instruction":"The call ends right after your next words. Say a brief goodbye now if you have not already, then stop."}`
)

var errCallEnding = errors.New("call is already ending")

// endCallDefinition is the end_call tool a step gets when its end_call flag is set.
func endCallDefinition() core.ToolDefinition {
	return core.ToolDefinition{
		Name:        endCallName,
		Description: "Hang up. Call this only after you have said goodbye and the caller needs nothing else; the call ends once you finish speaking.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Policy:      core.ToolPolicy{Timeout: 5 * time.Second},
	}
}

func anyEndCall(g flow.Definition) bool {
	for _, n := range g.Steps {
		if n.EndCall {
			return true
		}
	}
	return false
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
			InputSchema: json.RawMessage(transferSchema),
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
func stepExecutor(inner core.ToolExecutor, allowed map[string]bool, transfer map[string]string, commit func(target, note string) error, end func() error) core.ToolExecutor {
	return core.ToolExecutorFunc(func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
		now := time.Now()
		res := func(status core.ToolStatus, body string) core.ToolResult {
			return core.ToolResult{CallID: call.ID, Status: status, Result: json.RawMessage(body), StartedAt: now, EndedAt: time.Now()}
		}
		if target, ok := transfer[call.Name]; ok {
			if err := commit(target, parseNote(call.Arguments)); err != nil {
				if errors.Is(err, core.ErrHandoffPending) || errors.Is(err, core.ErrHandoffLimit) {
					return res(core.ToolFailed, `{"error":"transfer_unavailable"}`), nil
				}
				return res(core.ToolFailed, `{"error":"transfer_failed"}`), nil
			}
			return res(core.ToolSucceeded, transferOK), nil
		}
		if call.Name == endCallName && end != nil {
			if err := end(); err != nil && !errors.Is(err, errCallEnding) {
				return res(core.ToolFailed, `{"error":"end_unavailable"}`), nil
			}
			return res(core.ToolSucceeded, endCallOK), nil
		}
		if !allowed[call.Name] || inner == nil {
			return res(core.ToolFailed, `{"error":"unknown_tool"}`), nil
		}
		return inner.Execute(ctx, call)
	})
}

// maxTransferNote bounds the note a model may pass with a transfer.
const maxTransferNote = 500

var transferSchema = fmt.Sprintf(`{"type":"object","properties":{"note":{"type":"string","maxLength":%d,`+
	`"description":"For the next agent: the caller's name and what they need, in one sentence."}},"additionalProperties":false}`, maxTransferNote)

// parseNote reads the optional note argument; anything that is not a string is dropped.
func parseNote(raw json.RawMessage) string {
	var a struct {
		Note string `json:"note"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &a) != nil {
		return ""
	}
	note := strings.TrimSpace(a.Note)
	if len(note) > maxTransferNote {
		note = note[:maxTransferNote]
		for !utf8.ValidString(note) {
			note = note[:len(note)-1]
		}
	}
	return note
}

// transferGreeting is the message that makes the new step greet the caller. The
// note is model-written from the caller's words: untrusted, so server markers in it are neutralised.
func transferGreeting(stepName, note string) string {
	msg := "[The caller was just transferred to you (" + stepName + ")."
	if note != "" {
		msg += " Note from the previous agent: " + neutralizeNoteMarker(note) + "."
	}
	return msg + " Greet them briefly, by name if you know it, and continue.]"
}

// setNote keeps the note of the transfer in progress for the greeting; every transfer overwrites it.
func (c *CallFlow) setNote(n string) { c.noteMu.Lock(); c.note = n; c.noteMu.Unlock() }

// commitWithNote records note for the greeting and commits the transfer; on any
// failure (a duplicate while one is pending, the limit) the previous note is restored.
func (c *CallFlow) commitWithNote(target, note string) error {
	if c.Commit == nil {
		return errors.New("route not ready")
	}
	// Commit only starts the handoff build and returns; the greeting (takeNote) runs
	// later on the trigger path, so holding commitMu here cannot deadlock with it.
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	c.noteMu.Lock()
	prev := c.note
	c.note = note
	c.noteMu.Unlock()
	if err := c.Commit(target); err != nil {
		c.setNote(prev)
		return err
	}
	return nil
}

// takeNote returns the note once.
func (c *CallFlow) takeNote() string {
	c.noteMu.Lock()
	defer c.noteMu.Unlock()
	n := c.note
	c.note = ""
	return n
}
