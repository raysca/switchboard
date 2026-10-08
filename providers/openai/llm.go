// Package openai is a streaming Chat Completions LLM processor for OpenAI and
// any compatible server (Groq, Together, vLLM, ...) selected by BaseURL.
//
// Design note: providers/xai speaks the Responses API (function_call items,
// call_id, response.* events), a different wire format from chat completions
// (choices[].delta.tool_calls, role:"tool" messages). Sharing its parser would
// mean a translation shim larger than this file, and would couple vendors'
// request bodies (xai sends store:false and Responses-only fields). So this is
// a small standalone module: it sends only model/messages/tools/stream/
// stream_options, nothing vendor-specific. The shared pieces that matter,
// core/resilience (breaker, retry, timeout) and core/tools (ToolLoop), are
// reused as is.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/netpool"
	"govoice/core/resilience"
	coretools "govoice/core/tools"
)

const (
	DefaultBaseURL = "https://api.openai.com/v1"
	DefaultModel   = "gpt-4o-mini"

	maxLine = 256 * 1024
	maxBody = 4 << 20
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Config configures the processor. BaseURL is the API root ("/chat/completions"
// is appended); Model is free-form.
type Config struct {
	BaseURL         string
	Model           string
	APIKey          string
	Provider        string // usage-frame label; default "openai"
	SystemPrompt    string
	ContextProvider func() []core.Message
	ToolDefinitions []core.ToolDefinition
	ToolLoop        *coretools.ToolLoop
	Timeout         time.Duration // bounds each request attempt
	// AllowPrivate permits http:// and loopback/private addresses (self-hosted
	// vLLM). Off, BaseURL must be https and resolve to a public address.
	AllowPrivate   bool
	RetryPolicy    resilience.Policy
	CircuitBreaker *resilience.Breaker
	HTTPClient     *http.Client // tests only: bypasses the SSRF dial guard
}

type Processor struct {
	pool     *netpool.Pool // nil when HTTPClient was injected
	cfg      Config
	endpoint string
	tools    []wireTool
	session  core.SessionController
	index    int

	mu           sync.Mutex
	activeCancel context.CancelFunc
	activeGen    uint64
	closed       bool

	Requests atomic.Uint64
	Errors   atomic.Uint64
	Retries  atomic.Uint64
	Stale    atomic.Uint64

	noStreamOptions atomic.Bool // server rejected stream_options once
}

// LLMCounts implements core.LLMCounters.
func (p *Processor) LLMCounts() (req, errs, retries, stale uint64) {
	return p.Requests.Load(), p.Errors.Load(), p.Retries.Load(), p.Stale.Load()
}

func NewProcessor(cfg Config) (*Processor, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if u, err := url.Parse(cfg.BaseURL); err != nil || u.Host == "" || !(u.Scheme == "https" || u.Scheme == "http" && cfg.AllowPrivate) {
		return nil, errors.New("openai: base_url must be an https URL (http needs allow_private)")
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Provider == "" {
		cfg.Provider = "openai"
	}
	if cfg.CircuitBreaker == nil {
		cfg.CircuitBreaker = resilience.NewBreaker(resilience.DefaultConfig())
	}
	if cfg.RetryPolicy.MaxAttempts <= 0 {
		cfg.RetryPolicy.MaxAttempts = resilience.DefaultPolicy().MaxAttempts
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	cfg.RetryPolicy.AttemptTimeout = cfg.Timeout
	var pool *netpool.Pool
	if cfg.HTTPClient == nil {
		pool = guardedPool(cfg.AllowPrivate)
		cfg.HTTPClient = guardedClient(pool) // attempt deadline comes from the retry policy
	}
	tools, err := mapTools(cfg.ToolDefinitions)
	if err != nil {
		return nil, err
	}
	if len(tools) > 0 && cfg.ToolLoop == nil {
		return nil, errors.New("openai: tool definitions require a shared ToolLoop")
	}
	return &Processor{pool: pool, cfg: cfg, endpoint: strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions", tools: tools}, nil
}

// Warm pre-dials TCP+TLS to the base URL through the SSRF-guarded dialer
// (the first request then skips DNS/TCP/TLS and negotiates HTTP/2 on the
// spare). Skipped while the breaker is open; never reports to it.
func (p *Processor) Warm(ctx context.Context) error {
	if p.pool == nil {
		return nil
	}
	if p.cfg.CircuitBreaker.WouldRefuse() {
		p.pool.NoteSkipped()
		return nil
	}
	return p.pool.Warm(ctx, p.cfg.BaseURL)
}

func (p *Processor) SetContextProvider(fn func() []core.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.ContextProvider = fn
}

func (p *Processor) Attach(s core.SessionController, index int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.session, p.index = s, index
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

func (p *Processor) cancelActive() {
	p.mu.Lock()
	if p.activeCancel != nil {
		p.activeCancel()
		p.activeCancel = nil
	}
	p.mu.Unlock()
}

func (p *Processor) ProcessFrame(ctx context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	switch frame := f.(type) {
	case *core.UserMessageFrame:
		return p.handleUserTurn(ctx, frame, e)
	case *core.InterruptionFrame, *core.CancelFrame:
		p.cancelActive()
		return e.PushDirection(f, dir)
	case *core.LLMTextDeltaFrame, *core.LLMTextFrame:
		return e.Push(f)
	}
	return e.PushDirection(f, dir)
}

// ---- wire types ----

type wireFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type wireCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function wireFunc `json:"function"`
}
type wireMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}
type request struct {
	Model         string          `json:"model"`
	Messages      []wireMsg       `json:"messages"`
	Stream        bool            `json:"stream"`
	StreamOptions map[string]bool `json:"stream_options,omitempty"`
	Tools         []wireTool      `json:"tools,omitempty"`
}

type chunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	RawUsage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"` // mid-stream failure (OpenAI, vLLM)
}

func mapTools(defs []core.ToolDefinition) ([]wireTool, error) {
	out := make([]wireTool, 0, len(defs))
	for _, d := range defs {
		if !toolNamePattern.MatchString(d.Name) || !json.Valid(d.InputSchema) {
			return nil, fmt.Errorf("openai: invalid tool definition %q", d.Name)
		}
		var t wireTool
		t.Type = "function"
		t.Function.Name, t.Function.Description = d.Name, d.Description
		t.Function.Parameters = append(json.RawMessage(nil), d.InputSchema...)
		out = append(out, t)
	}
	return out, nil
}

// toolResultOutput keeps success payloads distinguishable from failures.
func toolResultOutput(r core.ToolResult) (string, error) {
	switch r.Status {
	case core.ToolSucceeded:
		if len(r.Result) == 0 || !json.Valid(r.Result) {
			return "", errors.New("openai: invalid successful tool result")
		}
		return `{"output":` + string(r.Result) + `}`, nil
	case core.ToolFailed, core.ToolCancelled:
		cat := r.ErrorCategory
		if cat == "" || len(cat) > 64 || strings.ContainsAny(cat, "\"\\ \n") {
			cat = string(r.Status)
		}
		b, _ := json.Marshal(map[string]string{"error": cat})
		return string(b), nil
	}
	return "", errors.New("openai: invalid tool result status")
}

// historyMessages maps context history; consecutive tool calls fold into one
// assistant message so parallel calls round-trip as the API requires.
func historyMessages(history []core.Message) []wireMsg {
	var out []wireMsg
	var pending []wireCall
	flush := func() {
		if len(pending) > 0 {
			out = append(out, wireMsg{Role: "assistant", ToolCalls: pending})
			pending = nil
		}
	}
	for _, m := range history {
		if m.Kind == core.MessageToolCall {
			args := m.Content
			if args == "" {
				args = "{}"
			}
			pending = append(pending, wireCall{ID: m.ToolCallID, Type: "function", Function: wireFunc{Name: m.ToolName, Arguments: args}})
			continue
		}
		flush()
		switch {
		case m.Kind == core.MessageToolResult || m.Role == core.RoleTool:
			status := m.ToolStatus
			if status == "" {
				status = core.ToolSucceeded
			}
			wire, err := toolResultOutput(core.ToolResult{CallID: m.ToolCallID, Status: status, Result: json.RawMessage(m.Content), ErrorCategory: m.ToolErrorCategory})
			if err != nil {
				wire = `{"error":"invalid_result"}`
			}
			out = append(out, wireMsg{Role: "tool", ToolCallID: m.ToolCallID, Content: wire})
		case m.Role == core.RoleSystem:
			out = append(out, wireMsg{Role: "system", Content: m.Content})
		case m.Content == "":
			// an empty user/assistant message (e.g. an interrupted reply) is a 400
		case m.Role == core.RoleAssistant:
			out = append(out, wireMsg{Role: "assistant", Content: m.Content})
		case m.Role == core.RoleUser:
			out = append(out, wireMsg{Role: "user", Content: m.Content})
		}
	}
	flush()
	return out
}

func (p *Processor) isCurrent(gen uint64) bool {
	p.mu.Lock()
	ok, s := p.activeGen == gen && !p.closed, p.session
	p.mu.Unlock()
	return ok && s != nil && s.CurrentGeneration() == gen
}

type usage struct{ prompt, completion, total int }

func (p *Processor) handleUserTurn(ctx context.Context, f *core.UserMessageFrame, e core.Emitter) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return core.ErrSessionClosed
	}
	if p.activeCancel != nil {
		p.activeCancel()
	}
	s := p.session
	if s == nil {
		p.mu.Unlock()
		return errors.New("processor not attached to session")
	}
	turn, gen := f.Turn, s.CurrentGeneration()
	p.activeGen = gen
	if ctx == nil {
		ctx = context.Background()
	}
	streamCtx, cancel := context.WithCancel(ctx)
	p.activeCancel = cancel
	p.mu.Unlock()

	if err := e.Push(f); err != nil {
		cancel()
		return err
	}
	var history []core.Message
	if p.cfg.ContextProvider != nil {
		history = p.cfg.ContextProvider()
	} else if f.Content != "" {
		history = []core.Message{{Role: core.RoleUser, Content: f.Content, TurnID: f.Turn}}
	}
	req := request{Model: p.cfg.Model, Stream: true, Tools: p.tools}
	if p.cfg.SystemPrompt != "" {
		req.Messages = append(req.Messages, wireMsg{Role: "system", Content: p.cfg.SystemPrompt})
	}
	req.Messages = append(req.Messages, historyMessages(history)...)

	p.Requests.Add(1)
	sid := fmt.Sprint(s.ID())
	return s.Go(func(sessionCtx context.Context) {
		defer cancel()
		var text strings.Builder
		var total usage
		add := func(u usage) { total.prompt += u.prompt; total.completion += u.completion; total.total += u.total }

		calls, u, opErr := p.round(streamCtx, sessionCtx, req, gen, turn, s, &text)
		add(u)
		if opErr == nil && !p.isCurrent(gen) {
			p.Stale.Add(1)
			return
		}
		if opErr == nil && len(calls) > 0 {
			if p.cfg.ToolLoop == nil {
				opErr = errors.New("openai: model returned tool calls but no ToolLoop is configured")
			} else {
				loop := p.cfg.ToolLoop.WithTraceContext(streamCtx).WithCurrentFence(func(c core.ToolCall) bool {
					return c.SessionID == core.SessionID(sid) && c.TurnID == core.TurnID(turn) && c.Generation == core.Generation(gen) && p.isCurrent(gen)
				})
				toolCalls := toCoreCalls(calls, sid, turn, gen)
				_, opErr = loop.Run(streamCtx, toolCalls, func(rc context.Context, results []core.ToolResult) ([]core.ToolCall, bool, error) {
					if rc.Err() != nil || sessionCtx.Err() != nil || !p.isCurrent(gen) {
						return nil, false, context.Canceled
					}
					req.Messages = append(req.Messages, wireMsg{Role: "assistant", ToolCalls: calls})
					known := make(map[string]bool, len(calls))
					for _, c := range calls {
						known[c.ID] = true
					}
					for _, r := range results {
						if !known[r.CallID] {
							return nil, false, errors.New("openai: tool result has no matching provider call")
						}
						out, err := toolResultOutput(r)
						if err != nil {
							return nil, false, err
						}
						req.Messages = append(req.Messages, wireMsg{Role: "tool", ToolCallID: r.CallID, Content: out})
					}
					next, nu, err := p.round(rc, sessionCtx, req, gen, turn, s, &text)
					add(nu)
					calls = next
					return toCoreCalls(next, sid, turn, gen), len(next) == 0, err
				})
			}
		}
		if opErr != nil {
			if errors.Is(opErr, context.Canceled) || streamCtx.Err() != nil {
				return
			}
			p.Errors.Add(1)
			_ = s.Reenter(&core.ErrorFrame{Err: sanitizeErr(opErr), Fatal: false}, core.Downstream, p.index, gen)
			return
		}
		if !p.isCurrent(gen) {
			p.Stale.Add(1)
			return
		}
		_ = s.Reenter(&core.LLMTextFrame{Text: text.String(), Turn: turn, Gen: gen}, core.Downstream, p.index, gen)
		if total.prompt > 0 || total.completion > 0 || total.total > 0 {
			ptr := func(v int) *int64 {
				if v > 0 {
					return core.Int64Ptr(int64(v))
				}
				return nil
			}
			_ = s.Reenter(&core.UsageMetricsFrame{
				SessionID: core.SessionID(sid), Turn: turn, Gen: gen, Provider: p.cfg.Provider, Model: p.cfg.Model, Service: "llm",
				Timestamp: time.Now(), PromptTokens: ptr(total.prompt), CompletionTokens: ptr(total.completion), TotalTokens: ptr(total.total),
				Requests: core.Int64Ptr(1),
			}, core.Downstream, p.index, gen)
		}
	})
}

func toCoreCalls(calls []wireCall, session, turn string, gen uint64) []core.ToolCall {
	out := make([]core.ToolCall, len(calls))
	for i, c := range calls {
		out[i] = core.ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: json.RawMessage(c.Function.Arguments), SessionID: core.SessionID(session), TurnID: core.TurnID(turn), Generation: core.Generation(gen)}
	}
	return out
}

