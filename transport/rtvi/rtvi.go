package rtvi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"

	"govoice/core"
	"govoice/core/audio/dtmf"
)

const (
	ProtocolVersion = "2.1.0"
	LabelRTVI       = "rtvi-ai"

	TypeClientReady         = "client-ready"
	TypeBotReady            = "bot-ready"
	TypeUserStartedSpeaking = "user-started-speaking"
	TypeUserStoppedSpeaking = "user-stopped-speaking"
	TypeUserTranscription   = "user-transcription"
	TypeBotLLMStarted       = "bot-llm-started" // first text of a reply follows
	TypeBotLLMText          = "bot-llm-text"
	TypeBotLLMStopped       = "bot-llm-stopped" // the reply text for a turn is complete
	TypeBotTTSStarted       = "bot-tts-started"
	TypeBotTTSStopped       = "bot-tts-stopped"
	TypeBotStartedSpeaking  = "bot-started-speaking"
	TypeBotStoppedSpeaking  = "bot-stopped-speaking"
	TypeBotInterrupted      = "bot-interrupted" // a barge-in cut the bot's reply off (Pipecat BotInterruptedMessage)
	TypeError               = "error"
	TypeDisconnectBot       = "disconnect-bot"
	TypeSendText            = "send-text"    // client text turn: {"content": "..."}
	TypeDTMF                = "dtmf"         // client key presses: {"buttons": ["1","#"]} (2.0.x clients: {"button": "1"})
	TypeSetMuted            = "set-muted"    // client mic mute: {"muted": true|false}; the session drops input audio while muted
	TypePushToTalk          = "push-to-talk" // {"pressed": true|false}; only on calls whose client-ready set push_to_talk

	// Function calls, at Pipecat's default function_call_report_level "none".
	TypeLLMFunctionCallStarted    = "llm-function-call-started"
	TypeLLMFunctionCallInProgress = "llm-function-call-in-progress"
	TypeLLMFunctionCallStopped    = "llm-function-call-stopped"
)

