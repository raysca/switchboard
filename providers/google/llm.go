package google

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"govoice/core"

	"google.golang.org/genai"
)

// LLMConfig configures the Google Gemini LLM processor.
type LLMConfig struct {
	Model           string // Pinned: "gemini-2.5-flash"
	SystemPrompt    string // System instructions
	MaxTokens       int    // Output token limit
	Temperature     float32
	AuthMode        AuthMode
	Credentials     *ResolvedCredentials
	Client          LLMStreamClient
	RetryPolicy     RetryPolicy
	CircuitBreaker  *CircuitBreaker
	ContextProvider func() []core.Message
}

// DefaultLLMConfig returns configuration matching v1-lock.json.
func DefaultLLMConfig() LLMConfig {
	return LLMConfig{
		Model:          "gemini-2.5-flash",
		Temperature:    0.7,
		RetryPolicy:    DefaultRetryPolicy(),
		CircuitBreaker: NewCircuitBreaker(DefaultCircuitConfig()),
	}
}

// LLMStreamChunk represents a single chunk from the LLM stream.
type LLMStreamChunk struct {
	Text         string
	PromptTokens int
	OutputTokens int
	TotalTokens  int
	IsDone       bool
}

// LLMStreamClient abstracts streamed content generation.
type LLMStreamClient interface {
	GenerateStream(ctx context.Context, model string, systemPrompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error)
}

// RealGoogleLLMClient uses google.golang.org/genai.
type RealGoogleLLMClient struct {
	client *genai.Client
}

func NewRealGoogleLLMClient(ctx context.Context, creds *ResolvedCredentials) (*RealGoogleLLMClient, error) {
	if creds == nil {
		return nil, fmt.Errorf("credentials cannot be nil")
	}

	cfg := &genai.ClientConfig{}
	switch creds.Mode {
	case AuthModeVertexADC:
		cfg.Backend = genai.BackendVertexAI
		cfg.Project = creds.ProjectID
		cfg.Location = creds.Location
	case AuthModeGeminiAPIKey:
		cfg.Backend = genai.BackendGeminiAPI
		cfg.APIKey = creds.APIKey
	default:
		return nil, fmt.Errorf("unsupported auth mode: %v", creds.Mode)
	}

	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, MapError("llm", err)
	}
	return &RealGoogleLLMClient{client: client}, nil
}

func (c *RealGoogleLLMClient) GenerateStream(ctx context.Context, model string, systemPrompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
	var contents []*genai.Content
	for _, m := range history {
		role := "user"
		if m.Role == core.RoleAssistant {
			role = "model"
		} else if m.Role == core.RoleSystem {
			continue // System instructions passed via config
		}
		contents = append(contents, &genai.Content{
			Role: role,
			Parts: []*genai.Part{
				{Text: m.Content},
			},
		})
	}

	var genConfig *genai.GenerateContentConfig
	if systemPrompt != "" {
		genConfig = &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{
				Parts: []*genai.Part{{Text: systemPrompt}},
			},
		}
	}

	chunkCh := make(chan LLMStreamChunk, 32)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)
		defer close(errCh)

		seq := c.client.Models.GenerateContentStream(ctx, model, contents, genConfig)
		for resp, err := range seq {
			if err != nil {
				errCh <- MapError("llm", err)
				return
			}
			if resp == nil {
				continue
			}

			text := resp.Text()
			promptTok := 0
			outTok := 0
			totTok := 0
			if resp.UsageMetadata != nil {
				promptTok = int(resp.UsageMetadata.PromptTokenCount)
				outTok = int(resp.UsageMetadata.CandidatesTokenCount)
				totTok = int(resp.UsageMetadata.TotalTokenCount)
			}

			select {
			case <-ctx.Done():
				return
			case chunkCh <- LLMStreamChunk{
				Text:         text,
				PromptTokens: promptTok,
				OutputTokens: outTok,
				TotalTokens:  totTok,
			}:
			}
		}
	}()

	return chunkCh, errCh, nil
}

// LLMProcessor implements core.Processor for Google Gemini LLM.
type LLMProcessor struct {
	cfg     LLMConfig
	session core.SessionController
	index   int

	mu           sync.Mutex
	activeCancel context.CancelFunc
	activeGen    uint64
	activeTurn   string
	closed       bool

	Requests atomic.Uint64
	Errors   atomic.Uint64
	Retries  atomic.Uint64
	Stale    atomic.Uint64

	// Usage tracking
	PromptTokens atomic.Int64
	OutputTokens atomic.Int64
	TotalTokens  atomic.Int64
}

// NewLLMProcessor creates a new LLMProcessor.
func NewLLMProcessor(cfg LLMConfig) (*LLMProcessor, error) {
	if cfg.Model == "" {
		cfg.Model = "gemini-2.5-flash"
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = NewCircuitBreaker(DefaultCircuitConfig())
	}
	return &LLMProcessor{
		cfg: cfg,
	}, nil
}

// Attach registers session controller and index.
// SetContextProvider supplies bounded multi-turn history for each generation.
func (p *LLMProcessor) SetContextProvider(fn func() []core.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.ContextProvider = fn
}

