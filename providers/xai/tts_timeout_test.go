//go:build xaispeech

package xai

import (
	"testing"
	"time"
)

// TTSConfig.Timeout is the per-attempt deadline, and retries are on by default.
func TestTTSTimeoutIsTheAttemptDeadline(t *testing.T) {
	tts, err := NewTTSProcessor(DefaultTTSConfig())
	if err != nil {
		t.Fatal(err)
	}
	if tts.cfg.RetryPolicy.AttemptTimeout != 10*time.Second {
		t.Fatalf("tts attempt timeout %v, want TTSConfig.Timeout (10s)", tts.cfg.RetryPolicy.AttemptTimeout)
	}
	bare, err := NewTTSProcessor(TTSConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if bare.cfg.RetryPolicy.MaxAttempts != 3 {
		t.Fatalf("tts without a policy gets %d attempts", bare.cfg.RetryPolicy.MaxAttempts)
	}
}
