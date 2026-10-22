//go:build xaispeech

package xai

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// writeFailStream is ready at once (transcript.created) and then fails the
// write it is armed for.
type writeFailStream struct {
	sendFail, doneFail atomic.Bool
	created            atomic.Bool
	closes             atomic.Int32
	hold               chan struct{} // Recv ignores ctx until closed, so only the writer can release the stream
}

func (s *writeFailStream) SendAudio(context.Context, []byte) error {
	if s.sendFail.Load() {
		return errors.New("write: broken pipe")
	}
	return nil
}

func (s *writeFailStream) CloseSend(context.Context) error {
	if s.doneFail.Load() {
		return errors.New("audio.done: broken pipe")
	}
	return nil
}

func (s *writeFailStream) Close() error { s.closes.Add(1); return nil }

func (s *writeFailStream) Recv(ctx context.Context) (*STTMessage, error) {
	if s.created.CompareAndSwap(false, true) {
		return &STTMessage{Type: "transcript.created"}, nil
	}
	<-s.hold
	return nil, ctx.Err()
}

type writeFailOpener struct {
	streams []*writeFailStream
	opens   atomic.Int32
}

func (o *writeFailOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	return o.streams[o.opens.Add(1)-1], nil
}

type frameRecorder struct {
	audio int
	errs  []*core.ErrorFrame
}

func (r *frameRecorder) Push(f core.Frame) error {
	switch v := f.(type) {
	case *core.InputAudioFrame:
		r.audio++
	case *core.ErrorFrame:
		r.errs = append(r.errs, v)
	}
	return nil
}
func (r *frameRecorder) PushDirection(core.Frame, core.Direction) error { return nil }

func startWriteFail(t *testing.T, bound bool) (*STTProcessor, *core.Session, *coretest.PassthroughProcessor, *writeFailOpener) {
	t.Helper()
	o := &writeFailOpener{streams: []*writeFailStream{{hold: make(chan struct{})}, {hold: make(chan struct{})}}}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = o
	p, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(p, sink)
	s, _ := core.NewSession(990, pipe, core.DefaultResourceBudget())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	t.Cleanup(func() {
		for _, st := range o.streams {
			close(st.hold)
		}
	})
	if bound {
		_ = s.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	}
	_ = s.Post(&core.InputAudioFrame{Gen: 1, Samples: []byte{0, 0}})
	waitXAI(t, "stream ready", func() bool {
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
	return p, s, sink, o
}

func waitXAI(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func sinkErrors(sink *coretest.PassthroughProcessor) (out []*core.ErrorFrame) {
	for _, f := range sink.GetFrames() {
		if v, ok := f.(*core.ErrorFrame); ok {
			out = append(out, v)
		}
	}
	return out
}

// A live SendAudio failure is the stream's failure, handled like a failed
// Recv: the bound turn is invalidated for an STT failover, the frame still
// passes, nothing is returned (that would end the call), and the next audio
// opens a fresh stream.
func TestXAISTT_LiveSendFailureIsReportedNotFatal(t *testing.T) {
	for _, bound := range []bool{true, false} {
		name := map[bool]string{true: "bound", false: "unbound"}[bound]
		t.Run(name, func(t *testing.T) {
			p, s, sink, o := startWriteFail(t, bound)
			o.streams[0].sendFail.Store(true)
			out := &frameRecorder{}
			if err := p.ProcessFrame(context.Background(), &core.InputAudioFrame{Gen: 1, Samples: []byte{1, 1}}, core.Downstream, out); err != nil {
				t.Fatalf("ProcessFrame err = %v, want nil", err)
			}
			if out.audio != 1 {
				t.Fatalf("audio pushed = %d, want 1", out.audio)
			}
			p.mu.Lock()
			stale := p.active != nil
			p.mu.Unlock()
			if stale {
				t.Fatal("the failed stream is still active: the next audio would write to it")
			}
			waitXAI(t, "error frame", func() bool { return len(sinkErrors(sink)) == 1 })
			ef := sinkErrors(sink)[0]
			inv := 0
			for _, f := range sink.GetFrames() {
				if _, ok := f.(*core.TranscriptInvalidationFrame); ok {
					inv++
				}
			}
			if bound && (inv != 1 || !errors.Is(ef.Err, core.ErrTranscriptInvalidated) || !errors.Is(ef.Err, core.ErrSTTUnavailable) || ef.Turn != "turn-1-1") {
				t.Fatalf("bound: invalidations %d, error %+v", inv, ef)
			}
			if !bound && (inv != 0 || errors.Is(ef.Err, core.ErrTranscriptInvalidated) || errors.Is(ef.Err, core.ErrSTTUnavailable)) {
				t.Fatalf("unbound: invalidations %d, error %+v", inv, ef)
			}
			if n := p.ErrorsTotal.Load(); n != 1 {
				t.Fatalf("errors counted = %d, want 1", n)
			}
			waitXAI(t, "stream closed", func() bool { return o.streams[0].closes.Load() >= 1 })
			_ = s.Post(&core.InputAudioFrame{Gen: 1, Samples: []byte{2, 2}})
			waitXAI(t, "reopen", func() bool { return o.opens.Load() == 2 })
		})
	}
}

// A failed audio.done write (CloseSend) is reported the same way, on both
// paths that drain a stream: the speaker stopping and the turn completing.
func TestXAISTT_CloseSendFailureIsReportedNotFatal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame core.Frame
	}{
		{"user stopped", &core.UserStoppedSpeakingFrame{}},
		{"turn completed", &core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "turn-1-1", Gen: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, sink, o := startWriteFail(t, true)
			o.streams[0].doneFail.Store(true)
			if err := p.ProcessFrame(context.Background(), tc.frame, core.Downstream, &frameRecorder{}); err != nil {
				t.Fatalf("ProcessFrame err = %v, want nil", err)
			}
			waitXAI(t, "error frame", func() bool { return len(sinkErrors(sink)) == 1 })
			if n := p.ErrorsTotal.Load(); n != 1 {
				t.Fatalf("errors counted = %d, want 1", n)
			}
			waitXAI(t, "stream closed", func() bool { return o.streams[0].closes.Load() >= 1 })
			time.Sleep(30 * time.Millisecond)
			if n := len(sinkErrors(sink)); n != 1 {
				t.Fatalf("error frames = %d, want 1", n)
			}
		})
	}
}
