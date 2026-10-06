package google

import (
	"bytes"
	"testing"
	"time"

	"govoice/core"
)

// VAD reports speech start after the onset audio was already sent; from the
// second utterance on, that onset must reach the new utterance's stream.
func TestGoogleSTTNextUtteranceStreamGetsItsOnsetAudio(t *testing.T) {
	opener := &mockSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(proc, &core.PassthroughProcessor{})
	session, _ := core.NewSession(34, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()

	audio := func(b byte) *core.InputAudioFrame {
		return &core.InputAudioFrame{Samples: bytes.Repeat([]byte{b}, 640), Gen: 1}
	}
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(audio(1))
	_ = session.Post(&core.UserStoppedSpeakingFrame{})
	_ = session.Post(audio(2)) // onset of the next utterance
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 2, Gen: 1})
	_ = session.Post(audio(3))

	deadline := time.Now().Add(time.Second)
	for {
		var got []byte
		opener.mu.Lock()
		if n := len(opener.streams); n > 0 {
			last := opener.streams[n-1]
			last.mu.Lock()
			for _, c := range last.audioChunks {
				got = append(got, c[0])
			}
			last.mu.Unlock()
		}
		opener.mu.Unlock()
		if bytes.HasSuffix(got, []byte{3}) {
			if !bytes.Equal(got, []byte{2, 3}) {
				t.Fatalf("second utterance stream got chunks %v, want [2 3] (onset lost)", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("audio never reached the stream: %v", got)
		}
		time.Sleep(time.Millisecond)
	}
}
