// Package otel provides optional OpenTelemetry trace export correlated to internal IDs.
package otel

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"govoice/core"
	"govoice/server/storage"
)

const defaultQueueSize = 2048

// UUIDToTraceIDHex converts a UUID string into standard 32-character lowercase hex TraceID.
func UUIDToTraceIDHex(s string) string {
	u, err := uuid.Parse(s)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(u[:])
}

// UUIDToSpanIDHex converts a UUID string into standard 16-character lowercase hex SpanID
// using the lower 8 bytes of the UUID.
func UUIDToSpanIDHex(s string) string {
	u, err := uuid.Parse(s)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(u[8:16])
}

func parseTraceID(s string) oteltrace.TraceID {
	u, err := uuid.Parse(s)
	if err != nil {
		return oteltrace.TraceID{}
	}
	return oteltrace.TraceID(u)
}

func parseSpanID(s string) oteltrace.SpanID {
	u, err := uuid.Parse(s)
	if err != nil {
		return oteltrace.SpanID{}
	}
	var id oteltrace.SpanID
	copy(id[:], u[8:16])
	return id
}

type ctxTraceIDKey struct{}
type ctxSpanIDKey struct{}

type idGen struct{}

func (g *idGen) NewIDs(ctx context.Context) (oteltrace.TraceID, oteltrace.SpanID) {
	var tid oteltrace.TraceID
	if val, ok := ctx.Value(ctxTraceIDKey{}).(oteltrace.TraceID); ok && val.IsValid() {
		tid = val
	} else {
		u, _ := uuid.NewV7()
		tid = oteltrace.TraceID(u)
	}

	var sid oteltrace.SpanID
	if val, ok := ctx.Value(ctxSpanIDKey{}).(oteltrace.SpanID); ok && val.IsValid() {
		sid = val
	} else {
		u, _ := uuid.NewV7()
		copy(sid[:], u[8:16])
	}
	return tid, sid
}

func (g *idGen) NewSpanID(ctx context.Context, traceID oteltrace.TraceID) oteltrace.SpanID {
	if val, ok := ctx.Value(ctxSpanIDKey{}).(oteltrace.SpanID); ok && val.IsValid() {
		return val
	}
	u, _ := uuid.NewV7()
	var sid oteltrace.SpanID
	copy(sid[:], u[8:16])
	return sid
}

type exportItemKind uint8

const (
	itemSpan exportItemKind = iota + 1
	itemEvent
	itemToolCall
	itemFlush
)

type exportItem struct {
	kind     exportItemKind
	span     storage.TraceSpan
	event    storage.TraceEvent
	toolCall storage.ToolCallRecord
	syncChan chan struct{}
}

// Option configures an Exporter.
type Option func(*Exporter)

// WithTracerProvider sets an injectable TracerProvider.
func WithTracerProvider(tp oteltrace.TracerProvider) Option {
	return func(e *Exporter) {
		if tp != nil {
			e.tp = tp
		}
	}
}

// WithSpanExporter sets an injectable SpanExporter.
func WithSpanExporter(exp sdktrace.SpanExporter) Option {
	return func(e *Exporter) {
		e.spanExporter = exp
	}
}

// WithPolicy sets the trace privacy content policy.
func WithPolicy(policy core.TracePolicy) Option {
	return func(e *Exporter) {
		e.policy = policy
	}
}

// WithQueueSize sets the bounded asynchronous queue capacity.
func WithQueueSize(size int) Option {
	return func(e *Exporter) {
		if size > 0 {
			e.queueSize = size
		}
	}
}

// Exporter provides non-blocking, privacy-preserving OpenTelemetry export.
type Exporter struct {
	tp           oteltrace.TracerProvider
	spanExporter sdktrace.SpanExporter
	tracer       oteltrace.Tracer
	policy       core.TracePolicy
	queueSize    int
	queue        chan exportItem

	dropped   atomic.Uint64
	errors    atomic.Uint64
	closed    atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
	closeMu   sync.RWMutex
}

