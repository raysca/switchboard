//go:build xaispeech

package xai

import (
	"context"
	"govoice/core"
	"govoice/core/coretest"
	tr "govoice/core/transcript"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRound4XAIBargeInTranscriptActuallyCommits(t *testing.T) {
	opener := &sequenceSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	fc := tr.DefaultConfig()
	fc.GracePeriod = 5 * time.Millisecond
	fc.FinalizationTimeout = 20 * time.Millisecond
	fp, _ := tr.NewProcessor(fc)
	pool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	user := core.NewUserContextProcessor(pool, core.UserContextConfig{RequireCommittedTranscript: true})
	assistant := core.NewAssistantContextProcessor(pool, core.DefaultAssistantContextConfig())
	pipe, _ := core.NewPipeline(p, fp, user, assistant, &coretest.PassthroughProcessor{})
	s, _ := core.NewSession(990, pipe, core.DefaultResourceBudget())
	s.Start()
	defer s.Close()
	s.Post(&core.UserMessageFrame{Turn: "bot", Gen: 1, Content: "prior"})
	s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	for limit := time.Now().Add(time.Second); s.CurrentGeneration() != 2 && time.Now().Before(limit); {
		time.Sleep(time.Millisecond)
	}
	if s.CurrentGeneration() != 2 {
		t.Fatal("no barge-in")
	}
	s.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 2})
	st := waitRound4Stream(t, opener, 2)
	st.messages <- &STTMessage{Type: "transcript.done", Text: "new user"}
	s.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 2})
	time.Sleep(100 * time.Millisecond)
	for _, m := range pool.Messages() {
		if m.Content == "new user" {
			return
		}
	}
	t.Fatalf("xAI real barge-in final lost: messages=%v stale=%d err=%v", pool.Messages(), s.StaleDiscarded.Load(), s.Err())
}

type round4LifecycleStream struct {
	closes   atomic.Int32
	messages chan *STTMessage
	entered  chan struct{}
	block    bool
}

func (s *round4LifecycleStream) SendAudio(ctx context.Context, _ []byte) error {
	if s.block {
		close(s.entered)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (*round4LifecycleStream) CloseSend(context.Context) error { return nil }
func (s *round4LifecycleStream) Close() error                  { s.closes.Add(1); return nil }
func (s *round4LifecycleStream) Recv(ctx context.Context) (*STTMessage, error) {
	select {
	case m := <-s.messages:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type round4LifecycleOpener struct{ st *round4LifecycleStream }

func (o round4LifecycleOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	return o.st, nil
}

type round4Session struct{ ctx context.Context }

func (*round4Session) ID() uint64                                            { return 1 }
func (s *round4Session) Context() context.Context                            { return s.ctx }
func (*round4Session) CurrentGeneration() uint64                             { return 1 }
func (*round4Session) IncrementGeneration() uint64                           { return 1 }
func (*round4Session) Reenter(core.Frame, core.Direction, int, uint64) error { return nil }
func (s *round4Session) Go(fn func(context.Context)) error {
	go fn(s.ctx)
	return nil
}

func waitRound4Stream(t *testing.T, opener *sequenceSTTOpener, count int) *channelSTTStream {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		opener.mu.Lock()
		if len(opener.streams) >= count {
			st := opener.streams[count-1]
			opener.mu.Unlock()
			return st
		}
		opener.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stream %d did not open", count)
	return nil
}

func TestRound4XAIDoneClosesStream(t *testing.T) {
	st := &round4LifecycleStream{messages: make(chan *STTMessage, 2)}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = round4LifecycleOpener{st}
	p, _ := NewSTTProcessor(cfg)
	s := newMockSessionController(1, 1)
	p.Attach(s, 0)
	p.ProcessFrame(context.Background(), &core.UserStartedSpeakingFrame{Gen: 1, Seq: 1}, core.Downstream, &mockEmitter{})
	st.messages <- &STTMessage{Type: "transcript.done", Text: "done"}
	s.taskWg.Wait()
	if st.closes.Load() != 1 {
		t.Fatalf("completed stream was removed without Close: closes=%d", st.closes.Load())
	}
	p.Close()
	if st.closes.Load() != 1 {
		t.Fatalf("completed stream closed twice: closes=%d", st.closes.Load())
	}
}

type round4ManyStreamsOpener struct {
	mu      sync.Mutex
	streams []*round4LifecycleStream
}

type round4DelayedOpener struct {
	entered chan struct{}
	release chan struct{}
	stream  *round4LifecycleStream
}

func (o *round4DelayedOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	close(o.entered)
	<-o.release
	return o.stream, nil
}

func TestRound4XAICloseDuringOpenReleasesReturnedSocket(t *testing.T) {
	st := &round4LifecycleStream{messages: make(chan *STTMessage)}
	opener := &round4DelayedOpener{entered: make(chan struct{}), release: make(chan struct{}), stream: st}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.ProcessFrame(context.Background(), &core.UserStartedSpeakingFrame{Gen: 1, Seq: 1}, core.Downstream, &mockEmitter{})
	}()
	<-opener.entered
	_ = p.Close()
	close(opener.release)
	<-done
	if got := st.closes.Load(); got != 1 {
		t.Fatalf("socket opened after Close was lost: closes=%d", got)
	}
}

func (o *round4ManyStreamsOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	st := &round4LifecycleStream{messages: make(chan *STTMessage, 1)}
	o.mu.Lock()
	o.streams = append(o.streams, st)
	o.mu.Unlock()
	return st, nil
}
func (o *round4ManyStreamsOpener) snapshot() []*round4LifecycleStream {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*round4LifecycleStream(nil), o.streams...)
}

func TestRound4XAIClosesEveryDrainingStreamAfterSessionCancel(t *testing.T) {
	opener := &round4ManyStreamsOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(p, &coretest.PassthroughProcessor{})
	s, _ := core.NewSession(991, pipe, core.DefaultResourceBudget())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 12; i++ {
		if err := s.Post(&core.UserStartedSpeakingFrame{Seq: uint64(i), Gen: 1}); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for len(opener.snapshot()) < i && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if len(opener.snapshot()) != i {
			t.Fatalf("opened %d of %d streams", len(opener.snapshot()), i)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for i, st := range opener.snapshot() {
		if n := st.closes.Load(); n != 1 {
			t.Fatalf("stream %d closed %d times", i, n)
		}
	}
}

func TestRound4XAICancelCanUnblockBufferedAudioSend(t *testing.T) {
	st := &round4LifecycleStream{messages: make(chan *STTMessage, 2), entered: make(chan struct{}), block: true}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = round4LifecycleOpener{st}
	p, _ := NewSTTProcessor(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &round4Session{ctx: ctx}
	p.Attach(session, 0)
	p.ProcessFrame(ctx, &core.UserStartedSpeakingFrame{Gen: 1, Seq: 1}, core.Downstream, &mockEmitter{})
	p.ProcessFrame(ctx, &core.InputAudioFrame{Gen: 1, Samples: []byte{0, 0}}, core.Downstream, &mockEmitter{})
	st.messages <- &STTMessage{Type: "transcript.created"}
	<-st.entered
	done := make(chan struct{})
	go func() {
		p.ProcessFrame(ctx, &core.TurnEventFrame{Kind: core.TurnEventInterrupted, Turn: "turn-1-1", Gen: 1}, core.Downstream, &mockEmitter{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("typed interruption blocked behind stream mutex held by SendAudio; only session cancellation releases it")
	}
	p.Close()
}
