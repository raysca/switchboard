package core_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/tools"
)

func TestIdentityDistinctAndStable(t *testing.T) {
	if reflect.TypeOf(core.SessionID("s")) == reflect.TypeOf(core.TurnID("t")) {
		t.Fatal("session and turn identities must be distinct defined types")
	}
	turn := core.TurnID("turn-1")
	gen := core.Generation(1)
	gen++
	if turn != "turn-1" || gen != 2 {
		t.Fatal("generation advancement must not change turn identity")
	}
}

func TestTraceContractNoopAndParent(t *testing.T) {
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

func TestToolContractSignatures(t *testing.T) {
	var _ tools.ToolExecutor = tools.ToolExecutorFunc(func(context.Context, tools.ToolCall) (tools.ToolResult, error) { return tools.ToolResult{}, nil })
	call := tools.ToolCall{ID: "call-1", Name: "lookup", Arguments: []byte(`{"city":"London"}`), SessionID: "s", TurnID: "t", Generation: 2}
	definition := tools.ToolDefinition{Name: "lookup", Description: "Find data", InputSchema: []byte(`{"type":"object"}`)}
	result := tools.ToolResult{CallID: call.ID, Status: tools.ToolSucceeded, StartedAt: time.Now(), EndedAt: time.Now()}
	if call.TurnID != core.TurnID("t") || definition.Name != "lookup" || result.CallID != call.ID {
		t.Fatal("tool contracts lost correlation")
	}
}
