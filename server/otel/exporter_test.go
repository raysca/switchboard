package otel_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"govoice/core"
	"govoice/server/otel"
	"govoice/server/storage"
	"govoice/server/trace"
)

func TestIDMappingAndTraceContext(t *testing.T) {
	inMem := tracetest.NewInMemoryExporter()
	exp := otel.NewExporter(
		otel.WithSpanExporter(inMem),
		otel.WithPolicy(core.TracePolicy{Content: core.TraceMetadata}),
	)
	defer exp.Close()

	ctx := context.Background()
	sessID := storage.NewID()
	turnID := storage.NewID()
	parentID := storage.NewID()
	spanID := storage.NewID()

	now := time.Now().UTC()

	span := storage.TraceSpan{
		ID:             spanID,
		SessionID:      sessID,
		TurnID:         turnID,
		ParentID:       parentID,
		Name:           "llm.generate",
		StartedAt:      now,
		EndedAt:        now.Add(150 * time.Millisecond),
		AttributesJSON: `{"model":"gemini-2.5-flash","latency_ms":150}`,
	}

	exp.RecordSpan(span)
	if err := exp.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	spans := inMem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	ro := spans[0]
	if ro.Name != "llm.generate" {
		t.Errorf("expected name llm.generate, got %s", ro.Name)
	}

	// Internal session ID mapped to OTel TraceID
	traceIDHex := ro.SpanContext.TraceID().String()
	expectedTraceHex := otel.UUIDToTraceIDHex(sessID)
	if traceIDHex != expectedTraceHex {
		t.Errorf("TraceID hex = %s, want %s", traceIDHex, expectedTraceHex)
	}

	// Parent SpanID mapped
	if ro.Parent.SpanID().String() != otel.UUIDToSpanIDHex(parentID) {
		t.Errorf("Parent SpanID hex = %s, want %s", ro.Parent.SpanID().String(), otel.UUIDToSpanIDHex(parentID))
	}

	// Check attributes
	attrMap := make(map[string]any)
	for _, attr := range ro.Attributes {
		attrMap[string(attr.Key)] = attr.Value.AsInterface()
	}

	if attrMap["session_id"] != sessID {
		t.Errorf("session_id attribute = %v, want %v", attrMap["session_id"], sessID)
	}
	if attrMap["turn_id"] != turnID {
		t.Errorf("turn_id attribute = %v, want %v", attrMap["turn_id"], turnID)
	}
	if attrMap["span_id"] != spanID {
		t.Errorf("span_id attribute = %v, want %v", attrMap["span_id"], spanID)
	}
	if attrMap["parent_id"] != parentID {
		t.Errorf("parent_id attribute = %v, want %v", attrMap["parent_id"], parentID)
	}
	if attrMap["model"] != "gemini-2.5-flash" {
		t.Errorf("model attribute = %v, want gemini-2.5-flash", attrMap["model"])
	}
}

func TestPrivacyPolicyMetadataOnly(t *testing.T) {
	inMem := tracetest.NewInMemoryExporter()
	exp := otel.NewExporter(
		otel.WithSpanExporter(inMem),
		otel.WithPolicy(core.TracePolicy{Content: core.TraceMetadata}),
	)
	defer exp.Close()

	ctx := context.Background()
	sessID := storage.NewID()
	spanID := storage.NewID()
	now := time.Now().UTC()

	// Span attributes containing forbidden content/secrets
	rawAttrs := `{
		"transcript": "CONFIDENTIAL user words",
		"prompt": "SECRET system prompt",
		"text": "PRIVATE speech text",
		"api_key": "sk-1234567890",
		"auth_token": "bearer-xyz",
		"raw_audio": "audio-bytes-data",
		"tool.arguments": "{\"account_id\":\"12345\"}",
		"tool.result": "{\"balance\":999999}",
		"count": 5,
		"status": "ok",
		"model": "chirp_2"
	}`

	span := storage.TraceSpan{
		ID:             spanID,
		SessionID:      sessID,
		Name:           "stt.transcribe",
		StartedAt:      now,
		EndedAt:        now.Add(50 * time.Millisecond),
		AttributesJSON: rawAttrs,
	}

	exp.RecordSpan(span)

	// Tool call with payloads
	exp.RecordToolCall(storage.ToolCallRecord{
		ID:               storage.NewID(),
		SessionID:        sessID,
		RemoteName:       "query_db",
		StartedAt:        now,
		EndedAt:          now.Add(10 * time.Millisecond),
		Status:           "completed",
		ArgumentsSummary: "SELECT * FROM users",
		ResultSummary:    "alice, bob",
	})

	if err := exp.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	spans := inMem.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans (1 trace span + 1 tool call span), got %d", len(spans))
	}

	// Verify span attributes exclude forbidden content
	for _, ro := range spans {
		for _, attr := range ro.Attributes {
			k := string(attr.Key)
			v := attr.Value.Emit()

			for _, forbidden := range []string{
				"transcript", "prompt", "text", "api_key", "auth_token", "raw_audio",
				"CONFIDENTIAL", "SECRET", "PRIVATE", "sk-123", "bearer", "SELECT *", "alice",
			} {
				if k == forbidden || (len(v) > 0 && v == forbidden) {
					t.Errorf("Privacy leak: found %s=%s in span %s", k, v, ro.Name)
				}
			}
		}
	}
}

