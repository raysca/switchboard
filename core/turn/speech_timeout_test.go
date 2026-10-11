package turn

import (
	"testing"
	"time"

	"govoice/core"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestSpeechTimeoutRule(t *testing.T) {
	cfg := SpeechTimeoutConfig{VADStop: ms(200), UserSpeechTimeout: ms(600), STTP99: ms(1570), NoTranscriptTimeout: 5 * time.Second}
	for _, c := range []struct {
		name  string
		in    DetectionInput
		cfg   SpeechTimeoutConfig
		stops bool
	}{
		{"resume window not reached", DetectionInput{SilenceFor: ms(799), Text: "hi", TextFinal: true}, cfg, false},
		{"final text at the resume window", DetectionInput{SilenceFor: ms(800), Text: "hi", TextFinal: true}, cfg, true},
		{"no text yet", DetectionInput{SilenceFor: ms(1600)}, cfg, false},
		{"whitespace is no text", DetectionInput{SilenceFor: ms(1600), Text: "  ", TextFinal: true}, cfg, false},
		{"interim text before p99", DetectionInput{SilenceFor: ms(1569), Text: "hi"}, cfg, false},
		{"interim text at p99", DetectionInput{SilenceFor: ms(1570), Text: "hi"}, cfg, true},
		{"no transcript fallback", DetectionInput{SilenceFor: 5 * time.Second}, cfg, true},
		{"fast STT: resume window dominates", DetectionInput{SilenceFor: ms(800), Text: "hi"}, SpeechTimeoutConfig{VADStop: ms(200), STTP99: ms(350)}, true},
		{"fast STT: still waits the window", DetectionInput{SilenceFor: ms(700), Text: "hi"}, SpeechTimeoutConfig{VADStop: ms(200), STTP99: ms(350)}, false},
		{"zero config: 600ms window from onset", DetectionInput{SilenceFor: ms(600), Text: "hi", TextFinal: true}, SpeechTimeoutConfig{}, true},
		{"zero config: 1s p99", DetectionInput{SilenceFor: ms(999), Text: "hi"}, SpeechTimeoutConfig{}, false},
		{"zero config: 5s fallback", DetectionInput{SilenceFor: 5 * time.Second}, SpeechTimeoutConfig{}, true},
	} {
		d, ok := SpeechTimeout(c.cfg).Detect(c.in)
		if ok != c.stops {
			t.Errorf("%s: stops=%t, want %t", c.name, ok, c.stops)
		}
		if ok && d.Reason != core.TurnReasonSilence {
			t.Errorf("%s: reason %v, want silence", c.name, d.Reason)
		}
	}
}

func TestSpeechTimeoutInterimWaitsForFinalOrP99(t *testing.T) {
	st := SpeechTimeout(SpeechTimeoutConfig{VADStop: ms(200), STTP99: ms(2140)})
	if _, ok := st.Detect(DetectionInput{SilenceFor: ms(1500), Text: "book a table"}); ok {
		t.Fatal("ended on interim text before p99")
	}
	if _, ok := st.Detect(DetectionInput{SilenceFor: ms(1500), Text: "book a table for two", TextFinal: true}); !ok {
		t.Fatal("did not end when the final arrived after the resume window")
	}
}

func TestDefaultSTTP99(t *testing.T) {
	for p, want := range map[string]time.Duration{"google": ms(1570), "xai": ms(2140), "deepgram": ms(350), "cartesia": ms(810), "elevenlabs": ms(410), "nvidia": time.Second, "unknown": time.Second} {
		if got := DefaultSTTP99(p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}
