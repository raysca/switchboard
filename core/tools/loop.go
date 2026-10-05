package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"govoice/core"
)

var (
	ErrInvalidToolLoopConfig = errors.New("tools: invalid tool loop configuration")
	ErrMalformedToolCall     = errors.New("tools: malformed tool call")
	ErrMaxToolCalls          = errors.New("tools: maximum total tool calls exceeded")
	ErrMaxToolRounds         = errors.New("tools: maximum tool rounds exceeded")
	ErrStaleToolCall         = errors.New("tools: tool call belongs to a stale generation")
)

const minimumToolResultBytes = len("{\"error\":\"arguments_too_deep\"}")

var toolNamePattern = regexp.MustCompile("^[A-Za-z0-9_.-]{1,64}$")

// ToolLoopConfig places explicit bounds around one provider-neutral tool loop.
// IsCurrent is an optional caller-owned generation fence.
type ToolLoopConfig struct {
	MaxRounds          int
	MaxCalls           int
	MaxArgumentBytes   int
	MaxResultBytes     int
	MaxJSONDepth       int
	DefaultCallTimeout time.Duration
	RoundTimeout       time.Duration
	Now                func() time.Time
	IsCurrent          func(ToolCall) bool
	Tracer             core.Tracer
}

func DefaultToolLoopConfig() ToolLoopConfig {
	return ToolLoopConfig{MaxRounds: 4, MaxCalls: 16, MaxArgumentBytes: 32 << 10, MaxResultBytes: 64 << 10, MaxJSONDepth: 16, DefaultCallTimeout: 30 * time.Second, RoundTimeout: 2 * time.Minute}
}

func (c ToolLoopConfig) validate() error {
	if c.MaxRounds <= 0 || c.MaxCalls <= 0 || c.MaxArgumentBytes <= 0 || c.MaxResultBytes < minimumToolResultBytes || c.MaxJSONDepth <= 0 || c.DefaultCallTimeout <= 0 || c.RoundTimeout <= 0 {
		return ErrInvalidToolLoopConfig
	}
	return nil
}

// Continuation receives ordered results only after they have entered context.
type Continuation func(context.Context, []ToolResult) (next []ToolCall, done bool, err error)

type ToolLoopResult struct {
	Results []ToolResult
	Rounds  int
	Calls   int
	Done    bool
}

// ToolLoop executes application-supplied handlers sequentially in provider order.
// It does not own a session, provider, model request, or pipeline.
type ToolLoop struct {
	definitions map[string]ToolDefinition
	executor    ToolExecutor
	history     *core.ConversationContext
	cfg         ToolLoopConfig
}

