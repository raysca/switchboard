// Package whisker adapts framework DebugEvent values to a semantic Whisker
// observer shape. No byte-level protocol claim is made until Whisker pins one.
package whisker

import (
	"errors"
	"unicode/utf8"

	"govoice/core"
)

var ErrMalformedEvent = errors.New("whisker: malformed debug event")

type Event struct {
	Name           string `json:"name"`
	Time           string `json:"time,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	TurnID         string `json:"turn_id,omitempty"`
	Generation     uint64 `json:"generation,omitempty"`
	PipelineID     string `json:"pipeline_id,omitempty"`
	Processor      string `json:"processor,omitempty"`
	ProcessorIndex int    `json:"processor_index,omitempty"`
	FrameKind      uint16 `json:"frame_kind,omitempty"`
	Direction      string `json:"direction,omitempty"`
	Sequence       uint64 `json:"sequence,omitempty"`
	ToolCallID     string `json:"tool_call_id,omitempty"`
	attributes     map[string]any
}

func (e Event) Attributes() map[string]any { return cloneMap(e.attributes) }

type Adapter struct{}

func (Adapter) Adapt(in core.DebugEvent) (Event, error) {
	if !validScalar(in.Name, true) || !validScalar(string(in.SessionID), false) || !validScalar(string(in.TurnID), false) || !validScalar(in.PipelineID, false) || !validScalar(in.Processor, false) || !validScalar(in.ToolCallID, false) || in.ProcessorIndex < 0 || (in.Direction != core.Downstream && in.Direction != core.Upstream) {
		return Event{}, ErrMalformedEvent
	}
	out := Event{Name: in.Name, SessionID: string(in.SessionID), TurnID: string(in.TurnID), Generation: uint64(in.Generation), PipelineID: in.PipelineID, Processor: in.Processor, ProcessorIndex: in.ProcessorIndex, FrameKind: uint16(in.FrameKind), Direction: in.Direction.String(), Sequence: in.Sequence, ToolCallID: in.ToolCallID, attributes: cloneMap(in.Attributes())}
	if !in.Time.IsZero() {
		out.Time = in.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	return out, nil
}

func cloneMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(value any) any {
	switch x := value.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return value
	}
}

func validScalar(s string, required bool) bool {
	return (!required || s != "") && len(s) <= 256 && utf8.ValidString(s)
}
