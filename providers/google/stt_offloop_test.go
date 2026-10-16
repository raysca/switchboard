package google

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

type slowOpener struct {
	delay time.Duration
	st    *mockSTTStream
}

func (o slowOpener) OpenStream(ctx context.Context, _ STTConfig) (STTStream, error) {
	select {
	case <-time.After(o.delay):
		return o.st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type failingOpener struct{ opens atomic.Int32 }

func (o *failingOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	o.opens.Add(1)
	return nil, errors.New("dial refused")
}

// endingStream is a mock whose provider ends it (io.EOF) when end closes.
type endingStream struct {
	*mockSTTStream
	end chan struct{}
}

func (s endingStream) Recv() ([]*STTResult, error) {
	select {
	case res := <-s.results:
		return res, nil
	case <-s.end:
		return nil, io.EOF
	case <-s.closed:
		return nil, context.Canceled
	}
}

type endingOpener struct{ st endingStream }

func (o endingOpener) OpenStream(context.Context, STTConfig) (STTStream, error) { return o.st, nil }

func startGoogle(t *testing.T, opener STTStreamOpener) (*core.Session, *coretest.PassthroughProcessor) {
	t.Helper()
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(40, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, sink
}

func gwait(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func countFrames[T core.Frame](sink *coretest.PassthroughProcessor) (out []T) {
	for _, f := range sink.GetFrames() {
		if v, ok := f.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

// A1: a stream open longer than the 2 s frame watchdog runs off the run loop;
// the audio sent meanwhile reaches the stream in order once it opens.
func TestGoogleSTTOpenRunsOffTheRunLoop(t *testing.T) {
	st := newMockSTTStream()
	session, _ := startGoogle(t, slowOpener{3 * time.Second, st})
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	for b := byte(1); b <= 2; b++ {
		_ = session.Post(&core.InputAudioFrame{Samples: bytes.Repeat([]byte{b}, 640), Gen: 1})
	}
	time.Sleep(2500 * time.Millisecond)
	if err := session.Err(); err != nil {
		t.Fatalf("session failed while the open hung: %v", err)
	}
	gwait(t, "held audio sent", 3*time.Second, func() bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		return len(st.audioChunks) == 2
	})
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.audioChunks[0][0] != 1 || st.audioChunks[1][0] != 2 {
		t.Fatal("held audio sent out of order")
	}
}

func TestGoogleSTTOpenFailureReportsOncePerTurn(t *testing.T) {
	op := &failingOpener{}
	session, sink := startGoogle(t, op)
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	for i := 0; i < 3; i++ {
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	}
	gwait(t, "error frame", time.Second, func() bool { return len(countFrames[*core.ErrorFrame](sink)) == 1 })
	gwait(t, "audio passed", time.Second, func() bool { return len(countFrames[*core.InputAudioFrame](sink)) == 3 })
	if n := op.opens.Load(); n != 1 {
		t.Fatalf("opens = %d, want one per turn", n)
	}
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 2, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	gwait(t, "second turn reopens", time.Second, func() bool { return op.opens.Load() == 2 })
	if err := session.Err(); err != nil {
		t.Fatalf("a failed open ended the call: %v", err)
	}
}

// A8: the provider ends the stream (io.EOF) while the turn is bound, not
// half-closed, with words heard: the turn is invalidated, then an error raised.
func TestGoogleSTTProviderEndMidUtteranceInvalidatesTurn(t *testing.T) {
	st := endingStream{newMockSTTStream(), make(chan struct{})}
	session, sink := startGoogle(t, endingOpener{st})
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	st.PushResult(&STTResult{Transcript: "hello"})
	gwait(t, "interim", time.Second, func() bool { return len(countFrames[*core.TranscriptCandidateFrame](sink)) == 1 })
	close(st.end)
	gwait(t, "invalidation and error", time.Second, func() bool {
		return len(countFrames[*core.TranscriptInvalidationFrame](sink)) == 1 && len(countFrames[*core.ErrorFrame](sink)) == 1
	})
	inv := countFrames[*core.TranscriptInvalidationFrame](sink)[0]
	ef := countFrames[*core.ErrorFrame](sink)[0]
	if inv.Turn != "turn-1-1" || !errors.Is(ef.Err, core.ErrTranscriptInvalidated) {
		t.Fatalf("invalidation %+v error %v", inv, ef.Err)
	}
	// No redial for the rest of the turn: further audio passes without a new open.
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	gwait(t, "audio passed", time.Second, func() bool { return len(countFrames[*core.InputAudioFrame](sink)) == 2 })
	if n := len(countFrames[*core.ErrorFrame](sink)); n != 1 {
		t.Fatalf("error frames = %d, want 1", n)
	}
}
