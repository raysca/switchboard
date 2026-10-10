package vad

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

type frameLog struct{ starts, stops, muted int }

func (l *frameLog) Push(f core.Frame) error {
	switch f.(type) {
	case *core.UserStartedSpeakingFrame:
		l.starts++
	case *core.UserStoppedSpeakingFrame:
		l.stops++
	case *core.InputMutedStateFrame:
		l.muted++
	}
	return nil
}
func (l *frameLog) PushDirection(f core.Frame, _ core.Direction) error { return l.Push(f) }

func speak(p *Processor, l *frameLog, chunks int) {
	for i := 0; i < chunks; i++ {
		_ = p.detect(0.9, 0, time.Now(), l, nil)
	}
}

// Muting mid-speech ends the segment once; results that land while muted
// cannot start a new one; unmuting restores detection.
func TestMuteEndsOpenSegmentAndBlocksRestart(t *testing.T) {
	cfg := DefaultProcessorConfig()
	p := NewProcessor(nil, cfg)
	l := &frameLog{}
	speak(p, l, cfg.StartChunks)
	if l.starts != 1 {
		t.Fatalf("starts=%d before mute, want 1", l.starts)
	}
	if err := p.ProcessFrame(context.Background(), &core.InputMutedStateFrame{Muted: true}, core.Downstream, l); err != nil {
		t.Fatal(err)
	}
	if l.stops != 1 || l.muted != 1 {
		t.Fatalf("after mute: stops=%d forwarded=%d, want 1 and 1", l.stops, l.muted)
	}
	speak(p, l, cfg.StartChunks*2) // in-flight results arriving while muted
	if l.starts != 1 {
		t.Fatal("a result arriving while muted started a segment")
	}
	_ = p.ProcessFrame(context.Background(), &core.InputMutedStateFrame{Muted: false}, core.Downstream, l)
	speak(p, l, cfg.StartChunks)
	if l.starts != 2 {
		t.Fatalf("starts=%d after unmute, want 2", l.starts)
	}
}

// Muting while silent emits no stop.
func TestMuteWhileSilentEmitsNoStop(t *testing.T) {
	p := NewProcessor(nil, DefaultProcessorConfig())
	l := &frameLog{}
	_ = p.ProcessFrame(context.Background(), &core.InputMutedStateFrame{Muted: true}, core.Downstream, l)
	if l.stops != 0 {
		t.Fatalf("stops=%d, want 0", l.stops)
	}
}

// Audio queued before the mute but delivered after it is not buffered.
func TestMuteIgnoresQueuedAudio(t *testing.T) {
	p := NewProcessor(nil, DefaultProcessorConfig())
	l := &frameLog{}
	_ = p.ProcessFrame(context.Background(), &core.InputMutedStateFrame{Muted: true}, core.Downstream, l)
	_ = p.ProcessFrame(context.Background(), &core.InputAudioFrame{Samples: make([]byte, ChunkSamples*4)}, core.Downstream, l)
	if len(p.sampleBuf) != 0 || len(p.pending) != 0 {
		t.Fatalf("muted audio buffered: samples=%d pending=%d", len(p.sampleBuf), len(p.pending))
	}
}
