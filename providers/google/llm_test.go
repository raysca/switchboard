package google

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/resilience"
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

	// Verify UsageMetricsFrame emission, token usage and correlation
	if len(usageFrames) != 1 {
		t.Fatalf("expected 1 UsageMetricsFrame, got %d", len(usageFrames))
	}
	uf := usageFrames[0]
	if *uf.PromptTokens != 10 || *uf.CompletionTokens != 2 || *uf.TotalTokens != 12 {
		t.Fatalf("usage = %d/%d/%d, want 10/2/12", *uf.PromptTokens, *uf.CompletionTokens, *uf.TotalTokens)
	}
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
	cfg.RetryPolicy = resilience.Policy{
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

// A panic in the stream goroutine is reported as an error, never a crash or a
// silent empty reply.
func TestStreamPanicIsReportedAsError(t *testing.T) {
	c := &RealGoogleLLMClient{} // no genai client: the goroutine dereferences nil
	chunks, errs, err := c.GenerateStream(t.Context(), "m", "", nil, Sampling{})
	if err != nil {
		t.Fatal(err)
	}
	for range chunks {
	}
	if e := <-errs; e == nil {
		t.Fatal("want a stream error")
	}
}

// The plain (no tools) path hands the configured sampling to the client.
func TestLLM_SamplingReachesTheClient(t *testing.T) {
	mock := &mockLLMClient{}
	cfg := DefaultLLMConfig()
	cfg.Client = mock
	topP := 0.9
	cfg.Sampling = Sampling{TopP: &topP, MaxOutputTokens: 200}
	proc, err := NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 0)
	done := make(chan struct{})
	session.OnOutput = func(f core.Frame) {
		if _, ok := f.(*core.LLMTextFrame); ok {
			close(done)
		}
	}
	_ = session.Start()
	defer session.Close()
	_ = session.Post(&core.UserMessageFrame{Turn: "t", Gen: 1, Content: "hi"})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("no reply")
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.samplings) != 1 || !reflect.DeepEqual(mock.samplings[0], cfg.Sampling) {
		t.Fatalf("samplings = %+v, want %+v", mock.samplings, cfg.Sampling)
	}
}

// The request config carries sampling next to the system prompt and tools; an
// explicit temperature 0 is sent, not mistaken for unset.
func TestGenerateContentConfigCarriesSampling(t *testing.T) {
	if c, err := generateContentConfig("", nil, Sampling{}); err != nil || c != nil {
		t.Fatalf("nothing set must send no config: %+v %v", c, err)
	}
	zero, topP := 0.0, 0.9
	c, err := generateContentConfig("sys", nil, Sampling{Temperature: &zero, TopP: &topP, MaxOutputTokens: 200})
	if err != nil || c == nil {
		t.Fatalf("%v %v", c, err)
	}
	if c.Temperature == nil || *c.Temperature != 0 || c.TopP == nil || *c.TopP != float32(0.9) || c.MaxOutputTokens != 200 || c.SystemInstruction == nil {
		t.Fatalf("config = %+v", c)
	}
	if c, _ := generateContentConfig("", nil, Sampling{MaxOutputTokens: 5}); c == nil || c.Temperature != nil || c.TopP != nil || c.MaxOutputTokens != 5 {
		t.Fatalf("unset temperature/top_p must stay nil: %+v", c)
	}
}
