package core_test

import (
	"context"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/tools"
)

func TestIdentityDistinctAndStable(t *testing.T) {
	types := []reflect.Type{reflect.TypeOf(core.SessionID("s")), reflect.TypeOf(core.TurnID("t")), reflect.TypeOf(core.Generation(1))}
	for i, typ := range types {
		if typ.PkgPath() != "govoice/core" || typ.Name() == "" {
			t.Fatalf("identity %d must remain a core-defined non-alias type: %v", i, typ)
		}
		for j := 0; j < i; j++ {
			if typ == types[j] {
				t.Fatalf("identities %d and %d must remain distinct", i, j)
			}
		}
	}
	turn := core.TurnID("turn-1")
	gen := core.Generation(1)
	gen++
	if turn != "turn-1" || gen != 2 {
		t.Fatal("generation advancement must not change turn identity")
	}
}

type contractSpan struct{ label string }

func (*contractSpan) SetAttribute(string, any) {}
func (*contractSpan) AddEvent(core.TraceEvent) {}
func (*contractSpan) End(error)                {}

func TestTraceContractParentSelection(t *testing.T) {
	parent := &contractSpan{label: "parent"}
	child := &contractSpan{label: "child"}
	root := context.Background()
	parentContext := core.ContextWithSpan(root, parent)
	childContext := core.ContextWithSpan(parentContext, child)
	if core.SpanFromContext(root) != nil || core.SpanFromContext(parentContext) != parent || core.SpanFromContext(childContext) != child {
		t.Fatal("nested context must select nearest span without mutating parent context")
	}
}

func TestTraceContractNoop(t *testing.T) {
	var tracer core.Tracer = core.NoopTracer{}
	parent := tracer.StartSpan(context.Background(), core.TraceEvent{Name: "turn.start"})
	ctx := core.ContextWithSpan(context.Background(), parent)
	child := tracer.StartSpan(ctx, core.TraceEvent{Name: "stt.start"})
	if parent == nil || child == nil || core.SpanFromContext(ctx) != parent {
		t.Fatal("disabled tracing must supply reusable non-nil spans and context parentage")
	}
	child.SetAttribute("model", "safe")
	child.AddEvent(core.TraceEvent{Name: "stt.final"})
	child.End(nil)
	parent.End(nil)
}

func TestTraceContractStructuredCustomMetadata(t *testing.T) {
	data := map[string]any{"scores": []any{0.2, 0.8}, "counts": map[string]any{"frames": 3}}
	event := core.NewTraceEvent("custom.vad.decision", time.Now(), "s", "t", 1, core.TracePolicy{}, map[string]any{
		"custom.vad":    data,
		"custom.unsafe": map[string]any{"transcript": "unique-private-text"},
	})
	data["counts"].(map[string]any)["frames"] = 999
	data["scores"].([]any)[0] = 999.0
	attrs := event.Attributes()
	if _, ok := attrs["custom.unsafe"]; ok {
		t.Fatal("custom text entered metadata envelope")
	}
	value, ok := attrs["custom.vad"]
	if !ok {
		t.Fatal("structured custom metadata was dropped")
	}
	got := value.(map[string]any)
	if got["counts"].(map[string]any)["frames"] != 3 || got["scores"].([]any)[0] != 0.2 {
		t.Fatalf("nested input aliased: %#v", got)
	}
	got["scores"].([]any)[0] = 999.0
	if event.Attributes()["custom.vad"].(map[string]any)["scores"].([]any)[0] != 0.2 {
		t.Fatal("nested accessor aliased event")
	}
}

func TestTraceContractCustomMetadataRejectsInnocentStringsEverywhere(t *testing.T) {
	const innocent = "ordinary-label-that-is-not-a-secret"
	attrs := map[string]any{"custom.vad": map[string]any{"label": innocent, "score": 0.8}}
	trace := core.NewTraceEvent("custom.vad", time.Now(), "s", "t", 1, core.TracePolicy{}, attrs)
	observer := core.NewObserverEvent("custom.vad", time.Now(), "s", "t", 1, core.FrameInputAudio, core.Downstream, "vad", 0, attrs)
	debug := core.NewDebugEvent("custom.vad", time.Now(), "s", "t", 1, attrs)
	if len(trace.Attributes()) != 0 || len(observer.Attributes()) != 0 || len(debug.Attributes()) != 0 {
		t.Fatalf("custom string content must be rejected: trace=%#v observer=%#v debug=%#v", trace.Attributes(), observer.Attributes(), debug.Attributes())
	}
}

