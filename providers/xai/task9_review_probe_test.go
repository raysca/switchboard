//go:build xaispeech

package xai

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
)

type reviewClosingStream struct {
	closeSends atomic.Int32
	closes     atomic.Int32
	created    atomic.Bool
}

func (*reviewClosingStream) SendAudio(context.Context, []byte) error { return nil }
func (s *reviewClosingStream) CloseSend(context.Context) error {
	s.closeSends.Add(1)
	return nil
}
func (s *reviewClosingStream) Recv(ctx context.Context) (*STTMessage, error) {
	if s.created.CompareAndSwap(false, true) {
		return &STTMessage{Type: "transcript.created"}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *reviewClosingStream) Close() error { s.closes.Add(1); return nil }

type reviewClosingOpener struct{ stream *reviewClosingStream }

func (o reviewClosingOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	return o.stream, nil
}

type reviewRejectGoSession struct{ *mockSessionController }

func (*reviewRejectGoSession) Go(func(context.Context)) error { return core.ErrTooManyTasks }

func TestXAIReceiverStartFailureLosesTheTurnNotTheCall(t *testing.T) {
	st := &reviewClosingStream{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = reviewClosingOpener{st}
	p, _ := NewSTTProcessor(cfg)
	session := &reviewRejectGoSession{newMockSessionController(1, 1)}
	_ = p.Attach(session, 0)
	e := reviewEmitter{}
	_ = p.ProcessFrame(context.Background(), &core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "A", Gen: 1}, core.Downstream, e)
	err := p.ProcessFrame(context.Background(), &core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1}, core.Downstream, e)
	if err != nil { // a local task limit loses the turn's transcript (a non-fatal ErrorFrame), not the call
		t.Fatalf("receiver task start failure failed the frame: %v", err)
	}
}

type reviewSequenceOpener struct {
	mu      sync.Mutex
	streams []*reviewClosingStream
}

func (o *reviewSequenceOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := &reviewClosingStream{}
	o.streams = append(o.streams, st)
	return st, nil
}

func TestXAITypedInterruptionCancelsAndRestartsMatchingStream(t *testing.T) {
	opener := &reviewSequenceOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	defer p.Close()
	bareSession(p)
	e := reviewEmitter{}
	ctx := context.Background()
	_ = p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "A", Gen: 1}, core.Downstream, e)
	_ = p.ProcessFrame(ctx, &core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1}, core.Downstream, e)
	waitReady(t, p) // audio.done is sent only on a ready stream
	_ = p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "A", Gen: 1}, core.Downstream, e)
	_ = p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventInterrupted, Turn: "A", Gen: 1}, core.Downstream, e)
	_ = p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "B", Gen: 1}, core.Downstream, e)
	_ = p.ProcessFrame(ctx, &core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1}, core.Downstream, e)
	eventually(t, "second stream", func() bool {
		opener.mu.Lock()
		defer opener.mu.Unlock()
		return len(opener.streams) == 2
	})
	opener.mu.Lock()
	first := opener.streams[0]
	opener.mu.Unlock()
	if first.closeSends.Load() != 1 || first.closes.Load() == 0 {
		t.Fatalf("typed stream lifecycle mismatch: first closeSends=%d closes=%d", first.closeSends.Load(), first.closes.Load())
	}
}

type reviewBudgetSession struct {
	ctx        context.Context
	active     atomic.Int32
	rejections atomic.Int32
}

func (s *reviewBudgetSession) ID() uint64                                            { return 1 }
func (s *reviewBudgetSession) Context() context.Context                              { return s.ctx }
func (s *reviewBudgetSession) CurrentGeneration() uint64                             { return 1 }
func (s *reviewBudgetSession) IncrementGeneration() uint64                           { return 1 }
func (s *reviewBudgetSession) Reenter(core.Frame, core.Direction, int, uint64) error { return nil }
func (s *reviewBudgetSession) Go(fn func(context.Context)) error {
	if s.active.Add(1) > 16 {
		s.active.Add(-1)
		s.rejections.Add(1)
		return core.ErrTooManyTasks
	}
	go func() { defer s.active.Add(-1); fn(s.ctx) }()
	return nil
}

func TestXAIRepeatedTypedTurnsDoNotExhaustReceiverBudget(t *testing.T) {
	opener := &sequenceSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &reviewBudgetSession{ctx: ctx}
	_ = p.Attach(session, 0)
	e := reviewEmitter{}
	for i := 0; i < 20; i++ {
		turn := core.TurnID(fmt.Sprintf("T%d", i))
		if err := p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: turn, Gen: 1}, core.Downstream, e); err != nil {
			t.Fatal(err)
		}
		if err := p.ProcessFrame(ctx, &core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1}, core.Downstream, e); err != nil {
			t.Fatal(err)
		}
		if err := p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: turn, Gen: 1}, core.Downstream, e); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	if got := session.rejections.Load(); got != 0 {
		t.Fatalf("receiver task budget rejected %d starts", got)
	}
}

type reviewEmitter struct{}

func (reviewEmitter) Push(core.Frame) error                          { return nil }
func (reviewEmitter) PushDirection(core.Frame, core.Direction) error { return nil }

func TestReviewXAITypedCompletionClosesAudio(t *testing.T) {
	st := &reviewClosingStream{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = reviewClosingOpener{st}
	p, _ := NewSTTProcessor(cfg)
	defer p.Close()
	bareSession(p)
	ctx := context.Background()
	e := reviewEmitter{}
	p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "A", Gen: 1}, core.Downstream, e)
	p.ProcessFrame(ctx, &core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1}, core.Downstream, e)
	waitReady(t, p)
	p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "A", Gen: 1}, core.Downstream, e)
	if st.closeSends.Load() != 1 {
		t.Fatalf("typed completion did not send audio.done; close sends=%d", st.closeSends.Load())
	}
}

// A turn manager barges in mid-utterance: the open stream moves to the new
// generation instead of draining.
func TestXAISameTurnNewerGenerationAdoptsStream(t *testing.T) {
	opener := &reviewSequenceOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	defer p.Close()
	bareSession(p)
	e := reviewEmitter{}
	ctx := context.Background()
	_ = p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "A", Gen: 1}, core.Downstream, e)
	_ = p.ProcessFrame(ctx, &core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1}, core.Downstream, e)
	_ = p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "A", Gen: 2}, core.Upstream, e)
	_ = p.ProcessFrame(ctx, &core.InputAudioFrame{Samples: make([]byte, 640), Gen: 2}, core.Downstream, e)
	p.mu.Lock()
	gen, draining := p.active.gen, len(p.draining)
	p.mu.Unlock()
	waitAttached(t, p)
	time.Sleep(50 * time.Millisecond) // a wrongly opened second stream would show within this window
	opener.mu.Lock()
	defer opener.mu.Unlock()
	if len(opener.streams) != 1 || opener.streams[0].closeSends.Load() != 0 || gen != 2 || draining != 0 {
		t.Fatalf("streams=%d gen=%d draining=%d", len(opener.streams), gen, draining)
	}
}
