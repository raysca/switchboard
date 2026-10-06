package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSerializer_RoundTripAndFidelity(t *testing.T) {
	serializer := DefaultJSONEventSerializer()

	origEvent := DebugEvent{
		Name:           "speech.start",
		Time:           time.Now().UTC().Truncate(time.Millisecond),
		SessionID:      SessionID("sess-123"),
		TurnID:         TurnID("turn-456"),
		Generation:     Generation(7),
		PipelineID:     "pipe-main",
		Processor:      "google.STT",
		ProcessorIndex: 1,
		FrameKind:      FrameUserStartedSpeaking,
		Direction:      Downstream,
		Sequence:       42,
		ToolCallID:     "call-abc",
		attributes: safeEventAttributes(TracePolicy{}, map[string]any{
			"latency_ms":        45.5,
			"custom.confidence": 0.95,
			"custom.active":     true,
		}),
	}

	encoded, err := serializer.Encode(origEvent)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	decodedRaw, err := serializer.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	decoded, ok := decodedRaw.(DebugEvent)
	if !ok {
		t.Fatalf("expected DebugEvent, got %T", decodedRaw)
	}

	if decoded.Name != origEvent.Name {
		t.Fatalf("name mismatch: %s vs %s", decoded.Name, origEvent.Name)
	}
	if decoded.SessionID != origEvent.SessionID || decoded.TurnID != origEvent.TurnID || decoded.Generation != origEvent.Generation {
		t.Fatalf("correlation mismatch: %+v vs %+v", decoded, origEvent)
	}
	if decoded.Sequence != origEvent.Sequence || decoded.ProcessorIndex != origEvent.ProcessorIndex || decoded.Direction != origEvent.Direction {
		t.Fatalf("lifecycle mismatch: %+v vs %+v", decoded, origEvent)
	}
	if decoded.ToolCallID != origEvent.ToolCallID {
		t.Fatalf("tool call id mismatch: %s vs %s", decoded.ToolCallID, origEvent.ToolCallID)
	}
	attrs := decoded.Attributes()
	if attrs["latency_ms"] != 45.5 || attrs["custom.confidence"] != 0.95 || attrs["custom.active"] != true {
		t.Fatalf("attributes mismatch: %+v", attrs)
	}
}

func TestSerializer_UnknownFieldsRejection(t *testing.T) {
	serializer := DefaultJSONEventSerializer()
	serializer.DisallowUnknownFields = true

	// Payload containing an injected unknown property
	payloadWithUnknown := []byte(`{"name":"test.event","unknown_field":"malicious_data"}`)

	_, err := serializer.Decode(payloadWithUnknown)
	if err == nil {
		t.Fatal("expected decode failure on unknown field, got nil")
	}
	if !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("expected ErrMalformedPayload, got %v", err)
	}
}

func TestSerializer_MalformedInput(t *testing.T) {
	serializer := DefaultJSONEventSerializer()

	malformedJSON := []byte(`{"name": 12345, "invalid": [}`)
	_, err := serializer.Decode(malformedJSON)
	if err == nil {
		t.Fatal("expected decode failure on malformed JSON")
	}
	if !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("expected ErrMalformedPayload, got %v", err)
	}
}

func TestSerializer_PayloadSizeBound(t *testing.T) {
	serializer := DefaultJSONEventSerializer()
	serializer.MaxPayloadBytes = 100

	oversizedPayload := []byte(`{"name":"` + strings.Repeat("a", 200) + `"}`)
	_, err := serializer.Decode(oversizedPayload)
	if err == nil {
		t.Fatal("expected failure on payload exceeding bound")
	}
	if !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("expected ErrMalformedPayload, got %v", err)
	}
}

func TestSerializer_UnsupportedType(t *testing.T) {
	serializer := DefaultJSONEventSerializer()

	_, err := serializer.Encode(struct{ Incompatible int }{42})
	if err == nil {
		t.Fatal("expected error encoding unsupported type")
	}
	if !errors.Is(err, ErrUnsupportedSerialization) {
		t.Fatalf("expected ErrUnsupportedSerialization, got %v", err)
	}
}