func NewToolLoop(definitions []ToolDefinition, executor ToolExecutor, history *core.ConversationContext, cfg ToolLoopConfig) (*ToolLoop, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if executor == nil || history == nil {
		return nil, ErrInvalidToolLoopConfig
	}
	defs := make(map[string]ToolDefinition, len(definitions))
	for _, def := range definitions {
		if !toolNamePattern.MatchString(def.Name) || len(def.Description) > 4096 || len(def.InputSchema) > 32<<10 || !json.Valid(def.InputSchema) {
			return nil, fmt.Errorf("%w: invalid definition", ErrInvalidToolLoopConfig)
		}
		if _, exists := defs[def.Name]; exists {
			return nil, fmt.Errorf("%w: duplicate definition", ErrInvalidToolLoopConfig)
		}
		def.InputSchema = append(json.RawMessage(nil), def.InputSchema...)
		defs[def.Name] = def
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Tracer == nil {
		cfg.Tracer = core.NoopTracer{}
	}
	return &ToolLoop{definitions: defs, executor: executor, history: history, cfg: cfg}, nil
}

func (l *ToolLoop) Run(ctx context.Context, initial []ToolCall, continuation Continuation) (ToolLoopResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := ToolLoopResult{Done: len(initial) == 0}
	calls := cloneCalls(initial)
	for len(calls) > 0 {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if result.Rounds == l.cfg.MaxRounds {
			return result, ErrMaxToolRounds
		}
		roundCtx, cancelRound := context.WithTimeout(ctx, l.cfg.RoundTimeout)
		result.Rounds++
		roundResultStart := len(result.Results)
		roundResults := make([]ToolResult, 0, len(calls))
		for _, call := range calls {
			if result.Calls == l.cfg.MaxCalls {
				cancelRound()
				return result, ErrMaxToolCalls
			}
			result.Calls++
			toolResult, err := l.execute(roundCtx, call)
			if err != nil {
				cancelRound()
				return result, err
			}
			roundResults = append(roundResults, cloneToolResult(toolResult))
			result.Results = append(result.Results, cloneToolResult(toolResult))
			if toolResult.Status == ToolCancelled && toolResult.ErrorCategory == "stale_generation" {
				cancelRound()
				return result, ErrStaleToolCall
			}
			if err := ctx.Err(); err != nil {
				cancelRound()
				return result, err
			}
			if err := roundCtx.Err(); err != nil {
				cancelRound()
				return result, err
			}
		}
		for i, call := range calls {
			if !l.isCurrent(call) {
				started := result.Results[roundResultStart+i].StartedAt
				result.Results[roundResultStart+i] = staleToolResult(call, started, l.cfg.Now())
				cancelRound()
				return result, ErrStaleToolCall
			}
		}
		if continuation == nil {
			cancelRound()
			result.Done = true
			return result, nil
		}
		if err := roundCtx.Err(); err != nil {
			cancelRound()
			return result, err
		}
		next, done, err := continuation(roundCtx, cloneResults(roundResults))
		roundErr := roundCtx.Err()
		cancelRound()
		if err != nil {
			return result, err
		}
		if roundErr != nil {
			return result, roundErr
		}
		if done {
			result.Done = true
			return result, nil
		}
		if len(next) == 0 {
			return result, ErrMaxToolRounds
		}
		if result.Rounds == l.cfg.MaxRounds {
			return result, ErrMaxToolRounds
		}
		if len(next) > l.cfg.MaxCalls-result.Calls {
			return result, ErrMaxToolCalls
		}
		calls = cloneCalls(next)
		result.Done = false
	}
	return result, nil
}

func (l *ToolLoop) execute(roundCtx context.Context, call ToolCall) (ToolResult, error) {
	if call.ID == "" || call.Name == "" || call.TurnID == "" || call.Generation == 0 || call.SessionID == "" {
		return ToolResult{}, ErrMalformedToolCall
	}
	if len(call.Arguments) > l.cfg.MaxArgumentBytes {
		return ToolResult{}, ErrMalformedToolCall
	}
	started := l.cfg.Now()
	prior, claimed, err := l.history.ClaimToolCall(call, started)
	if err != nil {
		return ToolResult{}, err
	}
	if !claimed {
		if !l.isCurrent(call) {
			return staleToolResult(call, prior.StartedAt, l.cfg.Now()), nil
		}
		return cloneToolResult(prior), nil
	}
	if !toolNamePattern.MatchString(call.Name) {
		return l.commit(call, ToolFailed, errorPayload("invalid_tool_name"), "invalid_tool_name", started)
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return l.commit(call, ToolFailed, errorPayload("invalid_arguments"), "invalid_arguments", started)
	}
	if !validJSONDepth(call.Arguments, l.cfg.MaxJSONDepth) {
		return l.commit(call, ToolFailed, errorPayload("arguments_too_deep"), "arguments_too_deep", started)
	}
	if _, ok := l.definitions[call.Name]; !ok {
		return l.commit(call, ToolFailed, errorPayload("unknown_tool"), "unknown_tool", started)
	}
	if !l.isCurrent(call) {
		return l.commit(call, ToolCancelled, errorPayload("stale_generation"), "stale_generation", started)
	}
	if err := roundCtx.Err(); err != nil {
		category := "timeout"
		if errors.Is(err, context.Canceled) {
			category = "cancelled"
		}
		return l.commit(call, ToolCancelled, errorPayload(category), category, started)
	}
	def := l.definitions[call.Name]
	timeout := def.Policy.Timeout
	if timeout <= 0 || timeout > l.cfg.DefaultCallTimeout {
		timeout = l.cfg.DefaultCallTimeout
	}
	callCtx, cancel := context.WithTimeout(roundCtx, timeout)
	defer cancel()
	var toolResult ToolResult
	var execErr error
	event := core.NewTraceEvent("tool.execute", started, call.SessionID, call.TurnID, call.Generation, core.TracePolicy{Content: core.TraceNone}, nil)
	event.ToolCallID = call.ID
	span := l.cfg.Tracer.StartSpan(callCtx, event)
	func() {
		defer func() {
			if recover() != nil {
				execErr = errExecutorPanic
			}
		}()
		toolResult, execErr = l.executor.Execute(callCtx, cloneCall(call))
	}()
	ended := l.cfg.Now()
	// Executor errors can contain arbitrary provider or application details.
	// Trace only normalized loop outcomes through the existing event fields.
	span.End(nil)
	if !l.isCurrent(call) {
		return l.commit(call, ToolCancelled, errorPayload("stale_generation"), "stale_generation", started)
	}
	if callCtx.Err() != nil {
		category := "timeout"
		if errors.Is(roundCtx.Err(), context.Canceled) {
			category = "cancelled"
		}
		return l.commit(call, ToolCancelled, errorPayload(category), category, started)
	}
	if execErr != nil {
		category := "executor_error"
		if errors.Is(execErr, errExecutorPanic) {
			category = "executor_panic"
		}
		return l.commit(call, ToolFailed, errorPayload(category), category, started)
	}
	toolResult.CallID, toolResult.StartedAt, toolResult.EndedAt = call.ID, started, ended
	if toolResult.Status == "" {
		toolResult.Status = ToolSucceeded
	}
	if toolResult.Status != ToolSucceeded && toolResult.Status != ToolFailed && toolResult.Status != ToolCancelled {
		return l.commit(call, ToolFailed, errorPayload("invalid_status"), "invalid_status", started)
	}
	if toolResult.Status == ToolFailed || toolResult.Status == ToolCancelled {
		category := normalizeCategory(toolResult.ErrorCategory)
		if category == "" {
			category = "tool_failed"
		}
		toolResult.ErrorCategory = category
	} else {
		toolResult.ErrorCategory = ""
	}
	payload, category := normalizePayload(toolResult.Result, l.cfg.MaxResultBytes, l.cfg.MaxJSONDepth)
	if category != "" {
		toolResult.Status, toolResult.ErrorCategory = ToolFailed, category
	}
	toolResult.Result = payload
	if !l.isCurrent(call) {
		return l.commit(call, ToolCancelled, errorPayload("stale_generation"), "stale_generation", started)
	}
	if err := l.history.AddToolResultStatus(string(call.TurnID), uint64(call.Generation), call.ID, toolResult.Status, payload, toolResult.ErrorCategory, ended); err != nil {
		return ToolResult{}, err
	}
	return cloneToolResult(toolResult), nil
}

func (l *ToolLoop) isCurrent(call ToolCall) bool {
	return l.cfg.IsCurrent == nil || l.cfg.IsCurrent(call)
}

func staleToolResult(call ToolCall, started, ended time.Time) ToolResult {
	return ToolResult{CallID: call.ID, Status: ToolCancelled, Result: errorPayload("stale_generation"), ErrorCategory: "stale_generation", StartedAt: started, EndedAt: ended}
}

var errExecutorPanic = errors.New("tools: executor panic")

func (l *ToolLoop) commit(call ToolCall, status ToolStatus, payload json.RawMessage, category string, started time.Time) (ToolResult, error) {
	ended := l.cfg.Now()
	if err := l.history.AddToolResultStatus(string(call.TurnID), uint64(call.Generation), call.ID, status, payload, category, ended); err != nil {
		return ToolResult{}, err
	}
	return ToolResult{CallID: call.ID, Status: status, Result: append(json.RawMessage(nil), payload...), ErrorCategory: category, StartedAt: started, EndedAt: ended}, nil
}

func errorPayload(category string) json.RawMessage {
	return json.RawMessage("{\"error\":\"" + category + "\"}")
}

func normalizeCategory(category string) string {
	category = strings.TrimSpace(category)
	if len(category) > 64 {
		return "tool_failed"
	}
	if category == "" {
		return ""
	}
	for _, r := range category {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.') {
			return "tool_failed"
		}
	}
	return category
}

