package whisker

import (
	"strings"
	"testing"
	"time"

	"govoice/core"
)

func TestAdapterProducesWhiskerSemanticEventWithCopiedMetadata(t *testing.T) {
	attrs := map[string]any{"custom.phase": map[string]any{"ok": true}}
	in := Observed{DebugEvent: core.NewDebugEvent("turn.started", time.Unix(1, 0), "s1", "t1", 4, attrs)}
	in.PipelineID = "main"
	in.Processor = "turn-manager"
	in.ProcessorIndex = 2
	out, err := (Adapter{}).Adapt(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != in.Name || out.SessionID != "s1" || out.TurnID != "t1" || out.Generation != 4 || out.PipelineID != "main" || out.ProcessorIndex != 2 {
		t.Fatalf("semantic fields not preserved: %#v", out)
	}
	first := out.Attributes()
	first["custom.phase"] = false
	if got := out.Attributes()["custom.phase"]; got == false {
		t.Fatal("adapter exposed mutable attributes")
	}
}

func TestAdapterRejectsMalformedDebugEvents(t *testing.T) {
	if _, err := (Adapter{}).Adapt(Observed{}); err == nil {
		t.Fatal("malformed event accepted")
	}
}

func TestAdapterCopiesNestedArraysAtEveryBoundary(t *testing.T) {
	input := map[string]any{"custom.nested": []any{[]any{map[string]any{"ok": true}}}}
	in := Observed{DebugEvent: core.NewDebugEvent("frame.flow", time.Unix(1, 0), "s", "t", 1, input)}
	out, err := (Adapter{}).Adapt(in)
	if err != nil {
		t.Fatal(err)
	}
	first := out.Attributes()["custom.nested"].([]any)
	first[0].([]any)[0].(map[string]any)["ok"] = false
	if got := out.Attributes()["custom.nested"].([]any)[0].([]any)[0].(map[string]any)["ok"]; got != true {
		t.Fatalf("output metadata aliased: %v", got)
	}
	input["custom.nested"].([]any)[0].([]any)[0].(map[string]any)["ok"] = false
	if got := out.Attributes()["custom.nested"].([]any)[0].([]any)[0].(map[string]any)["ok"]; got != true {
		t.Fatalf("input metadata aliased: %v", got)
	}
}

func TestAdapterRejectsInvalidIdentifiersAndDirection(t *testing.T) {
	base := Observed{DebugEvent: core.NewDebugEvent("frame.flow", time.Unix(1, 0), "s", "t", 1, nil)}
	cases := map[string]func(*Observed){
		"name":      func(e *Observed) { e.Name = strings.Repeat("n", 257) },
		"session":   func(e *Observed) { e.SessionID = core.SessionID(strings.Repeat("s", 257)) },
		"turn":      func(e *Observed) { e.TurnID = core.TurnID("\xff") },
		"pipeline":  func(e *Observed) { e.PipelineID = strings.Repeat("p", 257) },
		"processor": func(e *Observed) { e.Processor = "\xff" },
		"tool":      func(e *Observed) { e.ToolCallID = strings.Repeat("x", 257) },
		"index":     func(e *Observed) { e.ProcessorIndex = -1 },
		"direction": func(e *Observed) { e.Direction = core.Direction(255) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := base
			mutate(&e)
			if _, err := (Adapter{}).Adapt(e); err == nil {
				t.Fatal("invalid event accepted")
			}
		})
	}
}
