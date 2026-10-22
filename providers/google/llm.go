package google

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	"govoice/core/tools"

	"cloud.google.com/go/auth/credentials"
	"google.golang.org/genai"
)

// LLMConfig configures the Google Gemini LLM processor.
type LLMConfig struct {
	Model           string // Pinned: "gemini-2.5-flash"
	SystemPrompt    string // System instructions
	Client          LLMStreamClient
	RetryPolicy     resilience.Policy
	CircuitBreaker  *resilience.Breaker
	ContextProvider func() []core.Message
	ToolDefinitions []core.ToolDefinition
	ToolExecutor    core.ToolExecutor
	ToolHistory     *core.ConversationContext
	ToolLoopConfig  tools.ToolLoopConfig
	Provider        string // label on frames, usage and traces; empty means "google"
	Sampling        Sampling
}

// Sampling tunes generation; nil / 0 keep the model's defaults. Note that on
// Gemini 2.5 models maxOutputTokens also counts thinking tokens, so a low cap
// can leave a short or empty reply.
type Sampling struct {
	Temperature     *float64
	TopP            *float64
	MaxOutputTokens int
}

// DefaultLLMConfig returns configuration matching v1-lock.json.
func DefaultLLMConfig() LLMConfig {
	return LLMConfig{
		Model:          "gemini-2.5-flash",
		RetryPolicy:    resilience.DefaultPolicy(),
		CircuitBreaker: resilience.NewBreaker(resilience.DefaultConfig()),
	}
}

// LLMStreamChunk represents a single chunk from the LLM stream.
type LLMStreamChunk struct {
	Text           string
	ToolCalls      []core.ToolCall
	NativeContents []*genai.Content
	PromptTokens   int
	OutputTokens   int
	TotalTokens    int
	IsDone         bool
	// FinishReason is the candidate's finish reason, or the prompt's block
	// reason when the prompt itself was blocked; "" while streaming.
	FinishReason string
}

// LLMStreamClient abstracts streamed content generation.
type LLMStreamClient interface {
	GenerateStream(ctx context.Context, model string, systemPrompt string, history []core.Message, sampling Sampling) (<-chan LLMStreamChunk, <-chan error, error)
}

type LLMNativeToolStreamClient interface {
	GenerateStreamWithNativeTools(ctx context.Context, model string, systemPrompt string, history []core.Message, nativeHistory []GoogleNativeExchange, definitions []core.ToolDefinition, identity core.ToolCall, sampling Sampling) (<-chan LLMStreamChunk, <-chan error, error)
}

// GoogleNativeExchange retains one streamed model response occurrence for
// continuation, including all original SDK content parts and call ordering.
type GoogleNativeExchange struct {
	CallIDs  []string
	Contents []*genai.Content
}

// RealGoogleLLMClient uses google.golang.org/genai.
type RealGoogleLLMClient struct {
	client *genai.Client
	pool   *netpool.Pool // API-key mode only
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
		if len(creds.CredentialsJSON) > 0 {
			ac, err := credentials.NewCredentialsFromJSON(credentials.ServiceAccount, creds.CredentialsJSON, &credentials.DetectOptions{
				Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
			})
			if err != nil {
				return nil, errors.New("google llm: invalid service account credentials")
			}
			cfg.Credentials = ac
		}
	case AuthModeGeminiAPIKey:
		cfg.Backend = genai.BackendGeminiAPI
		cfg.APIKey = creds.APIKey
	default:
		return nil, fmt.Errorf("unsupported auth mode: %v", creds.Mode)
	}

	var pool *netpool.Pool
	if creds.Mode == AuthModeGeminiAPIKey {
		pool = netpool.Get(netpool.Options{Provider: "google", Stage: "llm"})
		cfg.HTTPClient = pool.Client(0)
	}
	client, err := genai.NewClient(ctx, cfg)
	if err != nil {
		return nil, MapError("llm", err)
	}
	return &RealGoogleLLMClient{client: client, pool: pool}, nil
}

func (c *RealGoogleLLMClient) GenerateStream(ctx context.Context, model string, systemPrompt string, history []core.Message, sampling Sampling) (<-chan LLMStreamChunk, <-chan error, error) {
	return c.generateStreamWithNativeHistory(ctx, model, systemPrompt, history, nil, nil, core.ToolCall{}, sampling)
}

