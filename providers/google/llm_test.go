package google

import (
	"context"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

func TestLLM_StreamedTextAndUsage(t *testing.T) {
	mockClient := &mockLLMClient{}
	cfg := DefaultLLMConfig()
	cfg.Client = mockClient

	proc, err := NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}

	pipe, _ := core.NewPipeline(proc)
	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Attach(session, 0)

	var deltas []string
	var fullText string
	var usageFrames []*core.UsageMetricsFrame
	var mu sync.Mutex

	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		switch frame := f.(type) {
		case *core.LLMTextDeltaFrame:
			deltas = append(deltas, frame.Delta)
		case *core.LLMTextFrame:
			fullText = frame.Text
		case *core.UsageMetricsFrame:
			usageFrames = append(usageFrames, frame)
		}
	}

	_ = session.Start()
	defer session.Close()

	um := &core.UserMessageFrame{
		Turn:    "turn-1",
		Gen:     1,
		Content: "Hi assistant",
	}
	_ = session.Post(um)

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(deltas) != 2 {
		t.Fatalf("expected 2 deltas, got %d", len(deltas))
	}
	if deltas[0] != "Hello " || deltas[1] != "there!" {
		t.Fatalf("unexpected deltas: %v", deltas)
	}
	if fullText != "Hello there!" {
		t.Fatalf("expected 'Hello there!', got '%s'", fullText)
	}

	// Verify token usage metadata was captured
	if proc.PromptTokens.Load() != 10 {
		t.Fatalf("expected 10 prompt tokens, got %d", proc.PromptTokens.Load())
	}
	if proc.OutputTokens.Load() != 2 {
		t.Fatalf("expected 2 output tokens, got %d", proc.OutputTokens.Load())
	}
	if proc.TotalTokens.Load() != 12 {
		t.Fatalf("expected 12 total tokens, got %d", proc.TotalTokens.Load())
	}

	// Verify UsageMetricsFrame emission and correlation
	if len(usageFrames) != 1 {
		t.Fatalf("expected 1 UsageMetricsFrame, got %d", len(usageFrames))
	}
	uf := usageFrames[0]
	if uf.Turn != "turn-1" || uf.Gen != 1 || uf.Provider != "google" || uf.Service != "llm" {
		t.Fatalf("usage frame correlation mismatch: %+v", uf)
	}
	if uf.PromptTokens == nil || *uf.PromptTokens != 10 {
		t.Fatalf("expected prompt tokens 10, got %v", uf.PromptTokens)
	}
	if uf.CompletionTokens == nil || *uf.CompletionTokens != 2 {
		t.Fatalf("expected completion tokens 2, got %v", uf.CompletionTokens)
	}
	if uf.TotalTokens == nil || *uf.TotalTokens != 12 {
		t.Fatalf("expected total tokens 12, got %v", uf.TotalTokens)
	}
	if uf.Characters != nil {
		t.Fatalf("expected characters to remain absent, got %v", uf.Characters)
	}
}

func TestLLM_CancellationAndLateOutputFencing(t *testing.T) {
	streamStarted := make(chan struct{})
	mockClient := &mockLLMClient{
		generateFn: func(ctx context.Context, model string, systemPrompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
			chunkCh := make(chan LLMStreamChunk, 2)
			errCh := make(chan error, 1)

			go func() {
				defer close(chunkCh)
				defer close(errCh)

				close(streamStarted)

				// First chunk emitted
				select {
				case <-ctx.Done():
					return
				case chunkCh <- LLMStreamChunk{Text: "Started..."}:
				}

				// Wait until cancelled
				<-ctx.Done()

				// Attempt to emit late chunk after cancellation
				chunkCh <- LLMStreamChunk{Text: " Late chunk"}
			}()

			return chunkCh, errCh, nil
		},
	}

	cfg := DefaultLLMConfig()
	cfg.Client = mockClient
	proc, _ := NewLLMProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)

	var deltas []string
	var mu sync.Mutex
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		if delta, ok := f.(*core.LLMTextDeltaFrame); ok {
			deltas = append(deltas, delta.Delta)
		}
	}

	_ = session.Start()
	defer session.Close()

	_ = session.Post(&core.UserMessageFrame{
		Turn:    "turn-1",
		Gen:     1,
		Content: "Tell me a long story",
	})

	<-streamStarted

	// User barge-in: advance generation and send InterruptionFrame
	session.IncrementGeneration() // Gen = 2
	_ = session.Post(&core.InterruptionFrame{Turn: "turn-1", Gen: 1})

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// Should not have received " Late chunk"
	for _, d := range deltas {
		if d == " Late chunk" {
			t.Fatal("late chunk was delivered after interruption!")
		}
	}
}

func TestLLM_RetryBeforeAndAfterCommit(t *testing.T) {
	attempts := 0
	mockClient := &mockLLMClient{
		generateFn: func(ctx context.Context, model string, systemPrompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
			attempts++
			chunkCh := make(chan LLMStreamChunk, 1)
			errCh := make(chan error, 1)

			go func() {
				defer close(chunkCh)
				defer close(errCh)

				if attempts == 1 {
					// Fail before committing any chunk
					errCh <- &ProviderError{
						Capability: "llm",
						BaseErr:    ErrProviderUnavailable,
						StatusCode: 503,
						Retryable:  true,
					}
					return
				}
				// Attempt 2: succeed
				chunkCh <- LLMStreamChunk{Text: "Recovered output"}
			}()

			return chunkCh, errCh, nil
		},
	}

	cfg := DefaultLLMConfig()
	cfg.Client = mockClient
	cfg.RetryPolicy = RetryPolicy{
		InitialBackoff: 10 * time.Millisecond,
		MaxAttempts:    2,
	}
	proc, _ := NewLLMProcessor(cfg)
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)

	var deltas []string
	var mu sync.Mutex
	session.OnOutput = func(f core.Frame) {
		mu.Lock()
		defer mu.Unlock()
		if delta, ok := f.(*core.LLMTextDeltaFrame); ok {
			deltas = append(deltas, delta.Delta)
		}
	}

	_ = session.Start()
	defer session.Close()

	_ = session.Post(&core.UserMessageFrame{
		Turn:    "turn-1",
		Gen:     1,
		Content: "Retry test",
	})

	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(deltas) != 1 || deltas[0] != "Recovered output" {
		t.Fatalf("expected recovered output after retry before commit, got: %v", deltas)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestLLMForwardsUpstreamInterruption(t *testing.T) {
	proc, err := NewLLMProcessor(DefaultLLMConfig())
	if err != nil {
		t.Fatal(err)
	}
	em := &directionEmitter{}
	frame := &core.InterruptionFrame{Turn: "turn-1", Gen: 1}
	if err := proc.ProcessFrame(context.Background(), frame, core.Upstream, em); err != nil {
		t.Fatal(err)
	}
	if em.dir != core.Upstream {
		t.Fatalf("interruption direction = %v, want upstream", em.dir)
	}
}
