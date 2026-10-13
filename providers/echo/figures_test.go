package echo

import (
	"testing"
	"time"
)

func TestGoogleFiguresFollowTheMeasurements(t *testing.T) {
	g := MimicGoogle
	if f := g.STT("chirp_2"); f.Interims || f.FinalDelay != 1200*time.Millisecond {
		t.Fatalf("chirp_2 = %+v", f)
	}
	if f := g.STT("something-else"); f.Interims || f.FinalDelay != 1200*time.Millisecond {
		t.Fatalf("unknown model falls back to chirp_2: %+v", f)
	}
	if f := g.STT("latest_long"); !f.Interims || f.FinalDelay != 500*time.Millisecond {
		t.Fatalf("latest_long = %+v", f)
	}
	if f := g.TTS("en-US-Chirp3-HD-Aoede"); f.FirstByte != 400*time.Millisecond || f.FirstBytePerChar != 0 {
		t.Fatalf("chirp3-hd should stream: %+v", f)
	}
	if f := g.TTS(""); f.FirstByte != 400*time.Millisecond {
		t.Fatalf("empty voice is the streaming default: %+v", f)
	}
	if f := g.TTS("en-US-Journey-F"); f.FirstByte != 600*time.Millisecond || f.FirstBytePerChar != 50*time.Millisecond {
		t.Fatalf("other voices are unary: %+v", f)
	}
	if f := g.LLM(); f.FirstToken != 700*time.Millisecond || !f.WordChunks {
		t.Fatalf("llm = %+v", f)
	}
	if g.TTS("x").CharsPerSecond != 15 {
		t.Fatal("speech rate is 15 chars/s")
	}
}

func TestInstantHasNoDelays(t *testing.T) {
	i := MimicInstant
	if i.STT("chirp_2") != (STTFigures{FramesPerWord: 1}) || i.LLM() != (LLMFigures{}) || i.TTS("v") != (TTSFigures{}) {
		t.Fatalf("instant: %+v %+v %+v", i.STT("chirp_2"), i.LLM(), i.TTS("v"))
	}
}

func TestMimicValid(t *testing.T) {
	for m, want := range map[Mimic]bool{MimicGoogle: true, MimicInstant: true, "": false, "x": false} {
		if m.Valid() != want {
			t.Errorf("%q.Valid() = %v", m, !want)
		}
	}
}