func TestTraceContractConcurrentToolCorrelation(t *testing.T) {
	first := core.NewTraceEvent("tool.start", time.Now(), "s", "t", 2, core.TracePolicy{}, nil)
	first.ToolCallID = "call-a"
	second := first
	second.ToolCallID = "call-b"
	if first.ToolCallID != "call-a" || second.ToolCallID != "call-b" || first.TurnID != second.TurnID {
		t.Fatal("concurrent tool events lost distinct call identity")
	}
}

func TestTraceContractCustomMetadataWorkBudget(t *testing.T) {
	shared := any(map[string]any{"value": true})
	for range 7 {
		shared = map[string]any{"a": shared, "b": shared, "c": shared, "d": shared}
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	event := core.NewTraceEvent("custom.vad.decision", time.Now(), "s", "t", 1, core.TracePolicy{}, map[string]any{
		"custom.c_shared": shared,
		"custom.b_cycle":  cycle,
		"custom.a_valid":  map[string]any{"confidence": 0.8},
	})
	attrs := event.Attributes()
	if _, ok := attrs["custom.c_shared"]; ok {
		t.Fatal("high-expansion shared subtree was admitted")
	}
	if _, ok := attrs["custom.b_cycle"]; ok {
		t.Fatal("cyclic metadata was admitted")
	}
	if attrs["custom.a_valid"].(map[string]any)["confidence"] != 0.8 {
		t.Fatal("valid metadata was lost")
	}
}

func TestTraceContractCustomMetadataKeyBudget(t *testing.T) {
	input := make(map[string]any)
	for i := range 1025 {
		input["custom."+strconv.Itoa(i)] = true
	}
	event := core.NewTraceEvent("custom.counts", time.Now(), "s", "t", 1, core.TracePolicy{}, input)
	if len(event.Attributes()) != 0 {
		t.Fatal("too many custom keys should fail closed")
	}
}

func TestTraceContractPrivacyEnvelope(t *testing.T) {
	const secret = "unique-secret-4782"
	attrs := map[string]any{"model": "safe-model", "transcript": secret, "api_key": secret, "audio": []byte(secret), "tool.arguments": secret}
	event := core.NewTraceEvent("stt.final", time.Now(), "session-1", "turn-1", 2, core.TracePolicy{}, attrs)
	attrs["model"] = "changed"
	got := event.Attributes()
	if got["model"] != "safe-model" || len(got) != 1 {
		t.Fatalf("metadata envelope leaked content or aliased input: %#v", got)
	}
	got["model"] = secret
	if event.Attributes()["model"] != "safe-model" {
		t.Fatal("trace attributes are mutable through accessor")
	}
	textEvent := core.NewTraceEvent("stt.final", time.Now(), "s", "t", 1, core.TracePolicy{Content: core.TraceText}, attrs)
	if textEvent.Attributes()["transcript"] != secret || textEvent.Attributes()["tool.arguments"] != nil {
		t.Fatal("text mode must allow selected text but require explicit tool redaction")
	}
	redacted := core.NewTraceEvent("tool.complete", time.Now(), "s", "t", 1, core.TracePolicy{Content: core.TraceText, RedactToolContent: func(string) string { return "[redacted]" }}, attrs)
	if redacted.Attributes()["tool.arguments"] != "[redacted]" {
		t.Fatal("tool content did not pass explicit redaction")
	}
	invalid := core.NewTraceEvent("stt.final", time.Now(), "s", "t", 1, core.TracePolicy{Content: "unexpected"}, attrs)
	if len(invalid.Attributes()) != 0 {
		t.Fatal("unknown content mode must fail closed")
	}
}

func TestTraceContractRedactorPanicFailsClosed(t *testing.T) {
	const secret = "unique-tool-secret-914"
	event := core.NewTraceEvent("tool.complete", time.Now(), "s", "t", 1, core.TracePolicy{
		Content:           core.TraceText,
		RedactToolContent: func(string) string { panic(secret) },
	}, map[string]any{"tool.result": secret, "status": "ok"})
	attrs := event.Attributes()
	if _, ok := attrs["tool.result"]; ok || attrs["status"] != "ok" {
		t.Fatalf("redactor panic must drop only sensitive content: %#v", attrs)
	}
}

func TestTraceContractTextAndScalarValuesAreBoundedAndFinite(t *testing.T) {
	const secret = "finite-check-secret"
	event := core.NewTraceEvent("llm.complete", time.Now(), "s", "t", 1, core.TracePolicy{Content: core.TraceText, MaxTextBytes: 4}, map[string]any{
		"text":           "too-long:" + secret,
		"status":         strings.Repeat("x", 257),
		"latency_ms":     math.NaN(),
		"duration_ms":    math.Inf(1),
		"sample_rate":    16000,
		"transcript":     "ok",
		"tool.arguments": secret,
	})
	attrs := event.Attributes()
	if _, ok := attrs["text"]; ok || attrs["status"] != nil || attrs["latency_ms"] != nil || attrs["duration_ms"] != nil || attrs["tool.arguments"] != nil || attrs["sample_rate"] != 16000 || attrs["transcript"] != "ok" {
		t.Fatalf("trace bounded-value policy failed: %#v", attrs)
	}
}

func TestObserverContractImmutableValue(t *testing.T) {
	attrs := map[string]any{"sample_rate": 16000, "transcript": "hidden"}
	event := core.NewObserverEvent("frame.flow", time.Now(), "s", "t", 3, core.FrameInputAudio, core.Downstream, "vad", 2, attrs)
	attrs["sample_rate"] = 8000
	got := event.Attributes()
	if got["sample_rate"] != 16000 || got["transcript"] != nil {
		t.Fatalf("observer event leaked or aliased content: %#v", got)
	}
	got["sample_rate"] = 1
	if event.Attributes()["sample_rate"] != 16000 {
		t.Fatal("observer accessor exposes mutable state")
	}
	var _ core.Observer = core.ObserverFunc(func(core.ObserverEvent) {})
}

func TestDebugContractImmutableValue(t *testing.T) {
	attrs := map[string]any{"status": "ready", "prompt": "hidden"}
	event := core.NewDebugEvent("provider.ready", time.Now(), "s", "t", 1, attrs)
	attrs["status"] = "changed"
	got := event.Attributes()
	if got["status"] != "ready" || got["prompt"] != nil {
		t.Fatalf("debug event leaked or aliased content: %#v", got)
	}
	got["status"] = "changed"
	if event.Attributes()["status"] != "ready" {
		t.Fatal("debug accessor exposes mutable state")
	}
}

func TestDebugContractFlowAndToolCorrelation(t *testing.T) {
	first := core.NewDebugEvent("frame.flow", time.Now(), "s", "t", 2, nil)
	first.PipelineID = "voice-pipeline"
	first.Processor = "vad"
	first.ProcessorIndex = 1
	first.FrameKind = core.FrameInputAudio
	first.Direction = core.Downstream
	first.Sequence = 42
	first.ToolCallID = "call-a"
	second := first
	second.ToolCallID = "call-b"
	if first.PipelineID != "voice-pipeline" || first.ProcessorIndex != 1 || first.FrameKind != core.FrameInputAudio || first.Direction != core.Downstream || first.Sequence != 42 || first.ToolCallID != "call-a" || second.ToolCallID != "call-b" {
		t.Fatal("debug values lost frame, processor, order, or tool correlation")
	}
}

func TestToolContractSignatures(t *testing.T) {
	var _ tools.ToolExecutor = tools.ToolExecutorFunc(func(context.Context, tools.ToolCall) (tools.ToolResult, error) { return tools.ToolResult{}, nil })
	call := tools.ToolCall{ID: "call-1", Name: "lookup", Arguments: []byte(`{"city":"London"}`), SessionID: "s", TurnID: "t", Generation: 2}
	definition := tools.ToolDefinition{Name: "lookup", Description: "Find data", InputSchema: []byte(`{"type":"object"}`)}
	result := tools.ToolResult{CallID: call.ID, Status: tools.ToolSucceeded, StartedAt: time.Now(), EndedAt: time.Now()}
	if call.TurnID != core.TurnID("t") || definition.Name != "lookup" || result.CallID != call.ID {
		t.Fatal("tool contracts lost correlation")
	}
}
