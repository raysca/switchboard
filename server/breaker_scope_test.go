package server

import (
	"errors"
	"testing"

	"govoice/providers/xai"
	"govoice/server/flow"
)

// M3-04: one xAI stage's outage must not block the others.
func TestXAIBreakersAreSplitPerStage(t *testing.T) {
	r := NewRegistry()
	llm, stt, tts := r.llmBreaker(StageSpec{Provider: "xai"}), r.sttBreaker("xai"), r.ttsBreaker("xai")
	if llm == nil || stt == nil || tts == nil || llm == stt || llm == tts || stt == tts {
		t.Fatalf("xai stages share a breaker: llm=%p stt=%p tts=%p", llm, stt, tts)
	}
}

// M3-03: an xAI transport error still fails over (server_error, not client_error).
func TestTTSClassifyXAITransportErrorFailsOver(t *testing.T) {
	err := xai.MapHTTPError("tts", nil, "", errors.New("dial tcp: connection refused"))
	got := ttsClassify(err)
	if got != flow.ClassServer {
		t.Fatalf("class = %q, want %q", got, flow.ClassServer)
	}
	var policy flow.FailoverPolicy
	if !policy.Allows(got) {
		t.Fatal("default failover policy refuses an xAI TTS network failure")
	}
}