func (c *RealGoogleLLMClient) GenerateStreamWithNativeTools(ctx context.Context, model, systemPrompt string, history []core.Message, nativeHistory []GoogleNativeExchange, definitions []core.ToolDefinition, identity core.ToolCall, sampling Sampling) (<-chan LLMStreamChunk, <-chan error, error) {
	return c.generateStreamWithNativeHistory(ctx, model, systemPrompt, history, nativeHistory, definitions, identity, sampling)
}

func (c *RealGoogleLLMClient) generateStreamWithNativeHistory(ctx context.Context, model, systemPrompt string, history []core.Message, nativeHistory []GoogleNativeExchange, definitions []core.ToolDefinition, identity core.ToolCall, sampling Sampling) (<-chan LLMStreamChunk, <-chan error, error) {
	contents := GoogleContentsWithNativeExchanges(history, nativeHistory)
	genConfig, err := generateContentConfig(systemPrompt, definitions, sampling)
	if err != nil {
		return nil, nil, err
	}

	chunkCh := make(chan LLMStreamChunk, 32)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)
		defer close(errCh)
		defer func() { // two closed channels read as a finished reply: report the panic
			if recover() != nil {
				select {
				case errCh <- errors.New("google llm: stream panicked"):
				default:
				}
			}
		}()

		if c.pool == nil { // Vertex: genai's own HTTP client is not on a netpool Pool, so count here
			netpool.CountRequest("google", "llm")
		}
		seq := c.client.Models.GenerateContentStream(ctx, model, contents, genConfig)
		streamCallOrdinal := 0
		for resp, err := range seq {
			if err != nil {
				errCh <- MapError("llm", err)
				return
			}
			if resp == nil {
				continue
			}

			text := resp.Text()
			toolCalls, callErr := GoogleToolCallsAt(resp, identity, streamCallOrdinal)
			if callErr != nil {
				errCh <- errInvalidGoogleTool
				return
			}
			nativeContents := googleNativeContents(resp, toolCalls)
			streamCallOrdinal += len(toolCalls)
			promptTok := 0
			outTok := 0
			totTok := 0
			if resp.UsageMetadata != nil {
				promptTok = int(resp.UsageMetadata.PromptTokenCount)
				outTok = int(resp.UsageMetadata.CandidatesTokenCount)
				totTok = int(resp.UsageMetadata.TotalTokenCount)
			}

			finish := ""
			if len(resp.Candidates) > 0 && resp.Candidates[0] != nil {
				finish = string(resp.Candidates[0].FinishReason)
			}
			if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
				finish = string(resp.PromptFeedback.BlockReason)
			}

			select {
			case <-ctx.Done():
				return
			case chunkCh <- LLMStreamChunk{
				FinishReason:   finish,
				Text:           text,
				ToolCalls:      toolCalls,
				NativeContents: nativeContents,
				PromptTokens:   promptTok,
				OutputTokens:   outTok,
				TotalTokens:    totTok,
			}:
			}
		}
	}()

	return chunkCh, errCh, nil
}

// generateContentConfig is the request config: system prompt, tools and sampling. nil when none is set.
func generateContentConfig(systemPrompt string, definitions []core.ToolDefinition, sampling Sampling) (*genai.GenerateContentConfig, error) {
	var c genai.GenerateContentConfig
	var set bool
	if systemPrompt != "" {
		c.SystemInstruction, set = &genai.Content{Parts: []*genai.Part{{Text: systemPrompt}}}, true
	}
	if len(definitions) > 0 {
		declarations, err := GoogleToolDeclarations(definitions)
		if err != nil {
			return nil, errInvalidGoogleTool
		}
		c.Tools, set = []*genai.Tool{{FunctionDeclarations: declarations}}, true
	}
	if t := sampling.Temperature; t != nil {
		v := float32(*t)
		c.Temperature, set = &v, true
	}
	if t := sampling.TopP; t != nil {
		v := float32(*t)
		c.TopP, set = &v, true
	}
	if n := sampling.MaxOutputTokens; n > 0 {
		c.MaxOutputTokens, set = int32(n), true
	}
	if !set {
		return nil, nil
	}
	return &c, nil
}

