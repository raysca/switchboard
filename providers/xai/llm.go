package xai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
)

const (
	PinnedModelID  = "grok-2-1212"
	PinnedEndpoint = "https://api.x.ai/v1/responses"
)

// Config configures the xAI LLM processor.
type Config struct {
	Endpoint        string
	Model           string
	APIKey          string
	SystemPrompt    string
	ContextProvider func() []core.Message
	Timeout         time.Duration
	RetryPolicy     RetryPolicy
	CircuitBreaker  *CircuitBreaker
	MaxLineLength   int
	MaxEventSize    int
	MaxTotalSize    int
	HTTPClient      *http.Client
}

// DefaultConfig returns the frozen production configuration for xAI LLM.
func DefaultConfig() Config {
	return Config{
		Endpoint:       PinnedEndpoint,
		Model:          PinnedModelID,
		Timeout:        30 * time.Second,
		RetryPolicy:    DefaultRetryPolicy(),
		CircuitBreaker: NewCircuitBreaker(DefaultCircuitConfig()),
		MaxLineLength:  DefaultMaxLineLength,
		MaxEventSize:   DefaultMaxEventSize,
		MaxTotalSize:   DefaultMaxTotalResponseSize,
	}
}

// Processor implements core.Processor for the xAI LLM Responses API.
type Processor struct {
	cfg     Config
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

	// Token usage telemetry
	PromptTokens atomic.Int64
	OutputTokens atomic.Int64
	TotalTokens  atomic.Int64
}

// NewProcessor creates an xAI LLM Processor.
func NewProcessor(cfg Config) (*Processor, error) {
	if cfg.Model == "" {
		cfg.Model = PinnedModelID
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = PinnedEndpoint
	}
	if cfg.APIKey == "" {
		cfg.APIKey = os.Getenv("XAI_API_KEY")
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = NewCircuitBreaker(DefaultCircuitConfig())
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy = DefaultRetryPolicy()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Timeout: cfg.Timeout,
		}
	}

	return &Processor{
		cfg: cfg,
	}, nil
}

// SetContextProvider supplies bounded conversation history for each request.
func (p *Processor) SetContextProvider(fn func() []core.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.ContextProvider = fn
}

func (p *Processor) Attach(s core.SessionController, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session = s
	p.index = index
	return nil
}

func (p *Processor) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.activeCancel != nil {
		p.activeCancel()
		p.activeCancel = nil
	}
	return nil
}

func (p *Processor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.UserMessageFrame:
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

	case *core.LLMTextDeltaFrame, *core.LLMTextFrame:
		// Re-entered frames proceed downstream
		return e.Push(f)
	}

	return e.PushDirection(f, dir)
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responsesRequest struct {
	Model    string        `json:"model"`
	Store    bool          `json:"store"`
	Stream   bool          `json:"stream"`
	Input    []chatMessage `json:"input,omitempty"`
	Messages []chatMessage `json:"messages,omitempty"`
}

