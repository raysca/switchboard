package ws_test

import (
	"testing"
	"time"

	"govoice/core"
)

// A barge-in that cuts a reply off (the assistant's InterruptionFrame + FlushFrame) sends bot-interrupted
// once, after the paced writer dropped the reply's queued audio: of the cut reply only the frame that may
// already be on the socket can follow it, and the next reply plays after it.
func TestBargeInSendsBotInterruptedAfterTheCutReplysQueuedAudio(t *testing.T) {
	r := newOutputRig(t)
	gen := r.sess.CurrentGeneration()
	frame := func(g, unit, seq uint64, v int16, last bool) *core.OutputAudioFrame {
		return &core.OutputAudioFrame{Samples: constPCM(24000, 20*time.Millisecond, v), Encoding: core.EncodingPCM16LE,
			SampleRate: 24000, Channels: 1, Duration: 20 * time.Millisecond, Gen: g, Turn: "t", UnitID: unit, Seq: seq,
			IsFinal: last, TurnFinal: last}
	}
	posted := make(chan struct{})
	go func() { // the cut reply: 1 s of audio, posted at media pace by the session's output gate
		defer close(posted)
		for i := uint64(1); i <= 50; i++ {
			_ = r.sess.Post(frame(gen, 1, i, 1000, i == 50)) // after the flush these are stale and refused
		}
	}()
	r.waitFor(t, "the reply to start playing", func(log []wireEvent) bool { return countAudio(log, 1000) >= 3 })
	if err := r.sess.Post(&core.InterruptionFrame{Gen: gen}); err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Post(&core.FlushFrame{Gen: gen}); err != nil {
		t.Fatal(err)
	}
	<-posted
	for i := uint64(1); i <= 3; i++ { // the next reply
		if err := r.sess.Post(frame(gen+1, 2, i, -1000, i == 3)); err != nil {
			t.Fatal(err)
		}
	}
	r.waitFor(t, "the next reply", func(log []wireEvent) bool { return countAudio(log, -1000) == 3 })
	log := r.events()
	at := -1
	for i, ev := range log {
		if ev.msg == "bot-interrupted" {
			if at >= 0 {
				t.Fatalf("bot-interrupted twice: %v", log)
			}
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("no bot-interrupted: %v", log)
	}
	before, after := countAudio(log[:at], 1000), countAudio(log[at:], 1000)
	if before < 3 || before+after >= 50 {
		t.Fatalf("the reply was not cut mid-play: %d frames before bot-interrupted, %d after", before, after)
	}
	if after > 1 {
		t.Fatalf("%d frames of the cut reply followed bot-interrupted (at most the one already on the socket may): %v", after, log)
	}
	if countAudio(log[:at], -1000) != 0 {
		t.Fatal("the next reply played before bot-interrupted")
	}
}
