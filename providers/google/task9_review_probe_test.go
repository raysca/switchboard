package google

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

func TestReviewGooglePreservesNonoverlappingRepeatedWords(t *testing.T) {
	p, _ := NewSTTProcessor(DefaultSTTConfig())
	st := &activeStream{turnID: "A", gen: 1}
	_, _ = p.assembleResultLocked(st, &STTResult{Transcript: "very", IsFinal: true, ResultEndOffset: time.Second})
	got, _ := p.assembleResultLocked(st, &STTResult{Transcript: "very good", IsFinal: true, ResultEndOffset: 2 * time.Second})
	if got != "very very good" {
		t.Fatalf("nonoverlapping segments lost speech: got %q", got)
	}
}

func TestReviewGoogleBargeInUsesNewGeneration(t *testing.T) {
	cfg := DefaultSTTConfig()
	opener := &mockSTTOpener{}
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	pool := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	assistant := core.NewAssistantContextProcessor(pool, core.DefaultAssistantContextConfig())
	pipe, _ := core.NewPipeline(p, assistant, &coretest.PassthroughProcessor{})
	s, _ := core.NewSession(999, pipe, core.DefaultResourceBudget())
	s.Start()
	defer s.Close()
	s.Post(&core.UserMessageFrame{Turn: "bot", Gen: 1, Content: "prior question"})
	s.Post(&core.LLMTextDeltaFrame{Turn: "bot", Gen: 1, Delta: "bot is generating"})
	s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	deadline := time.Now().Add(time.Second)
	for s.CurrentGeneration() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.CurrentGeneration() != 2 {
		t.Fatal("barge-in did not advance generation")
	}
	s.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 2})
	for time.Now().Before(deadline) {
		p.mu.Lock()
		st := p.active
		var gen uint64
		if st != nil {
			gen = st.gen
		}
		p.mu.Unlock()
		if st != nil {
			if gen != 2 {
				t.Fatalf("barge-in stream has stale generation %d; session=2", gen)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream not opened")
}

type probeEmitter struct{}

func (probeEmitter) Push(core.Frame) error                          { return nil }
func (probeEmitter) PushDirection(core.Frame, core.Direction) error { return nil }

func TestReviewGoogleRotationOriginIsCurrentTurn(t *testing.T) {
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &mockSTTOpener{}
	p, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(p, &coretest.PassthroughProcessor{})
	s, _ := core.NewSession(998, pipe, core.DefaultResourceBudget())
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	for i := 0; i < 10; i++ {
		_ = s.Post(&core.InputAudioFrame{Samples: make([]byte, 3200), Gen: 1})
	}
	_ = s.Post(&core.UserStartedSpeakingFrame{Seq: 2, Gen: 1})
	for i := 0; i < 10; i++ {
		_ = s.Post(&core.InputAudioFrame{Samples: make([]byte, 3200), Gen: 1})
	}
	// The stream opens on a session task: wait for the second turn's audio and its stream.
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(time.Millisecond) {
		p.mu.Lock()
		ready := p.turnAudioBytes == 10*3200 && p.active != nil && p.active.stream != nil
		p.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second turn's stream never opened")
		}
	}
	p.rotateStream(context.Background())
	p.mu.Lock()
	got := p.active.offsetBase
	p.mu.Unlock()
	if got != 500*time.Millisecond {
		t.Fatalf("rotation offset includes previous turn: got=%v want=500ms", got)
	}
}

func TestReviewGoogleAssemblyCapacityPreservesNewTurn(t *testing.T) {
	p, _ := NewSTTProcessor(DefaultSTTConfig())
	for i := 0; i < 128; i++ {
		_, _ = p.assembleResultLocked(&activeStream{turnID: fmt.Sprint(i), gen: 1}, &STTResult{Transcript: "old", IsFinal: true, ResultEndOffset: time.Second})
	}
	st := &activeStream{turnID: "new", gen: 2}
	_, _ = p.assembleResultLocked(st, &STTResult{Transcript: "hello", IsFinal: true, ResultEndOffset: time.Second})
	got, _ := p.assembleResultLocked(st, &STTResult{Transcript: "world", IsFinal: true, ResultEndOffset: 2 * time.Second})
	if got != "hello world" {
		t.Fatalf("assembly capacity silently discarded prior segment: got %q", got)
	}
}

func TestReviewGoogleSegmentCapacityPreservesPrefix(t *testing.T) {
	p, _ := NewSTTProcessor(DefaultSTTConfig())
	st := &activeStream{turnID: "A", gen: 1}
	var got string
	for i := 1; i <= 129; i++ {
		got, _ = p.assembleResultLocked(st, &STTResult{Transcript: fmt.Sprintf("word%d", i), IsFinal: true, ResultEndOffset: time.Duration(i) * time.Second})
	}
	if !strings.HasPrefix(got, "word1 ") {
		t.Fatalf("segment capacity silently truncated first speech: prefix %.40q", got)
	}
}

func TestReviewGoogleBindUnownedStreamRace(t *testing.T) {
	cfg := DefaultSTTConfig()
	opener := &mockSTTOpener{}
	cfg.StreamOpener = opener
	p, _ := NewSTTProcessor(cfg)
	pipe, _ := core.NewPipeline(p)
	s, _ := core.NewSession(998, pipe, core.DefaultResourceBudget())
	s.Start()
	defer s.Close()
	s.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	var st *mockSTTStream
	deadline := time.Now().Add(time.Second)
	for st == nil && time.Now().Before(deadline) {
		opener.mu.Lock()
		if len(opener.streams) > 0 {
			st = opener.streams[0]
		}
		opener.mu.Unlock()
		time.Sleep(time.Microsecond)
	}
	if st == nil {
		t.Fatal("unowned audio stream did not open")
	}
	results := make([]*STTResult, 100000)
	result := &STTResult{Transcript: "early speech", IsFinal: true, ResultEndOffset: time.Second}
	for i := range results {
		results[i] = result
	}
	st.PushResults(results...)
	time.Sleep(100 * time.Microsecond)
	s.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	time.Sleep(20 * time.Millisecond)
}

func TestGoogleAssemblyBoundsFailExplicitlyWithoutTruncatingPrefix(t *testing.T) {
	p, _ := NewSTTProcessor(DefaultSTTConfig())
	for i := 0; i < maxGoogleAssemblies; i++ {
		if _, err := p.assembleResultLocked(&activeStream{turnID: fmt.Sprint(i), gen: 1}, &STTResult{Transcript: "kept", IsFinal: true, ResultEndOffset: time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.assembleResultLocked(&activeStream{turnID: "overflow", gen: 1}, &STTResult{Transcript: "reject", IsFinal: true, ResultEndOffset: time.Second}); !errors.Is(err, ErrTranscriptAssemblyCapacity) {
		t.Fatalf("assembly overflow did not fail explicitly: %v", err)
	}

	p, _ = NewSTTProcessor(DefaultSTTConfig())
	st := &activeStream{turnID: "A", gen: 1}
	for i := 1; i <= maxGoogleSegments; i++ {
		if _, err := p.assembleResultLocked(st, &STTResult{Transcript: fmt.Sprintf("w%d", i), IsFinal: true, ResultEndOffset: time.Duration(i) * time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	before := p.assemblies[googleAssemblyKey{turn: "A", gen: 1}].prefix
	if _, err := p.assembleResultLocked(st, &STTResult{Transcript: "overflow", IsFinal: true, ResultEndOffset: time.Duration(maxGoogleSegments+1) * time.Second}); !errors.Is(err, ErrTranscriptAssemblyCapacity) {
		t.Fatalf("segment overflow did not fail explicitly: %v", err)
	}
	if got := p.assemblies[googleAssemblyKey{turn: "A", gen: 1}].prefix; got != before || !strings.HasPrefix(got, "w1 ") {
		t.Fatalf("failed overflow corrupted prefix: got %.40q before %.40q", got, before)
	}
}
