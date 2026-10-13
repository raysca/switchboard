package echo

import (
	"context"
	"io"
	"testing"
	"time"

	"govoice/providers/google"
)

// The google mimic must reproduce the measurements in docs/benchmarks/first-audio-latency.md:
// STT final about 1.2 s after the stop on chirp_2 (no interims) and about 0.5 s on latest_long
// (with interims), an 11-character unary TTS unit about 1.13 s to first audio, a streamed one
// about 0.4 s, the model's first word about 0.7 s.
func TestGoogleMimicReproducesTheBenchmarks(t *testing.T) {
	within := func(t *testing.T, name string, got, want time.Duration) {
		t.Helper()
		if got < want-50*time.Millisecond || got > want+400*time.Millisecond {
			t.Errorf("%s took %v, want about %v", name, got, want)
		}
	}
	sttFinal := func(model string) (final time.Duration, interims int) {
		s, _ := NewSTTOpener(MimicGoogle.STT(model), true).OpenStream(context.Background(), google.DefaultSTTConfig())
		defer s.Close()
		for i := 0; i < 40; i++ {
			_ = s.Send(loud())
		}
		_ = s.CloseSend()
		start := time.Now()
		for {
			rs, err := s.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rs {
				if r.IsFinal {
					final = time.Since(start)
				} else {
					interims++
				}
			}
		}
	}
	t.Run("chirp_2 finalizes about 1.2 s after the stop, without interims", func(t *testing.T) {
		t.Parallel()
		final, interims := sttFinal("chirp_2")
		within(t, "final", final, 1200*time.Millisecond)
		if interims != 0 {
			t.Errorf("%d interims on chirp_2", interims)
		}
	})
	t.Run("latest_long finalizes about 0.5 s after the stop, with interims", func(t *testing.T) {
		t.Parallel()
		final, interims := sttFinal("latest_long")
		within(t, "final", final, 500*time.Millisecond)
		if interims == 0 {
			t.Error("no interims on latest_long")
		}
	})
	t.Run("an 11-character unary unit takes about 1.13 s to first audio", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		_, _ = NewTTSClient(MimicGoogle.TTS(false), false).SynthesizeSpeech(context.Background(), "Hello there", "en-US-Journey-F", 24000)
		within(t, "unary first audio", time.Since(start), 1130*time.Millisecond)
	})
	t.Run("a Chirp 3 HD unit streams its first audio in about 0.4 s", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		var first time.Duration
		_ = NewTTSClient(MimicGoogle.TTS(true), false).StreamSynthesize(context.Background(), []string{"Hello there"}, "en-US-Chirp3-HD-Aoede", 24000, func([]byte) error {
			if first == 0 {
				first = time.Since(start)
			}
			return nil
		})
		within(t, "streamed first audio", first, 400*time.Millisecond)
	})
	t.Run("the model's first word comes in about 0.7 s", func(t *testing.T) {
		t.Parallel()
		c := NewLLMClient(LLMOptions{Figures: MimicGoogle.LLM()})
		start := time.Now()
		ch, _, _ := c.GenerateStream(context.Background(), "m", "", user("hi"))
		<-ch
		within(t, "first word", time.Since(start), 700*time.Millisecond)
	})
}
