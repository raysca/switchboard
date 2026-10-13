package core_test

import (
	"govoice/core"
	"govoice/core/coretest"

	"testing"
	"time"
)

// Word timings ride on OutputAudioFrame through the pipeline untouched (P3-1).
func TestOutputAudioFrameCarriesWordTimings(t *testing.T) {
	sink := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(&coretest.PassthroughProcessor{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	want := []core.WordTiming{{Word: "hi", Start: 0, End: 120 * time.Millisecond}, {Word: "there", Start: 150 * time.Millisecond, End: 400 * time.Millisecond}}
	if err := sess.Post(&core.OutputAudioFrame{Samples: make([]byte, 960), SampleRate: 24000, Channels: 1, Turn: "t1", WordTimings: want}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range sink.GetFrames() {
			if a, ok := f.(*core.OutputAudioFrame); ok {
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
