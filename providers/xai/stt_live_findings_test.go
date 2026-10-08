//go:build xaispeech

package xai

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/resilience"
)

// Live finding: xAI chunk finals and interims carry only their own segment and speech_final the
// stitched utterance. Candidates must be cumulative for the turn (what the live capture showed:
// "What is" F, "the capital of France?" F, "What is the capital of France?" speech_final), and
// speech resumed after a speech_final must extend it.
func TestXAISTT_CandidatesAreCumulativeAcrossChunksAndUtterances(t *testing.T) {
	ms := &mockSTTStream{messages: []*STTMessage{
		{Type: "transcript.created"},
		{Type: "transcript.partial", Text: "What is"},
		{Type: "transcript.partial", Text: "What is", IsFinal: true},
		{Type: "transcript.partial", Text: "the capital"},
		{Type: "transcript.partial", Text: "the capital of France?", IsFinal: true},
		{Type: "transcript.partial", Text: "What is the capital of France?", IsFinal: true, SpeechFinal: true},
		{Type: "transcript.partial", Text: "And Spain"},
		{Type: "transcript.partial", Text: "And Spain?", IsFinal: true, SpeechFinal: true},
	}}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &mockSTTOpener{stream: ms}
	proc, _ := NewSTTProcessor(cfg)
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(31, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})

	want := []string{"What is", "What is", "What is the capital", "What is the capital of France?",
		"What is the capital of France?", "What is the capital of France? And Spain", "What is the capital of France? And Spain?"}
	var got []string
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		got = nil
		for _, f := range sink.GetFrames() {
			if c, ok := f.(*core.TranscriptCandidateFrame); ok {
				got = append(got, c.Text)
			}
		}
		if len(got) >= len(want) {
			break
		}
	}
	if len(got) < len(want) {
		t.Fatalf("got %q", got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("candidate %d = %q, want %q (all %q)", i, got[i], w, got)
		}
	}
}

// An open breaker must stop the dial (and a half-open probe must not be taken by the gate); a
// cancelled open is not a provider failure.
func TestXAISTT_BreakerGatesDialAndCancelIsNotAFailure(t *testing.T) {
	var opens atomic.Int32
	op := &countOpener{n: &opens, err: errors.New("dial refused")}
	br := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = op
	cfg.CircuitBreaker = br
	proc, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(proc, &core.PassthroughProcessor{})
	session, _ := core.NewSession(32, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if err := proc.ensureStream(context.Background()); err == nil {
		t.Fatal("want dial error")
	}
	if opens.Load() != 1 || !br.WouldRefuse() {
		t.Fatalf("opens=%d refuse=%v: one failure should open the breaker", opens.Load(), br.WouldRefuse())
	}
	if err := proc.ensureStream(context.Background()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	if opens.Load() != 1 {
		t.Fatalf("open breaker still dialed (%d opens)", opens.Load())
	}

	// A closed breaker plus a session that is already gone: the failed open is not counted.
	br2 := resilience.NewBreaker(resilience.Config{FailureThreshold: 1, Cooldown: time.Hour})
	cfg.CircuitBreaker = br2
	proc2, _ := NewSTTProcessor(cfg)
	pipe2, _ := core.NewPipeline(proc2, &core.PassthroughProcessor{})
	s2, _ := core.NewSession(33, pipe2, core.DefaultResourceBudget())
	_ = s2.Start()
	_ = s2.Close()
	_ = proc2.ensureStream(context.Background())
	if br2.WouldRefuse() {
		t.Fatal("a cancelled open tripped the breaker")
	}
}

type countOpener struct {
	n   *atomic.Int32
	err error
}

func (o *countOpener) OpenStream(ctx context.Context, cfg STTConfig) (STTStream, error) {
	o.n.Add(1)
	return nil, o.err
}

// transcript.done after a speech_final must not re-emit the utterance (a late duplicate final);
// after an open utterance it still finalizes it.
func TestXAISTT_DoneFinalizesOnlyAnOpenUtterance(t *testing.T) {
	run := func(msgs ...*STTMessage) (finals int) {
		ms := &mockSTTStream{messages: append([]*STTMessage{{Type: "transcript.created"}}, msgs...)}
		cfg := DefaultSTTConfig()
		cfg.StreamOpener = &mockSTTOpener{stream: ms}
		proc, _ := NewSTTProcessor(cfg)
		sink := &core.PassthroughProcessor{}
		pipe, _ := core.NewPipeline(proc, sink)
		session, _ := core.NewSession(34, pipe, core.DefaultResourceBudget())
		_ = session.Start()
		defer session.Close()
		_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})
		time.Sleep(400 * time.Millisecond)
		for _, f := range sink.GetFrames() {
			if c, ok := f.(*core.TranscriptCandidateFrame); ok && c.EndOfUtterance {
				finals++
			}
		}
		return finals
	}
	if n := run(&STTMessage{Type: "transcript.partial", Text: "hi", IsFinal: true, SpeechFinal: true}, &STTMessage{Type: "transcript.done", Text: "hi"}); n != 1 {
		t.Fatalf("settled utterance + done: %d end-of-utterance candidates, want 1", n)
	}
	if n := run(&STTMessage{Type: "transcript.partial", Text: "hi"}, &STTMessage{Type: "transcript.done", Text: "hi there"}); n != 1 {
		t.Fatalf("open utterance + done: %d end-of-utterance candidates, want 1", n)
	}
}
