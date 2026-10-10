package google

import (
	"context"

	"govoice/core"
)

// toolFrameRecorder writes tool frames into history, standing in for the tool
// loop so these provider tests can check continuation requests in isolation.
type toolFrameRecorder struct{ history *core.ConversationContext }

func (r *toolFrameRecorder) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if dir == core.Downstream {
		switch v := f.(type) {
		case *core.ToolCallFrame:
			if err := r.history.AddToolCall(string(v.Call.TurnID), uint64(v.Call.Generation), v.Call.ID, v.Call.Name, v.Call.Arguments, v.Timestamp); err != nil {
				return err
			}
		case *core.ToolResultFrame:
			if err := r.history.AddToolResultStatus(string(v.Turn), uint64(v.Gen), v.Result.CallID, v.Result.Status, v.Result.Result, v.Result.ErrorCategory, v.Timestamp); err != nil {
				return err
			}
		}
	}
	return e.PushDirection(f, dir)
}
