package rtvi_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/transport/rtvi"
)

func TestRTVI_ClientReadySuccess(t *testing.T) {
	h := rtvi.NewHandler()
	var callbackVersion string
	h.OnClientReady = func(v string) error {
		callbackVersion = v
		return nil
	}

	rawJSON := `{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`
	frame, err := h.HandleIncomingJSON(rawJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frame != nil {
		t.Fatalf("expected nil frame for client-ready, got %v", frame)
	}
	if !h.IsReady() {
		t.Fatalf("expected handler to be ready")
	}
	if callbackVersion != "2.1.0" {
		t.Fatalf("expected callback version 2.1.0, got %s", callbackVersion)
	}
}

func TestRTVI_ClientReadyIncompatibleVersion(t *testing.T) {
	h := rtvi.NewHandler()
	rawJSON := `{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"1.0.0"}}`
	_, err := h.HandleIncomingJSON(rawJSON)
	if err == nil {
		t.Fatalf("expected error for incompatible version, got nil")
	}
	if !errors.Is(err, rtvi.ErrIncompatibleProtocol) {
		t.Fatalf("expected ErrIncompatibleProtocol, got: %v", err)
	}
	if h.IsReady() {
		t.Fatalf("expected handler to not be ready")
	}
}

func TestRTVI_Disconnect(t *testing.T) {
	h := rtvi.NewHandler()
	var disconnected bool
	h.OnDisconnect = func() {
		disconnected = true
	}

	rawJSON := `{"id":"2","label":"rtvi-ai","type":"disconnect-bot"}`
	frame, err := h.HandleIncomingJSON(rawJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !disconnected {
		t.Fatalf("expected OnDisconnect to be called")
	}
	if _, ok := frame.(*core.EndFrame); !ok {
		t.Fatalf("expected EndFrame, got %T", frame)
	}
}

func TestRTVI_MalformedJSON(t *testing.T) {
	h := rtvi.NewHandler()
	_, err := h.HandleIncomingJSON(`{invalid-json`)
	if err == nil {
		t.Fatalf("expected error for malformed json")
	}
	if !errors.Is(err, rtvi.ErrInvalidRTVIMessage) {
		t.Fatalf("expected ErrInvalidRTVIMessage, got: %v", err)
	}
}

func TestRTVI_NonRTVIMessage(t *testing.T) {
	h := rtvi.NewHandler()
	frame, err := h.HandleIncomingJSON(`{"label":"other-proto","type":"hello"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frame != nil {
		t.Fatalf("expected nil frame for non-RTVI message")
	}
}

func TestRTVI_FormatMessages(t *testing.T) {
	h := rtvi.NewHandler()

	// 1. Bot Ready
	botReadyStr, err := h.FormatBotReady()
	if err != nil {
		t.Fatalf("FormatBotReady error: %v", err)
	}
	var msg rtvi.Message
	if err := json.Unmarshal([]byte(botReadyStr), &msg); err != nil {
		t.Fatalf("Unmarshal botReady error: %v", err)
	}
	if msg.Type != rtvi.TypeBotReady || msg.Label != rtvi.LabelRTVI {
		t.Fatalf("Unexpected botReady message: %v", msg)
	}
	var readyData rtvi.BotReadyData
	if err := json.Unmarshal(msg.Data, &readyData); err != nil || readyData.Version != "2.1.0" {
		t.Fatalf("Unexpected readyData: %v", readyData)
	}

	// 2. User Speaking State
	startedStr, err := h.FormatUserStartedSpeaking()
	if err != nil {
		t.Fatalf("FormatUserStartedSpeaking error: %v", err)
	}
	if err := json.Unmarshal([]byte(startedStr), &msg); err != nil || msg.Type != rtvi.TypeUserStartedSpeaking {
		t.Fatalf("Unexpected startedStr: %v", startedStr)
	}

	stoppedStr, err := h.FormatUserStoppedSpeaking()
	if err != nil {
		t.Fatalf("FormatUserStoppedSpeaking error: %v", err)
	}
	if err := json.Unmarshal([]byte(stoppedStr), &msg); err != nil || msg.Type != rtvi.TypeUserStoppedSpeaking {
		t.Fatalf("Unexpected stoppedStr: %v", stoppedStr)
	}

	// 3. User Transcription
	transcriptStr, err := h.FormatUserTranscriptionWithMeta("hello world", true, "2026-09-27T10:00:00Z", "user-1")
	if err != nil {
		t.Fatalf("FormatUserTranscription error: %v", err)
	}
	if err := json.Unmarshal([]byte(transcriptStr), &msg); err != nil || msg.Type != rtvi.TypeUserTranscription {
		t.Fatalf("Unexpected transcriptStr: %v", transcriptStr)
	}
	var txData rtvi.TranscriptionData
	if err := json.Unmarshal(msg.Data, &txData); err != nil || txData.Text != "hello world" || !txData.Final || txData.UserID != "user-1" {
		t.Fatalf("Unexpected txData: %v", txData)
	}

	// 4. Bot Text
	botTextStr, err := h.FormatBotLLMText("hello back")
	if err != nil {
		t.Fatalf("FormatBotLLMText error: %v", err)
	}
	if err := json.Unmarshal([]byte(botTextStr), &msg); err != nil || msg.Type != rtvi.TypeBotLLMText {
		t.Fatalf("Unexpected botTextStr: %v", botTextStr)
	}

	// 5. Bot Speaking State
	botStartedStr, err := h.FormatBotStartedSpeaking()
	if err != nil {
		t.Fatalf("FormatBotStartedSpeaking error: %v", err)
	}
	if err := json.Unmarshal([]byte(botStartedStr), &msg); err != nil || msg.Type != rtvi.TypeBotStartedSpeaking {
		t.Fatalf("Unexpected botStartedStr: %v", botStartedStr)
	}

	botStoppedStr, err := h.FormatBotStoppedSpeaking()
	if err != nil {
		t.Fatalf("FormatBotStoppedSpeaking error: %v", err)
	}
	if err := json.Unmarshal([]byte(botStoppedStr), &msg); err != nil || msg.Type != rtvi.TypeBotStoppedSpeaking {
		t.Fatalf("Unexpected botStoppedStr: %v", botStoppedStr)
	}

	// 6. Error
	errStr, err := h.FormatError("something went wrong", true)
	if err != nil {
		t.Fatalf("FormatError error: %v", err)
	}
	if err := json.Unmarshal([]byte(errStr), &msg); err != nil || msg.Type != rtvi.TypeError {
		t.Fatalf("Unexpected errStr: %v", errStr)
	}
	var errData rtvi.ErrorData
	if err := json.Unmarshal(msg.Data, &errData); err != nil || errData.Error != "something went wrong" || !errData.Fatal {
		t.Fatalf("Unexpected errData: %v", errData)
	}
}

func TestSendText(t *testing.T) {
	h := rtvi.NewHandler()
	var got []string
	h.OnSendText = func(c string) error { got = append(got, c); return nil }
	msg := func(content string) string {
		b, _ := json.Marshal(map[string]any{"id": "1", "label": rtvi.LabelRTVI, "type": rtvi.TypeSendText, "data": map[string]string{"content": content}})
		return string(b)
	}
	if _, err := h.HandleIncomingJSON(msg("hi")); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("send-text before client-ready: %v", err)
	}
	if _, err := h.HandleIncomingJSON(`{"id":"r","label":"rtvi-ai","type":"client-ready","data":{"version":"` + rtvi.ProtocolVersion + `"}}`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", rtvi.MaxSendTextBytes+1)} {
		if _, err := h.HandleIncomingJSON(msg(bad)); !errors.Is(err, rtvi.ErrRejectedMessage) {
			t.Fatalf("content %q accepted: %v", bad[:min(len(bad), 10)], err)
		}
	}
	if _, err := h.HandleIncomingJSON(msg("  What is two plus two?  ")); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "What is two plus two?" {
		t.Fatalf("delivered %q", got)
	}
	h.OnSendText = func(string) error { return errors.New("no active session") }
	if _, err := h.HandleIncomingJSON(msg("hello")); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("callback error must be a rejection: %v", err)
	}
}

// A connection may send a short burst of text turns, then is throttled without
// being disconnected; throttled turns never reach the LLM.
func TestSendTextRateLimit(t *testing.T) {
	h := rtvi.NewHandler()
	delivered := 0
	h.OnSendText = func(string) error { delivered++; return nil }
	if _, err := h.HandleIncomingJSON(`{"id":"r","label":"rtvi-ai","type":"client-ready","data":{"version":"` + rtvi.ProtocolVersion + `"}}`); err != nil {
		t.Fatal(err)
	}
	send := `{"id":"t","label":"rtvi-ai","type":"send-text","data":{"content":"hi"}}`
	for i := 0; i < rtvi.SendTextBurst; i++ {
		if _, err := h.HandleIncomingJSON(send); err != nil {
			t.Fatalf("turn %d within burst: %v", i, err)
		}
	}
	if _, err := h.HandleIncomingJSON(send); !errors.Is(err, rtvi.ErrRejectedMessage) || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("turn over burst: %v, want rate-limit rejection", err)
	}
	if delivered != rtvi.SendTextBurst {
		t.Fatalf("delivered %d turns, want %d", delivered, rtvi.SendTextBurst)
	}
	time.Sleep(rtvi.SendTextInterval + 50*time.Millisecond)
	if _, err := h.HandleIncomingJSON(send); err != nil {
		t.Fatalf("turn after refill: %v", err)
	}
}

func TestRTVI_RepeatClientReadyIsRejectedNotReRun(t *testing.T) {
	h := rtvi.NewHandler()
	calls := 0
	h.OnClientReady = func(string) error { calls++; return nil }
	ready := `{"id":"1","label":"rtvi-ai","type":"client-ready","data":{"version":"2.1.0"}}`
	if _, err := h.HandleIncomingJSON(ready); err != nil {
		t.Fatal(err)
	}
	if _, err := h.HandleIncomingJSON(ready); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("repeat client-ready: err = %v, want ErrRejectedMessage", err)
	}
	if calls != 1 {
		t.Fatalf("OnClientReady ran %d times, want 1", calls)
	}
}
