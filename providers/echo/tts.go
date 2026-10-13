package echo

import (
	"context"
	"math"
	"time"
	"unicode/utf8"
)

// TTSClient is a simulated Google Text-to-Speech backend: the real google TTS processors run
// against it (google.StreamingTTSClient), so splitting, retries, pacing, usage and tracing are the real ones.
type TTSClient struct {
	f       TTSFigures
	audible bool
}

func NewTTSClient(f TTSFigures, audible bool) *TTSClient { return &TTSClient{f: f, audible: audible} }

func (c *TTSClient) delay(text string) time.Duration {
	return c.f.FirstByte + time.Duration(utf8.RuneCountInString(text))*c.f.FirstBytePerChar
}

// wait sleeps d unless ctx ends first.
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *TTSClient) SynthesizeSpeech(ctx context.Context, text, _ string, sampleRate int) ([]byte, error) {
	if err := wait(ctx, c.delay(text)); err != nil {
		return nil, err
	}
	return pcm(text, sampleRate, c.f.CharsPerSecond, c.audible), nil
}

func (c *TTSClient) StreamSynthesize(ctx context.Context, pieces []string, _ string, sampleRate int, onAudio func([]byte) error) error {
	chunk := frameSamples(sampleRate) * 2 // 20 ms of PCM16 mono, in bytes
	for i, piece := range pieces {
		if i == 0 {
			if err := wait(ctx, c.delay(piece)); err != nil {
				return err
			}
		}
		audio := pcm(piece, sampleRate, c.f.CharsPerSecond, c.audible)
		for len(audio) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := min(chunk, len(audio))
			if err := onAudio(audio[:n]); err != nil {
				return err
			}
			audio = audio[n:]
		}
	}
	return nil
}

// pcm is PCM16LE mono as long as the text takes to say: silence, or a quiet 220 Hz tone.
func pcm(text string, rate, charsPerSecond int, audible bool) []byte {
	samples := frameSamples(rate) * 2 // two frames
	if charsPerSecond > 0 {
		samples = max((utf8.RuneCountInString(text)*rate+charsPerSecond-1)/charsPerSecond, frameSamples(rate))
	}
	out := make([]byte, samples*2)
	if audible {
		for i := 0; i < samples; i++ {
			v := int16(3000 * math.Sin(2*math.Pi*220*float64(i)/float64(rate)))
			out[2*i], out[2*i+1] = byte(v), byte(v>>8)
		}
	}
	return out
}

// frameSamples is the samples in 20 ms of mono audio at rate.
func frameSamples(rate int) int { return rate / 50 }
