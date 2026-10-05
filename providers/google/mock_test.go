package google

import (
	"context"
	"errors"
	"sync"

	"govoice/core"
)

type mockSTTStream struct {
	mu          sync.Mutex
	audioChunks [][]byte
	results     chan []*STTResult
	closed      chan struct{}
	closeOnce   sync.Once
}

func newMockSTTStream() *mockSTTStream {
	return &mockSTTStream{
		results: make(chan []*STTResult, 64),
		closed:  make(chan struct{}),
	}
}

func (s *mockSTTStream) Send(audio []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return errors.New("stream closed")
	default:
	}
	cpy := make([]byte, len(audio))
	copy(cpy, audio)
	s.audioChunks = append(s.audioChunks, cpy)
	return nil
}

func (s *mockSTTStream) PushResult(res *STTResult) {
	s.results <- []*STTResult{res}
}

func (s *mockSTTStream) PushResults(results ...*STTResult) {
	s.results <- results
}

func (s *mockSTTStream) Recv() ([]*STTResult, error) {
	select {
	case <-s.closed:
		return nil, context.Canceled
	case res, ok := <-s.results:
		if !ok {
			return nil, context.Canceled
		}
		return res, nil
	}
}

func (s *mockSTTStream) CloseSend() error {
	return nil
}

func (s *mockSTTStream) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
	})
	return nil
}

type mockSTTOpener struct {
	mu           sync.Mutex
	streams      []*mockSTTStream
	onOpenStream func(cfg STTConfig) *mockSTTStream
}

func (o *mockSTTOpener) OpenStream(ctx context.Context, cfg STTConfig) (STTStream, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	var st *mockSTTStream
	if o.onOpenStream != nil {
		st = o.onOpenStream(cfg)
	} else {
		st = newMockSTTStream()
	}
	o.streams = append(o.streams, st)
	return st, nil
}

type mockLLMClient struct {
	mu         sync.Mutex
	generateFn func(ctx context.Context, model string, systemPrompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error)
	calls      []string
}

func (m *mockLLMClient) GenerateStream(ctx context.Context, model string, systemPrompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
	m.mu.Lock()
	if len(history) > 0 {
		m.calls = append(m.calls, history[len(history)-1].Content)
	}
	fn := m.generateFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, model, systemPrompt, history)
	}

	chunkCh := make(chan LLMStreamChunk, 2)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)
		defer close(errCh)

		chunkCh <- LLMStreamChunk{Text: "Hello ", PromptTokens: 10, OutputTokens: 1, TotalTokens: 11}
		chunkCh <- LLMStreamChunk{Text: "there!", PromptTokens: 10, OutputTokens: 2, TotalTokens: 12}
	}()

	return chunkCh, errCh, nil
}

type mockTTSClient struct {
	mu           sync.Mutex
	synthesizeFn func(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error)
	calls        []string
}

func (m *mockTTSClient) SynthesizeSpeech(ctx context.Context, text string, voice string, sampleRate int) ([]byte, error) {
	m.mu.Lock()
	m.calls = append(m.calls, text)
	fn := m.synthesizeFn
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, text, voice, sampleRate)
	}
	// Return 100ms of 24kHz PCM16LE mono (4800 bytes)
	return make([]byte, 4800), nil
}
