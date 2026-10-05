package xai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	coretools "govoice/core/tools"
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
	ToolDefinitions []core.ToolDefinition
	ToolLoop        *coretools.ToolLoop
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
	if _, err := MapToolDefinitions(cfg.ToolDefinitions); err != nil {
		return nil, err
	}
	if len(cfg.ToolDefinitions) > 0 && cfg.ToolLoop == nil {
		return nil, errors.New("xai: tool definitions require a shared ToolLoop")
	}
	if len(cfg.ToolDefinitions) > 0 && strings.Contains(cfg.Endpoint, "chat/completions") {
		return nil, errors.New("xai: shared tool adapter requires the Responses API endpoint")
	}
	cfg.ToolDefinitions = cloneXAIToolDefinitions(cfg.ToolDefinitions)

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
	if p.cfg.ToolLoop != nil {
		p.cfg.ToolLoop.Close()
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
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	Type      string          `json:"type,omitempty"`
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

func (m chatMessage) MarshalJSON() ([]byte, error) {
	if len(m.Raw) > 0 {
		return append([]byte(nil), m.Raw...), nil
	}
	type plain chatMessage
	return json.Marshal(plain(m))
}

type responsesRequest struct {
	Model    string              `json:"model"`
	Store    bool                `json:"store"`
	Stream   bool                `json:"stream"`
	Tools    []XAIToolDefinition `json:"tools,omitempty"`
	Input    []chatMessage       `json:"input,omitempty"`
	Messages []chatMessage       `json:"messages,omitempty"`
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
	Type        string            `json:"type"`
	Delta       string            `json:"delta"`
	Text        string            `json:"text"`
	ItemID      string            `json:"item_id"`
	OutputIndex int               `json:"output_index"`
	Arguments   string            `json:"arguments"`
	Choices     []chatChunkChoice `json:"choices"`
	Usage       *chatChunkUsage   `json:"usage"`
	Response    *struct {
		OutputText string                  `json:"output_text"`
		Usage      *chatChunkUsage         `json:"usage"`
		Output     []XAIResponseOutputItem `json:"output"`
	} `json:"response"`
	Item *XAIResponseOutputItem `json:"item"`
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

	// The ProcessFrame context carries the session/turn span. Preserve it when
	// making this cancellable provider request instead of replacing it with the
	// lifetime-only session context.
	parent := ctx
	if parent == nil {
		parent = context.Background()
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

	messages = append(messages, xaiHistoryItems(history)...)

	providerTools, err := MapToolDefinitions(p.cfg.ToolDefinitions)
	if err != nil {
		cancel()
		return err
	}
	reqBody := responsesRequest{
		Model:  p.cfg.Model,
		Store:  false, // Spec: store: false MUST be sent on every request
		Stream: true,
		Tools:  providerTools,
	}
	if strings.Contains(p.cfg.Endpoint, "chat/completions") {
		reqBody.Messages = messages
	} else {
		reqBody.Input = messages
	}

	// Supervised async execution
	p.Requests.Add(1)
	return s.Go(func(sessionCtx context.Context) {
		defer cancel()
		var fullText strings.Builder
		calls, promptTok, outTok, totTok, opErr := p.requestRound(streamCtx, sessionCtx, reqBody, gen, turn, s, &fullText)
		if opErr == nil && !p.isCurrent(gen) {
			p.Stale.Add(1)
			return
		}
		initialCalls := ParseXAIToolCalls(calls, fmt.Sprint(s.ID()), turn, gen)
		if opErr == nil && len(initialCalls) > 0 {
			if p.cfg.ToolLoop == nil {
				opErr = errors.New("xai: model returned tool calls but no shared ToolLoop is configured")
			} else {
				currentLoop := p.cfg.ToolLoop.WithTraceContext(streamCtx).WithCurrentFence(func(call core.ToolCall) bool {
					return call.SessionID == core.SessionID(fmt.Sprint(s.ID())) && call.TurnID == core.TurnID(turn) && call.Generation == core.Generation(gen) && p.isCurrent(gen)
				})
				_, opErr = currentLoop.Run(streamCtx, initialCalls, func(roundCtx context.Context, results []core.ToolResult) ([]core.ToolCall, bool, error) {
					if roundCtx.Err() != nil || sessionCtx.Err() != nil || !p.isCurrent(gen) {
						return nil, false, context.Canceled
					}
					byID := make(map[string]XAIResponseOutputItem, len(calls))
					for _, item := range calls {
						if item.Type == "function_call" {
							byID[item.CallID] = item
						}
					}
					for _, item := range calls {
						var callInput chatMessage
						if len(item.Raw) > 0 {
							callInput.Raw = append(json.RawMessage(nil), item.Raw...)
						} else {
							callInput = chatMessage{Type: "function_call", ID: item.ID, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments}
						}
						reqBody.Input = append(reqBody.Input, callInput)
					}
					for _, result := range results {
						if _, ok := byID[result.CallID]; !ok {
							return nil, false, errors.New("xai: tool result has no matching provider call")
						}
						wireOutput, encodeErr := XAIToolResultOutput(result)
						if encodeErr != nil {
							return nil, false, encodeErr
						}
						reqBody.Input = append(reqBody.Input, chatMessage{Type: "function_call_output", CallID: result.CallID, Output: wireOutput})
					}
					nextCalls, pt, ot, tt, nextErr := p.requestRound(roundCtx, sessionCtx, reqBody, gen, turn, s, &fullText)
					promptTok += pt
					outTok += ot
					totTok += tt
					calls = nextCalls
					nextToolCalls := ParseXAIToolCalls(nextCalls, fmt.Sprint(s.ID()), turn, gen)
					return nextToolCalls, len(nextToolCalls) == 0, nextErr
				})
			}
		}

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
		if !p.isCurrent(gen) {
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

func xaiHistoryItems(history []core.Message) []chatMessage {
	items := make([]chatMessage, 0, len(history))
	for _, message := range history {
		switch {
		case message.Kind == core.MessageToolCall:
			items = append(items, chatMessage{Type: "function_call", CallID: message.ToolCallID, Name: message.ToolName, Arguments: message.Content})
		case message.Kind == core.MessageToolResult || message.Role == core.RoleTool:
			status := message.ToolStatus
			if status == "" {
				status = core.ToolSucceeded
			}
			wire, err := XAIToolResultOutput(core.ToolResult{CallID: message.ToolCallID, Status: status, Result: json.RawMessage(message.Content), ErrorCategory: message.ToolErrorCategory})
			if err != nil {
				wire = `{"error":"invalid_result"}`
			}
			items = append(items, chatMessage{Type: "function_call_output", CallID: message.ToolCallID, Output: wire})
		case message.Role == core.RoleSystem:
			items = append(items, chatMessage{Role: "system", Content: message.Content})
		case message.Role == core.RoleAssistant:
			items = append(items, chatMessage{Role: "assistant", Content: message.Content})
		case message.Role == core.RoleUser:
			items = append(items, chatMessage{Role: "user", Content: message.Content})
		}
	}
	return items
}

func (p *Processor) isCurrent(gen uint64) bool {
	p.mu.Lock()
	current := p.activeGen == gen && !p.closed
	session := p.session
	p.mu.Unlock()
	return current && session != nil && session.CurrentGeneration() == gen
}

func cloneXAIToolDefinitions(definitions []core.ToolDefinition) []core.ToolDefinition {
	cloned := make([]core.ToolDefinition, len(definitions))
	for i, definition := range definitions {
		cloned[i] = definition
		cloned[i].InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
	}
	return cloned
}
