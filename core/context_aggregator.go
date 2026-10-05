package core

import "context"

// ContextAggregator projects system and tool frames into ConversationContext.
// User and assistant text remain owned by their delivery-aware processors.
type ContextAggregator struct{ Context *ConversationContext }

func (p *ContextAggregator) ProcessFrame(_ context.Context, f Frame, dir Direction, e Emitter) error {
	if dir != Downstream || p.Context == nil {
		return e.PushDirection(f, dir)
	}
	switch v := f.(type) {
	case *SystemMessageFrame:
		if err := p.Context.SetSystemInstructionChecked(v.Content, v.Timestamp); err != nil {
			return err
		}
	case *ToolCallFrame:
		if err := p.Context.AddToolCall(string(v.Call.TurnID), uint64(v.Call.Generation), v.Call.ID, v.Call.Name, v.Call.Arguments, v.Timestamp); err != nil {
			return err
		}
	case *ToolResultFrame:
		if v.Result.Status != ToolSucceeded && v.Result.ErrorCategory == "" {
			return ErrInvalidToolMessage
		}
		if err := p.Context.AddToolResultStatus(string(v.Turn), uint64(v.Gen), v.Result.CallID, v.Result.Status, v.Result.Result, v.Result.ErrorCategory, v.Timestamp); err != nil {
			return err
		}
	}
	return e.PushDirection(f, dir)
}
