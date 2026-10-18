package dtmf

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"govoice/core"
)

// ToolName is the built-in tool an LLM calls to press keys on an IVR.
const ToolName = "send_dtmf"

// ToolDefinition describes send_dtmf. Pass it to the LLM beside the agent's own tools.
func ToolDefinition() core.ToolDefinition {
	return core.ToolDefinition{
		Name:        ToolName,
		Description: "Press keys on the phone keypad by playing DTMF tones to the other party, e.g. to navigate an IVR menu or enter an extension. Use digits 0-9, * , # and A-D. The call returns once the tones have played.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"digits":{"type":"string","pattern":"^[0-9*#A-D]{1,32}$","minLength":1,"maxLength":32,"description":"Keys to press, in order."}},"required":["digits"],"additionalProperties":false}`),
		// Covers MaxDigitsPerSend at the default 160 ms per digit (about 5 s).
		Policy: core.ToolPolicy{Timeout: 8 * time.Second},
	}
}

// Executor serves send_dtmf from out and delegates every other call to next
// (which may be nil: other names then fail as unknown).
func Executor(out *OutputProcessor, next core.ToolExecutor) core.ToolExecutor {
	return core.ToolExecutorFunc(func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
		if call.Name != ToolName {
			if next == nil {
				return failed(call, "unknown_tool"), nil
			}
			return next.Execute(ctx, call)
		}
		started := time.Now()
		var args struct {
			Digits string `json:"digits"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil || ValidateDigits(args.Digits) != nil {
			r := failed(call, "invalid_arguments")
			r.StartedAt = started
			return r, nil
		}
		// Wait for playback so the reply that follows is spoken after the tones,
		// not interleaved with them in the output queue. A barge-in ends the wait
		// through ctx: the tool loop cancels it when the call's generation goes stale.
		res := make(chan error, 1)
		frame := &core.OutputDTMFFrame{Digits: args.Digits, Gen: uint64(call.Generation), Result: res}
		if err := out.session.Reenter(frame, core.Downstream, out.index, frame.Gen); err != nil {
			r := failed(call, "unavailable")
			r.StartedAt = started
			return r, nil
		}
		select {
		case err := <-res:
			if err != nil {
				cat := "unavailable"
				if errors.Is(err, ErrBusy) {
					cat = "busy"
				}
				r := failed(call, cat)
				r.StartedAt = started
				return r, nil
			}
		case <-ctx.Done():
		}
		return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{"played":true}`), StartedAt: started, EndedAt: time.Now()}, nil
	})
}

func failed(call core.ToolCall, category string) core.ToolResult {
	now := time.Now()
	return core.ToolResult{CallID: call.ID, Status: core.ToolFailed, Result: json.RawMessage(`{"error":"` + category + `"}`), ErrorCategory: category, StartedAt: now, EndedAt: now}
}
