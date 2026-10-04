package core

import (
	"context"
	"math"
	"strings"
	"time"
)

// TraceContentMode controls which content can enter a trace event.
type TraceContentMode string

const (
	TraceNone     TraceContentMode = "none"
	TraceMetadata TraceContentMode = "metadata"
	TraceText     TraceContentMode = "text"
)

// TracePolicy defaults to metadata. Tool content requires an explicit redactor.
type TracePolicy struct {
	Content           TraceContentMode
	RedactToolContent func(string) string
}

// TraceEvent is an immutable event value when constructed with NewTraceEvent.
// Attributes returns a separate map on each call.
type TraceEvent struct {
	Name       string
	Time       time.Time
	SessionID  SessionID
	TurnID     TurnID
	Generation Generation
	attributes map[string]any
}

func NewTraceEvent(name string, at time.Time, session SessionID, turn TurnID, gen Generation, policy TracePolicy, attributes map[string]any) TraceEvent {
	return TraceEvent{Name: name, Time: at, SessionID: session, TurnID: turn, Generation: gen, attributes: safeEventAttributes(policy, attributes)}
}

func (e TraceEvent) Attributes() map[string]any { return cloneAttributes(e.attributes) }

// Tracer and Span are provider-neutral. Implementations are responsible for
// preserving parentage from the context and isolating exporter errors.
type Tracer interface {
	StartSpan(ctx context.Context, event TraceEvent) Span
}

type Span interface {
	SetAttribute(key string, value any)
	AddEvent(event TraceEvent)
	End(err error)
}

type NoopTracer struct{}
type noopSpan struct{}

func (NoopTracer) StartSpan(context.Context, TraceEvent) Span { return noopSpan{} }
func (noopSpan) SetAttribute(string, any)                     {}
func (noopSpan) AddEvent(TraceEvent)                          {}
func (noopSpan) End(error)                                    {}

type spanContextKey struct{}

// ContextWithSpan carries a parent span into child work.
func ContextWithSpan(ctx context.Context, span Span) context.Context {
	return context.WithValue(ctx, spanContextKey{}, span)
}

func SpanFromContext(ctx context.Context) Span {
	if ctx == nil {
		return nil
	}
	span, _ := ctx.Value(spanContextKey{}).(Span)
	return span
}

// safeEventAttributes admits only known metadata fields and selected text.
// Unknown keys and mutable values are dropped; audio and credentials are never admitted.
func safeEventAttributes(policy TracePolicy, input map[string]any) map[string]any {
	mode := policy.Content
	if mode == "" {
		mode = TraceMetadata
	}
	if mode != TraceMetadata && mode != TraceText {
		return nil
	}
	out := make(map[string]any)
	for key, value := range input {
		switch key {
		case "transcript", "prompt", "text":
			if mode == TraceText {
				if s, ok := value.(string); ok {
					out[key] = s
				}
			}
		case "tool.arguments", "tool.result":
			if mode == TraceText && policy.RedactToolContent != nil {
				if s, ok := value.(string); ok {
					out[key] = policy.RedactToolContent(s)
				}
			}
		case "count", "sample_rate", "channels", "duration_ms", "latency_ms", "sequence", "model", "provider", "format", "status", "error.category", "unit", "quantity", "reason", "component":
			switch value.(type) {
			case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
				out[key] = value
			}
		default:
			if strings.HasPrefix(key, "custom.") && safeMetadataKey(key) {
				if copied, ok := copyStructuredMetadata(value, 0); ok {
					out[key] = copied
				}
			}
		}
	}
	return out
}

func cloneAttributes(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		if strings.HasPrefix(key, "custom.") {
			out[key], _ = copyStructuredMetadata(value, 0)
		} else {
			out[key] = value
		}
	}
	return out
}

// copyStructuredMetadata accepts bounded JSON-like numeric/boolean metadata.
// Strings, bytes, pointers, and unsupported collections fail closed.
func copyStructuredMetadata(value any, depth int) (any, bool) {
	if depth > 8 {
		return nil, false
	}
	switch v := value.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v, true
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, false
		}
		return v, true
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		return v, true
	case map[string]any:
		if len(v) > 64 {
			return nil, false
		}
		out := make(map[string]any, len(v))
		for key, child := range v {
			if !safeMetadataKey(key) {
				return nil, false
			}
			copied, ok := copyStructuredMetadata(child, depth+1)
			if !ok {
				return nil, false
			}
			out[key] = copied
		}
		return out, true
	case []any:
		if len(v) > 64 {
			return nil, false
		}
		out := make([]any, len(v))
		for i, child := range v {
			copied, ok := copyStructuredMetadata(child, depth+1)
			if !ok {
				return nil, false
			}
			out[i] = copied
		}
		return out, true
	default:
		return nil, false
	}
}

func safeMetadataKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for _, part := range []string{"secret", "token", "password", "credential", "api_key", "audio", "transcript", "prompt", "argument", "result", "text"} {
		if strings.Contains(strings.ToLower(key), part) {
			return false
		}
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
