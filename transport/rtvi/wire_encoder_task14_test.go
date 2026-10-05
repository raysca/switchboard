package rtvi

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"govoice/core"
)

func TestDebugEventWireEncoderPreservesRTVISemanticsAndCorrelation(t *testing.T) {
	event := core.NewDebugEvent("user.speech.started", time.Unix(12, 0), "session-1", "turn-2", 3, map[string]any{"source": "vad"})
	event.Direction = core.Downstream
	event.FrameKind = core.FrameUserStartedSpeaking
	event.Sequence = 7
	message, err := EncodeDebugEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	if message.Label != LabelRTVI || message.Type != TypeUserStartedSpeaking {
		t.Fatalf("unexpected wire event: %#v", message)
	}
	var data map[string]any
	if err := json.Unmarshal(message.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["session_id"] != "session-1" || data["turn_id"] != "turn-2" || data["generation"] != float64(3) || data["sequence"] != float64(7) {
		t.Fatalf("correlation missing from wire data: %#v", data)
	}
}

func TestDebugEventWireEncoderMapsAdmittedSafeErrorCategory(t *testing.T) {
	for _, name := range []string{"session.error", "provider.error"} {
		e := core.NewDebugEvent(name, time.Unix(12, 0), "s", "t", 1, map[string]any{"error.category": "provider_unavailable", "fatal": true})
		msg, err := EncodeDebugEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		var payload ErrorData
		if err := json.Unmarshal(msg.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Error != "provider_unavailable" || payload.Fatal != (name == "session.error") {
			t.Fatalf("unsafe or missing error payload: %#v", payload)
		}
	}
}

func TestDebugEventWireEncoderRejectsInvalidIdentifiersAndDirection(t *testing.T) {
	base := core.NewDebugEvent("user.speech.started", time.Unix(1, 0), "s", "t", 1, nil)
	cases := map[string]func(*core.DebugEvent){
		"name":      func(e *core.DebugEvent) { e.Name = strings.Repeat("n", 257) },
		"session":   func(e *core.DebugEvent) { e.SessionID = core.SessionID(strings.Repeat("s", 257)) },
		"turn":      func(e *core.DebugEvent) { e.TurnID = core.TurnID("\xff") },
		"pipeline":  func(e *core.DebugEvent) { e.PipelineID = strings.Repeat("p", 257) },
		"processor": func(e *core.DebugEvent) { e.Processor = "\xff" },
		"tool":      func(e *core.DebugEvent) { e.ToolCallID = strings.Repeat("x", 257) },
		"index":     func(e *core.DebugEvent) { e.ProcessorIndex = -1 },
		"direction": func(e *core.DebugEvent) { e.Direction = core.Direction(255) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := base
			mutate(&e)
			if _, err := EncodeDebugEvent(e); err == nil {
				t.Fatal("invalid event accepted")
			}
		})
	}
}

func TestDebugEventWireEncoderRejectsUnknownSemanticEvent(t *testing.T) {
	_, err := EncodeDebugEvent(core.NewDebugEvent("unknown.event", time.Time{}, "", "", 0, nil))
	if err == nil {
		t.Fatal("unknown event was silently encoded")
	}
}

func TestDebugEventWireEncoderRejectsContentNotPresentUnderDebugPrivacyPolicy(t *testing.T) {
	_, err := EncodeDebugEvent(core.NewDebugEvent("user.transcription.final", time.Unix(12, 0), "s", "t", 1, map[string]any{"text": "secret"}))
	if !errors.Is(err, ErrMissingDebugPayload) {
		t.Fatalf("expected explicit missing-content error, got %v", err)
	}
}
