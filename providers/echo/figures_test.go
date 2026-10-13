package echo

import (
	"testing"
	"time"
)

// A unary unit is synthesized whole and the real processor gives one attempt 10 s: even the longest
// unit it sends (250 characters) must come back well inside that, or echo times out on long sentences.
func TestUnaryUnitsStayInsideTheAttemptTimeout(t *testing.T) {
	f := MimicGoogle.TTS(false)
	if d := f.FirstByte + 250*f.FirstBytePerChar; d > 5*time.Second {
		t.Fatalf("a 250-character unary unit takes %v before any audio", d)
	}
}

func TestStreamingVoicesPayNoPerCharacterCost(t *testing.T) {
	if f := MimicGoogle.TTS(true); f.FirstBytePerChar != 0 || f.FirstByte != 400*time.Millisecond {
		t.Fatalf("streaming figures %+v", f)
	}
}