type chatChunkChoice struct {
	Delta struct {
		Content string `json:"content"`
	} `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type chatChunkUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

type chatChunkResponse struct {
	Type     string            `json:"type"`
	Delta    string            `json:"delta"`
	Text     string            `json:"text"`
	Choices  []chatChunkChoice `json:"choices"`
	Usage    *chatChunkUsage   `json:"usage"`
	Response *struct {
		OutputText string          `json:"output_text"`
		Usage      *chatChunkUsage `json:"usage"`
	} `json:"response"`
}

func (p *Processor) handleUserTurn(ctx context.Context, f *core.UserMessageFrame, e core.Emitter) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return core.ErrSessionClosed
	}

	// Cancel any active previous generation
	if p.activeCancel != nil {
		p.activeCancel()
	}

	s := p.session
	if s == nil {
		p.mu.Unlock()
		return errors.New("processor not attached to session")
	}

	turn := f.Turn
	gen := s.CurrentGeneration()
	p.activeTurn = turn
	p.activeGen = gen

	parent := context.Background()
	if s != nil {
		parent = s.Context()
	}
	streamCtx, cancel := context.WithTimeout(parent, p.cfg.Timeout)
	p.activeCancel = cancel
	p.mu.Unlock()

	// Push UserMessageFrame downstream so AssistantContextProcessor initializes activeTurn
	if err := e.Push(f); err != nil {
		cancel()
		return err
	}

	// Build message history
	var history []core.Message
	if p.cfg.ContextProvider != nil {
		history = p.cfg.ContextProvider()
	} else if f.Content != "" {
		history = []core.Message{
			{Role: core.RoleUser, Content: f.Content, TurnID: f.Turn},
		}
	}

	var messages []chatMessage
	if p.cfg.SystemPrompt != "" {
		messages = append(messages, chatMessage{
			Role:    "system",
			Content: p.cfg.SystemPrompt,
		})
	}

	for _, m := range history {
		role := "user"
		switch m.Role {
		case core.RoleSystem:
			role = "system"
		case core.RoleAssistant:
			role = "assistant"
		case core.RoleUser:
			role = "user"
		}
		messages = append(messages, chatMessage{
			Role:    role,
			Content: m.Content,
		})
	}

	reqBody := responsesRequest{
		Model:  p.cfg.Model,
		Store:  false, // Spec: store: false MUST be sent on every request
		Stream: true,
	}
	if strings.Contains(p.cfg.Endpoint, "chat/completions") {
		reqBody.Messages = messages
	} else {
		reqBody.Input = messages
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		cancel()
		return fmt.Errorf("marshal xai request: %w", err)
	}

	// Supervised async execution
	p.Requests.Add(1)
	return s.Go(func(sessionCtx context.Context) {
		defer cancel()

		var committed bool
		var fullText strings.Builder
		var promptTok, outTok, totTok int

		opErr := ExecuteWithRetry(streamCtx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, &committed, func(attempt int) error {
			if attempt > 0 {
				p.Retries.Add(1)
			}
			httpReq, err := http.NewRequestWithContext(streamCtx, http.MethodPost, p.cfg.Endpoint, bytes.NewReader(jsonBytes))
			if err != nil {
				return err
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Accept", "text/event-stream")
			if p.cfg.APIKey != "" {
				httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
			}

			resp, err := p.cfg.HTTPClient.Do(httpReq)
			if err != nil {
				return MapHTTPError("llm", nil, "", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
				return MapHTTPError("llm", resp, string(bodyBytes), nil)
			}

			sse := NewSSEReader(resp.Body, p.cfg.MaxLineLength, p.cfg.MaxEventSize, p.cfg.MaxTotalSize)

			for {
				select {
				case <-streamCtx.Done():
					return streamCtx.Err()
				case <-sessionCtx.Done():
					return sessionCtx.Err()
				default:
				}

				event, err := sse.ReadEvent()
				if err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					return err
				}

				if event == nil || event.Data == "" {
					continue
				}

				var chunk chatChunkResponse
				if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
					return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
				}

				usage := chunk.Usage
				if usage == nil && chunk.Response != nil {
					usage = chunk.Response.Usage
				}
				if usage != nil {
					if usage.PromptTokens > 0 {
						promptTok = usage.PromptTokens
					} else if usage.InputTokens > 0 {
						promptTok = usage.InputTokens
					}
					if usage.CompletionTokens > 0 {
						outTok = usage.CompletionTokens
					} else if usage.OutputTokens > 0 {
						outTok = usage.OutputTokens
					}
					if usage.TotalTokens > 0 {
						totTok = usage.TotalTokens
					} else {
						totTok = promptTok + outTok
					}
				}
				if chunk.Type == "response.failed" {
					return fmt.Errorf("xai responses API returned response.failed")
				}
				if chunk.Type == "response.completed" || chunk.Type == "response.done" {
					return nil
				}

				// Extract delta content
				deltaText := chunk.Delta
				if deltaText == "" {
					deltaText = chunk.Text
				}
				if deltaText == "" && chunk.Response != nil {
					deltaText = chunk.Response.OutputText
				}
				if deltaText == "" && len(chunk.Choices) > 0 {
					deltaText = chunk.Choices[0].Delta.Content
				}

				if deltaText != "" {
					committed = true
					fullText.WriteString(deltaText)

					// Generation fencing check
					p.mu.Lock()
					isStale := p.activeGen != gen || p.closed
					p.mu.Unlock()

					if isStale {
						return nil // Discard stale delta
					}

					// Re-enter session run loop
					_ = s.Reenter(&core.LLMTextDeltaFrame{
						Delta: deltaText,
						Turn:  turn,
						Gen:   gen,
					}, core.Downstream, p.index, gen)
				}
			}

			if !sse.SawTerminal() {
				return ErrMissingTerminalEvent
			}

			return nil
		})

		if opErr != nil {
			if errors.Is(opErr, context.Canceled) || streamCtx.Err() != nil {
				return
			}
			p.Errors.Add(1)
			sanitizedErr := SanitizeError(opErr)
			_ = s.Reenter(&core.ErrorFrame{
				Err:   sanitizedErr,
				Fatal: false,
			}, core.Downstream, p.index, gen)
			return
		}

		// Update token telemetry
		p.PromptTokens.Add(int64(promptTok))
		p.OutputTokens.Add(int64(outTok))
		p.TotalTokens.Add(int64(totTok))

		// Emit final LLMTextFrame
		p.mu.Lock()
		isStale := p.activeGen != gen || p.closed
		p.mu.Unlock()

		if isStale {
			p.Stale.Add(1)
		} else {
			_ = s.Reenter(&core.LLMTextFrame{
				Text: fullText.String(),
				Turn: turn,
				Gen:  gen,
			}, core.Downstream, p.index, gen)
		}
	})
}
