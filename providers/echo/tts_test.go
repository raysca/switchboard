package echo

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/providers/google"
)

var _ google.StreamingTTSClient = (*TTSClient)(nil)

func peak(pcm []byte) int {
	m := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
		m = max(m, int(math.Abs(float64(v))))
	}
	return m
}

func TestPCMLengthFollowsText(t *testing.T) {
	if got := len(pcm(strings.Repeat("a", 30), 24000, 15, false)); got != 48000*2 {
		t.Fatalf("30 runes at 15 cps, 24 kHz: %d bytes, want %d", got, 48000*2)
	}
	if got := len(pcm("hello", 24000, 0, false)); got != 24000/50*2*2 {
		t.Fatalf("instant is two 20 ms frames: %d bytes", got)
	}
	if got := len(pcm("", 24000, 15, false)); got < 24000/50*2 {
		t.Fatalf("empty text still makes a frame: %d bytes", got)
	}
}

func TestPCMToneIsQuietAndAudible(t *testing.T) {
	if p := peak(pcm("hello world", 24000, 15, true)); p == 0 || p > 3000 {
		t.Fatalf("audible peak = %d, want in (0, 3000]", p)
	}
	if p := peak(pcm("hello world", 24000, 15, false)); p != 0 {
		t.Fatalf("silent peak = %d", p)
	}
}

func TestUnaryTTSWaitsThenReturnsAllAudio(t *testing.T) {
	c := NewTTSClient(TTSFigures{FirstByte: 40 * time.Millisecond, FirstBytePerChar: time.Millisecond, CharsPerSecond: 15}, false)
	start := time.Now()
	audio, err := c.SynthesizeSpeech(context.Background(), strings.Repeat("a", 20), "v", 24000)
	if err != nil || len(audio) == 0 {
		t.Fatalf("audio=%d err=%v", len(audio), err)
	}
	if d := time.Since(start); d < 60*time.Millisecond {
		t.Fatalf("returned after %v, want >= 40ms + 20ms", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SynthesizeSpeech(ctx, "x", "v", 24000); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call: %v", err)
	}
}

func TestStreamingTTSDeliversTwentyMillisecondChunks(t *testing.T) {
	c := NewTTSClient(TTSFigures{CharsPerSecond: 15}, false)
	var sizes []int
	err := c.StreamSynthesize(context.Background(), []string{"Hello there, friend."}, "v", 24000, func(b []byte) error {
		sizes = append(sizes, len(b))
		return nil
	})
	if err != nil || len(sizes) < 2 {
		t.Fatalf("chunks=%v err=%v", sizes, err)
	}
	for _, n := range sizes[:len(sizes)-1] {
		if n != 960 {
			t.Fatalf("chunk of %d bytes, want 960 (20 ms at 24 kHz)", n)
		}
	}
	boom := errors.New("boom")
	if err := c.StreamSynthesize(context.Background(), []string{"hello"}, "v", 24000, func([]byte) error { return boom }); err != boom {
		t.Fatalf("onAudio's error must come back unchanged: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.StreamSynthesize(ctx, []string{"hello"}, "v", 24000, func([]byte) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stream: %v", err)
	}
}

// ttsRun drives a real Google TTS processor built around the echo client.
func ttsRun(t *testing.T, proc core.Processor, text string) (audio int) {
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
	_ = sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: 1, UnitID: 1, Text: text, IsFinal: true})
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		final := false
		audio = 0
		for _, f := range sink.GetFrames() {
			if v, ok := f.(*core.OutputAudioFrame); ok {
				audio++
				final = final || v.IsFinal
			}
		}
		if final {
			return
		}
	}
	t.Fatalf("no final audio frame: %d frames", audio)
	return
}

func TestRealStreamingProcessorSpeaksThroughEchoClient(t *testing.T) {
	cfg := google.DefaultTTSConfig()
	cfg.Provider = "echo"
	cfg.Client = NewTTSClient(TTSFigures{FirstByte: 20 * time.Millisecond, CharsPerSecond: 15}, false)
	proc, err := google.NewStreamingTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if audio := ttsRun(t, proc, "Hello there, how can I help?"); audio < 80 {
		t.Fatalf("%d audio frames for ~1.8 s of speech", audio)
	}
}

func TestRealUnaryProcessorSpeaksThroughEchoClientAndSplitsLongUnits(t *testing.T) {
	cfg := google.DefaultTTSConfig()
	cfg.Provider = "echo"
	cfg.Voice = "en-US-Journey-F"
	cfg.MaxSynthesisUnitChars = 40
	counting := &countingTTS{TTSClient: NewTTSClient(TTSFigures{CharsPerSecond: 15}, false)}
	cfg.Client = counting
	proc, err := google.NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("Hello there, how can I help you today? ", 3) // over 40 runes: the real processor splits it
	ttsRun(t, proc, long)
	if n := counting.calls.Load(); n < 2 {
		t.Fatalf("the real processor should split a %d-rune unit into several client calls, got %d", len([]rune(long)), n)
	}
}

type countingTTS struct {
	*TTSClient
	calls atomic.Int32
}

func (c *countingTTS) SynthesizeSpeech(ctx context.Context, text, voice string, rate int) ([]byte, error) {
	c.calls.Add(1)
	return c.TTSClient.SynthesizeSpeech(ctx, text, voice, rate)
}