var (
	ErrIncompatibleProtocol = errors.New("rtvi: incompatible client protocol version")
	ErrInvalidRTVIMessage   = errors.New("rtvi: invalid or malformed RTVI message")
	// ErrRejectedMessage marks a well-formed client message the bot refuses;
	// the connection stays open and the client receives a non-fatal error.
	ErrRejectedMessage = errors.New("rtvi: message rejected")
	// ErrSessionSetup: client-ready was valid but the server could not start
	// the call (capacity, agent, providers). Not the client's fault.
	ErrSessionSetup = errors.New("rtvi: session setup failed")
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
	// PushToTalk: turns start and end on push-to-talk messages, not on voice detection.
	PushToTalk bool `json:"push_to_talk,omitempty"`
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

// FunctionCallInProgressData is llm-function-call-in-progress at report level
// "none": the call's id, never its name or arguments.
type FunctionCallInProgressData struct {
	ToolCallID string `json:"tool_call_id"`
}

// FunctionCallStoppedData is llm-function-call-stopped at report level "none":
// the call's id and whether it was cancelled (barge-in, a loop close or the
// loop's own call timeout; an MCP server timeout is a failed call, cancelled
// false), never its result.
type FunctionCallStoppedData struct {
	ToolCallID string `json:"tool_call_id"`
	Cancelled  bool   `json:"cancelled"`
}

// Handler manages RTVI protocol handshake, event conversion, and message serialization.
type Handler struct {
	idCounter  atomic.Uint64
	ready      atomic.Bool
	pushToTalk atomic.Bool // set once by client-ready

	OnClientReady func(version string) error
	// OnSendText receives a validated client text turn.
	OnSendText func(content string) error
	// OnDTMF receives validated client key presses (digits, in order).
	OnDTMF func(digits string) error

	sendText *rate.Limiter
	dtmf     *rate.Limiter // keys, not messages

	llmMu    sync.Mutex
	llmOpen  bool   // bot-llm-started sent and not yet stopped
	llmReply string // key of the open reply

	fnMu    sync.Mutex
	fnCalls map[string]struct{} // tool_call_ids sent as in-progress and not yet stopped
}

const (
	// MaxSendTextBytes bounds one client text turn.
	MaxSendTextBytes = 4096
	// Each text turn starts a paid LLM request: a connection may send
	// SendTextBurst turns at once, then one per SendTextInterval.
	SendTextBurst    = 3
	SendTextInterval = time.Second
	// DTMFKeysPerSecond caps client key presses (a '#' can commit an LLM turn):
	// about a human's top keypad speed, with one full send as burst.
	DTMFKeysPerSecond = 10
)

// SendTextData is the payload of a send-text client message.
type SendTextData struct {
	Content string `json:"content"`
}

// NewHandler initializes a new RTVI Handler.
func NewHandler() *Handler {
	return &Handler{
		sendText: rate.NewLimiter(rate.Every(SendTextInterval), SendTextBurst),
		dtmf:     rate.NewLimiter(DTMFKeysPerSecond, dtmf.MaxDigitsPerSend),
	}
}

// IsReady returns true if client-ready handshake succeeded.
func (h *Handler) IsReady() bool {
	return h.ready.Load()
}

// PushToTalk reports whether client-ready opted this call into push-to-talk.
func (h *Handler) PushToTalk() bool { return h.pushToTalk.Load() }

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
		if h.ready.Swap(true) {
			// Session setup runs once per connection; a repeat would leak the first session.
			return nil, fmt.Errorf("%w: duplicate client-ready", ErrRejectedMessage)
		}
		h.pushToTalk.Store(readyData.PushToTalk)
		if h.OnClientReady != nil {
			if err := h.OnClientReady(readyData.Version); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrSessionSetup, err)
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
		if !h.sendText.Allow() {
			return nil, fmt.Errorf("%w: send-text rate limit exceeded; wait and retry", ErrRejectedMessage)
		}
		if err := h.OnSendText(content); err != nil {
			// The server's reason stays server-side; the client gets a fixed message.
			return nil, fmt.Errorf("%w: text turn could not be accepted", ErrRejectedMessage)
		}
		return nil, nil

	case TypeDTMF:
		if !h.ready.Load() {
			return nil, fmt.Errorf("%w: dtmf before client-ready", ErrRejectedMessage)
		}
		var data struct {
			Buttons []string `json:"buttons"`
			Button  string   `json:"button"` // RTVI 2.0.x: one message per key
		}
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			return nil, fmt.Errorf("%w: dtmf payload is invalid", ErrRejectedMessage)
		}
		if data.Button != "" {
			data.Buttons = append(data.Buttons, data.Button)
		}
		digits := strings.Join(data.Buttons, "")
		if err := dtmf.ValidateDigits(digits); err != nil || len(digits) != len(data.Buttons) {
			return nil, fmt.Errorf("%w: dtmf must be 1 to %d keys from 0-9, *, #, A-D", ErrRejectedMessage, dtmf.MaxDigitsPerSend)
		}
		if h.OnDTMF == nil {
			return nil, fmt.Errorf("%w: dtmf input is not supported", ErrRejectedMessage)
		}
		if !h.dtmf.AllowN(time.Now(), len(digits)) {
			return nil, fmt.Errorf("%w: dtmf rate limit exceeded; wait and retry", ErrRejectedMessage)
		}
		if err := h.OnDTMF(digits); err != nil {
			return nil, fmt.Errorf("%w: dtmf could not be accepted", ErrRejectedMessage)
		}
		return nil, nil

	case TypeSetMuted:
		if !h.ready.Load() {
			return nil, fmt.Errorf("%w: set-muted before client-ready", ErrRejectedMessage)
		}
		var data struct {
			Muted *bool `json:"muted"`
		}
		if err := json.Unmarshal(msg.Data, &data); err != nil || data.Muted == nil {
			return nil, fmt.Errorf(`%w: set-muted needs {"muted": true|false}`, ErrRejectedMessage)
		}
		if *data.Muted {
			return &core.InputMuteFrame{}, nil
		}
		return &core.InputUnmuteFrame{}, nil

	case TypePushToTalk:
		if !h.ready.Load() {
			return nil, fmt.Errorf("%w: push-to-talk before client-ready", ErrRejectedMessage)
		}
		if !h.pushToTalk.Load() {
			return nil, fmt.Errorf("%w: push-to-talk is not enabled for this call", ErrRejectedMessage)
		}
		var data struct {
			Pressed *bool `json:"pressed"`
		}
		if err := json.Unmarshal(msg.Data, &data); err != nil || data.Pressed == nil {
			return nil, fmt.Errorf(`%w: push-to-talk needs {"pressed": true|false}`, ErrRejectedMessage)
		}
		return &core.PushToTalkFrame{Pressed: *data.Pressed}, nil

	case TypeDisconnectBot, "disconnect":
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

