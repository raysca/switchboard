package ws

import (
	"context"
	"errors"
	"testing"
	"time"

	"govoice/core"
)

// A session ending while bot audio is backpressured is a hang-up, not a slow
// client; a live session whose client stops draining still is one.
func TestEnqueueOutputHangUpIsNotSlowClient(t *testing.T) {
	cfg := PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 960, MaxQueueFrames: 1, WriteTimeout: 60 * time.Millisecond, EnablePacing: true}
	frame := func() *core.OutputAudioFrame {
		return &core.OutputAudioFrame{Samples: make([]byte, 960), Encoding: core.EncodingPCM16LE, SampleRate: 24000, Channels: 1, Gen: 1, Turn: "t", UnitID: 1}
	}
	c := &Connection{cfg: ServerConfig{Pacer: cfg}, pacer: NewPacedWriter(cfg, func(context.Context, []byte) error { return nil })} // Run never started: nothing drains
	if err := c.enqueueOutput(context.Background(), frame()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if err := c.enqueueOutput(ctx, frame()); err != nil {
		t.Fatalf("hang-up during backpressure returned %v", err)
	}
	if err := c.enqueueOutput(context.Background(), frame()); !errors.Is(err, ErrSlowClient) {
		t.Fatalf("undrained live session: %v, want ErrSlowClient", err)
	}
}

// The output gate holds the producer (not the run loop) while the pacer is
// full and releases it as soon as audio drains, is flushed, or the writer
// closes; it gives up after its timeout so Enqueue's own budget still rules.
func TestWaitRoomTracksQueuedAudio(t *testing.T) {
	cfg := PacerConfig{MaxQueueDuration: 100 * time.Millisecond, MaxQueueBytes: 96000, MaxQueueFrames: 64, WriteTimeout: time.Second, EnablePacing: true}
	w := NewPacedWriter(cfg, func(context.Context, []byte) error { return nil }) // Run never started
	// 20 ms at 24 kHz
	frame := func(gen uint64, unit uint64) *core.OutputAudioFrame {
		return &core.OutputAudioFrame{Samples: make([]byte, 960), Encoding: core.EncodingPCM16LE, SampleRate: 24000, Channels: 1, Gen: gen, Turn: "t", UnitID: unit}
	}
	w.SetActiveGeneration(1)
	for i := 0; i < 4; i++ { // 80 of 100 ms
		if err := w.Enqueue(frame(1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	w.WaitRoom(context.Background(), 20*time.Millisecond, time.Second) // fits
	w.WaitRoom(context.Background(), 60*time.Millisecond, 30*time.Millisecond)
	if d := time.Since(start); d < 25*time.Millisecond {
		t.Fatalf("WaitRoom returned after %v with no room", d)
	}

	done := make(chan struct{})
	go func() { w.WaitRoom(context.Background(), 60*time.Millisecond, 5*time.Second); close(done) }()
	time.Sleep(10 * time.Millisecond)
	if err := w.Flush(context.Background(), 1); err != nil { // barge-in drops the queue
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flush did not release the waiting producer")
	}

	w.SetActiveGeneration(2)
	for i := 0; i < 5; i++ {
		_ = w.Enqueue(frame(2, 2))
	}
	done = make(chan struct{})
	go func() { w.WaitRoom(context.Background(), 20*time.Millisecond, 5*time.Second); close(done) }()
	time.Sleep(10 * time.Millisecond)
	w.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close did not release the waiting producer")
	}
}

// The connection closes the pacer before it closes the session (client hang-up):
// bot audio or a turn-final marker still in flight must not fail the session.
func TestOutputAfterPacerClosedIsHangUp(t *testing.T) {
	cfg := PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 96000, MaxQueueFrames: 64, WriteTimeout: time.Second, EnablePacing: true}
	c := &Connection{cfg: ServerConfig{Pacer: cfg, FixedPlayerRate: 24000}, pacer: NewPacedWriter(cfg, func(context.Context, []byte) error { return nil })}
	c.pacer.Close()
	audio := &core.OutputAudioFrame{Samples: make([]byte, 960), Encoding: core.EncodingPCM16LE, SampleRate: 24000, Channels: 1, Gen: 1, Turn: "t", UnitID: 1}
	if err := c.ProcessFrame(context.Background(), audio, core.Downstream, nopEmitter{}); err != nil {
		t.Fatalf("audio after hang-up: %v", err)
	}
	if err := c.ProcessFrame(context.Background(), &core.OutputTurnFinalFrame{Turn: "t", Gen: 1, UnitID: 1}, core.Downstream, nopEmitter{}); err != nil {
		t.Fatalf("turn-final marker after hang-up: %v", err)
	}
}

type nopEmitter struct{}

func (nopEmitter) Push(core.Frame) error                          { return nil }
func (nopEmitter) PushDirection(core.Frame, core.Direction) error { return nil }
