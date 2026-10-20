package ws_test

import (
	"testing"
	"time"

	"govoice/core"
)

func ttsFrame(g, unit, seq uint64, v int16, last bool, text string) *core.OutputAudioFrame {
	return &core.OutputAudioFrame{Samples: constPCM(24000, 20*time.Millisecond, v), Encoding: core.EncodingPCM16LE,
		SampleRate: 24000, Channels: 1, Duration: 20 * time.Millisecond, Gen: g, Turn: "t", UnitID: unit, Seq: seq,
		IsFinal: last, Text: text}
}

func ttsTexts(log []wireEvent) (at []int) {
	for i, ev := range log {
		if ev.msg == "bot-tts-text" {
			at = append(at, i)
		}
	}
	return
}

// (a) bot-tts-text follows the unit's last audio frame (and bot-started-speaking), once, with the unit text.
func TestBotTTSTextFollowsTheUnitsFinalAudio(t *testing.T) {
	r := newOutputRig(t)
	gen := r.sess.CurrentGeneration()
	for i := uint64(1); i <= 3; i++ {
		text := ""
		if i == 3 {
			text = "Hi."
		}
		if err := r.sess.Post(ttsFrame(gen, 1, i, 1000, i == 3, text)); err != nil {
			t.Fatal(err)
		}
	}
	r.waitFor(t, "bot-tts-text", func(log []wireEvent) bool { return len(ttsTexts(log)) > 0 })
	time.Sleep(50 * time.Millisecond)
	log := r.events()
	at := ttsTexts(log)
	if len(at) != 1 || log[at[0]].text != "Hi." {
		t.Fatalf("want one bot-tts-text Hi.; wire: %v", log)
	}
	if countAudio(log[:at[0]], 1000) != 3 {
		t.Fatalf("bot-tts-text before the final audio frame was written: %v", log)
	}
	started := -1
	for i, ev := range log {
		if ev.msg == "bot-started-speaking" {
			started = i
			break
		}
	}
	if started < 0 || started > at[0] {
		t.Fatalf("bot-tts-text before bot-started-speaking: %v", log)
	}
}

// (b) a unit flushed by an interruption before its final frame is written sends no text.
func TestBotTTSTextIsDroppedWithAFlushedUnit(t *testing.T) {
	r := newOutputRig(t)
	gen := r.sess.CurrentGeneration()
	posted := make(chan struct{})
	go func() {
		defer close(posted)
		for i := uint64(1); i <= 50; i++ {
			text := ""
			if i == 50 {
				text = "Cut off."
			}
			_ = r.sess.Post(ttsFrame(gen, 1, i, 1000, i == 50, text))
		}
	}()
	r.waitFor(t, "the unit to start playing", func(log []wireEvent) bool { return countAudio(log, 1000) >= 3 })
	if err := r.sess.Post(&core.InterruptionFrame{Gen: gen}); err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Post(&core.FlushFrame{Gen: gen}); err != nil {
		t.Fatal(err)
	}
	<-posted
	if err := r.sess.Post(ttsFrame(gen+1, 2, 1, -1000, true, "")); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "the next reply", func(log []wireEvent) bool { return countAudio(log, -1000) == 1 })
	time.Sleep(50 * time.Millisecond)
	if log := r.events(); len(ttsTexts(log)) != 0 {
		t.Fatalf("a flushed unit sent its text: %v", log)
	}
}

// (c) an empty final marker (the unit had no audio) still sends its text, once.
func TestBotTTSTextFromAnEmptyFinalMarker(t *testing.T) {
	r := newOutputRig(t)
	gen := r.sess.CurrentGeneration()
	if err := r.sess.Post(&core.OutputAudioFrame{Encoding: core.EncodingPCM16LE, SampleRate: 24000, Channels: 1,
		Gen: gen, Turn: "t", UnitID: 1, Seq: 1, IsFinal: true, Text: "Silent."}); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "bot-tts-text", func(log []wireEvent) bool { return len(ttsTexts(log)) > 0 })
	time.Sleep(50 * time.Millisecond)
	log := r.events()
	if at := ttsTexts(log); len(at) != 1 || log[at[0]].text != "Silent." {
		t.Fatalf("want one bot-tts-text Silent.; wire: %v", log)
	}
}

// (d) a non-final frame never sends text, even if it carries some (the producer never does).
func TestBotTTSTextNeverFromANonFinalFrame(t *testing.T) {
	r := newOutputRig(t)
	gen := r.sess.CurrentGeneration()
	for i := uint64(1); i <= 2; i++ {
		if err := r.sess.Post(ttsFrame(gen, 1, i, 1000, i == 2, "")); err != nil {
			t.Fatal(err)
		}
	}
	// Seq 1 is posted again as a non-final frame with text, ahead of the final in a second unit.
	if err := r.sess.Post(ttsFrame(gen, 2, 1, 2000, false, "Early.")); err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Post(ttsFrame(gen, 2, 2, 2000, true, "")); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "both units", func(log []wireEvent) bool { return countAudio(log, 1000) == 2 && countAudio(log, 2000) == 2 })
	time.Sleep(50 * time.Millisecond)
	if log := r.events(); len(ttsTexts(log)) != 0 {
		t.Fatalf("a non-final frame sent text: %v", log)
	}
}
