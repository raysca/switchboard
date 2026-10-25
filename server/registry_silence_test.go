package server

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"govoice/providers/google"
)

func TestSilenceSTTStreamNeverTranscribesAndEndsWhenTheAudioEnds(t *testing.T) {
	s, err := silenceSTTOpener{}.OpenStream(t.Context(), google.STTConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(make([]byte, 640)); err != nil { // audio a client sends anyway is ignored
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() { _, err := s.Recv(); got <- err }()
	select {
	case err := <-got:
		t.Fatalf("Recv returned before the audio ended: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	_ = s.CloseSend()
	if err := <-got; !errors.Is(err, io.EOF) {
		t.Fatalf("after CloseSend Recv = %v, want io.EOF (no transcript)", err)
	}
}

func TestSilenceSTTStreamCloseAndContextEndTheWait(t *testing.T) {
	for name, end := range map[string]func(cancel context.CancelFunc, s google.STTStream){
		"close":   func(_ context.CancelFunc, s google.STTStream) { _ = s.Close() },
		"context": func(cancel context.CancelFunc, _ google.STTStream) { cancel() },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s, _ := silenceSTTOpener{}.OpenStream(ctx, google.STTConfig{})
			got := make(chan error, 1)
			go func() { _, err := s.Recv(); got <- err }()
			end(cancel, s)
			select {
			case err := <-got:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Recv = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Recv did not return")
			}
		})
	}
}

func TestSilenceTTSIsOneSilentFramePerUnitWithNoDelay(t *testing.T) {
	const rate = 24000
	c := silenceTTSClient{}
	start := time.Now()
	audio, err := c.SynthesizeSpeech(t.Context(), "hello there", "none", rate)
	if err != nil || len(audio) != rate/50*2 {
		t.Fatalf("unary: %d bytes, err %v (want one 20 ms frame)", len(audio), err)
	}
	var frames int
	err = c.StreamSynthesize(t.Context(), []string{"one", "two", "three"}, "none", rate, func(pcm []byte) error {
		frames++
		for _, b := range pcm {
			if b != 0 {
				t.Fatal("silence has a non-zero sample")
			}
		}
		return nil
	})
	if err != nil || frames != 3 {
		t.Fatalf("stream: %d frames, err %v (want one per unit)", frames, err)
	}
	if time.Since(start) > 2*time.Second { // no sleeps in the backend; the bound only catches a regression to pacing
		t.Fatalf("synthesis took %v: silence must not wait", time.Since(start))
	}
}

func TestSilenceTTSStopsOnCancelAndPassesTheCallbackErrorThrough(t *testing.T) {
	c := silenceTTSClient{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.SynthesizeSpeech(ctx, "x", "none", 24000); !errors.Is(err, context.Canceled) {
		t.Fatalf("unary on a cancelled context = %v", err)
	}
	if err := c.StreamSynthesize(ctx, []string{"x"}, "none", 24000, func([]byte) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream on a cancelled context = %v", err)
	}
	boom := errors.New("write failed")
	if err := c.StreamSynthesize(t.Context(), []string{"x", "y"}, "none", 24000, func([]byte) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("stream must return the callback's error unchanged, got %v", err)
	}
}

func TestSilenceIsRegisteredWithBreakersOfItsOwn(t *testing.T) {
	r := NewRegistry()
	for _, stage := range []string{"stt", "tts"} {
		if !r.Supports(silenceProvider, stage) {
			t.Errorf("silence.%s is not registered", stage)
		}
	}
	if r.Supports(silenceProvider, "llm") {
		t.Error("silence has no model stage")
	}
	if r.breaker("stt", silenceProvider) == nil || r.breaker("stt", silenceProvider) == r.breaker("stt", "echo") {
		t.Error("silence STT must have its own breaker, found by its provider name")
	}
	if r.breaker("tts", silenceProvider) == nil || r.breaker("tts", silenceProvider) == r.breaker("tts", "echo") {
		t.Error("silence TTS must have its own breaker, found by its provider name")
	}
}
