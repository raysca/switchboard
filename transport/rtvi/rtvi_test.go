package rtvi_test

import (
	"encoding/json"
	"errors"
	"fmt"
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
	rawJSON := `{"id":"2","label":"rtvi-ai","type":"disconnect-bot"}`
	frame, err := h.HandleIncomingJSON(rawJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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

// A server-side setup failure is distinguishable from a malformed message, and
// a refused text turn never echoes the server's reason to the client.
func TestServerErrorsAreClassifiedAndNotEchoed(t *testing.T) {
	h := rtvi.NewHandler()
	h.OnClientReady = func(string) error { return errors.New("db password wrong") }
	ready := `{"id":"r","label":"rtvi-ai","type":"client-ready","data":{"version":"` + rtvi.ProtocolVersion + `"}}`
	if _, err := h.HandleIncomingJSON(ready); !errors.Is(err, rtvi.ErrSessionSetup) {
		t.Fatalf("setup failure = %v, want ErrSessionSetup", err)
	}

	h = rtvi.NewHandler()
	h.OnSendText = func(string) error { return errors.New("internal detail") }
	if _, err := h.HandleIncomingJSON(ready); err != nil {
		t.Fatal(err)
	}
	_, err := h.HandleIncomingJSON(`{"id":"t","label":"rtvi-ai","type":"send-text","data":{"content":"hi"}}`)
	if !errors.Is(err, rtvi.ErrRejectedMessage) || strings.Contains(err.Error(), "internal detail") {
		t.Fatalf("rejected text turn = %v", err)
	}
}

// Pipecat clients (client-js onBotLlmStarted, pipecat.evals llm_response) need
// bot-llm-started before a reply's text; a cut-off reply must not swallow the next one's.
func TestBotLLMStartedBracketsEachReply(t *testing.T) {
	h := rtvi.NewHandler()
	var got []string
	add := func(msgs []string) {
		for _, m := range msgs {
			var msg rtvi.Message
			if err := json.Unmarshal([]byte(m), &msg); err != nil {
				t.Fatal(err)
			}
			got = append(got, msg.Type)
		}
	}
	add(h.BotLLMText("t1/1", "a"))
	add(h.BotLLMText("t1/1", "b"))
	add(h.BotLLMStopped("t1/1"))
	add(h.BotLLMText("t2/1", "c")) // interrupted: no stop
	add(h.BotLLMText("t2/2", "d")) // new generation reopens
	add(h.BotLLMStopped("t2/2"))
	add(h.BotLLMStopped("t3/2")) // reply with no text still bracketed
	want := []string{
		"bot-llm-started", "bot-llm-text", "bot-llm-text", "bot-llm-stopped",
		"bot-llm-started", "bot-llm-text",
		"bot-llm-started", "bot-llm-text", "bot-llm-stopped",
		"bot-llm-started", "bot-llm-stopped",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}

func TestSetMuted(t *testing.T) {
	h := rtvi.NewHandler()
	msg := func(data string) string {
		return `{"id":"m","label":"rtvi-ai","type":"set-muted","data":` + data + `}`
	}
	if _, err := h.HandleIncomingJSON(msg(`{"muted":true}`)); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("set-muted before client-ready: %v", err)
	}
	if _, err := h.HandleIncomingJSON(`{"id":"r","label":"rtvi-ai","type":"client-ready","data":{"version":"` + rtvi.ProtocolVersion + `"}}`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{}`, `{"muted":"yes"}`, `null`} {
		if _, err := h.HandleIncomingJSON(msg(bad)); !errors.Is(err, rtvi.ErrRejectedMessage) {
			t.Fatalf("payload %s accepted: %v", bad, err)
		}
	}
	f, err := h.HandleIncomingJSON(msg(`{"muted":true}`))
	if _, ok := f.(*core.InputMuteFrame); err != nil || !ok {
		t.Fatalf("muted=true gave %T, %v", f, err)
	}
	f, err = h.HandleIncomingJSON(msg(`{"muted":false}`))
	if _, ok := f.(*core.InputUnmuteFrame); err != nil || !ok {
		t.Fatalf("muted=false gave %T, %v", f, err)
	}
}

func TestPushToTalk(t *testing.T) {
	ptt := func(pressed string) string {
		return `{"id":"p","label":"rtvi-ai","type":"push-to-talk","data":{"pressed":` + pressed + `}}`
	}
	ready := func(h *rtvi.Handler, extra string) {
		t.Helper()
		if _, err := h.HandleIncomingJSON(`{"id":"r","label":"rtvi-ai","type":"client-ready","data":{"version":"` + rtvi.ProtocolVersion + `"` + extra + `}}`); err != nil {
			t.Fatal(err)
		}
	}

	off := rtvi.NewHandler()
	ready(off, "")
	if off.PushToTalk() {
		t.Fatal("push-to-talk on without opt-in")
	}
	if _, err := off.HandleIncomingJSON(ptt("true")); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("push-to-talk accepted on a call that did not opt in: %v", err)
	}

	on := rtvi.NewHandler()
	if _, err := on.HandleIncomingJSON(ptt("true")); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("push-to-talk before client-ready: %v", err)
	}
	ready(on, `,"push_to_talk":true`)
	if !on.PushToTalk() {
		t.Fatal("opt-in not recorded")
	}
	if _, err := on.HandleIncomingJSON(`{"id":"p","label":"rtvi-ai","type":"push-to-talk","data":{}}`); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("missing pressed accepted: %v", err)
	}
	for _, want := range []bool{true, false} {
		f, err := on.HandleIncomingJSON(ptt(fmt.Sprint(want)))
		p, ok := f.(*core.PushToTalkFrame)
		if err != nil || !ok || p.Pressed != want {
			t.Fatalf("pressed=%t gave %#v, %v", want, f, err)
		}
	}
}

// Pipecat's default function_call_report_level is "none": the call's id only.
func TestFunctionCallMessagesCarryTheIDOnly(t *testing.T) {
	h := rtvi.NewHandler()
	var got []string
	add := func(msgs []string) {
		for _, m := range msgs {
			var msg rtvi.Message
			if err := json.Unmarshal([]byte(m), &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Label != rtvi.LabelRTVI || msg.ID == "" {
				t.Fatalf("envelope %s", m)
			}
			got = append(got, msg.Type+" "+string(msg.Data))
		}
	}
	add(h.FunctionCallStarted("call-1"))
	add(h.FunctionCallStarted("call-2"))
	add(h.FunctionCallStopped("call-1", false))
	add(h.FunctionCallStopped("call-2", true))
	want := []string{
		`llm-function-call-started {}`,
		`llm-function-call-in-progress {"tool_call_id":"call-1"}`,
		`llm-function-call-started {}`,
		`llm-function-call-in-progress {"tool_call_id":"call-2"}`,
		`llm-function-call-stopped {"tool_call_id":"call-1","cancelled":false}`,
		`llm-function-call-stopped {"tool_call_id":"call-2","cancelled":true}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestFunctionCallStoppedOnlyForAnnouncedCalls(t *testing.T) {
	h := rtvi.NewHandler()
	if msgs := h.FunctionCallStopped("never-announced", true); len(msgs) != 0 {
		t.Fatalf("stopped a call that was never announced: %v", msgs)
	}
	h.FunctionCallStarted("call-1")
	if msgs := h.FunctionCallStopped("call-1", false); len(msgs) != 1 {
		t.Fatalf("announced call not stopped: %v", msgs)
	}
	if msgs := h.FunctionCallStopped("call-1", false); len(msgs) != 0 {
		t.Fatal("a call was stopped twice")
	}
	for i := 0; i < 64; i++ { // results that never came
		h.FunctionCallStarted(fmt.Sprint("lost-", i))
	}
	h.FunctionCallStarted("late") // the 65th forgets the lost ones
	if len(h.FunctionCallStopped("lost-0", false)) != 0 || len(h.FunctionCallStopped("late", false)) != 1 {
		t.Fatal("the announced-call set is not bounded to 64")
	}
}
