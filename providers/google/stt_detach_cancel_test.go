package google

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type ctxRecordingOpener struct {
	mockSTTOpener
	ctxs []context.Context
}

func (o *ctxRecordingOpener) OpenStream(ctx context.Context, cfg STTConfig) (STTStream, error) {
	o.mu.Lock()
	o.ctxs = append(o.ctxs, ctx)
	o.mu.Unlock()
	return o.mockSTTOpener.OpenStream(ctx, cfg)
}

// A detached stream only gets CloseSend; once the provider ends it, its
// context must be released rather than held until the session closes.
func TestGoogleSTTDetachedStreamContextIsCancelledWhenItEnds(t *testing.T) {
	opener := &ctxRecordingOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(proc, &core.PassthroughProcessor{})
	session, _ := core.NewSession(35, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()

	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	_ = session.Post(&core.UserStoppedSpeakingFrame{})
	deadline := time.Now().Add(time.Second)
	for {
		opener.mu.Lock()
		n := len(opener.streams)
		opener.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream did not open")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the stop frame detach it
	opener.mu.Lock()
	st, ctx := opener.streams[0], opener.ctxs[0]
	opener.mu.Unlock()
	_ = st.Close() // provider ends the half-closed stream

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("detached stream's context was never cancelled")
	}
}

type failCloseSendStream struct{ *mockSTTStream }

func (s failCloseSendStream) CloseSend() error { return errors.New("close send failed") }

func TestGoogleSTTHalfCloseErrorCancelsAndReports(t *testing.T) {
	cfg := DefaultSTTConfig()
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var errs int
	pipe, err := core.NewPipeline(proc, &core.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(36, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	session.OnOutput = func(f core.Frame) {
		if _, ok := f.(*core.ErrorFrame); ok {
			errs++
		}
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, cancel := context.WithCancel(context.Background())
	st := &activeStream{ctx: ctx, cancel: cancel, stream: failCloseSendStream{newMockSTTStream()}, turnID: "turn-1-1", gen: 1}
	proc.halfClose(st)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("halfClose did not cancel after CloseSend error")
	}
	deadline := time.Now().Add(time.Second)
	for errs == 0 {
		if time.Now().After(deadline) {
			t.Fatal("halfClose did not emit an ErrorFrame")
		}
		time.Sleep(time.Millisecond)
	}
	okCtx, okCancel := context.WithCancel(context.Background())
	defer okCancel()
	proc.halfClose(&activeStream{ctx: okCtx, cancel: okCancel, stream: newMockSTTStream(), turnID: "ok", gen: 1})
	select {
	case <-okCtx.Done():
		t.Fatal("halfClose cancelled a stream whose CloseSend succeeded")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestGoogleSTTDetachedFinalSurvivesLaterGeneration(t *testing.T) {
	opener := &mockSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got string
	pipe, err := core.NewPipeline(proc, &core.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(37, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	session.OnOutput = func(f core.Frame) {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok && c.Final {
			mu.Lock()
			got = c.Text
			mu.Unlock()
		}
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	deadline := time.Now().Add(time.Second)
	for {
		opener.mu.Lock()
		n := len(opener.streams)
		opener.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream did not open")
		}
		time.Sleep(time.Millisecond)
	}
	_ = session.Post(&core.UserStoppedSpeakingFrame{})
	time.Sleep(20 * time.Millisecond)
	session.IncrementGeneration()
	opener.streams[0].PushResult(&STTResult{Transcript: "two plus two", IsFinal: true})
	deadline = time.Now().Add(time.Second)
	for {
		mu.Lock()
		text := got
		mu.Unlock()
		if text == "two plus two" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached final = %q, want two plus two", text)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGoogleSTTActiveStreamDropsStaleGeneration(t *testing.T) {
	opener := &mockSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got int
	pipe, err := core.NewPipeline(proc, &core.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(38, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	session.OnOutput = func(f core.Frame) {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok && c.Final {
			got++
		}
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	deadline := time.Now().Add(time.Second)
	for {
		opener.mu.Lock()
		n := len(opener.streams)
		opener.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream did not open")
		}
		time.Sleep(time.Millisecond)
	}
	session.IncrementGeneration()
	opener.streams[0].PushResult(&STTResult{Transcript: "stale", IsFinal: true})
	time.Sleep(50 * time.Millisecond)
	if got != 0 {
		t.Fatalf("active stream delivered %d finals after the generation moved", got)
	}
}
