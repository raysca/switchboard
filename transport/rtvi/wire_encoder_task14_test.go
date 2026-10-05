package rtvi

import (
	"encoding/json"
	"errors"
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