// NewExporter initializes an OpenTelemetry exporter.
func NewExporter(opts ...Option) *Exporter {
	e := &Exporter{
		policy:    core.TracePolicy{Content: core.TraceMetadata},
		queueSize: defaultQueueSize,
		done:      make(chan struct{}),
	}
	for _, opt := range opts {
		opt(e)
	}

	if e.tp == nil {
		tpOpts := []sdktrace.TracerProviderOption{
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
			sdktrace.WithIDGenerator(&idGen{}),
		}
		if e.spanExporter != nil {
			tpOpts = append(tpOpts, sdktrace.WithSyncer(e.spanExporter))
		}
		e.tp = sdktrace.NewTracerProvider(tpOpts...)
	}
	e.tracer = e.tp.Tracer("govoice")
	e.queue = make(chan exportItem, e.queueSize)

	go e.worker()
	return e
}

// DroppedCount returns the count of items discarded due to queue overflow.
func (e *Exporter) DroppedCount() uint64 {
	return e.dropped.Load()
}

// ErrorCount returns the count of failed export attempts.
func (e *Exporter) ErrorCount() uint64 {
	return e.errors.Load()
}

func (e *Exporter) enqueue(item exportItem) bool {
	if e.closed.Load() {
		e.dropped.Add(1)
		return false
	}
	e.closeMu.RLock()
	defer e.closeMu.RUnlock()
	if e.closed.Load() {
		e.dropped.Add(1)
		return false
	}
	select {
	case e.queue <- item:
		return true
	default:
		e.dropped.Add(1)
		return false
	}
}

// RecordSpan enqueues a span for non-blocking export.
func (e *Exporter) RecordSpan(span storage.TraceSpan) {
	e.enqueue(exportItem{
		kind: itemSpan,
		span: span,
	})
}

// RecordEvent enqueues a trace event for non-blocking export.
func (e *Exporter) RecordEvent(event storage.TraceEvent) {
	e.enqueue(exportItem{
		kind:  itemEvent,
		event: event,
	})
}

// RecordToolCall enqueues a tool call record for non-blocking export.
func (e *Exporter) RecordToolCall(call storage.ToolCallRecord) {
	e.enqueue(exportItem{
		kind:     itemToolCall,
		toolCall: call,
	})
}

// Flush waits for all currently enqueued items to be exported or context cancelled.
func (e *Exporter) Flush(ctx context.Context) error {
	if e.closed.Load() {
		return nil
	}
	syncChan := make(chan struct{})

	e.closeMu.RLock()
	if e.closed.Load() {
		e.closeMu.RUnlock()
		return nil
	}
	select {
	case e.queue <- exportItem{kind: itemFlush, syncChan: syncChan}:
		e.closeMu.RUnlock()
	case <-ctx.Done():
		e.closeMu.RUnlock()
		return ctx.Err()
	}

	select {
	case <-syncChan:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
		return nil
	}
}

// Close drains and flushes pending items, shuts down tracer provider, and returns.
func (e *Exporter) Close() error {
	e.closeOnce.Do(func() {
		e.closed.Store(true)
		e.closeMu.Lock()
		close(e.queue)
		e.closeMu.Unlock()
		<-e.done
		if tp, ok := e.tp.(*sdktrace.TracerProvider); ok {
			_ = tp.Shutdown(context.Background())
		}
	})
	return nil
}

func (e *Exporter) worker() {
	defer close(e.done)
	for item := range e.queue {
		if item.syncChan != nil {
			close(item.syncChan)
			continue
		}
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					e.errors.Add(1)
				}
			}()
			switch item.kind {
			case itemSpan:
				err = e.exportSpan(context.Background(), item.span)
			case itemEvent:
				err = e.exportEvent(context.Background(), item.event)
			case itemToolCall:
				err = e.exportToolCall(context.Background(), item.toolCall)
			}
		}()
		if err != nil {
			e.errors.Add(1)
		}
	}
}

