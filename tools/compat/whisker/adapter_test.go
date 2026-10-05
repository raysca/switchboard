package whisker

import (
	"testing"
	"time"

	"govoice/core"
)

func TestAdapterProducesWhiskerSemanticEventWithCopiedMetadata(t *testing.T) {
	attrs := map[string]any{"custom.phase": map[string]any{"ok": true}}
	in := core.NewDebugEvent("turn.started", time.Unix(1, 0), "s1", "t1", 4, attrs)
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
	if _, err := (Adapter{}).Adapt(core.DebugEvent{Name: "", Time: time.Time{}}); err == nil {
		t.Fatal("malformed event accepted")
	}
}
