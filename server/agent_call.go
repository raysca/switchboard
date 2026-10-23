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
	"govoice/server/agent"
	"govoice/server/runtime"
)

// StepSpec is one agent step's LLM and TTS stages, resolved with secrets read
// from the environment when the step is activated.
type StepSpec struct {
	Name         string
	LLM          StageSpec
	LLMFallbacks []StageSpec
	TTS          StageSpec
	TTSFallbacks []StageSpec
	// Greeting is what the step says, word for word, when a handoff hands the call to it ("" = the model greets).
	Greeting string
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

// CallAgent is what a call needs to move between steps. Commit is set by the
// pipeline builder once the handoff exists; transfer tools call it.
type CallAgent struct {
	Start   string
	Resolve func(ctx context.Context, stepID string) (StepSpec, error)
	Commit  func(target string) error
	Cue     *agent.HandoffCue
	// Backgrounds is the background each step names (by step id); the steps that name none are absent.
	Backgrounds map[string]*agent.BackgroundSettings

	noteMu   sync.Mutex
	note     string
	commitMu sync.Mutex // serialises commitWithNote's set, commit and restore
}

const (
	// EndReasonAgent is the hang-up reason, trace attribute and RTVI error code when
	// a step's end_call tool ends the call.
	EndReasonAgent = "ended_by_agent"
	endCallName    = "end_call"
	// endCallWait caps the wait for the goodbye to finish before the call ends anyway.
	endCallWait = 15 * time.Second
	endCallOK   = `{"status":"ending","instruction":"The call ends right after your next words. Say a brief goodbye now if you have not already, then stop."}`
)

var errCallEnding = errors.New("call is already ending")

// callEnder is how the end_call tool hangs up. End is set by the pipeline builder
// (the idle controller ends the call); a call whose agent has no end_call step has none.
type callEnder struct {
	End func(stepID string) error
}

// hangUp is the end_call tool's action for one step.
func (e *callEnder) hangUp(stepID string) func() error {
	return func() error {
		if e.End == nil {
			return errors.New("end not ready")
		}
		return e.End(stepID)
	}
}

// endCallDefinition is the end_call tool a step gets when its end_call flag is set.
func endCallDefinition() core.ToolDefinition {
	return core.ToolDefinition{
		Name:        endCallName,
		Description: "Hang up. Call this only after you have said goodbye and the caller needs nothing else; the call ends once you finish speaking.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Policy:      core.ToolPolicy{Timeout: 5 * time.Second},
	}
}

func anyEndCall(g agent.Definition) bool {
	for _, n := range g.Steps {
		if n.EndCall {
			return true
		}
	}
	return false
}

// transferTools are the transfer tools of one step: one per outgoing handoff, the
// handoff condition being the model's cue to call it.
func transferTools(g agent.Definition, stepID string) []core.ToolDefinition {
	handoffs := g.HandoffsFrom(stepID)
	defs := make([]core.ToolDefinition, 0, len(handoffs))
	for _, e := range handoffs {
		to, _ := g.Step(e.To)
		defs = append(defs, core.ToolDefinition{
			Name:        agent.TransferToolName(to.Name),
			Description: "Hand the caller to " + to.Name + ". Use when: " + e.Condition,
			InputSchema: json.RawMessage(transferSchema),
			Policy:      core.ToolPolicy{Timeout: 5 * time.Second},
		})
	}
	return defs
}

// transferTargets maps a step's transfer tool names to the target step ids.
func transferTargets(g agent.Definition, stepID string) map[string]string {
	out := map[string]string{}
	for _, e := range g.HandoffsFrom(stepID) {
		to, _ := g.Step(e.To)
		out[agent.TransferToolName(to.Name)] = to.ID
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

// transferPrefix opens the message that tells a step the call was just transferred to it. The note is
// model-written from the caller's words: untrusted, so server markers in it are neutralised.
func transferPrefix(stepName, note string) string {
	msg := "[The caller was just transferred to you (" + stepName + ")."
	if note != "" {
		msg += " Note from the previous agent: " + neutralizeNoteMarker(note) + "."
	}
	return msg
}

// transferGreeting is the message that makes the new step greet the caller.
func transferGreeting(stepName, note string) string {
	return transferPrefix(stepName, note) + " Greet them briefly, by name if you know it, and continue.]"
}

// transferNote carries the previous agent's note into the history when a fixed greeting replaces the model's
// greeting: context only, no reply is requested.
func transferNote(stepName, note string) string { return transferPrefix(stepName, note) + "]" }

// saysOwnGreeting reports whether a step speaks a fixed greeting when a handoff lands on it. The start step's
// greeting is the call opening only: a return to it gets the model's transfer greeting.
func saysOwnGreeting(stepID, start, greeting string) bool { return greeting != "" && stepID != start }

// greetStep is what Router.Greet does once the idle flip has swapped the new step in. A step that says its own
// greeting speaks it (after a context-only message carrying the previous agent's note, if any) and the model is
// not asked to greet; otherwise the model is told to greet, as before. add records a history message; speak
// queues the greeting (it must not block).
func (c *CallAgent) greetStep(stepID, stepName, greeting string, add func(core.Message) error, speak func(string)) error {
	note := c.takeNote()
	if saysOwnGreeting(stepID, c.Start, greeting) {
		if note != "" {
			if err := add(core.Message{Role: core.RoleUser, Content: transferNote(stepName, note)}); err != nil {
				return err
			}
		}
		speak(greeting)
		return nil
	}
	return add(core.Message{Role: core.RoleUser, Content: transferGreeting(stepName, note)})
}

// setNote keeps the note of the transfer in progress for the greeting; every transfer overwrites it.
func (c *CallAgent) setNote(n string) { c.noteMu.Lock(); c.note = n; c.noteMu.Unlock() }

// commitWithNote records note for the greeting and commits the transfer; on any
// failure (a duplicate while one is pending, the limit) the previous note is restored.
func (c *CallAgent) commitWithNote(target, note string) error {
	if c.Commit == nil {
		return errors.New("handoff not ready")
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

// takeNote returns the note once. In theory a greeting can read a losing commit's note for
// microseconds before it is restored; Router.Commit rejects at once while a transfer is pending.
func (c *CallAgent) takeNote() string {
	c.noteMu.Lock()
	defer c.noteMu.Unlock()
	n := c.note
	c.note = ""
	return n
}