func (e *Exporter) exportSpan(ctx context.Context, span storage.TraceSpan) error {
	traceID := parseTraceID(span.SessionID)
	parentSpanID := parseSpanID(span.ParentID)
	spanID := parseSpanID(span.ID)

	parentCtx := ctx
	if traceID.IsValid() {
		parentCtx = context.WithValue(parentCtx, ctxTraceIDKey{}, traceID)
	}
	if spanID.IsValid() {
		parentCtx = context.WithValue(parentCtx, ctxSpanIDKey{}, spanID)
	}
	if traceID.IsValid() && parentSpanID.IsValid() {
		var scConfig oteltrace.SpanContextConfig
		scConfig.TraceID = traceID
		scConfig.SpanID = parentSpanID
		scConfig.TraceFlags = oteltrace.FlagsSampled
		scConfig.Remote = true
		sc := oteltrace.NewSpanContext(scConfig)
		parentCtx = oteltrace.ContextWithRemoteSpanContext(parentCtx, sc)
	}

	startedAt := span.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	endedAt := span.EndedAt
	if endedAt.IsZero() {
		endedAt = startedAt
	}

	attrs := e.filterAndMapAttributes(span.AttributesJSON, span.SessionID, span.TurnID, span.ID, span.ParentID)

	name := span.Name
	if name == "" {
		name = "trace.span"
	}

	_, otelSpan := e.tracer.Start(
		parentCtx,
		name,
		oteltrace.WithTimestamp(startedAt),
		oteltrace.WithAttributes(attrs...),
	)

	var parsedAttrs map[string]any
	if err := json.Unmarshal([]byte(span.AttributesJSON), &parsedAttrs); err == nil {
		st, _ := parsedAttrs["status"].(string)
		ec, _ := parsedAttrs["error.category"].(string)
		if st == "error" || ec != "" {
			otelSpan.SetStatus(codes.Error, ec)
		} else {
			otelSpan.SetStatus(codes.Ok, "")
		}
	}

	otelSpan.End(oteltrace.WithTimestamp(endedAt))
	return nil
}

func (e *Exporter) exportEvent(ctx context.Context, ev storage.TraceEvent) error {
	traceID := parseTraceID(ev.SessionID)
	spanID := parseSpanID(ev.SpanID)
	eventID := parseSpanID(ev.ID)

	parentCtx := ctx
	if traceID.IsValid() {
		parentCtx = context.WithValue(parentCtx, ctxTraceIDKey{}, traceID)
	}
	if eventID.IsValid() {
		parentCtx = context.WithValue(parentCtx, ctxSpanIDKey{}, eventID)
	}
	if traceID.IsValid() && spanID.IsValid() {
		var scConfig oteltrace.SpanContextConfig
		scConfig.TraceID = traceID
		scConfig.SpanID = spanID
		scConfig.TraceFlags = oteltrace.FlagsSampled
		scConfig.Remote = true
		sc := oteltrace.NewSpanContext(scConfig)
		parentCtx = oteltrace.ContextWithRemoteSpanContext(parentCtx, sc)
	}

	occurredAt := ev.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}

	attrs := e.filterAndMapAttributes(ev.AttributesJSON, ev.SessionID, ev.TurnID, ev.SpanID, "")
	if ev.ID != "" {
		attrs = append(attrs, attribute.String("event_id", ev.ID))
	}

	name := ev.Name
	if name == "" {
		name = "trace.event"
	}

	_, otelSpan := e.tracer.Start(
		parentCtx,
		name,
		oteltrace.WithTimestamp(occurredAt),
		oteltrace.WithAttributes(attrs...),
	)
	otelSpan.End(oteltrace.WithTimestamp(occurredAt))
	return nil
}

