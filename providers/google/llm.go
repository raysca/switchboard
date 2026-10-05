package google

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"govoice/core"
	"govoice/core/tools"

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
	ToolDefinitions []core.ToolDefinition
	ToolExecutor    core.ToolExecutor
	ToolHistory     *core.ConversationContext
	ToolLoopConfig  tools.ToolLoopConfig
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
	ToolCalls    []core.ToolCall
	PromptTokens int
	OutputTokens int
	TotalTokens  int
	IsDone       bool
}

// LLMStreamClient abstracts streamed content generation.
type LLMStreamClient interface {
	GenerateStream(ctx context.Context, model string, systemPrompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error)
}

type LLMToolStreamClient interface {
	GenerateStreamWithTools(ctx context.Context, model string, systemPrompt string, history []core.Message, definitions []core.ToolDefinition) (<-chan LLMStreamChunk, <-chan error, error)
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
	return c.generateStream(ctx, model, systemPrompt, history, nil)
}

func (c *RealGoogleLLMClient) GenerateStreamWithTools(ctx context.Context, model, systemPrompt string, history []core.Message, definitions []core.ToolDefinition) (<-chan LLMStreamChunk, <-chan error, error) {
	return c.generateStream(ctx, model, systemPrompt, history, definitions)
}

func (c *RealGoogleLLMClient) generateStream(ctx context.Context, model, systemPrompt string, history []core.Message, definitions []core.ToolDefinition) (<-chan LLMStreamChunk, <-chan error, error) {
	contents := GoogleContents(history)

	var genConfig *genai.GenerateContentConfig
	if systemPrompt != "" {
		genConfig = &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{
				Parts: []*genai.Part{{Text: systemPrompt}},
			},
		}
	}
	if len(definitions) > 0 {
		declarations, err := GoogleToolDeclarations(definitions)
		if err != nil {
			return nil, nil, errInvalidGoogleTool
		}
		if genConfig == nil {
			genConfig = &genai.GenerateContentConfig{}
		}
		genConfig.Tools = []*genai.Tool{{FunctionDeclarations: declarations}}
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
			toolCalls, callErr := GoogleToolCalls(resp, core.ToolCall{})
			if callErr != nil {
				errCh <- errInvalidGoogleTool
				return
			}
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
				ToolCalls:    toolCalls,
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

var errGoogleToolClientUnsupported = errors.New("google: tool streaming client unsupported")

// NewLLMProcessor creates a new LLMProcessor.
func NewLLMProcessor(cfg LLMConfig) (*LLMProcessor, error) {
	if cfg.Model == "" {
		cfg.Model = "gemini-2.5-flash"
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = NewCircuitBreaker(DefaultCircuitConfig())
	}
	if len(cfg.ToolDefinitions) > 0 {
		if cfg.ToolExecutor == nil || cfg.ToolHistory == nil {
			return nil, errors.New("google: tool definitions require executor and history")
		}
		if _, err := GoogleToolDeclarations(cfg.ToolDefinitions); err != nil {
			return nil, errInvalidGoogleTool
		}
		if cfg.ToolLoopConfig.MaxRounds == 0 {
			cfg.ToolLoopConfig = tools.DefaultToolLoopConfig()
		}
		if _, err := tools.NewToolLoop(cfg.ToolDefinitions, cfg.ToolExecutor, cfg.ToolHistory, cfg.ToolLoopConfig); err != nil {
			return nil, err
		}
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

	initialHistory := []core.Message{
		{Role: core.RoleUser, Content: um.Content, TurnID: um.Turn},
	}
	if p.cfg.ContextProvider != nil {
		if provided := p.cfg.ContextProvider(); len(provided) > 0 {
			initialHistory = provided
		}
	} else if p.cfg.ToolHistory != nil {
		if provided := p.cfg.ToolHistory.Messages(); len(provided) > 0 {
			initialHistory = provided
		}
	}
	providerHistory := append([]core.Message(nil), initialHistory...)

	getHistory := func() []core.Message {
		return append([]core.Message(nil), providerHistory...)
	}

	// Launch async generation task on session supervisor
	p.Requests.Add(1)
	return p.session.Go(func(taskCtx context.Context) {
		defer cancel()

		var committed bool
		var fullTextBuilder strings.Builder
		requestModel := func(requestCtx context.Context) ([]core.ToolCall, error) {
			var chunkCh <-chan LLMStreamChunk
			var errCh <-chan error
			var err error
			if len(p.cfg.ToolDefinitions) > 0 {
				client, ok := p.cfg.Client.(LLMToolStreamClient)
				if !ok {
					return nil, errGoogleToolClientUnsupported
				}
				chunkCh, errCh, err = client.GenerateStreamWithTools(requestCtx, p.cfg.Model, p.cfg.SystemPrompt, getHistory(), p.cfg.ToolDefinitions)
			} else {
				chunkCh, errCh, err = p.cfg.Client.GenerateStream(requestCtx, p.cfg.Model, p.cfg.SystemPrompt, getHistory())
			}
			if err != nil {
				return nil, err
			}
			var calls []core.ToolCall
			var requestText strings.Builder
			for {
				select {
				case <-requestCtx.Done():
					return nil, requestCtx.Err()
				case <-taskCtx.Done():
					return nil, taskCtx.Err()
				case streamErr, ok := <-errCh:
					if ok && streamErr != nil {
						return nil, streamErr
					}
					errCh = nil
				case chunk, ok := <-chunkCh:
					if !ok {
						if len(calls) > 0 {
							if requestText.Len() > 0 {
								providerHistory = append(providerHistory, core.Message{Role: core.RoleAssistant, Content: requestText.String(), TurnID: turn})
							}
							for _, call := range calls {
								providerHistory = append(providerHistory, core.Message{Role: core.RoleAssistant, Kind: core.MessageToolCall, Content: string(call.Arguments), ToolCallID: call.ID, ToolName: call.Name, TurnID: turn, Generation: gen})
							}
						}
						return calls, nil
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
						if !p.isCurrentGeneration(gen) {
							p.Stale.Add(1)
							return nil, ErrGenerationMismatch
						}
						committed = true
						requestText.WriteString(chunk.Text)
						fullTextBuilder.WriteString(chunk.Text)
						deltaFrame := &core.LLMTextDeltaFrame{Delta: chunk.Text, Turn: turn, Gen: gen}
						_ = p.session.Reenter(deltaFrame, core.Downstream, p.index+1, gen)
					}
					if len(chunk.ToolCalls) > 0 {
						committed = true // Once a call is returned, do not retry this model request.
						for _, call := range chunk.ToolCalls {
							call.SessionID, call.TurnID, call.Generation = core.SessionID(strconv.FormatUint(p.session.ID(), 10)), core.TurnID(turn), core.Generation(gen)
							calls = append(calls, call)
						}
					}
				}
			}
		}
		requestWithRetry := func(requestCtx context.Context) ([]core.ToolCall, error) {
			var calls []core.ToolCall
			err := ExecuteWithRetry(requestCtx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, func() bool {
				return committed
			}, func(attemptCtx context.Context, attempt int) error {
				if attempt > 0 {
					p.Retries.Add(1)
				}
				var requestErr error
				calls, requestErr = requestModel(attemptCtx)
				return requestErr
			}, nil)
			return calls, err
		}

		calls, err := requestWithRetry(genCtx)
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
		if len(calls) > 0 {
			loopConfig := p.cfg.ToolLoopConfig
			loopConfig.IsCurrent = func(call core.ToolCall) bool { return p.isCurrentGeneration(uint64(call.Generation)) }
			loop, loopErr := tools.NewToolLoop(p.cfg.ToolDefinitions, p.cfg.ToolExecutor, p.cfg.ToolHistory, loopConfig)
			if loopErr != nil {
				p.Errors.Add(1)
				return
			}
			_, loopErr = loop.Run(genCtx, calls, func(continueCtx context.Context, results []core.ToolResult) ([]core.ToolCall, bool, error) {
				committed = false
				for _, result := range results {
					name := ""
					for i := len(providerHistory) - 1; i >= 0; i-- {
						if providerHistory[i].Kind == core.MessageToolCall && providerHistory[i].ToolCallID == result.CallID {
							name = providerHistory[i].ToolName
							break
						}
					}
					providerHistory = append(providerHistory, core.Message{Role: core.RoleTool, Kind: core.MessageToolResult, Content: string(result.Result), ToolCallID: result.CallID, ToolName: name, ToolStatus: result.Status, ToolErrorCategory: result.ErrorCategory, TurnID: turn, Generation: gen})
				}
				next, requestErr := requestWithRetry(continueCtx)
				return next, len(next) == 0, requestErr
			})
			if loopErr != nil {
				if errors.Is(loopErr, context.Canceled) || errors.Is(loopErr, ErrGenerationMismatch) || errors.Is(loopErr, tools.ErrStaleToolCall) {
					return
				}
				p.Errors.Add(1)
				_ = p.session.Reenter(&core.ErrorFrame{Err: errors.New("google: tool execution failed"), Fatal: false}, core.Downstream, p.index+1, gen)
				return
			}
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

func (p *LLMProcessor) isCurrentGeneration(generation uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.activeGen
	if p.session != nil {
		current = p.session.CurrentGeneration()
	}
	return generation == current
}
