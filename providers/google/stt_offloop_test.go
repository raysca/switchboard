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

	speechpb "cloud.google.com/go/speech/apiv2/speechpb"
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

// attachedStream waits until the processor's active stream has opened.
func attachedStream(t *testing.T, p *STTProcessor) {
	t.Helper()
	gwait(t, "stream attached", 2*time.Second, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.active != nil && p.active.stream != nil
	})
}

func startGoogleProc(t *testing.T, opener STTStreamOpener) (*STTProcessor, *core.Session, *coretest.PassthroughProcessor) {
	t.Helper()
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(42, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return proc, session, sink
}

// settleThenEnd drives: start, interim "uh", stop (half-close), then final(finalText), then the provider's end.
func settleThenEnd(t *testing.T, finalText string) *coretest.PassthroughProcessor {
	st := endingStream{newMockSTTStream(), make(chan struct{})}
	proc, session, sink := startGoogleProc(t, endingOpener{st})
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	attachedStream(t, proc)
	st.PushResult(&STTResult{Transcript: "uh"})
	gwait(t, "interim", time.Second, func() bool { return len(countFrames[*core.TranscriptCandidateFrame](sink)) == 1 })
	_ = session.Post(&core.UserStoppedSpeakingFrame{})
	gwait(t, "half-closed", time.Second, func() bool {
		proc.mu.Lock()
		defer proc.mu.Unlock()
		return proc.active == nil
	})
	st.PushResult(&STTResult{Transcript: finalText, IsFinal: true})
	gwait(t, "final consumed", time.Second, func() bool { return len(st.results) == 0 })
	close(st.end)
	time.Sleep(150 * time.Millisecond)
	return sink
}

// A coughed utterance: the provider retracts it with an empty final, then ends the
// stream. Nothing was lost, so no invalidation and no error (as the runner does).
func TestGoogleSTTEmptyFinalSettlesTheTurn(t *testing.T) {
	for _, final := range []string{"", "uh"} {
		sink := settleThenEnd(t, final)
		if n := len(countFrames[*core.TranscriptInvalidationFrame](sink)) + len(countFrames[*core.ErrorFrame](sink)); n != 0 {
			t.Fatalf("final %q: %d invalidation/error frames, want none", final, n)
		}
	}
}

// A mid-turn final does not settle a stream that is still bound and not half-closed:
// the user is still talking, so the provider's end loses the rest of the turn.
func TestGoogleSTTMidTurnFinalThenEndWhileBoundInvalidates(t *testing.T) {
	st := endingStream{newMockSTTStream(), make(chan struct{})}
	proc, session, sink := startGoogleProc(t, endingOpener{st})
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	attachedStream(t, proc)
	st.PushResult(&STTResult{Transcript: "hello", IsFinal: true})
	gwait(t, "final", time.Second, func() bool { return len(countFrames[*core.TranscriptCandidateFrame](sink)) == 1 })
	close(st.end)
	gwait(t, "invalidation and error", time.Second, func() bool {
		return len(countFrames[*core.TranscriptInvalidationFrame](sink)) == 1 && len(countFrames[*core.ErrorFrame](sink)) == 1
	})
}

// armedEOFStream fails Send with io.EOF once armed; its Recv ends only when released.
type armedEOFStream struct {
	endingStream
	armed atomic.Bool
}

func (s *armedEOFStream) Send(b []byte) error {
	if s.armed.Load() {
		return io.EOF
	}
	return s.mockSTTStream.Send(b)
}

type countingOpener struct {
	st    STTStream
	opens atomic.Int32
}

func (o *countingOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	o.opens.Add(1)
	return o.st, nil
}

// A Send that sees the stream already ended must not clear the active stream: the next
// audio frame would open a second stream for the turn before the receiver reports the
// end. The receiver clears it and the turn is not redialled.
func TestGoogleSTTSendEOFLeavesTheEndToTheReceiver(t *testing.T) {
	st := &armedEOFStream{endingStream: endingStream{newMockSTTStream(), make(chan struct{})}}
	op := &countingOpener{st: st}
	proc, session, sink := startGoogleProc(t, op)
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	attachedStream(t, proc)
	st.armed.Store(true)
	for i := 0; i < 3; i++ {
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	}
	gwait(t, "audio passed", time.Second, func() bool { return len(countFrames[*core.InputAudioFrame](sink)) == 4 })
	if n := op.opens.Load(); n != 1 {
		t.Fatalf("opens = %d after Send EOF, want 1 (the receiver reports the end)", n)
	}
	close(st.end)
	gwait(t, "invalidation", time.Second, func() bool { return len(countFrames[*core.TranscriptInvalidationFrame](sink)) == 1 })
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	gwait(t, "audio passed", time.Second, func() bool { return len(countFrames[*core.InputAudioFrame](sink)) == 5 })
	if n := op.opens.Load(); n != 1 {
		t.Fatalf("opens = %d: the invalidated turn was redialled", n)
	}
}

func TestGoogleSTTPrerollOverflowIsTraced(t *testing.T) {
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = slowOpener{10 * time.Second, newMockSTTStream()} // never opens within the test
	proc, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(proc, &coretest.PassthroughProcessor{})
	s, _ := core.NewSession(43, pipe, core.DefaultResourceBudget())
	r := &lifecycleRecorder{}
	if err := s.SetTracer(r, core.TracePolicy{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	for i := 1; i <= 80; i++ { // 2 MB held against a 1.92 MB bound, paced so none is shed
		_ = s.Post(&core.InputAudioFrame{Samples: make([]byte, 25000), Gen: 1})
		gwait(t, "frame handled", 5*time.Second, func() bool {
			proc.mu.Lock()
			defer proc.mu.Unlock()
			return proc.turnAudioBytes >= int64(i)*25000
		})
	}
	gwait(t, "preroll_dropped event", 10*time.Second, func() bool { return r.count("stt.preroll_dropped", "turn-1-1") == 1 })
}

// A failed open reports an error that says the STT is unreachable (the server ends
// the call after repeated ones).
func TestGoogleSTTOpenFailureWrapsErrSTTUnavailable(t *testing.T) {
	session, sink := startGoogle(t, &failingOpener{})
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	gwait(t, "error frame", time.Second, func() bool { return len(countFrames[*core.ErrorFrame](sink)) == 1 })
	if ef := countFrames[*core.ErrorFrame](sink)[0]; !errors.Is(ef.Err, core.ErrSTTUnavailable) {
		t.Fatalf("error %v does not wrap core.ErrSTTUnavailable", ef.Err)
	}
}

// When the rotation's receiver task cannot start (task budget), the replacement is
// cancelled: it must not stay the active stream, where Send would hit io.EOF and drop
// audio silently. The turn's loss is reported.
func TestGoogleSTTRotationReceiverStartFailureClearsTheStream(t *testing.T) {
	var nowNanos atomic.Int64
	nowNanos.Store(time.Unix(1, 0).UnixNano())
	cfg := DefaultSTTConfig()
	cfg.Now = func() time.Time { return time.Unix(0, nowNanos.Load()) }
	cfg.StreamRotationThreshold = time.Second
	cfg.StreamOpener = &mockSTTOpener{}
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	budget := core.DefaultResourceBudget()
	budget.MaxAsyncTasks = 1 // the first stream's task takes it
	session, _ := core.NewSession(44, pipe, budget)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	attachedStream(t, proc)
	nowNanos.Add(int64(2 * time.Second))
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1}) // rotates
	gwait(t, "error frame", 2*time.Second, func() bool {
		for _, ef := range countFrames[*core.ErrorFrame](sink) {
			if errors.Is(ef.Err, core.ErrTooManyTasks) {
				return true
			}
		}
		return false
	})
	proc.mu.Lock()
	cancelledStillActive := proc.active != nil && proc.active.ctx.Err() != nil
	proc.mu.Unlock()
	if cancelledStillActive {
		t.Fatal("a cancelled replacement stayed the active stream")
	}
	for _, ef := range countFrames[*core.ErrorFrame](sink) {
		if errors.Is(ef.Err, core.ErrTooManyTasks) && errors.Is(ef.Err, core.ErrSTTUnavailable) {
			t.Fatalf("the local task budget is not an unreachable STT: %v", ef.Err)
		}
	}
	if n := proc.Errors.Load(); n != 0 {
		t.Fatalf("the local task limit counted %d provider errors", n)
	}
}

// sendFailStream fails every Send, like a connection that died while its held audio flushed.
type sendFailStream struct{ *mockSTTStream }

func (sendFailStream) Send([]byte) error { return errors.New("write: broken pipe") }

// A stream that dies taking a BOUND turn's held audio lost the turn: it is
// invalidated (A8, so a failover replays it), not reported as an unreachable STT.
func TestGoogleSTTFlushFailureOfBoundTurnInvalidatesIt(t *testing.T) {
	proc, session, sink := startGoogleProc(t, openerFunc(func() STTStream { return sendFailStream{newMockSTTStream()} }))
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	gwait(t, "invalidation and error", 2*time.Second, func() bool {
		return len(countFrames[*core.TranscriptInvalidationFrame](sink)) == 1 && len(countFrames[*core.ErrorFrame](sink)) == 1
	})
	ef := countFrames[*core.ErrorFrame](sink)[0]
	if !errors.Is(ef.Err, core.ErrTranscriptInvalidated) || errors.Is(ef.Err, core.ErrSTTUnavailable) || ef.Turn != "turn-1-1" {
		t.Fatalf("error frame %+v", ef)
	}
	if n := proc.Errors.Load(); n != 1 {
		t.Fatalf("errors counted = %d, want 1", n)
	}
}

type openerFunc func() STTStream

func (f openerFunc) OpenStream(context.Context, STTConfig) (STTStream, error) { return f(), nil }

type recvOnlyClient struct {
	speechpb.Speech_StreamingRecognizeClient
	resp *speechpb.StreamingRecognizeResponse
}

func (c recvOnlyClient) Recv() (*speechpb.StreamingRecognizeResponse, error) { return c.resp, nil }

// A final with no alternatives (noise retracted) reaches the receiver as an empty final.
func TestGRPCSTTStreamRecvPassesAFinalWithoutAlternativesThrough(t *testing.T) {
	s := &grpcSTTStream{stream: recvOnlyClient{resp: &speechpb.StreamingRecognizeResponse{Results: []*speechpb.StreamingRecognitionResult{
		{IsFinal: true},
		{IsFinal: false}, // an interim with nothing recognised stays out
	}}}}
	got, err := s.Recv()
	if err != nil || len(got) != 1 || !got[0].IsFinal || got[0].Transcript != "" {
		t.Fatalf("Recv = %+v, %v", got, err)
	}
}

// hungOpener blocks every open until its ctx ends (a provider-wide hang).
type hungOpener struct{ live, peak atomic.Int32 }

func (o *hungOpener) OpenStream(ctx context.Context, _ STTConfig) (STTStream, error) {
	n := o.live.Add(1)
	defer o.live.Add(-1)
	for p := o.peak.Load(); n > p && !o.peak.CompareAndSwap(p, n); p = o.peak.Load() {
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// A provider hang must not fail the call or starve its other tasks: each short turn
// detaches a still-opening stream and the next audio opens another. Past the cap a turn
// loses its transcript with a local, non-fatal error.
func TestGoogleSTTProviderHangDoesNotStackSessionTasks(t *testing.T) {
	opener := &hungOpener{}
	proc, session, sink := startGoogleProc(t, opener)
	const turns = 30
	for i := 1; i <= turns; i++ {
		_ = session.Post(&core.UserStartedSpeakingFrame{Seq: uint64(i), Gen: 1})
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
		_ = session.Post(&core.UserStoppedSpeakingFrame{})
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
		time.Sleep(5 * time.Millisecond)
	}
	gwait(t, "local errors", 3*time.Second, func() bool { return len(countFrames[*core.ErrorFrame](sink)) >= turns-maxLiveStreams })
	if err := session.Err(); err != nil {
		t.Fatalf("the call failed: %v", err)
	}
	if p := opener.peak.Load(); p > maxLiveStreams {
		t.Fatalf("peak live opens = %d, want <= %d", p, maxLiveStreams)
	}
	for _, ef := range countFrames[*core.ErrorFrame](sink) {
		if ef.Fatal || !errors.Is(ef.Err, core.ErrTooManyTasks) || errors.Is(ef.Err, core.ErrSTTUnavailable) {
			t.Fatalf("error frame %+v: a local limit is non-fatal and not an unreachable STT", ef)
		}
	}
	if n := proc.Errors.Load(); n != 0 {
		t.Fatalf("the local limit counted %d provider errors", n)
	}
	// LLM/TTS-style tasks can still be scheduled: the budget minus the capped streams is free.
	hold := make(chan struct{})
	defer close(hold)
	for i := 0; i < core.DefaultResourceBudget().MaxAsyncTasks-maxLiveStreams-4; i++ {
		if err := session.Go(func(ctx context.Context) {
			select {
			case <-hold:
			case <-ctx.Done():
			}
		}); err != nil {
			t.Fatalf("session task %d could not be scheduled: %v", i, err)
		}
	}
}

// The session's task budget exhausted at open: one non-fatal local error per turn, ProcessFrame returns nil.
func TestGoogleSTTTaskBudgetAtOpenReportsOncePerTurnAndKeepsTheCall(t *testing.T) {
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &hungOpener{}
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	budget := core.DefaultResourceBudget()
	budget.MaxAsyncTasks = 1
	session, _ := core.NewSession(45, pipe, budget)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	hold := make(chan struct{})
	defer close(hold)
	_ = session.Go(func(ctx context.Context) {
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}) // the only task
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	for i := 0; i < 3; i++ {
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	}
	gwait(t, "error frame", 2*time.Second, func() bool { return len(countFrames[*core.ErrorFrame](sink)) >= 1 })
	time.Sleep(50 * time.Millisecond)
	efs := countFrames[*core.ErrorFrame](sink)
	if len(efs) != 1 || !errors.Is(efs[0].Err, core.ErrTooManyTasks) || errors.Is(efs[0].Err, core.ErrSTTUnavailable) || efs[0].Fatal {
		t.Fatalf("error frames %+v, want one local non-fatal", efs)
	}
	if err := session.Err(); err != nil {
		t.Fatalf("the call failed: %v", err)
	}
	if n := proc.Errors.Load(); n != 0 {
		t.Fatalf("counted %d provider errors", n)
	}
}
