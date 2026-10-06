package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrUnsupportedSerialization = errors.New("core: unsupported serialization format or target")
	ErrMalformedPayload         = errors.New("core: malformed serialized payload")
	ErrUnknownField             = errors.New("core: unknown or disallowed field in serialized payload")
)

// Serializer converts between core semantic events/frames and wire bytes.
// It is intentionally decoupled from pipeline processors and storage.
type Serializer interface {
	Format() string
	Encode(v any) ([]byte, error)
	Decode(data []byte) (any, error)
}

// JSONEventSerializer serializes semantic events (such as DebugEvent and ObserverEvent)
// to and from bounded JSON.
type JSONEventSerializer struct {
	DisallowUnknownFields bool
	MaxPayloadBytes       int
}

func DefaultJSONEventSerializer() *JSONEventSerializer {
	return &JSONEventSerializer{
		DisallowUnknownFields: true,
		MaxPayloadBytes:       64 * 1024,
	}
}

func (s *JSONEventSerializer) Format() string {
	return "json"
}

// WireDebugEvent represents the stable JSON serialization structure for DebugEvent.
type WireDebugEvent struct {
	Name           string         `json:"name"`
	Time           time.Time      `json:"time"`
	SessionID      string         `json:"session_id,omitempty"`
	TurnID         string         `json:"turn_id,omitempty"`
	Generation     uint64         `json:"generation,omitempty"`
	PipelineID     string         `json:"pipeline_id,omitempty"`
	Processor      string         `json:"processor,omitempty"`
	ProcessorIndex int            `json:"processor_index,omitempty"`
	FrameKind      uint16         `json:"frame_kind,omitempty"`
	Direction      string         `json:"direction,omitempty"`
	Sequence       uint64         `json:"sequence,omitempty"`
	ToolCallID     string         `json:"tool_call_id,omitempty"`
	Attributes     map[string]any `json:"attributes,omitempty"`
}

func (s *JSONEventSerializer) Encode(v any) ([]byte, error) {
	switch event := v.(type) {
	case DebugEvent:
		wire := WireDebugEvent{
			Name:           event.Name,
			Time:           event.Time,
			SessionID:      string(event.SessionID),
			TurnID:         string(event.TurnID),
			Generation:     uint64(event.Generation),
			PipelineID:     event.PipelineID,
			Processor:      event.Processor,
			ProcessorIndex: event.ProcessorIndex,
			FrameKind:      uint16(event.FrameKind),
			Direction:      event.Direction.String(),
			Sequence:       event.Sequence,
			ToolCallID:     event.ToolCallID,
			Attributes:     event.Attributes(),
		}
		data, err := json.Marshal(wire)
		if err != nil {
			return nil, err
		}
		if s.MaxPayloadBytes > 0 && len(data) > s.MaxPayloadBytes {
			return nil, fmt.Errorf("%w: encoded payload %d exceeds bound %d", ErrMalformedPayload, len(data), s.MaxPayloadBytes)
		}
		return data, nil

	case *DebugEvent:
		if event == nil {
			return nil, errors.New("cannot encode nil DebugEvent")
		}
		return s.Encode(*event)

	default:
		return nil, fmt.Errorf("%w: cannot encode type %T", ErrUnsupportedSerialization, v)
	}
}

func (s *JSONEventSerializer) Decode(data []byte) (any, error) {
	if s.MaxPayloadBytes > 0 && len(data) > s.MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload size %d exceeds bound %d", ErrMalformedPayload, len(data), s.MaxPayloadBytes)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	if s.DisallowUnknownFields {
		dec.DisallowUnknownFields()
	}

	var wire WireDebugEvent
	if err := dec.Decode(&wire); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrMalformedPayload, err)
	}

	dir := Downstream
	if wire.Direction == "upstream" {
		dir = Upstream
	}

	ev := DebugEvent{
		Name:           wire.Name,
		Time:           wire.Time,
		SessionID:      SessionID(wire.SessionID),
		TurnID:         TurnID(wire.TurnID),
		Generation:     Generation(wire.Generation),
		PipelineID:     wire.PipelineID,
		Processor:      wire.Processor,
		ProcessorIndex: wire.ProcessorIndex,
		FrameKind:      FrameType(wire.FrameKind),
		Direction:      dir,
		Sequence:       wire.Sequence,
		ToolCallID:     wire.ToolCallID,
		attributes:     safeEventAttributes(TracePolicy{}, wire.Attributes),
	}
	return ev, nil
}