type callAcc struct {
	id, name string
	args     strings.Builder
}

// round performs one streamed request (with breaker, retry and per-attempt
// timeout) and returns the assembled tool calls, in model order, and usage.
func (p *Processor) round(ctx, sessionCtx context.Context, req request, gen uint64, turn string, s core.SessionController, text *strings.Builder) (_ []wireCall, _ usage, traceErr error) {
	sid := fmt.Sprint(s.ID())
	span := core.StartContextSpan(ctx, "llm.request", core.SessionID(sid), core.TurnID(turn), core.Generation(gen), map[string]any{"provider": p.cfg.Provider, "model": p.cfg.Model})
	defer func() {
		var endErr error
		name := "llm.complete"
		if traceErr != nil {
			name, endErr = "llm.failure", errors.New("provider_failure")
			if errors.Is(traceErr, context.Canceled) {
				endErr = errors.New("canceled")
			}
		}
		span.AddEvent(core.NewTraceEvent(name, time.Now(), core.SessionID(sid), core.TurnID(turn), core.Generation(gen), core.TraceContextFromContext(ctx).Policy, nil))
		span.End(endErr)
	}()
	calls, u, err := p.stream(ctx, sessionCtx, req, gen, turn, s, text, span)
	var pe *ProviderError
	if errors.As(err, &pe) && pe.Status == http.StatusBadRequest && strings.Contains(pe.Message, "stream_options") && !p.noStreamOptions.Swap(true) {
		// Some compatible servers reject stream_options; retry once without
		// it and remember (usage then arrives only if the server sends it anyway).
		return p.stream(ctx, sessionCtx, req, gen, turn, s, text, span)
	}
	return calls, u, err
}

