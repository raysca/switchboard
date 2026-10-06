package rtvi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"govoice/core"
)

const (
	ProtocolVersion = "2.1.0"
	LabelRTVI       = "rtvi-ai"

	TypeClientReady         = "client-ready"
	TypeBotReady            = "bot-ready"
	TypeUserStartedSpeaking = "user-started-speaking"
	TypeUserStoppedSpeaking = "user-stopped-speaking"
	TypeUserTranscription   = "user-transcription"
	TypeBotLLMText          = "bot-llm-text"
	TypeBotText             = "bot-llm-text"    // Alias for TypeBotLLMText
	TypeBotLLMStopped       = "bot-llm-stopped" // the reply text for a turn is complete
	TypeBotStartedSpeaking  = "bot-started-speaking"
	TypeBotStoppedSpeaking  = "bot-stopped-speaking"
	TypeError               = "error"
	TypeDisconnectBot       = "disconnect-bot"
	TypeSendText            = "send-text"      // client text turn: {"content": "..."}
	TypeDisconnect          = "disconnect-bot" // Alias for TypeDisconnectBot
)

var (
	ErrIncompatibleProtocol = errors.New("rtvi: incompatible client protocol version")
	ErrInvalidRTVIMessage   = errors.New("rtvi: invalid or malformed RTVI message")
	// ErrRejectedMessage marks a well-formed client message the bot refuses;
	// the connection stays open and the client receives a non-fatal error.
	ErrRejectedMessage = errors.New("rtvi: message rejected")
	ErrNotReady        = errors.New("rtvi: client sent audio/data before client-ready handshake")
)