func (p *LLMProcessor) Attach(s core.SessionController, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = s
	p.index = index
	return nil
}

// Close terminates any active generation.
func (p *LLMProcessor) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.activeCancel != nil {
		p.activeCancel()
		p.activeCancel = nil
	}
	return nil
}

// ProcessFrame handles incoming frames.
func (p *LLMProcessor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.UserMessageFrame:
		// User turn complete -> trigger LLM generation
		return p.handleUserTurn(ctx, frame, e)

	case *core.InterruptionFrame:
		p.mu.Lock()
		if p.activeCancel != nil {
			p.activeCancel()
			p.activeCancel = nil
		}
		p.mu.Unlock()
		return e.PushDirection(f, dir)

	case *core.CancelFrame:
		p.mu.Lock()
		if p.activeCancel != nil {
			p.activeCancel()
			p.activeCancel = nil
		}
		p.mu.Unlock()
		return e.PushDirection(f, dir)

	default:
		return e.PushDirection(f, dir)
	}
}

func (p *LLMProcessor) handleUserTurn(ctx context.Context, um *core.UserMessageFrame, e core.Emitter) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}

	// Cancel any existing generation
	if p.activeCancel != nil {
		p.activeCancel()
	}

	gen := um.Gen
	if p.session != nil {
		gen = p.session.CurrentGeneration()
	}
	turn := um.Turn

	parent := context.Background()
	if p.session != nil {
		parent = p.session.Context()
	}
	genCtx, cancel := context.WithCancel(parent)
	p.activeCancel = cancel
	p.activeGen = gen
	p.activeTurn = turn
	p.mu.Unlock()

	// Push UserMessageFrame downstream so context processor records it
	if err := e.Push(um); err != nil {
		cancel()
		return err
	}

	if p.session == nil || p.cfg.Client == nil {
		cancel()
		return nil
	}

	history := []core.Message{
		{Role: core.RoleUser, Content: um.Content, TurnID: um.Turn},
	}
	if p.cfg.ContextProvider != nil {
		if provided := p.cfg.ContextProvider(); len(provided) > 0 {
			history = provided
		}
	}

	// Launch async generation task on session supervisor
	p.Requests.Add(1)
	return p.session.Go(func(taskCtx context.Context) {
		defer cancel()

		var committed bool
		var fullTextBuilder strings.Builder

		err := ExecuteWithRetry(genCtx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, func() bool {
			return committed
		}, func(attemptCtx context.Context, attempt int) error {
			if attempt > 0 {
				p.Retries.Add(1)
			}
			chunkCh, errCh, err := p.cfg.Client.GenerateStream(attemptCtx, p.cfg.Model, p.cfg.SystemPrompt, history)
			if err != nil {
				return err
			}

			for {
				select {
				case <-attemptCtx.Done():
					return attemptCtx.Err()
				case <-taskCtx.Done():
					return taskCtx.Err()
				case err, ok := <-errCh:
					if ok && err != nil {
						return err
					}
				case chunk, ok := <-chunkCh:
					if !ok {
						return nil // Stream complete
					}

					if chunk.PromptTokens > 0 {
						p.PromptTokens.Store(int64(chunk.PromptTokens))
					}
					if chunk.OutputTokens > 0 {
						p.OutputTokens.Store(int64(chunk.OutputTokens))
					}
					if chunk.TotalTokens > 0 {
						p.TotalTokens.Store(int64(chunk.TotalTokens))
					}

					if chunk.Text != "" {
						p.mu.Lock()
						currentGen := p.activeGen
						if p.session != nil {
							currentGen = p.session.CurrentGeneration()
						}
						// Fence late output from old generation
						if gen != currentGen {
							p.mu.Unlock()
							p.Stale.Add(1)
							return ErrGenerationMismatch
						}
						p.mu.Unlock()

						committed = true
						fullTextBuilder.WriteString(chunk.Text)

						// Reenter delta into pipeline
						deltaFrame := &core.LLMTextDeltaFrame{
							Delta: chunk.Text,
							Turn:  turn,
							Gen:   gen,
						}
						_ = p.session.Reenter(deltaFrame, core.Downstream, p.index+1, gen)
					}
				}
			}
		}, nil)

		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, ErrGenerationMismatch) {
				return // Normal interruption / stale generation
			}
			p.Errors.Add(1)
			// Emit safe error frame
			provErr := MapError("llm", err)
			_ = p.session.Reenter(&core.ErrorFrame{Err: provErr, Fatal: false}, core.Downstream, p.index+1, gen)
			return
		}

		// Stream completed successfully, emit complete LLMTextFrame
		p.mu.Lock()
		currentGen := p.activeGen
		if p.session != nil {
			currentGen = p.session.CurrentGeneration()
		}
		if gen != currentGen {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		fullFrame := &core.LLMTextFrame{
			Text: fullTextBuilder.String(),
			Turn: turn,
			Gen:  gen,
		}
		_ = p.session.Reenter(fullFrame, core.Downstream, p.index+1, gen)
	})
}
