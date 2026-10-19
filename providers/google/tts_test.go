package google

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
)

func TestTTS_StripsWAVContainer(t *testing.T) {
	// Pinned requirement: "TTS bytes begin with PCM samples, not a WAV container".
	// LINEAR16 responses are WAV; the real client strips the container.
	pcm := []byte{1, 2, 3, 4}
	wav := append([]byte("RIFF\x00\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data\x04\x00\x00\x00"), pcm...)
	got, err := stripWAV(wav)
	if err != nil || !bytes.Equal(got, pcm) {
		t.Fatalf("stripWAV = %v, %v; want %v", got, err, pcm)
	}
	if got, _ := stripWAV(pcm); !bytes.Equal(got, pcm) {
		t.Fatalf("headerless PCM changed: %v", got)
	}
	if _, err := stripWAV(wav[:len(wav)-1]); err == nil {
		t.Fatal("truncated WAV data accepted")
	}
}

func TestTTS_UnitOverLimitIsSynthesizedInPieces(t *testing.T) {
	// Pinned requirement: max 250 characters per provider request. A longer
	// unit is split, synthesized in order and played as one unit.
	cfg := DefaultTTSConfig()
	cfg.MaxSynthesisUnitChars = 250
	mockClient := &mockTTSClient{}
	cfg.Client = mockClient

	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}

	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)
	var mu sync.Mutex
	var finals, turnFinals int
	session.OnOutput = func(f core.Frame) {
		if a, ok := f.(*core.OutputAudioFrame); ok {
			mu.Lock()
			if a.IsFinal {
				finals++
			}
			if a.TurnFinal {
				turnFinals++
			}
			mu.Unlock()
		}
	}
	_ = session.Start()
	defer session.Close()

	words := strings.Repeat("word ", 60) // 300 runes
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "turn-1", Gen: 1, UnitID: 1, Text: words, IsFinal: true})
	time.Sleep(100 * time.Millisecond)
	if err := session.Err(); err != nil {
		t.Fatalf("a long unit failed the session: %v", err)
	}
	mockClient.mu.Lock()
	calls := append([]string(nil), mockClient.calls...)
	mockClient.mu.Unlock()
	if len(calls) != 2 || calls[0]+" "+calls[1] != strings.TrimSpace(words) || len([]rune(calls[0])) > 250 || strings.HasSuffix(calls[0], "wor") {
		t.Fatalf("pieces = %q", calls)
	}
	mu.Lock()
	defer mu.Unlock()
	if finals != 1 || turnFinals != 1 {
		t.Fatalf("unit finals = %d, turn finals = %d; want one of each", finals, turnFinals)
	}
}