// LLMProcessor implements core.Processor for Google Gemini LLM.
type LLMProcessor struct {
	cfg     LLMConfig
	session core.SessionController
	index   int
	// loop is the session-lived tool loop: async tool calls outlive the
	// generation that started them and are cancelled only by Close.
	loop *tools.ToolLoop

	mu           sync.Mutex
	activeCancel context.CancelFunc
	activeGen    uint64
	activeTurn   string
	closed       bool

	Requests atomic.Uint64
	Errors   atomic.Uint64
	Retries  atomic.Uint64
	Stale    atomic.Uint64
}

// ErrBlockedReply: Gemini refused the prompt or stopped the reply (safety,
// recitation, prohibited content) and produced no text.
var ErrBlockedReply = errors.New("google llm: reply blocked by the model")

// ErrEmptyReply: the stream ended with no text and no stated reason.
var ErrEmptyReply = errors.New("google llm: model returned no text")

var errGoogleToolClientUnsupported = errors.New("google: tool streaming client unsupported")

// LLMCounts implements core.LLMCounters.
func (p *LLMProcessor) LLMCounts() (req, errs, retries, stale uint64) {
	return p.Requests.Load(), p.Errors.Load(), p.Retries.Load(), p.Stale.Load()
}

// NewLLMProcessor creates a new LLMProcessor.
func NewLLMProcessor(cfg LLMConfig) (*LLMProcessor, error) {
	if cfg.Provider == "" {
		cfg.Provider = "google"
	}
	if cfg.Model == "" {
		cfg.Model = "gemini-2.5-flash"
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = resilience.NewBreaker(resilience.DefaultConfig())
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
		cfg.ToolDefinitions = cloneGoogleToolDefinitions(cfg.ToolDefinitions)
		loop, err := tools.NewToolLoop(cfg.ToolDefinitions, cfg.ToolExecutor, cfg.ToolHistory, cfg.ToolLoopConfig)
		if err != nil {
			return nil, err
		}
		return &LLMProcessor{cfg: cfg, loop: loop}, nil
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
	p.closed = true
	if p.activeCancel != nil {
		p.activeCancel()
		p.activeCancel = nil
	}
	p.mu.Unlock()
	p.loop.Close() // cancels in-flight async tool calls (nil-safe)
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

	// Keep the pipeline's trace/span values while deriving cancellation for the
	// provider request; Session.Context alone would discard that parentage.
	parent := ctx
	if parent == nil {
		parent = context.Background()
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
	err := p.session.Go(func(taskCtx context.Context) {
		defer cancel()

		var committed bool
		var fullTextBuilder strings.Builder
		var finishReason string // last non-empty finish/block reason of the turn
		var nativeHistory []GoogleNativeExchange
		// Turn usage: the sum over every model request of the tool loop.
		var pTok, oTok, tTok, requests int64
		requestModel := func(requestCtx context.Context) (_ []core.ToolCall, traceErr error) {
			span := core.StartContextSpan(requestCtx, "llm.request", core.SessionID(strconv.FormatUint(p.session.ID(), 10)), core.TurnID(turn), core.Generation(gen), map[string]any{"provider": p.cfg.Provider, "model": p.cfg.Model})
			defer func() {
				if v := recover(); v != nil {
					span.AddEvent(core.NewTraceEvent("llm.failure", time.Now(), core.SessionID(strconv.FormatUint(p.session.ID(), 10)), core.TurnID(turn), core.Generation(gen), core.TraceContextFromContext(requestCtx).Policy, nil))
					span.End(errors.New("provider_failure"))
					panic(v)
				}
				if traceErr != nil {
					category := "provider_failure"
					if errors.Is(traceErr, context.Canceled) {
						category = "canceled"
					}
					span.AddEvent(core.NewTraceEvent("llm.failure", time.Now(), core.SessionID(strconv.FormatUint(p.session.ID(), 10)), core.TurnID(turn), core.Generation(gen), core.TraceContextFromContext(requestCtx).Policy, nil))
					span.End(errors.New(category))
					return
				}
				span.End(nil)
			}()
			firstOutput := false
			streamCtx, cancelStream := context.WithCancel(requestCtx)
			defer cancelStream()
			var chunkCh <-chan LLMStreamChunk
			var errCh <-chan error
			var err error
			if len(p.cfg.ToolDefinitions) > 0 {
				if nativeClient, ok := p.cfg.Client.(LLMNativeToolStreamClient); ok {
					identity := core.ToolCall{SessionID: core.SessionID(strconv.FormatUint(p.session.ID(), 10)), TurnID: core.TurnID(turn), Generation: core.Generation(gen)}
					chunkCh, errCh, err = nativeClient.GenerateStreamWithNativeTools(streamCtx, p.cfg.Model, p.cfg.SystemPrompt, getHistory(), nativeHistory, p.cfg.ToolDefinitions, identity, p.cfg.Sampling)
				} else {
					return nil, errGoogleToolClientUnsupported
				}
			} else {
				chunkCh, errCh, err = p.cfg.Client.GenerateStream(streamCtx, p.cfg.Model, p.cfg.SystemPrompt, getHistory(), p.cfg.Sampling)
			}
			if err != nil {
				return nil, err
			}
			if chunkCh == nil {
				return nil, errors.New("google: missing response stream")
			}
			var calls []core.ToolCall
			var nativeContents []*genai.Content
			var requestText strings.Builder
			var reqP, reqO, reqT int64 // cumulative within a response: keep the latest
			for chunkCh != nil || errCh != nil {
				select {
				case <-requestCtx.Done():
					return nil, requestCtx.Err()
				case <-taskCtx.Done():
					return nil, taskCtx.Err()
				case streamErr, ok := <-errCh:
					if !ok {
						errCh = nil
						continue
					}
					if streamErr != nil {
						return nil, streamErr
					}
				case chunk, ok := <-chunkCh:
					if !ok {
						chunkCh = nil
						continue
					}
					if chunk.FinishReason != "" {
						finishReason = chunk.FinishReason
					}
					if chunk.PromptTokens > 0 {
						reqP = int64(chunk.PromptTokens)
					}
					if chunk.OutputTokens > 0 {
						reqO = int64(chunk.OutputTokens)
					}
					if chunk.TotalTokens > 0 {
						reqT = int64(chunk.TotalTokens)
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
						if err := p.session.Reenter(deltaFrame, core.Downstream, p.index+1, gen); err != nil {
							return nil, err
						}
						if !firstOutput {
							firstOutput = true
							span.AddEvent(core.NewTraceEvent("llm.first_output", time.Now(), core.SessionID(strconv.FormatUint(p.session.ID(), 10)), core.TurnID(turn), core.Generation(gen), core.TraceContextFromContext(requestCtx).Policy, nil))
						}
					}
					if len(chunk.ToolCalls) > 0 {
						committed = true // Once a call is returned, do not retry this model request.
						for _, call := range chunk.ToolCalls {
							call.SessionID, call.TurnID, call.Generation = core.SessionID(strconv.FormatUint(p.session.ID(), 10)), core.TurnID(turn), core.Generation(gen)
							calls = append(calls, call)
						}
					}
					nativeContents = append(nativeContents, chunk.NativeContents...)
				}
			}
			span.AddEvent(core.NewTraceEvent("llm.complete", time.Now(), core.SessionID(strconv.FormatUint(p.session.ID(), 10)), core.TurnID(turn), core.Generation(gen), core.TraceContextFromContext(requestCtx).Policy, nil))
			pTok, oTok, tTok, requests = pTok+reqP, oTok+reqO, tTok+reqT, requests+1
			if len(calls) > 0 {
				// Native provider parts already contain the response text, including
				// reasoning/signature-bearing and text-only chunks. Avoid replaying
				// the SDK's accumulated text a second time.
				if requestText.Len() > 0 && len(nativeContents) == 0 {
					providerHistory = append(providerHistory, core.Message{Role: core.RoleAssistant, Content: requestText.String(), TurnID: turn})
				}
				for _, call := range calls {
					providerHistory = append(providerHistory, core.Message{Role: core.RoleAssistant, Kind: core.MessageToolCall, Content: string(call.Arguments), ToolCallID: call.ID, ToolName: call.Name, TurnID: turn, Generation: gen})
				}
				exchange := GoogleNativeExchange{Contents: make([]*genai.Content, 0, len(nativeContents)), CallIDs: make([]string, 0, len(calls))}
				for _, call := range calls {
					exchange.CallIDs = append(exchange.CallIDs, call.ID)
				}
				for _, content := range nativeContents {
					exchange.Contents = append(exchange.Contents, cloneGoogleContent(content))
				}
				nativeHistory = append(nativeHistory, exchange)
			}
			return calls, nil
		}
		requestWithRetry := func(requestCtx context.Context) ([]core.ToolCall, error) {
			var calls []core.ToolCall
			err := resilience.Execute(requestCtx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, classify, func() bool {
				return committed
			}, func(attemptCtx context.Context, attempt int) error {
				if attempt > 0 {
					p.Retries.Add(1)
				}
				var requestErr error
				calls, requestErr = requestModel(attemptCtx)
				return requestErr
			})
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
			loop := p.loop
			if loop == nil {
				p.Errors.Add(1)
				return
			}
			loop = loop.WithTraceContext(genCtx).WithCurrentFence(func(call core.ToolCall) bool { return p.isCurrentGeneration(uint64(call.Generation)) }).
				WithFrames(func(f core.Frame) { _ = p.session.Reenter(f, core.Downstream, p.index+1, gen) })
			_, loopErr := loop.Run(genCtx, calls, func(continueCtx context.Context, results []core.ToolResult) ([]core.ToolCall, bool, error) {
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

		if strings.TrimSpace(fullTextBuilder.String()) == "" {
			// A blocked prompt, a SAFETY/RECITATION stop or an empty candidate: surface
			// it so failover and metrics see it, instead of ending the turn in silence.
			p.Errors.Add(1)
			cause := ErrEmptyReply
			if finishReason != "" && finishReason != "STOP" && finishReason != "FINISH_REASON_UNSPECIFIED" {
				cause = fmt.Errorf("%w (finish reason %s)", ErrBlockedReply, finishReason)
			}
			_ = p.session.Reenter(&core.ErrorFrame{Err: cause, Fatal: false, Turn: core.TurnID(turn), Gen: core.Generation(gen)}, core.Downstream, p.index+1, gen)
			return
		}

		fullFrame := &core.LLMTextFrame{
			Text: fullTextBuilder.String(),
			Turn: turn,
			Gen:  gen,
		}
		_ = p.session.Reenter(fullFrame, core.Downstream, p.index+1, gen)

		// Emit optional usage metrics if available
		if pTok > 0 || oTok > 0 || tTok > 0 {
			var pTokPtr, oTokPtr, tTokPtr *int64
			if pTok > 0 {
				pTokPtr = core.Int64Ptr(pTok)
			}
			if oTok > 0 {
				oTokPtr = core.Int64Ptr(oTok)
			}
			if tTok > 0 {
				tTokPtr = core.Int64Ptr(tTok)
			}
			usageFrame := &core.UsageMetricsFrame{
				SessionID:        core.SessionID(strconv.FormatUint(p.session.ID(), 10)),
				Turn:             turn,
				Gen:              gen,
				Provider:         p.cfg.Provider,
				Model:            p.cfg.Model,
				Service:          "llm",
				Timestamp:        time.Now(),
				PromptTokens:     pTokPtr,
				CompletionTokens: oTokPtr,
				TotalTokens:      tTokPtr,
				Requests:         core.Int64Ptr(requests),
			}
			_ = p.session.Reenter(usageFrame, core.Downstream, p.index+1, gen)
		}
	})
	if errors.Is(err, core.ErrTooManyTasks) { // the session's task budget: lose this turn, not the call
		cancel() // the session's own budget is not a provider error: Errors stays untouched
		// Re-entered, not pushed: under LLMFailover a fallback's emitter swallows,
		// and the wrapper must see the error to end the generation.
		_ = p.session.Reenter(&core.ErrorFrame{Err: err, Turn: core.TurnID(turn), Gen: core.Generation(gen)}, core.Downstream, p.index+1, gen)
		return nil
	}
	return err
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
