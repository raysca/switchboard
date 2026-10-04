package core

import (
	"context"
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
		}
	}
	return out
}

func cloneAttributes(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
