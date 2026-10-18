package coretest

import (
	"fmt"

	"govoice/core"
)

// ValidateToolPairing reports whether history is acceptable to provider
// adapters (Gemini, OpenAI, xAI): every run of tool calls must be followed
// immediately by exactly its results (any order), and a result may not appear
// without its call in the run just before it.
func ValidateToolPairing(history []core.Message) error {
	for i := 0; i < len(history); {
		m := history[i]
		switch m.Kind {
		case core.MessageToolResult:
			return fmt.Errorf("tool result %q at %d has no call immediately before it", m.ToolCallID, i)
		case core.MessageToolCall:
			want := map[string]bool{}
			for ; i < len(history) && history[i].Kind == core.MessageToolCall; i++ {
				want[history[i].ToolCallID] = true
			}
			for n := len(want); n > 0; n-- {
				if i >= len(history) || history[i].Kind != core.MessageToolResult || !want[history[i].ToolCallID] {
					return fmt.Errorf("tool call run before %d is not immediately followed by its %d result(s)", i, len(want))
				}
				delete(want, history[i].ToolCallID)
				i++
			}
		default:
			i++
		}
	}
	return nil
}
