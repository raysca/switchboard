package rtvi

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"unicode/utf8"

	"govoice/core"
)

var (
	ErrUnsupportedDebugEvent = errors.New("rtvi: unsupported semantic debug event")
	ErrMissingDebugPayload   = errors.New("rtvi: semantic debug event does not contain required content")
)

var debugWireIDs atomic.Uint64

// EncodeDebugEvent maps only the framework events understood by the RTVI 2.1
// wire contract. It is intentionally protocol-specific, not a general serializer.
func EncodeDebugEvent(event core.DebugEvent) (Message, error) {
	if !validDebugScalar(event.Name, true) || !validDebugScalar(string(event.SessionID), false) || !validDebugScalar(string(event.TurnID), false) || !validDebugScalar(event.PipelineID, false) || !validDebugScalar(event.Processor, false) || !validDebugScalar(event.ToolCallID, false) || event.ProcessorIndex < 0 || (event.Direction != core.Downstream && event.Direction != core.Upstream) {
		return Message{}, ErrInvalidRTVIMessage
	}
	var typ string
	switch event.Name {
	case "user.speech.started":
		typ = TypeUserStartedSpeaking
	case "user.speech.stopped":
		typ = TypeUserStoppedSpeaking
	case "user.transcription.interim", "transcript.interim":
		typ = TypeUserTranscription
	case "user.transcription.final", "transcript.committed":
		typ = TypeUserTranscription
	case "assistant.text", "llm.text":
		typ = TypeBotLLMText
	case "assistant.speaking.started":
		typ = TypeBotStartedSpeaking
	case "assistant.speaking.stopped":
		typ = TypeBotStoppedSpeaking
	case "session.error", "provider.error":
		typ = TypeError
	default:
		return Message{}, fmt.Errorf("%w: %s", ErrUnsupportedDebugEvent, event.Name)
	}
	attrs := event.Attributes()
	if typ == TypeUserTranscription || typ == TypeBotLLMText {
		text, ok := attrs["text"].(string)
		if !ok || text == "" {
			return Message{}, ErrMissingDebugPayload
		}
	}
	if typ == TypeError {
		category, ok := attrs["error.category"].(string)
		if !ok || category == "" {
			return Message{}, ErrMissingDebugPayload
		}
	}
	data := make(map[string]any, len(attrs)+7)
	for key, value := range attrs {
		data[key] = value
	}
	data["session_id"], data["turn_id"], data["generation"] = event.SessionID, event.TurnID, uint64(event.Generation)
	data["sequence"], data["direction"] = event.Sequence, event.Direction.String()
	if event.ToolCallID != "" {
		data["tool_call_id"] = event.ToolCallID
	}
	if event.Time.IsZero() == false {
		data["timestamp"] = event.Time.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	if typ == TypeUserTranscription {
		data["text"], data["final"] = attrs["text"], event.Name == "user.transcription.final" || event.Name == "transcript.committed"
	}
	if typ == TypeBotLLMText {
		data["text"] = attrs["text"]
	}
	if typ == TypeError {
		data["error"] = safeErrorCategory(attrs["error.category"].(string))
		data["fatal"] = event.Name == "session.error"
	}
	b, err := json.Marshal(data)
	if err != nil {
		return Message{}, err
	}
	return Message{ID: "debug-" + fmt.Sprint(debugWireIDs.Add(1)), Label: LabelRTVI, Type: typ, Data: b}, nil
}

func validDebugScalar(s string, required bool) bool {
	return (!required || s != "") && len(s) <= 256 && utf8.ValidString(s)
}

func safeErrorCategory(category string) string {
	switch category {
	case "provider_unavailable", "provider_error", "pipeline_error", "internal_error", "timeout", "cancelled":
		return category
	default:
		return "internal_error"
	}
}
