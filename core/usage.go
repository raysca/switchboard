package core

import "time"

// UsageMetricsFrame represents optional usage metrics reported by a provider
// (such as LLM token counts, TTS character counts, STT audio duration, requests, or tools executed).
// Unavailable metrics remain absent (nil pointer). No billing decisions are made in core.
type UsageMetricsFrame struct {
	SessionID SessionID `json:"session_id,omitempty"`
	Turn      string    `json:"turn,omitempty"`
	Gen       uint64    `json:"gen,omitempty"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model,omitempty"`
	Service   string    `json:"service"` // "llm", "stt", "tts", "tool"
	Timestamp time.Time `json:"timestamp"`

	// Token metrics (LLM)
	PromptTokens     *int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens *int64 `json:"completion_tokens,omitempty"`
	TotalTokens      *int64 `json:"total_tokens,omitempty"`

	// Character metrics (TTS)
	Characters *int64 `json:"characters,omitempty"`

	// Audio duration metrics (STT/TTS)
	AudioDurationMs *int64 `json:"audio_duration_ms,omitempty"`

	// Request / tool call count
	Requests *int64 `json:"requests,omitempty"`
}

func (f *UsageMetricsFrame) Type() FrameType    { return FrameUsageMetrics }
func (f *UsageMetricsFrame) Generation() uint64 { return f.Gen }

// Int64Ptr is a helper that returns a pointer to an int64 value.
func Int64Ptr(v int64) *int64 {
	return &v
}
