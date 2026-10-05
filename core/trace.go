package core

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
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
	// MaxTextBytes bounds selected text and redactor output. Zero uses 4 KiB;
	// values above 16 KiB are clamped.
	MaxTextBytes int
}

const (
	defaultTraceTextBytes = 4 << 10
	maxTraceTextBytes     = 16 << 10
	maxTraceLabelBytes    = 256
)

// TraceEvent is an immutable event value when constructed with NewTraceEvent.
// Attributes returns a separate map on each call.
type TraceEvent struct {
	Name       string
	Time       time.Time
	SessionID  SessionID
	TurnID     TurnID
	Generation Generation
	ToolCallID string
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

// StartSpanSafe contains tracer panics and returns a span whose callbacks are
// also panic-isolated. Runtime instrumentation uses this boundary so an
// exporter cannot fail session processing.
func StartSpanSafe(ctx context.Context, tracer Tracer, event TraceEvent) Span {
	if tracer == nil {
		return noopSpan{}
	}
	var span Span
	func() {
		defer func() { _ = recover() }()
		span = tracer.StartSpan(ctx, event)
	}()
	if span == nil {
		return noopSpan{}
	}
	if _, asynchronous := tracer.(*AsyncTracer); asynchronous {
		return span
	}
	return &guardedSpan{inner: span}
}

type guardedSpan struct {
	inner Span
	end   sync.Once
}

func (s *guardedSpan) SetAttribute(key string, value any) {
	defer func() { _ = recover() }()
	s.inner.SetAttribute(key, value)
}

func (s *guardedSpan) AddEvent(event TraceEvent) {
	defer func() { _ = recover() }()
	s.inner.AddEvent(event)
}

func (s *guardedSpan) End(err error) {
	s.end.Do(func() {
		defer func() { _ = recover() }()
		s.inner.End(err)
	})
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
	var customKeys []string
	tooManyCustom := false
	for key, value := range input {
		switch key {
		case "transcript", "prompt", "text":
			if mode == TraceText {
				if s, ok := value.(string); ok {
					if bounded, ok := boundedTraceText(policy, s); ok {
						out[key] = bounded
					}
				}
			}
		case "tool.arguments", "tool.result":
			if mode == TraceText && policy.RedactToolContent != nil {
				if s, ok := value.(string); ok {
					if bounded, ok := redactTraceText(policy, s); ok {
						out[key] = bounded
					}
				}
			}
		case "count", "sample_rate", "channels", "duration_ms", "latency_ms", "sequence", "model", "provider", "format", "status", "error.category", "unit", "quantity", "reason", "component", "frame", "direction":
			switch v := value.(type) {
			case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
				out[key] = value
			case string:
				if len(v) <= maxTraceLabelBytes {
					out[key] = v
				}
			case float32:
				if !math.IsNaN(float64(v)) && !math.IsInf(float64(v), 0) {
					out[key] = v
				}
			case float64:
				if !math.IsNaN(v) && !math.IsInf(v, 0) {
					out[key] = v
				}
			}
		default:
			if strings.HasPrefix(key, "custom.") && safeMetadataKey(key) {
				if len(customKeys) == maxStructuredMetadataNodes {
					tooManyCustom = true
				} else {
					customKeys = append(customKeys, key)
				}
			}
		}
	}
	if tooManyCustom {
		return out
	}
	sort.Strings(customKeys)
	remaining := maxStructuredMetadataNodes
	for _, key := range customKeys {
		if copied, ok := copyStructuredMetadata(input[key], 0, &remaining); ok {
			out[key] = copied
		}
	}
	return out
}

func traceTextLimit(policy TracePolicy) int {
	if policy.MaxTextBytes <= 0 {
		return defaultTraceTextBytes
	}
	if policy.MaxTextBytes > maxTraceTextBytes {
		return maxTraceTextBytes
	}
	return policy.MaxTextBytes
}

func boundedTraceText(policy TracePolicy, text string) (string, bool) {
	if len(text) > traceTextLimit(policy) {
		return "", false
	}
	return text, true
}

func redactTraceText(policy TracePolicy, text string) (result string, ok bool) {
	defer func() {
		if recover() != nil {
			result, ok = "", false
		}
	}()
	result = policy.RedactToolContent(text)
	if len(result) > traceTextLimit(policy) {
		return "", false
	}
	return result, true
}

func cloneAttributes(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	var customKeys []string
	for key, value := range input {
		if strings.HasPrefix(key, "custom.") {
			customKeys = append(customKeys, key)
		} else {
			out[key] = value
		}
	}
	sort.Strings(customKeys)
	remaining := maxStructuredMetadataNodes
	for _, key := range customKeys {
		if copied, ok := copyStructuredMetadata(input[key], 0, &remaining); ok {
			out[key] = copied
		}
	}
	return out
}

const maxStructuredMetadataNodes = 1024

// copyStructuredMetadata accepts bounded JSON-like numeric/boolean metadata.
// Strings, bytes, pointers, and unsupported collections fail closed.
func copyStructuredMetadata(value any, depth int, remaining *int) (any, bool) {
	if depth > 8 || *remaining == 0 {
		return nil, false
	}
	*remaining--
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
			copied, ok := copyStructuredMetadata(child, depth+1, remaining)
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
			copied, ok := copyStructuredMetadata(child, depth+1, remaining)
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
