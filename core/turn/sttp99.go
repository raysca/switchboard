package turn

import "time"

// UnknownSTTP99 is the p99 for a provider without a measured value (Pipecat DEFAULT_TTFS_P99).
const UnknownSTTP99 = time.Second

// sttP99 is each STT provider's p99 time from end of speech to final transcript, seeded from
// Pipecat's benchmark (pipecat-impl/src/pipecat/services/stt_latency.py; elevenlabs is its
// realtime Scribe figure). The nightly live job reports our measured value next to these.
var sttP99 = map[string]time.Duration{
	"google":     1570 * time.Millisecond,
	"xai":        2140 * time.Millisecond,
	"deepgram":   350 * time.Millisecond,
	"cartesia":   810 * time.Millisecond,
	"elevenlabs": 410 * time.Millisecond,
	"nvidia":     time.Second,
}

// DefaultSTTP99 returns the provider's built-in p99 (UnknownSTTP99 when not listed).
func DefaultSTTP99(provider string) time.Duration {
	if d, ok := sttP99[provider]; ok {
		return d
	}
	return UnknownSTTP99
}
