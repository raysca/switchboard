package core

import (
	"testing"
	"time"
)

// Word timings ride on OutputAudioFrame through the pipeline untouched (P3-1).
func TestOutputAudioFrameCarriesWordTimings(t *testing.T) {
	sink := &PassthroughProcessor{}
	pipe, err := NewPipeline(&PassthroughProcessor{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewSession(1, pipe, DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	want := []WordTiming{{Word: "hi", Start: 0, End: 120 * time.Millisecond}, {Word: "there", Start: 150 * time.Millisecond, End: 400 * time.Millisecond}}
	if err := sess.Post(&OutputAudioFrame{Samples: make([]byte, 960), SampleRate: 24000, Channels: 1, Turn: "t1", WordTimings: want}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range sink.GetFrames() {
			if a, ok := f.(*OutputAudioFrame); ok {
				if len(a.WordTimings) != 2 || a.WordTimings[0] != want[0] || a.WordTimings[1] != want[1] {
					t.Fatalf("timings: %+v", a.WordTimings)
				}
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("audio frame never reached the sink")
}
