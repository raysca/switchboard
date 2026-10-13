package google

import (
	"bytes"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// VAD reports speech start after the onset audio was already sent; from the
// second utterance on, that onset must reach the new utterance's stream.
func TestGoogleSTTNextUtteranceStreamGetsItsOnsetAudio(t *testing.T) {
	opener := &mockSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(proc, &coretest.PassthroughProcessor{})
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

// A turn manager barges in mid-utterance: the open stream moves to the new
// generation with its assembled text instead of being cut or fenced.
func TestGoogleSTTSameTurnNewerGenerationAdoptsStream(t *testing.T) {
	opener := &mockSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, _ := NewSTTProcessor(cfg)
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(35, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	var st *mockSTTStream
	for deadline := time.Now().Add(time.Second); st == nil && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		opener.mu.Lock()
		if len(opener.streams) > 0 {
			st = opener.streams[0]
		}
		opener.mu.Unlock()
	}
	if st == nil {
		t.Fatal("stream never opened")
	}
	st.PushResult(&STTResult{Transcript: "stop", IsFinal: true, ResultEndOffset: time.Second})
	time.Sleep(20 * time.Millisecond)
	session.IncrementGeneration()
	_ = session.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "turn-1-1", Gen: 2})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 2})
	time.Sleep(20 * time.Millisecond)
	st.PushResult(&STTResult{Transcript: "now", IsFinal: true, ResultEndOffset: 2 * time.Second})
	time.Sleep(30 * time.Millisecond)
	var last *core.TranscriptCandidateFrame
	for _, f := range sink.GetFrames() {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			last = c
		}
	}
	opener.mu.Lock()
	opens := len(opener.streams)
	opener.mu.Unlock()
	if last == nil || last.Text != "stop now" || last.Turn != "turn-1-1" || last.Gen != 2 || opens != 1 {
		t.Fatalf("candidate %+v opens=%d", last, opens)
	}
}