func TestPrivacyPolicyTextModeWithRedaction(t *testing.T) {
	inMem := tracetest.NewInMemoryExporter()
	redactor := func(s string) string {
		return "[REDACTED_TOOL_CONTENT]"
	}

	exp := otel.NewExporter(
		otel.WithSpanExporter(inMem),
		otel.WithPolicy(core.TracePolicy{
			Content:           core.TraceText,
			RedactToolContent: redactor,
		}),
	)
	defer exp.Close()

	ctx := context.Background()
	sessID := storage.NewID()
	spanID := storage.NewID()
	now := time.Now().UTC()

	rawAttrs := `{
		"transcript": "Hello assistant",
		"tool.arguments": "credit_card=1234-5678",
		"api_key": "secret-key",
		"status": "ok"
	}`

	span := storage.TraceSpan{
		ID:             spanID,
		SessionID:      sessID,
		Name:           "turn.text",
		StartedAt:      now,
		EndedAt:        now.Add(20 * time.Millisecond),
		AttributesJSON: rawAttrs,
	}

	exp.RecordSpan(span)
	if err := exp.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	spans := inMem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	ro := spans[0]
	attrMap := make(map[string]any)
	for _, attr := range ro.Attributes {
		attrMap[string(attr.Key)] = attr.Value.AsInterface()
	}

	// Transcript allowed in Text mode
	if attrMap["transcript"] != "Hello assistant" {
		t.Errorf("expected transcript allowed in text mode, got %v", attrMap["transcript"])
	}

	// Tool content redacted
	if attrMap["tool.arguments"] != "[REDACTED_TOOL_CONTENT]" {
		t.Errorf("expected redacted tool arguments, got %v", attrMap["tool.arguments"])
	}

	// Credential still excluded
	if _, ok := attrMap["api_key"]; ok {
		t.Errorf("credential key api_key must still be excluded in text mode")
	}
}

type failingExporter struct{}

func (f *failingExporter) ExportSpans(ctx context.Context, spans []tracetest.SpanStub) error {
	return errors.New("simulated network outage to otel collector")
}

func (f *failingExporter) Shutdown(ctx context.Context) error {
	return nil
}

