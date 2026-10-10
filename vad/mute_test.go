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

// Unmute starts detection fresh: no mid-speech recurrent state or context
// survives the mute, and a result for a chunk submitted before the mute is
// discarded rather than restoring that state.
func TestUnmuteStartsWithFreshDetectorState(t *testing.T) {
	cfg := DefaultProcessorConfig()
	p := NewProcessor(nil, cfg)
	l := &frameLog{}
	speak(p, l, cfg.StartChunks)
	for i := range p.state {
		p.state[i] = 7 // mid-speech recurrent state
	}
	p.checkpointState = p.state
	for i := range p.ctx {
		p.ctx[i] = 0.5
	}
	p.busy = true // a chunk is in flight at the mute
	ctx := context.Background()
	_ = p.ProcessFrame(ctx, &core.InputMutedStateFrame{Muted: true}, core.Downstream, l)
	_ = p.ProcessFrame(ctx, &core.InputMutedStateFrame{Muted: false}, core.Downstream, l)
	var zeroState [StateLen]float32
	if p.state != zeroState || p.checkpointState != zeroState {
		t.Fatal("unmute kept the pre-mute recurrent state")
	}
	for _, v := range p.ctx {
		if v != 0 {
			t.Fatal("unmute kept the pre-mute audio context")
		}
	}
	var stale [StateLen]float32
	for i := range stale {
		stale[i] = 7
	}
	_ = p.ProcessFrame(ctx, &vadResultFrame{j: &Job{State: stale, Prob: 0.99}}, core.Downstream, l)
	if p.state != zeroState || p.checkpointState != zeroState {
		t.Fatal("a pre-mute result landing after unmute restored mid-speech state")
	}
	if p.above != 0 || p.busy {
		t.Fatalf("pre-mute result counted: above=%d busy=%t", p.above, p.busy)
	}
}

// An unmute while already unmuted (any client may send one) changes nothing:
// it must not reset detection in the middle of speech.
func TestRedundantUnmuteKeepsDetectorState(t *testing.T) {
	p := NewProcessor(nil, DefaultProcessorConfig())
	l := &frameLog{}
	for i := range p.state {
		p.state[i] = 7
	}
	for i := range p.ctx {
		p.ctx[i] = 0.5
	}
	_ = p.ProcessFrame(context.Background(), &core.InputMutedStateFrame{Muted: false}, core.Downstream, l)
	if p.state[0] != 7 || p.ctx[0] != 0.5 {
		t.Fatal("a redundant unmute reset the detector")
	}
	if l.muted != 1 {
		t.Fatalf("state frame forwarded %d times, want 1", l.muted)
	}
}