func (p *Processor) stream(ctx, sessionCtx context.Context, req request, gen uint64, turn string, s core.SessionController, text *strings.Builder, span core.Span) ([]wireCall, usage, error) {
	if !p.noStreamOptions.Load() {
		req.StreamOptions = map[string]bool{"include_usage": true}
	}
	sid := fmt.Sprint(s.ID())
	body, err := json.Marshal(req)
	if err != nil {
		return nil, usage{}, err
	}
	var committed, firstOutput bool
	var acc map[int]*callAcc
	var u usage
	err = resilience.Execute(ctx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, classify, func() bool { return committed }, func(ctx context.Context, attempt int) error {
		if attempt > 0 {
			p.Retries.Add(1)
		}
		acc, u = map[int]*callAcc{}, usage{}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if p.cfg.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
		}
		resp, err := p.cfg.HTTPClient.Do(httpReq)
		if errors.Is(err, ErrBlockedAddress) {
			return &ProviderError{Message: ErrBlockedAddress.Error()}
		}
		if err != nil {
			return errors.New(SanitizeString(err.Error()))
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return newHTTPError(resp, string(b), p.cfg.Model)
		}
		lr := &io.LimitedReader{R: resp.Body, N: maxBody + 1}
		sc := bufio.NewScanner(lr)
		sc.Buffer(make([]byte, 0, 8*1024), maxLine)
		done := false
		for sc.Scan() {
			if sessionCtx.Err() != nil {
				return sessionCtx.Err()
			}
			if lr.N <= 0 {
				return ErrOversizedBody
			}
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue // blank, comment, or event: line
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				done = true
				break
			}
			var c chunk
			if err := json.Unmarshal([]byte(data), &c); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
			}
			if c.Error != nil {
				return &ProviderError{Status: http.StatusBadGateway, Message: SanitizeString(c.Error.Message), Retryable: true}
			}
			if r := c.RawUsage; r != nil {
				u = usage{r.PromptTokens, r.CompletionTokens, r.TotalTokens}
				if u.total == 0 {
					u.total = u.prompt + u.completion
				}
			}
			for _, ch := range c.Choices {
				for _, tc := range ch.Delta.ToolCalls {
					a := acc[tc.Index]
					if a == nil {
						a = &callAcc{}
						acc[tc.Index] = a
					}
					if tc.ID != "" {
						a.id = tc.ID
					}
					if tc.Function.Name != "" {
						a.name = tc.Function.Name
					}
					a.args.WriteString(tc.Function.Arguments)
					committed = true
				}
				if d := ch.Delta.Content; d != "" {
					committed = true
					text.WriteString(d)
					if p.isCurrent(gen) {
						if err := s.Reenter(&core.LLMTextDeltaFrame{Delta: d, Turn: turn, Gen: gen}, core.Downstream, p.index, gen); err != nil {
							return err
						}
						if !firstOutput {
							firstOutput = true
							span.AddEvent(core.NewTraceEvent("llm.first_output", time.Now(), core.SessionID(sid), core.TurnID(turn), core.Generation(gen), core.TraceContextFromContext(ctx).Policy, nil))
						}
					}
				}
			}
		}
		if err := sc.Err(); err != nil {
			return err
		}
		if !done {
			return ErrMissingTerminalEvent
		}
		return nil
	})
	if err != nil {
		return nil, u, err
	}
	idx := make([]int, 0, len(acc))
	for i := range acc {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	calls := make([]wireCall, 0, len(idx))
	for _, i := range idx {
		a := acc[i]
		args := a.args.String()
		if args == "" {
			args = "{}"
		}
		if a.name == "" || !json.Valid([]byte(args)) {
			return nil, u, errors.New("openai: malformed tool call from model")
		}
		if a.id == "" {
			a.id = fmt.Sprintf("call_%d_%d", gen, i) // some servers omit ids
		}
		calls = append(calls, wireCall{ID: a.id, Type: "function", Function: wireFunc{Name: a.name, Arguments: args}})
	}
	return calls, u, nil
}