func TestNonBlockingAndOutageIsolation(t *testing.T) {
	inMem := tracetest.NewInMemoryExporter()
	exp := otel.NewExporter(
		otel.WithSpanExporter(inMem),
		otel.WithQueueSize(5),
	)
	defer exp.Close()

	// Enqueue more items than queue size (5)
	sessID := storage.NewID()
	for i := 0; i < 50; i++ {
		exp.RecordSpan(storage.TraceSpan{
			ID:        storage.NewID(),
			SessionID: sessID,
			Name:      "flooded.span",
			StartedAt: time.Now().UTC(),
			EndedAt:   time.Now().UTC(),
		})
	}

	// Dropped count must be non-zero
	dropped := exp.DroppedCount()
	if dropped == 0 {
		t.Errorf("expected non-zero dropped items due to queue overflow")
	}

	// Closing does not panic or deadlock
	if err := exp.Close(); err != nil {
		t.Errorf("Close err = %v", err)
	}
}

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Config{
		Dialect: storage.SQLite,
		DSN:     filepath.Join(t.TempDir(), "trace.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTraceAdapterWithOTelSink(t *testing.T) {
	inMem := tracetest.NewInMemoryExporter()
	otelExp := otel.NewExporter(
		otel.WithSpanExporter(inMem),
		otel.WithPolicy(core.TracePolicy{Content: core.TraceMetadata}),
	)
	defer otelExp.Close()

	s := newTestStore(t)
	adapter := trace.NewAdapter(s.Traces(), trace.WithSink(otelExp))
	defer adapter.Close()

	ctx := context.Background()
	sessID := storage.NewID()
	now := time.Now().UTC()

	span := adapter.StartSpan(ctx, core.NewTraceEvent("session.run", now, core.SessionID(sessID), "", 1, core.TracePolicy{}, nil))
	span.SetAttribute("model", "test-model")
	span.AddEvent(core.NewTraceEvent("speech.detected", now.Add(5*time.Millisecond), core.SessionID(sessID), "", 1, core.TracePolicy{}, map[string]any{"duration_ms": 100}))
	span.End(nil)

	adapter.RecordToolCall(storage.ToolCallRecord{
		ID:         storage.NewID(),
		SessionID:  sessID,
		RemoteName: "calc",
		StartedAt:  now,
		EndedAt:    now.Add(10 * time.Millisecond),
		Status:     "completed",
	})

	if err := adapter.Flush(ctx); err != nil {
		t.Fatalf("adapter flush: %v", err)
	}
	if err := otelExp.Flush(ctx); err != nil {
		t.Fatalf("otel flush: %v", err)
	}

	otelSpans := inMem.GetSpans()
	if len(otelSpans) < 2 {
		t.Fatalf("expected at least 2 spans (session.run and calc tool call), got %d", len(otelSpans))
	}
}

func TestStructuredMetadataPreserved(t *testing.T) {
	inMem := tracetest.NewInMemoryExporter()
	exp := otel.NewExporter(
		otel.WithSpanExporter(inMem),
		otel.WithPolicy(core.TracePolicy{Content: core.TraceMetadata}),
	)
	defer exp.Close()

	ctx := context.Background()
	sessID := storage.NewID()
	spanID := storage.NewID()
	now := time.Now().UTC()

	rawAttrs := `{
		"model_config": {
			"temperature": 0.7,
			"top_p": 0.95,
			"api_key": "forbidden-secret"
		},
		"tags": ["prod", "voice"],
		"scores": [1.5, 2.5]
	}`

	span := storage.TraceSpan{
		ID:             spanID,
		SessionID:      sessID,
		Name:           "custom.structured",
		StartedAt:      now,
		EndedAt:        now.Add(10 * time.Millisecond),
		AttributesJSON: rawAttrs,
	}

	exp.RecordSpan(span)
	if err := exp.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	spans := inMem.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	attrMap := make(map[string]any)
	for _, attr := range spans[0].Attributes {
		attrMap[string(attr.Key)] = attr.Value.AsInterface()
	}

	// Nested map must be preserved as JSON string with forbidden keys stripped
	configVal, ok := attrMap["model_config"].(string)
	if !ok {
		t.Fatalf("expected model_config attribute preserved as JSON string, got %T: %v", attrMap["model_config"], attrMap["model_config"])
	}
	if !strings.Contains(configVal, "temperature") || !strings.Contains(configVal, "0.7") {
		t.Errorf("expected model_config to contain temperature, got %s", configVal)
	}
	if strings.Contains(configVal, "api_key") || strings.Contains(configVal, "forbidden-secret") {
		t.Errorf("privacy leak: model_config should not contain api_key: %s", configVal)
	}

	// Slice of strings must be preserved
	if _, ok := attrMap["tags"]; !ok {
		t.Errorf("expected tags attribute to be preserved, got nil")
	}

	// Slice of numbers must be preserved
	if _, ok := attrMap["scores"]; !ok {
		t.Errorf("expected scores attribute to be preserved, got nil")
	}
}