func normalizePayload(raw json.RawMessage, maxBytes, maxDepth int) (json.RawMessage, string) {
	if len(raw) == 0 || !json.Valid(raw) {
		return errorPayload("invalid_result"), "invalid_result"
	}
	if len(raw) > maxBytes {
		return errorPayload("result_too_large"), "result_too_large"
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return errorPayload("invalid_result"), "invalid_result"
	}
	if !safeJSON(value, 0, maxDepth) {
		return errorPayload("result_too_deep"), "result_too_deep"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return errorPayload("invalid_result"), "invalid_result"
	}
	if len(encoded) > maxBytes {
		return errorPayload("result_too_large"), "result_too_large"
	}
	return json.RawMessage(encoded), ""
}

func validJSONDepth(raw json.RawMessage, maxDepth int) bool {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var value any
	return dec.Decode(&value) == nil && safeJSON(value, 0, maxDepth)
}

func safeJSON(value any, depth, maxDepth int) bool {
	if depth > maxDepth {
		return false
	}
	switch v := value.(type) {
	case map[string]any:
		for _, child := range v {
			if !safeJSON(child, depth+1, maxDepth) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !safeJSON(child, depth+1, maxDepth) {
				return false
			}
		}
	case json.Number:
		f, err := v.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return false
		}
	case nil, bool, string, float64:
	default:
		return false
	}
	return true
}

func cloneCalls(calls []ToolCall) []ToolCall {
	out := make([]ToolCall, len(calls))
	for i, call := range calls {
		out[i] = cloneCall(call)
	}
	return out
}
func cloneCall(call ToolCall) ToolCall {
	call.Arguments = append(json.RawMessage(nil), call.Arguments...)
	return call
}
func cloneToolResult(result ToolResult) ToolResult {
	result.Result = append(json.RawMessage(nil), result.Result...)
	return result
}
func cloneResults(results []ToolResult) []ToolResult {
	out := make([]ToolResult, len(results))
	for i, result := range results {
		out[i] = cloneToolResult(result)
	}
	return out
}