func (e *Exporter) exportToolCall(ctx context.Context, call storage.ToolCallRecord) error {
	traceID := parseTraceID(call.SessionID)
	turnSpanID := parseSpanID(call.TurnID)
	callSpanID := parseSpanID(call.ID)

	parentCtx := ctx
	if traceID.IsValid() {
		parentCtx = context.WithValue(parentCtx, ctxTraceIDKey{}, traceID)
	}
	if callSpanID.IsValid() {
		parentCtx = context.WithValue(parentCtx, ctxSpanIDKey{}, callSpanID)
	}
	if traceID.IsValid() && turnSpanID.IsValid() {
		var scConfig oteltrace.SpanContextConfig
		scConfig.TraceID = traceID
		scConfig.SpanID = turnSpanID
		scConfig.TraceFlags = oteltrace.FlagsSampled
		scConfig.Remote = true
		sc := oteltrace.NewSpanContext(scConfig)
		parentCtx = oteltrace.ContextWithRemoteSpanContext(parentCtx, sc)
	}

	startedAt := call.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	endedAt := call.EndedAt
	if endedAt.IsZero() {
		endedAt = startedAt
	}

	attrs := []attribute.KeyValue{
		attribute.String("session_id", call.SessionID),
		attribute.String("tool_call_id", call.ID),
		attribute.String("remote_name", call.RemoteName),
		attribute.String("status", call.Status),
	}
	if call.TurnID != "" {
		attrs = append(attrs, attribute.String("turn_id", call.TurnID))
	}
	if call.MCPServerID != "" {
		attrs = append(attrs, attribute.String("mcp_server_id", call.MCPServerID))
	}
	if call.SchemaHash != "" {
		attrs = append(attrs, attribute.String("schema_hash", call.SchemaHash))
	}

	// Content policy on tool payload summaries
	if e.policy.Content == core.TraceText && e.policy.RedactToolContent != nil {
		if call.ArgumentsSummary != "" {
			attrs = append(attrs, attribute.String("tool.arguments", e.safeRedact(call.ArgumentsSummary)))
		}
		if call.ResultSummary != "" {
			attrs = append(attrs, attribute.String("tool.result", e.safeRedact(call.ResultSummary)))
		}
	}

	name := "tool:" + call.RemoteName
	if call.RemoteName == "" {
		name = "tool_call"
	}

	_, otelSpan := e.tracer.Start(
		parentCtx,
		name,
		oteltrace.WithTimestamp(startedAt),
		oteltrace.WithAttributes(attrs...),
	)
	if call.Status == "error" || call.Status == "failed" {
		otelSpan.SetStatus(codes.Error, "tool_failure")
	} else {
		otelSpan.SetStatus(codes.Ok, "")
	}
	otelSpan.End(oteltrace.WithTimestamp(endedAt))
	return nil
}

func (e *Exporter) safeRedact(s string) (result string) {
	defer func() {
		if recover() != nil {
			result = ""
		}
	}()
	if e.policy.RedactToolContent != nil {
		return e.policy.RedactToolContent(s)
	}
	return ""
}

func isForbiddenKey(key string) bool {
	lower := strings.ToLower(key)
	for _, forbidden := range []string{
		"secret", "token", "password", "credential", "api_key", "audio", "raw_audio",
	} {
		if strings.Contains(lower, forbidden) {
			return true
		}
	}
	return false
}

func sanitizeMetadataValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(val))
		for k, subV := range val {
			if isForbiddenKey(k) {
				continue
			}
			cleaned[k] = sanitizeMetadataValue(subV)
		}
		return cleaned
	case []any:
		cleaned := make([]any, 0, len(val))
		for _, elem := range val {
			cleaned = append(cleaned, sanitizeMetadataValue(elem))
		}
		return cleaned
	default:
		return val
	}
}

func toStringSlice(slice []any) ([]string, bool) {
	if len(slice) == 0 {
		return []string{}, true
	}
	res := make([]string, len(slice))
	for i, item := range slice {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		res[i] = s
	}
	return res, true
}

func toBoolSlice(slice []any) ([]bool, bool) {
	if len(slice) == 0 {
		return []bool{}, true
	}
	res := make([]bool, len(slice))
	for i, item := range slice {
		b, ok := item.(bool)
		if !ok {
			return nil, false
		}
		res[i] = b
	}
	return res, true
}

func toFloat64Slice(slice []any) ([]float64, bool) {
	if len(slice) == 0 {
		return []float64{}, true
	}
	res := make([]float64, len(slice))
	for i, item := range slice {
		f, ok := item.(float64)
		if !ok {
			return nil, false
		}
		res[i] = f
	}
	return res, true
}

