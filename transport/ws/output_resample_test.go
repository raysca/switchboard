package ws_test

import (
	"testing"
	"time"

	"govoice/core"
)

// Output frames at 16 kHz, then 48 kHz (a TTS fallback or a tone at another rate), then 16 kHz again: each is
// resampled to the 24 kHz player rate with the ratio of its own rate, so 140 ms of audio is 3360 samples on the
// wire (a continuing resampler holds back 2 samples of its stream: 3358 measured; a fresh one per frame loses more and lands on 3356). Today the
// 48 kHz frames go through the 16 kHz resampler and come out three times too long.
func TestOutputResamplerFollowsEachFramesRate(t *testing.T) {
	r := newOutputRig(t)
	gen := r.sess.CurrentGeneration()
	segments := []struct{ rate, frames int }{{16000, 3}, {48000, 3}, {16000, 1}}
	total := 0
	for _, s := range segments {
		total += s.frames
	}
	seq := 0
	for _, s := range segments {
		for i := 0; i < s.frames; i++ {
			seq++
			last := seq == total
			if err := r.sess.Post(&core.OutputAudioFrame{Samples: constPCM(s.rate, 20*time.Millisecond, 1000), Encoding: core.EncodingPCM16LE,
				SampleRate: s.rate, Channels: 1, Duration: 20 * time.Millisecond, Gen: gen, Turn: "t", UnitID: 1, Seq: uint64(seq),
				IsFinal: last, TurnFinal: last}); err != nil {
				t.Fatal(err)
			}
		}
	}
	r.waitFor(t, "the reply to drain", func(log []wireEvent) bool {
		for _, ev := range log {
			if ev.msg == "bot-stopped-speaking" {
				return true
			}
		}
		return false
	})
	samples := 0
	for _, ev := range r.events() {
		if ev.msg != "" {
			continue
		}
		if ev.rate != 24000 {
			t.Fatalf("a frame left at %d Hz", ev.rate)
		}
		samples += ev.bytes / 2
	}
	if want := 3358; samples < want-1 || samples > want+1 {
		t.Fatalf("%d samples on the wire, want %d±1 (140 ms at 24 kHz less the held-back filter tail)", samples, want)
	}
}
