//go:build xaispeech

package xai

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	coretranscript "govoice/core/transcript"
)

type round3ReviewClosingStream struct {
	closes atomic.Int32
}

func (*round3ReviewClosingStream) SendAudio(context.Context, []byte) error { return nil }
func (*round3ReviewClosingStream) CloseSend(context.Context) error         { return nil }
func (*round3ReviewClosingStream) Recv(ctx context.Context) (*STTMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (s *round3ReviewClosingStream) Close() error {
	s.closes.Add(1)
	return nil
}

type round3ReviewSequenceOpener struct {
	mu      sync.Mutex
	streams []*round3ReviewClosingStream
}

func (o *round3ReviewSequenceOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := &round3ReviewClosingStream{}
	o.streams = append(o.streams, st)
	return st, nil
}

func TestRound3XAINewTurnPreservesPendingPriorFinal(t *testing.T) {
	opener := &round3ReviewSequenceOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	defer p.Close()
	ctx := context.Background()
	e := reviewEmitter{}
	p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "A", Gen: 1}, core.Downstream, e)
	p.ProcessFrame(ctx, &core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1}, core.Downstream, e)
	p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "A", Gen: 1}, core.Downstream, e)
	p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "B", Gen: 1}, core.Downstream, e)
	if opener.streams[0].closes.Load() != 0 {
		t.Fatalf("A's drain was canceled at B start before final/timeout: closes=%d", opener.streams[0].closes.Load())
	}
}

func TestRound3XAILatePriorFinalSurvivesV1AndTypedNextTurn(t *testing.T) {
	for _, typed := range []bool{false, true} {
		name := "v1"
		if typed {
			name = "typed"
		}
		t.Run(name, func(t *testing.T) {
			opener := &sequenceSTTOpener{}
			cfg := DefaultSTTConfig()
			cfg.StreamOpener = opener
			provider, _ := NewSTTProcessor(cfg)
			fc := coretranscript.DefaultConfig()
			fc.GracePeriod = 5 * time.Millisecond
			fc.FinalizationTimeout = 100 * time.Millisecond
			finalizer, _ := coretranscript.NewProcessor(fc)
			sink := &core.PassthroughProcessor{}
			pipe, _ := core.NewPipeline(provider, finalizer, sink)
			session, _ := core.NewSession(970, pipe, core.DefaultResourceBudget())
			_ = session.Start()
			defer session.Close()
			if typed {
				_ = session.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "A", Gen: 1})
			} else {
				_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
			}
			_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
			first := waitXAIReviewStream(t, opener, 1)
			if typed {
				_ = session.Post(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: "A", Gen: 1})
				_ = session.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: "B", Gen: 1})
			} else {
				_ = session.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, Timestamp: time.Now()})
				_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
			}
			_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
			_ = waitXAIReviewStream(t, opener, 2)
			first.messages <- &STTMessage{Type: "transcript.done", Text: "late A"}
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				for _, frame := range sink.GetFrames() {
					if final, ok := frame.(*core.FinalTranscriptionFrame); ok && final.Text == "late A" {
						return
					}
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("prior turn final was not committed while next turn was active")
		})
	}
}

func waitXAIReviewStream(t *testing.T, opener *sequenceSTTOpener, count int) *channelSTTStream {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		opener.mu.Lock()
		if len(opener.streams) >= count {
			stream := opener.streams[count-1]
			opener.mu.Unlock()
			return stream
		}
		opener.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stream %d did not open", count)
	return nil
}
