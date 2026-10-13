package google

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	tr "govoice/core/transcript"
)

func TestRound4GoogleFinalPrefixThenInterimTail(t *testing.T) {
	p, _ := NewSTTProcessor(DefaultSTTConfig())
	st := &activeStream{turnID: "A", gen: 1, id: 1}
	cfg := tr.DefaultConfig()
	f, _ := tr.NewFinalizer(cfg)
	at := time.Unix(100, 0)
	for i, r := range []*STTResult{{Transcript: "hello", IsFinal: true, ResultEndOffset: time.Second}, {Transcript: "world", ResultEndOffset: 2 * time.Second}} {
		p.resetInterimsLocked(st)
		text, err := p.assembleResultLocked(st, r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Observe(tr.Candidate{Turn: "A", Generation: 1, Text: text, At: at.Add(time.Duration(i) * time.Millisecond), Final: r.IsFinal}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.Complete("A", 1, at.Add(10*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	out, err := f.Advance(at.Add(3 * time.Second))
	if err != nil || len(out) != 1 || out[0].Text != "hello world" {
		t.Fatalf("final-prefix/interim-tail lost: %v %v", out, err)
	}
}

func TestRound4GoogleRotatedInterimProvenance(t *testing.T) {
	p, _ := NewSTTProcessor(DefaultSTTConfig())
	old := &activeStream{turnID: "A", gen: 1, id: 1}
	p.assembleResultLocked(old, &STTResult{Transcript: "hello world", IsFinal: true, ResultEndOffset: 2 * time.Second})
	rotated := &activeStream{turnID: "A", gen: 1, id: 2, offsetBase: time.Second}
	p.resetInterimsLocked(rotated)
	text, err := p.assembleResultLocked(rotated, &STTResult{Transcript: "world again", ResultEndOffset: 2 * time.Second})
	if err != nil || text != "hello world again" {
		t.Fatalf("overlap interim duplicated replay: text=%q err=%v", text, err)
	}
}

type round4Blocker struct {
	entered          chan struct{}
	release          chan struct{}
	candidateSeen    chan struct{}
	invalidationSeen chan struct{}
	errorSeen        chan struct{}
	once             sync.Once
	candidateOnce    sync.Once
	invalidationOnce sync.Once
	errorOnce        sync.Once
	mu               sync.Mutex
	ordered          []core.FrameType
}

func (b *round4Blocker) ProcessFrame(_ context.Context, f core.Frame, d core.Direction, e core.Emitter) error {
	if f.Type() == core.FrameTranscriptCandidate {
		b.candidateOnce.Do(func() { close(b.candidateSeen) })
	}
	if f.Type() == core.FrameTranscriptInvalidation {
		b.invalidationOnce.Do(func() { close(b.invalidationSeen) })
	}
	if ef, ok := f.(*core.ErrorFrame); ok && errors.Is(ef.Err, core.ErrTranscriptInvalidated) {
		b.errorOnce.Do(func() { close(b.errorSeen) })
	}
	if f.Type() == core.FrameTranscriptCandidate || f.Type() == core.FrameTranscriptInvalidation || f.Type() == core.FrameError {
		b.mu.Lock()
		b.ordered = append(b.ordered, f.Type())
		b.mu.Unlock()
	}
	if m, ok := f.(*core.UserMessageFrame); ok && m.Content == "block" {
		b.once.Do(func() { close(b.entered) })
		<-b.release
	}
	return e.PushDirection(f, d)
}

func TestRound4GoogleFullQueueInvalidationFailsClosed(t *testing.T) {
	opener := &mockSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	fc := tr.DefaultConfig()
	fc.GracePeriod = 0
	finalizer, _ := tr.NewProcessor(fc)
	sink := &coretest.PassthroughProcessor{}
	block := &round4Blocker{entered: make(chan struct{}), release: make(chan struct{}), candidateSeen: make(chan struct{}), invalidationSeen: make(chan struct{}), errorSeen: make(chan struct{})}
	pipe, _ := core.NewPipeline(p, block, finalizer, sink)
	budget := core.DefaultResourceBudget()
	budget.MaxQueuedFrames = 1
	s, _ := core.NewSession(994, pipe, budget)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertNoCommitted := func() {
		t.Helper()
		for _, frame := range sink.GetFrames() {
			if committed, ok := frame.(*core.FinalTranscriptionFrame); ok {
				t.Fatalf("invalidation committed %d-byte prefix; session err=%v", len(committed.Text), s.Err())
			}
		}
	}
	post := func(f core.Frame) {
		t.Helper()
		if err := s.Post(f); err != nil {
			t.Fatal(err)
		}
	}
	post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	time.Sleep(5 * time.Millisecond)
	post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	var st *mockSTTStream
	for limit := time.Now().Add(time.Second); time.Now().Before(limit); {
		opener.mu.Lock()
		if len(opener.streams) > 0 {
			st = opener.streams[0]
		}
		opener.mu.Unlock()
		if st != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if st == nil {
		t.Fatal("no stream")
	}
	st.PushResult(&STTResult{Transcript: strings.Repeat("x", 17000), IsFinal: true, ResultEndOffset: time.Second})
	select {
	case <-block.candidateSeen:
	case <-time.After(time.Second):
		t.Fatal("first candidate was not admitted")
	}
	post(&core.UserMessageFrame{Content: "block", Gen: 1})
	<-block.entered
	post(&core.UserMessageFrame{Content: "fill", Gen: 1})
	st.PushResult(&STTResult{Transcript: strings.Repeat("y", 17000), IsFinal: true, ResultEndOffset: 2 * time.Second})
	time.Sleep(10 * time.Millisecond)
	close(block.release)
	select {
	case <-block.invalidationSeen:
	case <-s.Context().Done():
		if s.Err() == nil {
			t.Fatal("session closed without a terminal error")
		}
		assertNoCommitted()
		return
	case <-time.After(time.Second):
		t.Fatal("invalidation neither admitted nor failed the session closed")
	}
	select {
	case <-block.errorSeen:
	case <-s.Context().Done():
		if s.Err() == nil {
			t.Fatal("session closed without a terminal error")
		}
		assertNoCommitted()
		return
	case <-time.After(time.Second):
		t.Fatal("ordered observable error neither admitted nor failed the session closed")
	}
	post(&core.UserStoppedSpeakingFrame{Gen: 1, Seq: 2})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, f := range sink.GetFrames() {
			if _, ok := f.(*core.UserStoppedSpeakingFrame); ok {
				goto drained
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("pipeline did not drain after invalidation")
drained:
	assertNoCommitted()
	block.mu.Lock()
	ordered := append([]core.FrameType(nil), block.ordered...)
	block.mu.Unlock()
	if len(ordered) < 3 || ordered[0] != core.FrameTranscriptCandidate || ordered[1] != core.FrameTranscriptInvalidation || ordered[2] != core.FrameError {
		t.Fatalf("candidate/invalidation/error order lost: %v", ordered)
	}
	for _, f := range sink.GetFrames() {
		if ef, ok := f.(*core.ErrorFrame); ok && errors.Is(ef.Err, core.ErrTranscriptInvalidated) && ef.Turn == "turn-1-1" && ef.Gen == 1 {
			return
		}
	}
	t.Fatal("correlated observable invalidation error missing")
}