func toInt64Slice(slice []any) ([]int64, bool) {
	if len(slice) == 0 {
		return []int64{}, true
	}
	res := make([]int64, len(slice))
	for i, item := range slice {
		switch num := item.(type) {
		case int64:
			res[i] = num
		case int:
			res[i] = int64(num)
		case float64:
			if float64(int64(num)) == num {
				res[i] = int64(num)
			} else {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return res, true
}

func (e *Exporter) filterAndMapAttributes(rawJSON, sessionID, turnID, spanID, parentID string) []attribute.KeyValue {
	var attrs []attribute.KeyValue

	if sessionID != "" {
		attrs = append(attrs, attribute.String("session_id", sessionID))
	}
	if turnID != "" {
		attrs = append(attrs, attribute.String("turn_id", turnID))
	}
	if spanID != "" {
		attrs = append(attrs, attribute.String("span_id", spanID))
	}
	if parentID != "" {
		attrs = append(attrs, attribute.String("parent_id", parentID))
	}

	if rawJSON == "" || rawJSON == "{}" {
		return attrs
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
		return attrs
	}

	mode := e.policy.Content
	if mode == "" {
		mode = core.TraceMetadata
	}

	for k, v := range parsed {
		if isForbiddenKey(k) {
			continue
		}

		switch k {
		case "transcript", "prompt", "text":
			if mode == core.TraceText {
				if s, ok := v.(string); ok {
					attrs = append(attrs, attribute.String(k, s))
				}
			}
		case "tool.arguments", "tool.result":
			if mode == core.TraceText && e.policy.RedactToolContent != nil {
				if s, ok := v.(string); ok {
					attrs = append(attrs, attribute.String(k, e.safeRedact(s)))
				}
			}
		default:
			switch val := v.(type) {
			case string:
				attrs = append(attrs, attribute.String(k, val))
			case bool:
				attrs = append(attrs, attribute.Bool(k, val))
			case float64:
				attrs = append(attrs, attribute.Float64(k, val))
			case int64:
				attrs = append(attrs, attribute.Int64(k, val))
			case int:
				attrs = append(attrs, attribute.Int(k, val))
			case []string:
				attrs = append(attrs, attribute.StringSlice(k, val))
			case []bool:
				attrs = append(attrs, attribute.BoolSlice(k, val))
			case []float64:
				attrs = append(attrs, attribute.Float64Slice(k, val))
			case []int64:
				attrs = append(attrs, attribute.Int64Slice(k, val))
			case []int:
				attrs = append(attrs, attribute.IntSlice(k, val))
			case []any:
				cleaned, _ := sanitizeMetadataValue(val).([]any)
				if strSlice, ok := toStringSlice(cleaned); ok {
					attrs = append(attrs, attribute.StringSlice(k, strSlice))
				} else if boolSlice, ok := toBoolSlice(cleaned); ok {
					attrs = append(attrs, attribute.BoolSlice(k, boolSlice))
				} else if floatSlice, ok := toFloat64Slice(cleaned); ok {
					attrs = append(attrs, attribute.Float64Slice(k, floatSlice))
				} else if intSlice, ok := toInt64Slice(cleaned); ok {
					attrs = append(attrs, attribute.Int64Slice(k, intSlice))
				} else {
					if b, err := json.Marshal(cleaned); err == nil {
						attrs = append(attrs, attribute.String(k, string(b)))
					}
				}
			case map[string]any:
				cleaned, _ := sanitizeMetadataValue(val).(map[string]any)
				if len(cleaned) > 0 || len(val) == 0 {
					if b, err := json.Marshal(cleaned); err == nil {
						attrs = append(attrs, attribute.String(k, string(b)))
					}
				}
			default:
				cleaned := sanitizeMetadataValue(val)
				if b, err := json.Marshal(cleaned); err == nil && string(b) != "null" {
					attrs = append(attrs, attribute.String(k, string(b)))
				}
			}
		}
	}

	return attrs
}