func TestTTS_HeaderlessPCM20msChunking(t *testing.T) {
	// 24000 Hz, 16-bit mono. 20ms chunk = 24000 * 2 * 0.02 = 960 bytes.
	// Provide 2400 bytes = 2 full chunks (960*2 = 1920) + 1 partial chunk (480 bytes).
	synthBytes := make([]byte, 2400)
	for i := range synthBytes {
		synthBytes[i] = byte(i % 128)
	}

	mockClient := &mockTTSClient{
		synthesizeFn: func(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error) {
			return synthBytes, nil
		},
	}

	cfg := DefaultTTSConfig()
	cfg.SampleRate = 24000
	cfg.Client = mockClient

	proc, _ := NewTTSProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)

	var frames []core.Frame
	var mu sync.Mutex
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		frames = append(frames, f)
	}

	_ = session.Start()
	defer session.Close()

	su := &core.SynthesisUnitFrame{
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  10,
		Text:    "Valid synthesis unit.",
		IsFinal: true,
	}
	_ = session.Post(su)

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// Verify order: SynthesisUnitFrame -> TTSStartedFrame -> OutputAudioFrames... -> TTSStoppedFrame
	var started *core.TTSStartedFrame
	var stopped *core.TTSStoppedFrame
	var audioFrames []*core.OutputAudioFrame

	for _, f := range frames {
		switch frame := f.(type) {
		case *core.TTSStartedFrame:
			started = frame
		case *core.TTSStoppedFrame:
			stopped = frame
		case *core.OutputAudioFrame:
			audioFrames = append(audioFrames, frame)
		}
	}

	if started == nil {
		t.Fatal("expected TTSStartedFrame, got none")
	}
	if started.UnitID != 10 {
		t.Fatalf("expected started UnitID 10, got %d", started.UnitID)
	}

	if len(audioFrames) != 3 {
		t.Fatalf("expected 3 audio chunks, got %d", len(audioFrames))
	}

	// Chunk 1: 960 bytes, not final
	if len(audioFrames[0].Samples) != 960 || audioFrames[0].IsFinal {
		t.Fatalf("chunk 0 invalid: len=%d isFinal=%v", len(audioFrames[0].Samples), audioFrames[0].IsFinal)
	}
	if audioFrames[0].SampleRate != 24000 {
		t.Fatalf("expected 24000 sample rate, got %d", audioFrames[0].SampleRate)
	}

	// Chunk 2: 960 bytes, not final
	if len(audioFrames[1].Samples) != 960 || audioFrames[1].IsFinal {
		t.Fatalf("chunk 1 invalid: len=%d isFinal=%v", len(audioFrames[1].Samples), audioFrames[1].IsFinal)
	}

	// Chunk 3: 480 bytes, final
	if len(audioFrames[2].Samples) != 480 || !audioFrames[2].IsFinal {
		t.Fatalf("chunk 2 invalid: len=%d isFinal=%v", len(audioFrames[2].Samples), audioFrames[2].IsFinal)
	}
	if audioFrames[0].TurnFinal || audioFrames[1].TurnFinal || !audioFrames[2].TurnFinal {
		t.Fatalf("whole-turn final marker was not limited to the last chunk: %v %v %v", audioFrames[0].TurnFinal, audioFrames[1].TurnFinal, audioFrames[2].TurnFinal)
	}

	// Reassembled samples must match exactly
	var reassembled []byte
	for _, af := range audioFrames {
		reassembled = append(reassembled, af.Samples...)
	}
	if !bytes.Equal(reassembled, synthBytes) {
		t.Fatal("reassembled audio does not match synthesized bytes!")
	}

	if stopped == nil {
		t.Fatal("expected TTSStoppedFrame, got none")
	}
	if stopped.UnitID != 10 {
		t.Fatalf("expected stopped UnitID 10, got %d", stopped.UnitID)
	}

	// Verify UsageMetricsFrame emission and correlation
	var usageFrames []*core.UsageMetricsFrame
	for _, f := range frames {
		if uf, ok := f.(*core.UsageMetricsFrame); ok {
			usageFrames = append(usageFrames, uf)
		}
	}
	if len(usageFrames) != 1 {
		t.Fatalf("expected 1 UsageMetricsFrame from TTS, got %d", len(usageFrames))
	}
	uf := usageFrames[0]
	if uf.Turn != "turn-1" || uf.Gen != 1 || uf.Provider != "google" || uf.Service != "tts" {
		t.Fatalf("usage frame correlation mismatch: %+v", uf)
	}
	if uf.Characters == nil || *uf.Characters != int64(len("Valid synthesis unit.")) {
		t.Fatalf("expected characters %d, got %v", len("Valid synthesis unit."), uf.Characters)
	}
	if uf.AudioDurationMs == nil || *uf.AudioDurationMs != 50 { // 2400 bytes / 2 = 1200 samples; 1200*1000/24000 = 50ms
		t.Fatalf("expected audio duration 50ms, got %v", uf.AudioDurationMs)
	}
	if uf.PromptTokens != nil || uf.TotalTokens != nil {
		t.Fatalf("token metrics must be absent for TTS: %+v", uf)
	}
}

