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
	tr "govoice/core/transcript"
)

type round5FailedBufferedStream struct {
	messages chan *STTMessage
	sent     atomic.Int32
	failed   chan struct{}
	closes   atomic.Int32
}

func (s *round5FailedBufferedStream) SendAudio(context.Context, []byte) error {
	if s.sent.Add(1) == 2 {
		close(s.failed)
		return errors.New("buffered audio write failed")
	}
	return nil
}
func (*round5FailedBufferedStream) CloseSend(context.Context) error { return nil }
func (s *round5FailedBufferedStream) Close() error {
	s.closes.Add(1)
	return nil
}
func (s *round5FailedBufferedStream) Recv(ctx context.Context) (*STTMessage, error) {
	select {
	case m := <-s.messages:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type round5FailedBufferedOpener struct{ st *round5FailedBufferedStream }

func (o round5FailedBufferedOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	return o.st, nil
}

func TestRound5XAIBufferedWriteFailureCannotCommitPartial(t *testing.T) {
	st := &round5FailedBufferedStream{messages: make(chan *STTMessage, 2), failed: make(chan struct{})}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = round5FailedBufferedOpener{st}
	p, _ := NewSTTProcessor(cfg)
	fc := tr.DefaultConfig()
	fc.GracePeriod = 0
	fc.InterimRepeats = 1
	fp, _ := tr.NewProcessor(fc)
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(p, fp, sink)
	s, _ := core.NewSession(981, pipe, core.DefaultResourceBudget())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, frame := range []core.Frame{
		&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1},
		&core.InputAudioFrame{Gen: 1, Samples: []byte{0, 0}},
		&core.InputAudioFrame{Gen: 1, Samples: []byte{1, 1}},
	} {
		if err := s.Post(frame); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		count := 0
		for _, frame := range sink.GetFrames() {
			if frame.Type() == core.FrameInputAudio {
				count++
			}
		}
		if count == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	st.messages <- &STTMessage{Type: "transcript.partial", Text: "first chunk only"}
	deadline = time.Now().Add(time.Second)
	interimSeen := false
	for time.Now().Before(deadline) {
		for _, frame := range sink.GetFrames() {
			if _, ok := frame.(*core.InterimTranscriptionFrame); ok {
				interimSeen = true
				break
			}
		}
		if interimSeen {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !interimSeen {
		t.Fatal("pre-failure partial never reached transcript policy")
	}
	st.messages <- &STTMessage{Type: "transcript.created"}
	select {
	case <-st.failed:
	case <-time.After(time.Second):
		t.Fatal("second buffered chunk did not fail")
	}
	st.messages <- &STTMessage{Type: "transcript.done", Text: "first chunk only"}
	deadline = time.Now().Add(time.Second)
	var observed *core.ErrorFrame
	for time.Now().Before(deadline) {
		for _, frame := range sink.GetFrames() {
			if value, ok := frame.(*core.ErrorFrame); ok {
				observed = value
			}
		}
		if observed != nil && st.closes.Load() == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if observed == nil || !errors.Is(observed.Err, core.ErrTranscriptInvalidated) || observed.Turn != "turn-1-1" || observed.Gen != 1 {
		t.Fatalf("missing correlated observable provider error: %v", observed)
	}
	if st.closes.Load() != 1 || p.ErrorsTotal.Load() != 1 {
		t.Fatalf("failed stream lifecycle: closes=%d providerErrors=%d", st.closes.Load(), p.ErrorsTotal.Load())
	}
	if err := s.Post(&core.UserStoppedSpeakingFrame{Gen: 1, Seq: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.Post(&core.UserMessageFrame{Turn: "barrier", Gen: 1, Content: "processed"}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	barrierSeen := false
	for time.Now().Before(deadline) {
		for _, frame := range sink.GetFrames() {
			if message, ok := frame.(*core.UserMessageFrame); ok && message.Turn == "barrier" {
				barrierSeen = true
				break
			}
		}
		if barrierSeen {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !barrierSeen {
		t.Fatal("post-failure turn completion barrier was not processed")
	}
	for _, frame := range sink.GetFrames() {
		if candidate, ok := frame.(*core.FinalTranscriptionFrame); ok {
			t.Fatalf("incomplete audio committed as %q", candidate.Text)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if st.closes.Load() != 1 {
		t.Fatalf("session shutdown closed failed stream %d times", st.closes.Load())
	}
}
