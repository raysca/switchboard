package trace_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
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
	agentID := storage.NewID()
	a := storage.Agent{ID: agentID, Name: "TraceAgent", Enabled: true, CreatedAt: time.Now()}
	if err := s.Agents().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := s.Agents().AddVersion(ctx, agentID, 1, []byte(`{"llm":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	sessID := storage.NewID()
	sess := storage.SessionRecord{
		ID:                  sessID,
		AgentID:             agentID,
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
		"count":      42,
		"latency_ms": 120,
	})
	span := adapter.StartSpan(ctx, ev)
	// Try setting sensitive and forbidden attributes
	span.SetAttribute("secret_key", "sk-secret123")
	span.SetAttribute("prompt", "system instructions")
	span.SetAttribute("model", "gpt-4o")
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
	if !contains(attrs, "gpt-4o") || !contains(attrs, "42") {
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

func TestAdapterFilterAttributesPrivacyPolicy(t *testing.T) {
	s := newTestStore(t)
	sessID := createTestSession(t, s)

	policy := core.TracePolicy{Content: core.TraceMetadata}
	adapter := trace.NewAdapter(s.Traces())
	defer adapter.Close()

	ctx := context.Background()
	ev := core.NewTraceEvent("policy.span", time.Now().UTC(), core.SessionID(sessID), "", 1, policy, nil)
	span := adapter.StartSpan(ctx, ev)

	// 1. Forbidden keys (must be safely filtered out)
	forbiddenKeys := []string{
		"transcript", "prompt", "argument", "result", "text",
		"secret", "token", "password", "credential", "api_key", "audio",
		"custom.transcript", "custom.prompt", "custom.argument", "custom.result", "custom.text",
		"custom.secret_key", "custom.auth_token", "custom.password_val", "custom.credential_data", "custom.api_key_str", "custom.audio_stream",
		"bad key with spaces", "invalid$key!", strings.Repeat("k", 65),
	}
	for _, fk := range forbiddenKeys {
		span.SetAttribute(fk, "leak_attempt")
		span.SetAttribute(fk, 123)
	}

	// 2. Raw byte slices (must be safely filtered out)
	span.SetAttribute("custom.raw_bytes", []byte{1, 2, 3, 4})
	span.SetAttribute("audio_chunk", []byte{0xDE, 0xAD, 0xBE, 0xEF})
	span.SetAttribute("custom.nested_bytes", map[string]any{"bytes": []byte{5, 6, 7}})
	span.SetAttribute("custom.slice_bytes", []any{[]byte{8, 9}})

	// 3. Audio-like objects (must be safely filtered out)
	span.SetAttribute("audio", "audio/raw")
	span.SetAttribute("custom.audio_buffer", []any{1, 2, 3})
	span.SetAttribute("custom.nested_audio", map[string]any{"audio_samples": 16000})

	// 4. Oversized and unsupported objects
	// Map with > 64 elements
	bigMap := make(map[string]any)
	for i := 0; i < 65; i++ {
		bigMap[fmt.Sprintf("k%d", i)] = i
	}
	span.SetAttribute("custom.big_map", bigMap)

	// Slice with > 64 elements
	bigSlice := make([]any, 65)
	for i := 0; i < 65; i++ {
		bigSlice[i] = i
	}
	span.SetAttribute("custom.big_slice", bigSlice)

	// Deeply nested map (> 8 levels)
	deepMap := map[string]any{"leaf": 1}
	for i := 0; i < 10; i++ {
		deepMap = map[string]any{"next": deepMap}
	}
	span.SetAttribute("custom.deep_map", deepMap)

	// Node budget exceeded (> 1024 nodes)
	var manyNodes []any
	for i := 0; i < 20; i++ {
		sub := make(map[string]any)
		for j := 0; j < 60; j++ {
			sub[fmt.Sprintf("f%d", j)] = j
		}
		manyNodes = append(manyNodes, sub)
	}
	span.SetAttribute("custom.many_nodes", manyNodes)

	// Oversized string in whitelisted metadata label (> 256 bytes)
	span.SetAttribute("model", strings.Repeat("m", 300))

	// Non-finite floats
	span.SetAttribute("latency_ms", math.NaN())
	span.SetAttribute("duration_ms", math.Inf(1))

	// Unsupported types (channels, funcs, structs)
	span.SetAttribute("custom.ch", make(chan int))
	span.SetAttribute("custom.fn", func() {})
	span.SetAttribute("custom.struct_val", struct{ A int }{A: 1})

	// 5. Allowed valid metadata (must be preserved)
	span.SetAttribute("status", "ok")
	span.SetAttribute("count", 99)
	span.SetAttribute("latency_ms", 12.34)
	span.SetAttribute("custom.metric", 100)
	span.SetAttribute("custom.flag", true)
	span.SetAttribute("custom.nested_valid", map[string]any{
		"sub_count": 5,
		"is_active": false,
	})

	span.End(nil)

	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}

	spans, err := s.Traces().Spans(ctx, sessID, "")
	if err != nil {
		t.Fatalf("query spans: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	attrs := spans[0].AttributesJSON

	// Verify that forbidden markers and leak attempts are absent
	forbiddenSubstrings := []string{
		"leak_attempt",
		"transcript",
		"prompt",
		"argument",
		"result",
		"text",
		"secret",
		"token",
		"password",
		"credential",
		"api_key",
		"audio",
		"raw_bytes",
		"nested_bytes",
		"slice_bytes",
		"audio_chunk",
		"big_map",
		"big_slice",
		"deep_map",
		"many_nodes",
		strings.Repeat("m", 300),
		"NaN",
		"+Inf",
	}
	for _, sub := range forbiddenSubstrings {
		if strings.Contains(attrs, sub) {
			t.Errorf("forbidden substring %q leaked into span attributes: %s", sub, attrs)
		}
	}

	// Verify allowed metadata is present
	expectedSubstrings := []string{
		`"status":"ok"`,
		`"count":99`,
		`"latency_ms":12.34`,
		`"custom.metric":100`,
		`"custom.flag":true`,
		`"custom.nested_valid"`,
		`"sub_count":5`,
		`"is_active":false`,
	}
	for _, exp := range expectedSubstrings {
		if !strings.Contains(attrs, exp) {
			t.Errorf("expected metadata %q missing from attributes: %s", exp, attrs)
		}
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