// BotLLMText returns the wire messages for one reply text delta, preceded by
// bot-llm-started when reply (any per-reply key, e.g. turn+generation) is not
// the open one. A new key also covers a reply cut off without its stop.
func (h *Handler) BotLLMText(reply, text string) []string {
	h.llmMu.Lock()
	defer h.llmMu.Unlock()
	var out []string
	if !h.llmOpen || h.llmReply != reply {
		h.llmOpen, h.llmReply = true, reply
		msg, _ := h.formatMessage(TypeBotLLMStarted, nil)
		out = append(out, msg)
	}
	msg, _ := h.FormatBotLLMText(text)
	return append(out, msg)
}

// BotLLMStopped closes the open reply. A reply with no text deltas still gets
// a started/stopped pair, as Pipecat brackets every LLM response.
func (h *Handler) BotLLMStopped(reply string) []string {
	h.llmMu.Lock()
	defer h.llmMu.Unlock()
	var out []string
	if !h.llmOpen || h.llmReply != reply {
		msg, _ := h.formatMessage(TypeBotLLMStarted, nil)
		out = append(out, msg)
	}
	h.llmOpen = false
	msg, _ := h.FormatBotLLMStopped()
	return append(out, msg)
}

// FormatBotTTSStarted / FormatBotTTSStopped bracket one synthesis unit.
func (h *Handler) FormatBotTTSStarted() (string, error) {
	return h.formatMessage(TypeBotTTSStarted, nil)
}

func (h *Handler) FormatBotTTSStopped() (string, error) {
	return h.formatMessage(TypeBotTTSStopped, nil)
}

// FormatBotStartedSpeaking produces bot-started-speaking state events.
func (h *Handler) FormatBotStartedSpeaking() (string, error) {
	return h.formatMessage(TypeBotStartedSpeaking, nil)
}

// FormatBotStoppedSpeaking produces bot-stopped-speaking state events and barge-in clear events.
func (h *Handler) FormatBotStoppedSpeaking() (string, error) {
	return h.formatMessage(TypeBotStoppedSpeaking, nil)
}

// FormatBotInterrupted produces bot-interrupted: the bot's in-flight reply was
// cut off. Pipecat's RTVIObserver sends it on an InterruptionFrame.
func (h *Handler) FormatBotInterrupted() (string, error) {
	return h.formatMessage(TypeBotInterrupted, nil)
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

// maxOpenFunctionCalls bounds the announced calls a connection remembers; a
// tool round runs at most 16 calls, so only lost results ever reach it.
const maxOpenFunctionCalls = 64

// FunctionCallStarted returns llm-function-call-started and
// llm-function-call-in-progress for one tool call. They carry only the
// call's id (Pipecat's default report level "none"): tool names, arguments
// and results can hold the caller's personal details.
func (h *Handler) FunctionCallStarted(toolCallID string) []string {
	h.fnMu.Lock()
	if h.fnCalls == nil || len(h.fnCalls) >= maxOpenFunctionCalls {
		h.fnCalls = make(map[string]struct{}) // ponytail: forgets calls whose result never came
	}
	h.fnCalls[toolCallID] = struct{}{}
	h.fnMu.Unlock()
	started, _ := h.formatMessage(TypeLLMFunctionCallStarted, json.RawMessage(`{}`))
	data, _ := json.Marshal(FunctionCallInProgressData{ToolCallID: toolCallID})
	inProgress, _ := h.formatMessage(TypeLLMFunctionCallInProgress, data)
	return []string{started, inProgress}
}

// FunctionCallStopped returns llm-function-call-stopped for a call announced
// by FunctionCallStarted, once, and nothing for any other call.
func (h *Handler) FunctionCallStopped(toolCallID string, cancelled bool) []string {
	h.fnMu.Lock()
	_, open := h.fnCalls[toolCallID]
	delete(h.fnCalls, toolCallID)
	h.fnMu.Unlock()
	if !open {
		return nil
	}
	data, _ := json.Marshal(FunctionCallStoppedData{ToolCallID: toolCallID, Cancelled: cancelled})
	msg, _ := h.formatMessage(TypeLLMFunctionCallStopped, data)
	return []string{msg}
}
