package trace_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/server/storage"
	"govoice/server/trace"
)

func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), storage.Config{
		Dialect: storage.SQLite,
		DSN:     filepath.Join(t.TempDir(), "voice.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func createTestSession(t *testing.T, s *storage.Store) string {
	t.Helper()
	ctx := context.Background()
	flowID := storage.NewID()
	a := storage.Agent{ID: flowID, Name: "TraceAgent", Enabled: true, CreatedAt: time.Now()}
	if err := s.Agents().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := publishTestVersion(ctx, s, flowID, []byte(`{"llm":"test"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	sessID := storage.NewID()
	sess := storage.SessionRecord{
		ID:                  sessID,
		AgentID:             flowID,
		AgentVersionID:      v.ID,
		ConfigHash:          v.ConfigHash,
		Status:              "active",
		StartedAt:           time.Now().UTC(),
		ProviderStackJSON:   `{}`,
		RuntimeSnapshotJSON: `{}`,
	}
	if err := s.Sessions().Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	return sessID
}

func TestAdapterParentOrdering(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)

	adapter := trace.NewAdapter(s.Traces())
	defer adapter.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// Start parent span
	parentEv := core.NewTraceEvent("session.run", now, core.SessionID(sessID), "", 1, core.TracePolicy{}, nil)
	parentSpan := adapter.StartSpan(ctx, parentEv)

	// Start child span with parent in context
	childCtx := core.ContextWithSpan(ctx, parentSpan)
	childEv := core.NewTraceEvent("llm.generate", now.Add(10*time.Millisecond), core.SessionID(sessID), "", 1, core.TracePolicy{}, nil)
	childSpan := adapter.StartSpan(childCtx, childEv)

	// Child ends BEFORE parent ends (nested span pattern)
	childSpan.SetAttribute("status", "completed")
	childSpan.End(nil)

	parentSpan.SetAttribute("status", "completed")
	parentSpan.End(nil)

	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}

	spans, err := s.Traces().Spans(ctx, sessID, "")
	if err != nil {
		t.Fatalf("query spans: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}

	// Verify parent-child correlation
	var parentRec, childRec *storage.TraceSpan
	for i := range spans {
		if spans[i].Name == "session.run" {
			parentRec = &spans[i]
		} else if spans[i].Name == "llm.generate" {
			childRec = &spans[i]
		}
	}
	if parentRec == nil || childRec == nil {
		t.Fatalf("missing parent or child record: %+v", spans)
	}
	if childRec.ParentID != parentRec.ID {
		t.Fatalf("child parent_id=%s does not match parent id=%s", childRec.ParentID, parentRec.ID)
	}
}

func TestAdapterConcurrentWrites(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)

	adapter := trace.NewAdapter(s.Traces())
	defer adapter.Close()

	ctx := context.Background()
	concurrency := 10
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			ev := core.NewTraceEvent("worker.span", time.Now().UTC(), core.SessionID(sessID), "", core.Generation(idx), core.TracePolicy{}, nil)
			span := adapter.StartSpan(ctx, ev)
			span.SetAttribute("count", idx)
			span.AddEvent(core.NewTraceEvent("worker.step", time.Now().UTC(), core.SessionID(sessID), "", core.Generation(idx), core.TracePolicy{}, map[string]any{"latency_ms": 50}))
			span.End(nil)
		}(i)
	}

	wg.Wait()
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}

	spans, err := s.Traces().Spans(ctx, sessID, "")
	if err != nil {
		t.Fatalf("query spans: %v", err)
	}
	if len(spans) != concurrency {
		t.Fatalf("expected %d spans, got %d", concurrency, len(spans))
	}

	events, err := s.Traces().Events(ctx, sessID, "")
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(events) != concurrency {
		t.Fatalf("expected %d events, got %d", concurrency, len(events))
	}
}

func TestAdapterOutageIsolation(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)

	adapter := trace.NewAdapter(s.Traces())
	ctx := context.Background()

	// Simulate database outage by closing store
	_ = s.Close()

	// Caller operations must NOT panic or block
	done := make(chan struct{})
	go func() {
		ev := core.NewTraceEvent("outage.span", time.Now().UTC(), core.SessionID(sessID), "", 1, core.TracePolicy{}, nil)
		span := adapter.StartSpan(ctx, ev)
		span.SetAttribute("latency_ms", 100)
		span.AddEvent(core.NewTraceEvent("outage.event", time.Now().UTC(), core.SessionID(sessID), "", 1, core.TracePolicy{}, nil))
		span.End(errors.New("db_down"))
		close(done)
	}()

	select {
	case <-done:
		// Succeeded without blocking
	case <-time.After(2 * time.Second):
		t.Fatal("trace operation blocked during storage outage")
	}

	_ = adapter.Close()
}

func TestAdapterMetadataOnly(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)

	// Explicit metadata-only policy
	policy := core.TracePolicy{Content: core.TraceMetadata}
	adapter := trace.NewAdapter(s.Traces())
	defer adapter.Close()

	ctx := context.Background()
	ev := core.NewTraceEvent("privacy.test", time.Now().UTC(), core.SessionID(sessID), "", 1, policy, map[string]any{
		"transcript": "sensitive user speech",
		"reason":     "hangup",
	})
	span := adapter.StartSpan(ctx, ev)
	// Try setting sensitive and forbidden attributes
	span.SetAttribute("secret_key", "sk-secret123")
	span.SetAttribute("prompt", "system instructions")
	span.SetAttribute("mcp.tool_name", "gpt-4o")
	span.End(nil)

	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}

	spans, err := s.Traces().Spans(ctx, sessID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	attrs := spans[0].AttributesJSON
	if contains(attrs, "sensitive user speech") {
		t.Fatalf("transcript leaked in metadata-only mode: %s", attrs)
	}
	if contains(attrs, "sk-secret123") {
		t.Fatalf("secret leaked in attributes: %s", attrs)
	}
	if contains(attrs, "system instructions") {
		t.Fatalf("prompt leaked in metadata-only mode: %s", attrs)
	}
	if !contains(attrs, "gpt-4o") || !contains(attrs, "hangup") {
		t.Fatalf("metadata fields missing from attributes: %s", attrs)
	}
}

func TestAdapterNonBlockingQueueOverflow(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)

	adapter := trace.NewAdapter(s.Traces())
	defer adapter.Close()

	ctx := context.Background()
	// Rapidly enqueue 100 spans without waiting
	for i := 0; i < 100; i++ {
		ev := core.NewTraceEvent("overflow.span", time.Now().UTC(), core.SessionID(sessID), "", 1, core.TracePolicy{}, nil)
		span := adapter.StartSpan(ctx, ev)
		span.End(nil)
	}
}

func TestAdapterConcurrentCloseNoPanic(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)

	for iter := 0; iter < 10; iter++ {
		adapter := trace.NewAdapter(s.Traces())
		ctx := context.Background()
		var wg sync.WaitGroup
		concurrency := 8
		wg.Add(concurrency)

		for i := 0; i < concurrency; i++ {
			go func(workerID int) {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					ev := core.NewTraceEvent("stress.span", time.Now().UTC(), core.SessionID(sessID), "", 1, core.TracePolicy{}, nil)
					span := adapter.StartSpan(ctx, ev)
					span.SetAttribute("count", j)
					span.SetAttribute("custom.worker", workerID)
					span.AddEvent(core.NewTraceEvent("stress.event", time.Now().UTC(), core.SessionID(sessID), "", 1, core.TracePolicy{}, map[string]any{"latency_ms": 10}))
					span.End(nil)
				}
			}(i)
		}

		time.Sleep(time.Duration(iter*150) * time.Microsecond)
		if err := adapter.Close(); err != nil {
			t.Fatalf("first Close failed: %v", err)
		}

		wg.Wait()

		// Subsequent Close calls must remain non-panicking and idempotent
		if err := adapter.Close(); err != nil {
			t.Fatalf("idempotent Close failed: %v", err)
		}
	}
}

// Only the whitelisted metadata keys are stored: content keys, custom.* keys, unknown keys, oversized
// strings and non-finite numbers are all dropped.
func TestAdapterFilterAttributesPrivacyPolicy(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)

	adapter := trace.NewAdapter(s.Traces())
	defer adapter.Close()

	ctx := context.Background()
	span := adapter.StartSpan(ctx, core.NewTraceEvent("policy.span", time.Now().UTC(), core.SessionID(sessID), "", 1, core.TracePolicy{}, nil))

	for _, k := range []string{"transcript", "prompt", "text", "tool.arguments", "tool.result", "secret", "audio", "custom.metric", "custom.text", "count", "model", "latency_ms"} {
		span.SetAttribute(k, "leak_attempt")
		span.SetAttribute(k, 123)
	}
	span.SetAttribute("custom.nested", map[string]any{"sub_count": 5})
	span.SetAttribute("mcp.tool_name", strings.Repeat("m", 300)) // whitelisted key, oversized string
	span.SetAttribute("mcp.duration_ms", math.NaN())
	span.SetAttribute("mcp.server_id", []byte{1, 2}) // unsupported type
	span.SetAttribute("reason", "hangup")
	span.SetAttribute("mcp.duration_ms", 12.5)
	span.End(nil)

	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	spans, err := s.Traces().Spans(ctx, sessID, "")
	if err != nil || len(spans) != 1 {
		t.Fatalf("spans %v err %v", spans, err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(spans[0].AttributesJSON), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"status": "ok", "reason": "hangup", "mcp.duration_ms": 12.5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored attributes %v, want %v", got, want)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || (len(s) > 0 && filepath.Base(s) != "" && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// publishTestVersion publishes config (with the given MCP selection) as the flow's next version.
func publishTestVersion(ctx context.Context, s *storage.Store, flowID string, config []byte, tools []storage.SelectedTool) (storage.AgentVersion, error) {
	v, _, err := s.Agents().MutateAndPublish(ctx, flowID, storage.AgentMutation{Tools: &tools}, config)
	return v, err
}

func TestSpanEnd_ErrorCategoryNeverContainsMessage(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)
	adapter := trace.NewAdapter(s.Traces())
	defer adapter.Close()

	ctx := context.Background()
	ev := core.NewTraceEvent("err.test", time.Now().UTC(), core.SessionID(sessID), "", 1, core.TracePolicy{Content: core.TraceMetadata}, nil)
	span := adapter.StartSpan(ctx, ev)
	span.End(errors.New("dial https://x.example?api_key=SECRET failed"))

	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	spans, err := s.Traces().Spans(ctx, sessID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	attrs := spans[0].AttributesJSON
	if contains(attrs, "SECRET") || contains(attrs, "api_key") {
		t.Fatalf("error message leaked into stored attributes: %s", attrs)
	}
	if !contains(attrs, "errorString") {
		t.Fatalf("error type missing from attributes: %s", attrs)
	}
}

func TestAdapterKeepsWhichMCPToolRan(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)
	adapter := trace.NewAdapter(s.Traces())
	ctx := context.Background()
	span := adapter.StartSpan(ctx, core.NewTraceEvent("mcp.tool.execute", time.Now().UTC(), core.SessionID(sessID), "", 1, core.TracePolicy{Content: core.TraceMetadata}, nil))
	span.SetAttribute("mcp.server_id", "0192aaaa-0000-7000-8000-000000000001")
	span.SetAttribute("mcp.tool_name", "lookup_order")
	span.SetAttribute("mcp.duration_ms", int64(42))
	span.SetAttribute("mcp.tool_count", 7) // discovery's count of tools
	span.End(nil)
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	spans, err := s.Traces().Spans(ctx, sessID, "")
	if err != nil || len(spans) != 1 {
		t.Fatalf("spans %+v err %v", spans, err)
	}
	for _, want := range []string{`"mcp.tool_count":7`, `"mcp.server_id":"0192aaaa-0000-7000-8000-000000000001"`, `"mcp.tool_name":"lookup_order"`, `"mcp.duration_ms":42`} {
		if !strings.Contains(spans[0].AttributesJSON, want) {
			t.Fatalf("attributes %s lack %s", spans[0].AttributesJSON, want)
		}
	}
}
