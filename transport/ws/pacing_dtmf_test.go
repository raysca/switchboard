package ws_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/transport/ws"
)

// DTMF tone audio is written like any other audio but never reported as bot speech.
func TestPacedWriterNonSpeechAudioDoesNotStartSpeaking(t *testing.T) {
	pacer := ws.NewPacedWriter(ws.PacerConfig{MaxQueueDuration: time.Second, MaxQueueBytes: 8192, WriteTimeout: time.Second, EnablePacing: true},
		func(context.Context, []byte) error { return nil })
	var transitions atomic.Int32
	pacer.OnSpeakingState = func(core.SpeakingStateFrame) { transitions.Add(1) }
	done := make(chan struct{}, 2)
	pacer.OnWriteComplete = func(*core.OutputAudioFrame, error) { done <- struct{}{} }

	tone := outputFrame(1, 1, core.OutputInterruptible)
	tone.Turn, tone.UnitID, tone.NonSpeech = "", 0, true
	if err := pacer.Enqueue(tone); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- pacer.Run(ctx) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tone frame was not written")
	}
	time.Sleep(50 * time.Millisecond)
	if n := transitions.Load(); n != 0 {
		t.Fatalf("%d speaking transitions for tone audio", n)
	}
	cancel()
	pacer.Close()
	<-runDone
}
