//go:build xaispeech

package xai

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

type slowXAIOpener struct {
	delay  time.Duration
	stream *mockSTTStream
}

func (o slowXAIOpener) OpenStream(ctx context.Context, _ STTConfig) (STTStream, error) {
	select {
	case <-time.After(o.delay):
		return o.stream, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func startXAI(t *testing.T, opener STTStreamOpener) (*core.Session, *coretest.PassthroughProcessor) {
	t.Helper()
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(41, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, sink
}

// A1: an open longer than the 2 s frame watchdog runs off the run loop; the
// audio held meanwhile goes out in order after transcript.created.
func TestXAISTTOpenRunsOffTheRunLoop(t *testing.T) {
	ms := &mockSTTStream{messages: []*STTMessage{{Type: "transcript.created"}}}
	session, _ := startXAI(t, slowXAIOpener{3 * time.Second, ms})
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for b := byte(1); b <= 2; b++ {
		_ = session.Post(&core.InputAudioFrame{Samples: bytes.Repeat([]byte{b}, 640), Gen: 1})
	}
	time.Sleep(2500 * time.Millisecond)
	if err := session.Err(); err != nil {
		t.Fatalf("session failed while the open hung: %v", err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		ms.mu.Lock()
		n := len(ms.audioIn)
		ordered := n >= 2 && ms.audioIn[0][0] == 1 && ms.audioIn[1][0] == 2
		ms.mu.Unlock()
		if n >= 2 {
			if !ordered {
				t.Fatal("held audio sent out of order")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("held audio never sent: %d chunks", n)
		}
	}
}

func TestXAISTTOpenFailureReportsOncePerTurn(t *testing.T) {
	var opens atomic.Int32
	session, sink := startXAI(t, &countOpener{n: &opens, err: errors.New("dial refused")})
	errorsSeen := func() (n int) {
		for _, f := range sink.GetFrames() {
			if _, ok := f.(*core.ErrorFrame); ok {
				n++
			}
		}
		return n
	}
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for i := 0; i < 3; i++ {
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	}
	for deadline := time.Now().Add(time.Second); errorsSeen() != 1; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("error frames = %d, want 1", errorsSeen())
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := opens.Load(); n != 1 || errorsSeen() != 1 {
		t.Fatalf("opens = %d, errors = %d: want one of each per turn", n, errorsSeen())
	}
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 2})
	for deadline := time.Now().Add(time.Second); opens.Load() != 2; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the next turn did not reopen")
		}
	}
	if err := session.Err(); err != nil {
		t.Fatalf("a failed open ended the call: %v", err)
	}
}

// bareSession gives a directly driven processor the session its open task needs.
func bareSession(p *STTProcessor) *mockSessionController {
	s := newMockSessionController(1, 1)
	_ = p.Attach(s, 0)
	return s
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// waitReady waits until the active stream has seen transcript.created.
func waitReady(t *testing.T, p *STTProcessor) {
	t.Helper()
	eventually(t, "stream ready", func() bool {
		p.mu.Lock()
		st := p.active
		p.mu.Unlock()
		if st == nil {
			return false
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.isReady
	})
}

// waitAttached waits until the active stream's open has completed (stream set).
func waitAttached(t *testing.T, p *STTProcessor) {
	t.Helper()
	eventually(t, "stream attached", func() bool {
		p.mu.Lock()
		st := p.active
		p.mu.Unlock()
		if st == nil {
			return false
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.stream != nil
	})
}

// A failed open reports an error that says the STT is unreachable (the server ends
// the call after repeated ones).
func TestXAISTTOpenFailureWrapsErrSTTUnavailable(t *testing.T) {
	var opens atomic.Int32
	session, sink := startXAI(t, &countOpener{n: &opens, err: errors.New("dial refused")})
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	eventually(t, "error frame", func() bool {
		for _, f := range sink.GetFrames() {
			if ef, ok := f.(*core.ErrorFrame); ok {
				return errors.Is(ef.Err, core.ErrSTTUnavailable)
			}
		}
		return false
	})
}

// hungXAIOpener blocks every open until its ctx ends (a provider-wide hang).
type hungXAIOpener struct{ live, peak atomic.Int32 }

func (o *hungXAIOpener) OpenStream(ctx context.Context, _ STTConfig) (STTStream, error) {
	n := o.live.Add(1)
	defer o.live.Add(-1)
	for p := o.peak.Load(); n > p && !o.peak.CompareAndSwap(p, n); p = o.peak.Load() {
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func startXAIBudget(t *testing.T, opener STTStreamOpener, maxTasks int) (*STTProcessor, *core.Session, *coretest.PassthroughProcessor) {
	t.Helper()
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	budget := core.DefaultResourceBudget()
	if maxTasks > 0 {
		budget.MaxAsyncTasks = maxTasks
	}
	session, _ := core.NewSession(46, pipe, budget)
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return proc, session, sink
}

func errorFramesOf(sink *coretest.PassthroughProcessor) (out []*core.ErrorFrame) {
	for _, f := range sink.GetFrames() {
		if ef, ok := f.(*core.ErrorFrame); ok {
			out = append(out, ef)
		}
	}
	return out
}

func waitUntil(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// A provider hang must not fail the call or starve its other tasks: each short turn parks a
// still-opening stream and the next audio opens another. Past the cap a turn loses its
// transcript with a local, non-fatal error.
func TestXAISTTProviderHangDoesNotStackSessionTasks(t *testing.T) {
	opener := &hungXAIOpener{}
	proc, session, sink := startXAIBudget(t, opener, 0)
	const turns = 30
	for i := 1; i <= turns; i++ {
		_ = session.Post(&core.UserStartedSpeakingFrame{Seq: uint64(i), Gen: 1})
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
		_ = session.Post(&core.UserStoppedSpeakingFrame{})
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
		time.Sleep(5 * time.Millisecond)
	}
	waitUntil(t, "local errors", 3*time.Second, func() bool { return len(errorFramesOf(sink)) >= turns-maxLiveStreams })
	if err := session.Err(); err != nil {
		t.Fatalf("the call failed: %v", err)
	}
	if p := opener.peak.Load(); p > maxLiveStreams {
		t.Fatalf("peak live opens = %d, want <= %d", p, maxLiveStreams)
	}
	for _, ef := range errorFramesOf(sink) {
		if ef.Fatal || !errors.Is(ef.Err, core.ErrTooManyTasks) || errors.Is(ef.Err, core.ErrSTTUnavailable) {
			t.Fatalf("error frame %+v: a local limit is non-fatal and not an unreachable STT", ef)
		}
	}
	if n := proc.ErrorsTotal.Load(); n != 0 {
		t.Fatalf("the local limit counted %d provider errors", n)
	}
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
func TestXAISTTTaskBudgetAtOpenReportsOncePerTurnAndKeepsTheCall(t *testing.T) {
	proc, session, sink := startXAIBudget(t, &hungXAIOpener{}, 1)
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
	waitUntil(t, "error frame", 2*time.Second, func() bool { return len(errorFramesOf(sink)) >= 1 })
	time.Sleep(50 * time.Millisecond)
	efs := errorFramesOf(sink)
	if len(efs) != 1 || !errors.Is(efs[0].Err, core.ErrTooManyTasks) || errors.Is(efs[0].Err, core.ErrSTTUnavailable) || efs[0].Fatal {
		t.Fatalf("error frames %+v, want one local non-fatal", efs)
	}
	if err := session.Err(); err != nil {
		t.Fatalf("the call failed: %v", err)
	}
	if n := proc.ErrorsTotal.Load(); n != 0 {
		t.Fatalf("counted %d provider errors", n)
	}
}
