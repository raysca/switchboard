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

	// Wait for raw candidates; provider adapters do not commit transcripts.
	deadline := time.After(2 * time.Second)
	var finalSeen bool
	var interimSeen bool

	for !finalSeen {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for transcription frames")
		default:
			for _, f := range sink.GetFrames() {
				if it, ok := f.(*core.TranscriptCandidateFrame); ok && it.Text == "Hello" && !it.Final {
					interimSeen = true
				}
				if ft, ok := f.(*core.TranscriptCandidateFrame); ok && ft.Text == "Hello world" && ft.Final {
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
			// Chunk final, but NOT speech final: provider-final candidate, not EOU.
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
	var finalFrame *core.TranscriptCandidateFrame
	var providerFinals []*core.TranscriptCandidateFrame

	for finalFrame == nil {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for transcription frames")
		default:
			for _, f := range sink.GetFrames() {
				if it, ok := f.(*core.TranscriptCandidateFrame); ok && it.Final && !it.EndOfUtterance {
					providerFinals = append(providerFinals, it)
				}
				if ft, ok := f.(*core.TranscriptCandidateFrame); ok && ft.Final {
					finalFrame = ft
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// is_final closes a provider segment; speech_final closes the utterance.
	foundChunkFinal := false
	for _, it := range providerFinals {
		if it.Text == "Good morning" {
			foundChunkFinal = true
			break
		}
	}
	if !foundChunkFinal {
		t.Error("expected is_final=true/speech_final=false provider-final candidate")
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
				if ft, ok := f.(*core.TranscriptCandidateFrame); ok && ft.Text == "Received buffered audio" && ft.Final {
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

func TestXAISTTEmitsRawCandidatesAndDoneIsUtteranceFinal(t *testing.T) {
	mockStream := &mockSTTStream{messages: []*STTMessage{
		{Type: "transcript.created"},
		{Type: "transcript.partial", Text: "chunk", IsFinal: true, SpeechFinal: false},
		{Type: "transcript.done"},
	}}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = &mockSTTOpener{stream: mockStream}
	cfg.Now = func() time.Time { return time.Unix(31, 0) }
	proc, _ := NewSTTProcessor(cfg)
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(31, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 8, Gen: 1})
	time.Sleep(100 * time.Millisecond)
	var candidates []*core.TranscriptCandidateFrame
	for _, frame := range sink.GetFrames() {
		if c, ok := frame.(*core.TranscriptCandidateFrame); ok {
			candidates = append(candidates, c)
		}
		if _, ok := frame.(*core.FinalTranscriptionFrame); ok {
			t.Fatalf("provider emitted a committed-frame type directly: %T", frame)
		}
	}
	if len(candidates) != 2 || !candidates[0].Final || candidates[0].EndOfUtterance || candidates[0].HasStability || !candidates[1].Final || !candidates[1].EndOfUtterance {
		t.Fatalf("xAI is_final/speech_final/done mapping mismatch: %#v", candidates)
	}
}

func TestXAISTTDoneUsesMessageTextAndFallsBackToLastText(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []*STTMessage
		want     string
	}{
		{"done-only", []*STTMessage{{Type: "transcript.created"}, {Type: "transcript.done", Text: "done text"}}, "done text"},
		{"correction", []*STTMessage{{Type: "transcript.created"}, {Type: "transcript.partial", Text: "old", IsFinal: true}, {Type: "transcript.done", Text: "corrected"}}, "corrected"},
		{"fallback", []*STTMessage{{Type: "transcript.created"}, {Type: "transcript.partial", Text: "last", IsFinal: true}, {Type: "transcript.done"}}, "last"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &mockSTTStream{messages: tc.messages}
			cfg := DefaultSTTConfig()
			cfg.StreamOpener = &mockSTTOpener{stream: stream}
			proc, _ := NewSTTProcessor(cfg)
			sink := &core.PassthroughProcessor{}
			pipe, _ := core.NewPipeline(proc, sink)
			session, _ := core.NewSession(40, pipe, core.DefaultResourceBudget())
			_ = session.Start()
			defer session.Close()
			_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
			time.Sleep(60 * time.Millisecond)
			var got string
			for _, frame := range sink.GetFrames() {
				if c, ok := frame.(*core.TranscriptCandidateFrame); ok && c.EndOfUtterance {
					got = c.Text
				}
			}
			if got != tc.want {
				t.Fatalf("done final=%q want=%q", got, tc.want)
			}
		})
	}
}

type channelSTTStream struct{ messages chan *STTMessage }

func (s *channelSTTStream) SendAudio(context.Context, []byte) error { return nil }
func (s *channelSTTStream) CloseSend(context.Context) error         { return nil }
func (s *channelSTTStream) Recv(ctx context.Context) (*STTMessage, error) {
	select {
	case msg := <-s.messages:
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *channelSTTStream) Close() error { return nil }

type sequenceSTTOpener struct {
	mu      sync.Mutex
	streams []*channelSTTStream
}

func (o *sequenceSTTOpener) OpenStream(context.Context, STTConfig) (STTStream, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := &channelSTTStream{messages: make(chan *STTMessage, 8)}
	o.streams = append(o.streams, st)
	return st, nil
}

func TestXAISTTStreamOwnershipIsImmutableAcrossTurns(t *testing.T) {
	opener := &sequenceSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, _ := NewSTTProcessor(cfg)
	sink := &core.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	session, _ := core.NewSession(41, pipe, core.DefaultResourceBudget())
	_ = session.Start()
	defer session.Close()
	openTurn := func(seq, gen uint64) *channelSTTStream {
		_ = session.Post(&core.UserStartedSpeakingFrame{Seq: seq, Gen: gen})
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			opener.mu.Lock()
			n := len(opener.streams)
			var st *channelSTTStream
			if n > 0 {
				st = opener.streams[n-1]
			}
			opener.mu.Unlock()
			if n >= int(seq) {
				return st
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("stream %d did not open", seq)
		return nil
	}
	a, b, c := openTurn(1, 1), openTurn(2, 1), openTurn(3, 2)
	a.messages <- &STTMessage{Type: "transcript.done", Text: "late A"}
	b.messages <- &STTMessage{Type: "transcript.done", Text: "turn B"}
	c.messages <- &STTMessage{Type: "transcript.done", Text: "turn C"}
	time.Sleep(30 * time.Millisecond)
	seen := map[string]string{}
	for _, frame := range sink.GetFrames() {
		if cand, ok := frame.(*core.TranscriptCandidateFrame); ok {
			seen[cand.Text] = string(cand.Turn)
		}
	}
	if seen["late A"] != "turn-1-1" || seen["turn B"] != "turn-1-2" || seen["turn C"] != "turn-2-3" {
		t.Fatalf("stream result relabeled: %#v", seen)
	}
}

type gatedSTTStream struct {
	*mockSTTStream
	gate chan struct{}
}

func (g *gatedSTTStream) Recv(ctx context.Context) (*STTMessage, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.mockSTTStream.Recv(ctx)
}

type gatedSTTOpener struct{ s *gatedSTTStream }

func (o gatedSTTOpener) OpenStream(context.Context, STTConfig) (STTStream, error) { return o.s, nil }

// An STT failover replays seconds of audio at once into a fresh stream, all of
// it before transcript.created: none may be dropped.
func TestXAISTTKeepsReplayBurstBeforeReady(t *testing.T) {
	ms := &mockSTTStream{messages: []*STTMessage{{Type: "transcript.created"}}}
	gs := &gatedSTTStream{mockSTTStream: ms, gate: make(chan struct{})}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = gatedSTTOpener{gs}
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(proc, &core.PassthroughProcessor{})
	session, _ := core.NewSession(4, pipe, core.DefaultResourceBudget())
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	const n = 500 // 10 s
	_ = proc.ProcessFrame(context.Background(), &core.UserStartedSpeakingFrame{Gen: 1, Seq: 1}, core.Downstream, nopEmit{})
	for i := 0; i < n; i++ {
		if err := proc.ProcessFrame(context.Background(), &core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000}, core.Downstream, nopEmit{}); err != nil {
			t.Fatal(err)
		}
	}
	close(gs.gate)
	deadline := time.Now().Add(2 * time.Second)
	for {
		ms.mu.Lock()
		got := len(ms.audioIn)
		ms.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("flushed %d of %d pre-ready chunks", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type nopEmit struct{}

func (nopEmit) Push(core.Frame) error                          { return nil }
func (nopEmit) PushDirection(core.Frame, core.Direction) error { return nil }
