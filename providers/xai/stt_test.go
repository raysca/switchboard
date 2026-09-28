//go:build xaispeech

package xai

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type mockSTTStream struct {
	mu       sync.Mutex
	messages []*STTMessage
	closed   bool
	audioIn  [][]byte
}

func (m *mockSTTStream) SendAudio(ctx context.Context, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("closed")
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.audioIn = append(m.audioIn, cp)
	return nil
}

func (m *mockSTTStream) CloseSend(ctx context.Context) error {
	return nil
}

func (m *mockSTTStream) Recv(ctx context.Context) (*STTMessage, error) {
	m.mu.Lock()
	if len(m.messages) > 0 {
		msg := m.messages[0]
		m.messages = m.messages[1:]
		m.mu.Unlock()
		return msg, nil
	}
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return nil, errors.New("stream closed")
	}
}

func (m *mockSTTStream) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

type mockSTTOpener struct {
	stream *mockSTTStream
	err    error
}

func (o *mockSTTOpener) OpenStream(ctx context.Context, cfg STTConfig) (STTStream, error) {
	if o.err != nil {
		return nil, o.err
	}
	return o.stream, nil
}

func TestXAISTT_Success(t *testing.T) {
	mockStream := &mockSTTStream{
		messages: []*STTMessage{
			{Type: "transcript.created"},
			{Type: "transcript.partial", Text: "Hello", IsFinal: false, SpeechFinal: false},
			{Type: "transcript.partial", Text: "Hello world", IsFinal: true, SpeechFinal: true},
			{Type: "transcript.done"},
		},
	}

	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &mockSTTOpener{stream: mockStream}

	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatalf("new stt processor: %v", err)
	}

	sink := &core.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}

	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := session.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer session.Close()

	// Post speech start and audio
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})

	// Wait for final transcription frame
	deadline := time.After(2 * time.Second)
	var finalSeen bool
	var interimSeen bool

	for !finalSeen {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for transcription frames")
		default:
			for _, f := range sink.GetFrames() {
				if it, ok := f.(*core.InterimTranscriptionFrame); ok && it.Text == "Hello" {
					interimSeen = true
				}
				if ft, ok := f.(*core.FinalTranscriptionFrame); ok && ft.Text == "Hello world" {
					finalSeen = true
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	if !interimSeen {
		t.Error("expected interim transcription frame")
	}
	if !finalSeen {
		t.Error("expected final transcription frame")
	}
}

func TestXAISTT_InterruptionFencing(t *testing.T) {
	mockStream := &mockSTTStream{
		messages: []*STTMessage{
			{Type: "transcript.created"},
		},
	}

	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &mockSTTOpener{stream: mockStream}

	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatalf("new stt processor: %v", err)
	}

	sink := &core.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}

	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := session.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer session.Close()

	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})

	// Trigger interruption
	_ = session.Post(&core.InterruptionFrame{Gen: 2, Turn: "turn-1"})

	time.Sleep(50 * time.Millisecond)
	proc.mu.Lock()
	act := proc.active
	proc.mu.Unlock()

	if act != nil {
		t.Error("expected active stream to be cleared on interruption")
	}
}

func TestXAISTT_TurnSemantics_IntermediateFinalVsSpeechFinal(t *testing.T) {
	mockStream := &mockSTTStream{
		messages: []*STTMessage{
			{Type: "transcript.created"},
			// Chunk final, but NOT speech final: must produce InterimTranscriptionFrame
			{Type: "transcript.partial", Text: "Good morning", IsFinal: true, SpeechFinal: false},
			// Utterance final: must produce FinalTranscriptionFrame
			{Type: "transcript.partial", Text: "Good morning everyone", IsFinal: true, SpeechFinal: true},
			{Type: "transcript.done"},
		},
	}

	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &mockSTTOpener{stream: mockStream}

	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatalf("new stt processor: %v", err)
	}

	sink := &core.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}

	session, err := core.NewSession(2, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := session.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer session.Close()

	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})

	deadline := time.After(2 * time.Second)
	var finalFrame *core.FinalTranscriptionFrame
	var interimFrames []*core.InterimTranscriptionFrame

	for finalFrame == nil {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for transcription frames")
		default:
			for _, f := range sink.GetFrames() {
				if it, ok := f.(*core.InterimTranscriptionFrame); ok {
					interimFrames = append(interimFrames, it)
				}
				if ft, ok := f.(*core.FinalTranscriptionFrame); ok {
					finalFrame = ft
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Verify "Good morning" with is_final=true and speech_final=false was emitted as Interim
	foundInterimChunkFinal := false
	for _, it := range interimFrames {
		if it.Text == "Good morning" {
			foundInterimChunkFinal = true
			break
		}
	}
	if !foundInterimChunkFinal {
		t.Error("expected 'Good morning' (speech_final=false) to be emitted as InterimTranscriptionFrame")
	}

	// Verify FinalTranscriptionFrame was emitted for speech_final=true
	if finalFrame.Text != "Good morning everyone" {
		t.Errorf("expected final transcript 'Good morning everyone', got %q", finalFrame.Text)
	}
}

func TestXAISTT_AudioBufferingBeforeReady(t *testing.T) {
	// Mock stream that delays emitting transcript.created
	mockStream := &mockSTTStream{
		messages: []*STTMessage{
			{Type: "transcript.created"},
			{Type: "transcript.partial", Text: "Received buffered audio", IsFinal: true, SpeechFinal: true},
		},
	}

	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &mockSTTOpener{stream: mockStream}

	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatalf("new stt processor: %v", err)
	}

	sink := &core.PassthroughProcessor{}
	pipe, err := core.NewPipeline(proc, sink)
	if err != nil {
		t.Fatalf("new pipeline: %v", err)
	}

	session, err := core.NewSession(3, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	if err := session.Start(); err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer session.Close()

	// Post audio immediately upon speech start (before transcript.created is processed)
	_ = session.Post(&core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	for i := 0; i < 5; i++ {
		_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})
	}

	deadline := time.After(2 * time.Second)
	var finalSeen bool
	for !finalSeen {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for final transcript")
		default:
			for _, f := range sink.GetFrames() {
				if ft, ok := f.(*core.FinalTranscriptionFrame); ok && ft.Text == "Received buffered audio" {
					finalSeen = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	deadlineFlush := time.After(1 * time.Second)
	var audioChunksReceived int
	for {
		mockStream.mu.Lock()
		audioChunksReceived = len(mockStream.audioIn)
		mockStream.mu.Unlock()
		if audioChunksReceived >= 5 {
			break
		}
		select {
		case <-deadlineFlush:
			t.Fatalf("expected at least 5 audio chunks flushed to stream, got %d", audioChunksReceived)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
