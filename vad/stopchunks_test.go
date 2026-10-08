package vad

import (
	"testing"
	"time"

	"govoice/core"
)

type stopEmitter struct{ stops int }

func (e *stopEmitter) Push(f core.Frame) error {
	if _, ok := f.(*core.UserStoppedSpeakingFrame); ok {
		e.stops++
	}
	return nil
}
func (e *stopEmitter) PushDirection(f core.Frame, _ core.Direction) error { return e.Push(f) }

// The speech stop fires after exactly StopChunks quiet chunks, not before.
func TestDetectHonorsStopChunks(t *testing.T) {
	for _, n := range []int{9, 16, 25, 63} {
		cfg := DefaultProcessorConfig()
		cfg.StopChunks = n
		p := NewProcessor(nil, cfg)
		e := &stopEmitter{}
		for i := 0; i < cfg.StartChunks; i++ {
			_ = p.detect(0.9, 0, time.Now(), e, nil)
		}
		for i := 0; i < n-1; i++ {
			_ = p.detect(0.0, 0, time.Now(), e, nil)
		}
		if e.stops != 0 {
			t.Fatalf("n=%d stopped early", n)
		}
		_ = p.detect(0.0, 0, time.Now(), e, nil)
		if e.stops != 1 {
			t.Fatalf("n=%d stops=%d, want 1", n, e.stops)
		}
	}
}