func TestTTS_CancellationAndLateOutputFencing(t *testing.T) {
	// Synthesizes slowly, so an interruption cancels mid-flight
	synthStarted := make(chan struct{})
	mockClient := &mockTTSClient{
		synthesizeFn: func(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error) {
			close(synthStarted)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
				return make([]byte, 4800), nil
			}
		},
	}

	cfg := DefaultTTSConfig()
	cfg.Client = mockClient
	proc, _ := NewTTSProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)

	var audioCount int
	var mu sync.Mutex
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		if f.Type() == core.FrameOutputAudio {
			audioCount++
		}
	}

	_ = session.Start()
	defer session.Close()

	// Push synthesis unit for Gen 1
	_ = session.Post(&core.SynthesisUnitFrame{
		Turn:    "turn-1",
		Gen:     1,
		UnitID:  1,
		Text:    "Interrupted text.",
		IsFinal: true,
	})

	// Wait for synthesis to start
	<-synthStarted

	// User barges in: push InterruptionFrame and advance generation
	_ = session.Post(&core.InterruptionFrame{Turn: "turn-1", Gen: 1})
	session.IncrementGeneration() // Gen = 2

	// Wait a bit
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// Verify no audio frames from old generation were delivered
	if audioCount != 0 {
		t.Fatalf("expected 0 audio frames delivered after interruption, got %d", audioCount)
	}
}

type directionEmitter struct {
	frame core.Frame
	dir   core.Direction
}

func (d *directionEmitter) Push(f core.Frame) error {
	return d.PushDirection(f, core.Downstream)
}

func (d *directionEmitter) PushDirection(f core.Frame, dir core.Direction) error {
	d.frame = f
	d.dir = dir
	return nil
}

func TestTTSForwardsUpstreamFlushAck(t *testing.T) {
	proc, err := NewTTSProcessor(DefaultTTSConfig())
	if err != nil {
		t.Fatal(err)
	}
	em := &directionEmitter{}
	frame := &core.FlushProcessedFrame{Gen: 1}
	if err := proc.ProcessFrame(context.Background(), frame, core.Upstream, em); err != nil {
		t.Fatal(err)
	}
	if em.dir != core.Upstream {
		t.Fatalf("flush ack direction = %v, want upstream", em.dir)
	}
	if em.frame != frame {
		t.Fatal("flush ack frame was replaced")
	}
}

// Idle prompts and goodbyes are synthesis units with generation 0 (never fenced by
// an interruption). The session generation starts at 1, so the post-synthesis
// staleness checks must exempt gen 0 or the prompt is silently never played.
func TestTTS_GenerationZeroUnitIsPlayed(t *testing.T) {
	cfg := DefaultTTSConfig()
	cfg.Client = &mockTTSClient{}
	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)
	var mu sync.Mutex
	var audio int
	session.OnOutput = func(f core.Frame) {
		if _, ok := f.(*core.OutputAudioFrame); ok {
			mu.Lock()
			audio++
			mu.Unlock()
		}
	}
	_ = session.Start()
	defer session.Close()
	if session.CurrentGeneration() == 0 {
		t.Fatal("test premise: session generation starts above 0")
	}
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "say-1", Gen: 0, UnitID: 1, Text: "Are you still there?", IsFinal: true})
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if audio == 0 {
		t.Fatal("generation-0 unit produced no audio")
	}
}

// A barge-in during a generation-0 unit (idle prompt) must still silence it: the
// generation fence exempts gen 0, so the cancelled unit context is what stops it.
func TestTTS_GenerationZeroUnitStopsOnInterruption(t *testing.T) {
	started := make(chan struct{})
	cfg := DefaultTTSConfig()
	cfg.Client = &mockTTSClient{synthesizeFn: func(ctx context.Context, _, _ string, _ int) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return make([]byte, 4800), nil // response landed just as the barge-in cancelled it
	}}
	proc, err := NewTTSProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)
	var audio atomic.Int32
	session.OnOutput = func(f core.Frame) {
		if _, ok := f.(*core.OutputAudioFrame); ok {
			audio.Add(1)
		}
	}
	_ = session.Start()
	defer session.Close()
	_ = session.Post(&core.SynthesisUnitFrame{Turn: "say-1", Gen: 0, UnitID: 1, Text: "Are you still there?", IsFinal: true})
	<-started
	_ = session.Post(&core.InterruptionFrame{Gen: session.CurrentGeneration()})
	time.Sleep(150 * time.Millisecond)
	if n := audio.Load(); n != 0 {
		t.Fatalf("interrupted generation-0 unit played %d audio frames", n)
	}
}