// Message is the standard RTVI envelope structure.
type Message struct {
	ID    string          `json:"id,omitempty"`
	Label string          `json:"label"`
	Type  string          `json:"type"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// ClientReadyData represents payload for client-ready.
type ClientReadyData struct {
	Version string           `json:"version"`
	About   *json.RawMessage `json:"about,omitempty"`
}

// BotReadyData represents payload for bot-ready.
type BotReadyData struct {
	Version string   `json:"version"`
	Config  []string `json:"config,omitempty"`
}

// TranscriptionData represents user transcription payloads matching @pipecat-ai/client-js TranscriptData.
type TranscriptionData struct {
	Text      string `json:"text"`
	Final     bool   `json:"final"`
	Timestamp string `json:"timestamp,omitempty"`
	UserID    string `json:"user_id,omitempty"`
}

// BotTextData represents assistant text chunks.
type BotTextData struct {
	Text string `json:"text"`
}

// ErrorData represents safe error notifications.
type ErrorData struct {
	Error string `json:"error"`
	Fatal bool   `json:"fatal"`
}

// Handler manages RTVI protocol handshake, event conversion, and message serialization.
type Handler struct {
	idCounter atomic.Uint64
	ready     atomic.Bool

	OnClientReady func(version string) error
	OnDisconnect  func()
	// OnSendText receives a validated client text turn.
	OnSendText func(content string) error
}

// MaxSendTextBytes bounds one client text turn.
const MaxSendTextBytes = 4096

// SendTextData is the payload of a send-text client message.
type SendTextData struct {
	Content string `json:"content"`
}

// NewHandler initializes a new RTVI Handler.
func NewHandler() *Handler {
	return &Handler{}
}

// IsReady returns true if client-ready handshake succeeded.
func (h *Handler) IsReady() bool {
	return h.ready.Load()
}

// HandleIncomingJSON parses an incoming RTVI JSON string and handles protocol events.
func (h *Handler) HandleIncomingJSON(rawJSON string) (core.Frame, error) {
	var msg Message
	if err := json.Unmarshal([]byte(rawJSON), &msg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRTVIMessage, err)
	}

	if msg.Label != LabelRTVI {
		// Non-RTVI message, ignore or pass through
		return nil, nil
	}

	switch msg.Type {
	case TypeClientReady:
		var readyData ClientReadyData
		if len(msg.Data) > 0 {
			_ = json.Unmarshal(msg.Data, &readyData)
		}
		if readyData.Version == "" || !isCompatibleVersion(readyData.Version) {
			return nil, fmt.Errorf("%w: expected %s, got %s", ErrIncompatibleProtocol, ProtocolVersion, readyData.Version)
		}
		h.ready.Store(true)
		if h.OnClientReady != nil {
			if err := h.OnClientReady(readyData.Version); err != nil {
				return nil, err
			}
		}
		return nil, nil

	case TypeSendText:
		if !h.ready.Load() {
			return nil, fmt.Errorf("%w: send-text before client-ready", ErrRejectedMessage)
		}
		var data SendTextData
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			return nil, fmt.Errorf("%w: send-text payload is invalid", ErrRejectedMessage)
		}
		content := strings.TrimSpace(data.Content)
		if content == "" || len(content) > MaxSendTextBytes || !utf8.ValidString(content) {
			return nil, fmt.Errorf("%w: send-text content must be 1 to %d bytes of text", ErrRejectedMessage, MaxSendTextBytes)
		}
		if h.OnSendText == nil {
			return nil, fmt.Errorf("%w: text input is not supported", ErrRejectedMessage)
		}
		if err := h.OnSendText(content); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRejectedMessage, err)
		}
		return nil, nil

	case TypeDisconnectBot, "disconnect":
		if h.OnDisconnect != nil {
			h.OnDisconnect()
		}
		return &core.EndFrame{}, nil

	default:
		return nil, nil
	}
}

// FormatBotReady produces the bot-ready wire message.
func (h *Handler) FormatBotReady() (string, error) {
	data, _ := json.Marshal(BotReadyData{
		Version: ProtocolVersion,
	})
	return h.formatMessage(TypeBotReady, data)
}

// FormatUserStartedSpeaking produces the user-started-speaking wire message.
func (h *Handler) FormatUserStartedSpeaking() (string, error) {
	return h.formatMessage(TypeUserStartedSpeaking, nil)
}

// FormatUserStoppedSpeaking produces the user-stopped-speaking wire message.
func (h *Handler) FormatUserStoppedSpeaking() (string, error) {
	return h.formatMessage(TypeUserStoppedSpeaking, nil)
}

// FormatUserTranscription produces user transcription events.
func (h *Handler) FormatUserTranscription(text string, isFinal bool) (string, error) {
	data, _ := json.Marshal(TranscriptionData{
		Text:  text,
		Final: isFinal,
	})
	return h.formatMessage(TypeUserTranscription, data)
}

// FormatUserTranscriptionWithMeta produces user transcription events with timestamp and user ID.
func (h *Handler) FormatUserTranscriptionWithMeta(text string, isFinal bool, timestamp string, userID string) (string, error) {
	data, _ := json.Marshal(TranscriptionData{
		Text:      text,
		Final:     isFinal,
		Timestamp: timestamp,
		UserID:    userID,
	})
	return h.formatMessage(TypeUserTranscription, data)
}

// FormatBotLLMText produces bot-llm-text stream events matching client-js.
func (h *Handler) FormatBotLLMText(text string) (string, error) {
	data, _ := json.Marshal(BotTextData{
		Text: text,
	})
	return h.formatMessage(TypeBotLLMText, data)
}

// FormatBotLLMStopped marks the end of a reply's LLM text, so clients know a
// turn's text is complete without waiting on audio or timeouts.
func (h *Handler) FormatBotLLMStopped() (string, error) {
	return h.formatMessage(TypeBotLLMStopped, nil)
}

// FormatBotText is an alias for FormatBotLLMText.
func (h *Handler) FormatBotText(text string) (string, error) {
	return h.FormatBotLLMText(text)
}

// FormatBotStartedSpeaking produces bot-started-speaking state events.
func (h *Handler) FormatBotStartedSpeaking() (string, error) {
	return h.formatMessage(TypeBotStartedSpeaking, nil)
}

// FormatBotStoppedSpeaking produces bot-stopped-speaking state events and barge-in clear events.
func (h *Handler) FormatBotStoppedSpeaking() (string, error) {
	return h.formatMessage(TypeBotStoppedSpeaking, nil)
}

// FormatSpeakingState maps the provider-neutral, output-derived framework
// transition to the matching RTVI lifecycle event.
func (h *Handler) FormatSpeakingState(event core.SpeakingStateFrame) (string, error) {
	switch event.State {
	case core.SpeakingStarted:
		return h.FormatBotStartedSpeaking()
	case core.SpeakingStopped:
		return h.FormatBotStoppedSpeaking()
	default:
		return "", fmt.Errorf("%w: invalid speaking state %d", ErrInvalidRTVIMessage, event.State)
	}
}

// FormatError produces safe error messages.
func (h *Handler) FormatError(errStr string, fatal bool) (string, error) {
	data, _ := json.Marshal(ErrorData{
		Error: errStr,
		Fatal: fatal,
	})
	return h.formatMessage(TypeError, data)
}

func (h *Handler) formatMessage(msgType string, data json.RawMessage) (string, error) {
	msg := Message{
		ID:    fmt.Sprintf("msg-%d", h.idCounter.Add(1)),
		Label: LabelRTVI,
		Type:  msgType,
		Data:  data,
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func isCompatibleVersion(clientVer string) bool {
	return clientVer == ProtocolVersion || clientVer == "2.1"
}
