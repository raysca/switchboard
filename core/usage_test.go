package core

import (
	"context"
	"testing"
	"time"
)

func TestUsageMetricsFrame_ContractAndTrace(t *testing.T) {
	promptTok := int64(42)
	completionTok := int64(18)
	totalTok := int64(60)

	usage := &UsageMetricsFrame{
		SessionID:        SessionID("sess-100"),
		Turn:             "turn-42",
		Gen:              1,
		Provider:         "google",
		Model:            "gemini-2.5-flash",
		Service:          "llm",
		Timestamp:        time.Now(),
		PromptTokens:     &promptTok,
		CompletionTokens: &completionTok,
		TotalTokens:      &totalTok,
	}

	if usage.Type() != FrameUsageMetrics {
		t.Fatalf("expected FrameUsageMetrics, got %v", usage.Type())
	}
	if usage.Generation() != 1 {
		t.Fatalf("expected Gen 1, got %d", usage.Generation())
	}

	// Verify trace mapping
	name, attrs := frameTrace(usage)
	if name != "usage.metrics" {
		t.Fatalf("expected trace name usage.metrics, got %s", name)
	}
	if attrs["provider"] != "google" || attrs["service"] != "llm" || attrs["model"] != "gemini-2.5-flash" {
		t.Fatalf("unexpected trace attrs: %+v", attrs)
	}
	if attrs["usage.prompt_tokens"] != int64(42) || attrs["usage.completion_tokens"] != int64(18) || attrs["usage.total_tokens"] != int64(60) {
		t.Fatalf("unexpected token usage in attrs: %+v", attrs)
	}

	// Verify absent metrics remain absent (no zero inflation)
	ttsUsage := &UsageMetricsFrame{
		SessionID:  SessionID("sess-100"),
		Turn:       "turn-42",
		Gen:        1,
		Provider:   "google",
		Service:    "tts",
		Timestamp:  time.Now(),
		Characters: Int64Ptr(150),
	}
	_, ttsAttrs := frameTrace(ttsUsage)
	if _, ok := ttsAttrs["usage.prompt_tokens"]; ok {
		t.Fatal("prompt_tokens should not be present for TTS usage")
	}
	if ttsAttrs["usage.characters"] != int64(150) {
		t.Fatalf("expected 150 characters, got %v", ttsAttrs["usage.characters"])
	}

	// Test pipeline delivery of usage metrics
	receivedUsage := make(chan *UsageMetricsFrame, 1)
	sinkProc := &testFnProcessor{fn: func(_ context.Context, f Frame, d Direction, e Emitter) error {
		if um, ok := f.(*UsageMetricsFrame); ok {
			receivedUsage <- um
		}
		return e.PushDirection(f, d)
	}}

	pipe, _ := NewPipeline(sinkProc)
	s, _ := NewSession(100, pipe, DefaultResourceBudget())
	_ = s.Start()
	defer s.Close()

	_ = s.Post(usage)

	select {
	case rec := <-receivedUsage:
		if rec.Turn != "turn-42" || *rec.TotalTokens != 60 {
			t.Fatalf("unexpected received usage: %+v", rec)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for UsageMetricsFrame delivery")
	}
}
