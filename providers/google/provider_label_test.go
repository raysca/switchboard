package google

import (
	"context"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// labelRun drives one processor in a session and returns every frame that reached the sink.
func labelRun(t *testing.T, proc core.Processor, post func(*core.Session)) []core.Frame {
	t.Helper()
	sink := &coretest.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
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
	post(sess)
	time.Sleep(300 * time.Millisecond)
	return sink.GetFrames()
}

func usageProviders(frames []core.Frame) (out []string) {
	for _, f := range frames {
		if u, ok := f.(*core.UsageMetricsFrame); ok {
			out = append(out, u.Provider)
		}
	}
	return out
}

func wantLabel(t *testing.T, got []string, want string) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("no frame carried a provider label; want %q", want)
	}
	for _, g := range got {
		if g != want {
			t.Fatalf("provider label = %q, want %q (all: %v)", g, want, got)
		}
	}
}

func sttLabels(t *testing.T, provider string) []string {
	st := newMockSTTStream()
	st.PushResult(&STTResult{Transcript: "hello", IsFinal: true})
	cfg := DefaultSTTConfig()
	cfg.Provider = provider
	cfg.StreamOpener = &mockSTTOpener{onOpenStream: func(STTConfig) *mockSTTStream { return st }}
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range labelRun(t, proc, func(s *core.Session) {
		_ = s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
		_ = s.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	}) {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok {
			out = append(out, c.Provider)
		}
	}
	return out
}

func llmLabels(t *testing.T, provider string) []string {
	cfg := DefaultLLMConfig()
	cfg.Provider = provider
	cfg.Client = &mockLLMClient{}
	proc, err := NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return usageProviders(labelRun(t, proc, func(s *core.Session) {
		_ = proc.Attach(s, 0)
		_ = s.Post(&core.UserMessageFrame{Turn: "t1", Gen: 1, Content: "hi"})
	}))
}

func ttsLabels(t *testing.T, provider string, streaming bool) []string {
	cfg := DefaultTTSConfig()
	cfg.Provider = provider
	client := &streamTestClient{streamFn: func(_ context.Context, _ []string, onAudio func([]byte) error) error {
		return onAudio(make([]byte, 960))
	}}
	cfg.Client = client
	var proc core.Processor
	var err error
	if streaming {
		proc, err = NewStreamingTTSProcessor(cfg)
	} else {
		proc, err = NewTTSProcessor(cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	return usageProviders(labelRun(t, proc, func(s *core.Session) {
		_ = s.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 1, UnitID: 1, Text: "Hello there.", IsFinal: true})
	}))
}

func TestProviderLabelDefaultsToGoogle(t *testing.T) {
	wantLabel(t, sttLabels(t, ""), "google")
	wantLabel(t, llmLabels(t, ""), "google")
	wantLabel(t, ttsLabels(t, "", false), "google")
	wantLabel(t, ttsLabels(t, "", true), "google")
}

func TestProviderLabelFollowsConfigSTT(t *testing.T) { wantLabel(t, sttLabels(t, "echo"), "echo") }
func TestProviderLabelFollowsConfigLLM(t *testing.T) { wantLabel(t, llmLabels(t, "echo"), "echo") }
func TestProviderLabelFollowsConfigTTSUnary(t *testing.T) {
	wantLabel(t, ttsLabels(t, "echo", false), "echo")
}
func TestProviderLabelFollowsConfigTTSStreaming(t *testing.T) {
	wantLabel(t, ttsLabels(t, "echo", true), "echo")
}
