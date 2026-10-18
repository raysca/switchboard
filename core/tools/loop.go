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
	ErrInvalidToolLoopConfig   = errors.New("tools: invalid tool loop configuration")
	ErrMalformedToolCall       = errors.New("tools: malformed tool call")
	ErrMaxToolCalls            = errors.New("tools: maximum total tool calls exceeded")
	ErrMaxToolRounds           = errors.New("tools: maximum tool rounds exceeded")
	ErrStaleToolCall           = errors.New("tools: tool call belongs to a stale generation")
	ErrReplayResultOutOfBounds = errors.New("tools: cached result exceeds current loop bounds")
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
	// MaxParallel bounds concurrently executing calls within one round. Zero or
	// one keeps the sequential behaviour. Results are committed and returned in
	// call order regardless.
	MaxParallel int
	// StaleCheckInterval is how often IsCurrent is polled to cancel in-flight
	// calls of a stale generation (default 25ms).
	StaleCheckInterval time.Duration
	// MaxAsyncInFlight bounds background ToolPolicy.Async executions (default 4).
	MaxAsyncInFlight int
	// AsyncSink, if set, replaces the immediate injection: the
	// sink receives the finished call and decides when to inject its message
	// (AsyncToolResult.Inject) and trigger a model turn. It must not block.
	AsyncSink func(core.AsyncToolResult)
	// OnAsyncStart runs when an async call is acknowledged, before the executor
	// goroutine starts (side-effect accounting for LLM failover).
	OnAsyncStart func(ToolCall)
	// AsyncTimeout is the default and cap for an async call (default 5m).
	AsyncTimeout time.Duration
	Now          func() time.Time
	IsCurrent    func(ToolCall) bool
	Tracer       core.Tracer
	TracePolicy  core.TracePolicy
}

func DefaultToolLoopConfig() ToolLoopConfig {
	return ToolLoopConfig{MaxRounds: 4, MaxCalls: 16, MaxArgumentBytes: 32 << 10, MaxResultBytes: 64 << 10, MaxJSONDepth: 16, DefaultCallTimeout: 30 * time.Second, RoundTimeout: 2 * time.Minute}
}

func (c ToolLoopConfig) validate() error {
	if c.MaxRounds <= 0 || c.MaxCalls <= 0 || c.MaxArgumentBytes <= 0 || c.MaxResultBytes < minimumToolResultBytes || c.MaxJSONDepth <= 0 || c.DefaultCallTimeout <= 0 || c.RoundTimeout <= 0 || c.MaxParallel < 0 || c.StaleCheckInterval < 0 || c.MaxAsyncInFlight < 0 || c.AsyncTimeout < 0 {
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
	ownedTracer *core.AsyncTracer
	async       *asyncState
	emit        func(core.Frame) // WithFrames; nil reports nothing
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
	if cfg.MaxParallel < 1 {
		cfg.MaxParallel = 1
	}
	if cfg.StaleCheckInterval == 0 {
		cfg.StaleCheckInterval = 25 * time.Millisecond
	}
	if cfg.MaxAsyncInFlight == 0 {
		cfg.MaxAsyncInFlight = 4
	}
	if cfg.AsyncTimeout == 0 {
		cfg.AsyncTimeout = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Tracer == nil {
		cfg.Tracer = core.NoopTracer{}
	}
	loop := &ToolLoop{definitions: defs, executor: executor, history: history, cfg: cfg, async: newAsyncState()}
	if _, noop := cfg.Tracer.(core.NoopTracer); !noop {
		if _, async := cfg.Tracer.(*core.AsyncTracer); !async {
			loop.ownedTracer = core.NewAsyncTracer(cfg.Tracer, 0)
			loop.cfg.Tracer = loop.ownedTracer
		}
	}
	return loop, nil
}

// Close releases the loop-owned asynchronous tracer. It is safe to call more
// than once and is a no-op when the caller supplied an AsyncTracer or tracing
// is disabled. Owners that retain a ToolLoop must call Close at teardown.
func (l *ToolLoop) Close() {
	if l != nil && l.ownedTracer != nil {
		defer l.ownedTracer.Close()
	}
	if l != nil && l.async != nil {
		l.async.mu.Lock()
		l.async.cancel() // abandons in-flight async tools; no result is injected
		l.async.mu.Unlock()
		l.async.wg.Wait()
	}
}

// WithCurrentFence returns a derived loop whose generation predicate is the
// logical AND of this loop's predicate and fence. The receiver is never
// mutated. The derived loop shares immutable definitions, executor, history,
// and tracer with the receiver; in particular, ConversationContext remains the
// shared concurrency-safe replay/history owner. Caller-owned predicate state
// must itself be safe for concurrent calls. A nil fence is a no-op derived
// copy.
func (l *ToolLoop) WithCurrentFence(fence func(ToolCall) bool) *ToolLoop {
	if l == nil {
		return nil
	}
	derived := &ToolLoop{definitions: l.definitions, executor: l.executor, history: l.history, cfg: l.cfg, async: l.async, emit: l.emit}
	if fence == nil {
		return derived
	}
	configured := l.cfg.IsCurrent
	if configured == nil {
		derived.cfg.IsCurrent = fence
		return derived
	}
	derived.cfg.IsCurrent = func(call ToolCall) bool {
		return configured(call) && fence(call)
	}
	return derived
}

// WithTraceContext derives a loop using the session's shared dispatcher and
// policy. The caller's context remains the parent of each execution span.
func (l *ToolLoop) WithTraceContext(ctx context.Context) *ToolLoop {
	if l == nil {
		return nil
	}
	derived := &ToolLoop{definitions: l.definitions, executor: l.executor, history: l.history, cfg: l.cfg, async: l.async, emit: l.emit}
	trace := core.TraceContextFromContext(ctx)
	if _, noop := trace.Tracer.(core.NoopTracer); !noop {
		derived.cfg.Tracer = trace.Tracer
		derived.cfg.TracePolicy = trace.Policy
	}
	return derived
}

// WithFrames derives a loop that reports the calls it runs through emit: a
// *core.ToolCallFrame when it claims a call whose generation is current (an
// announced call), and exactly one *core.ToolResultFrame per announced call
// once its result is final (for an async tool, when the background run ends).
// Replays and stale calls are never announced. emit runs on the loop's
// goroutines; providers re-enter the frames into the session.
func (l *ToolLoop) WithFrames(emit func(core.Frame)) *ToolLoop {
	if l == nil {
		return nil
	}
	return &ToolLoop{definitions: l.definitions, executor: l.executor, history: l.history, cfg: l.cfg, async: l.async, emit: emit}
}

// announce reports a claimed call when frames are wanted and its generation is
// current, and says whether it did; only an announced call is later settled. A
// name the loop has no definition for is announced as "unknown_tool": the model
// wrote it, and it can hold the caller's words.
func (l *ToolLoop) announce(call ToolCall, started time.Time) bool {
	if l.emit == nil || !l.isCurrent(call) {
		return false
	}
	c := cloneCall(call)
	if _, ok := l.definitions[c.Name]; !ok {
		c.Name = "unknown_tool"
	}
	l.emit(&core.ToolCallFrame{Call: c, Timestamp: started})
	return true
}

// report settles an announced call. A loop error (history refused the result)
// settles it as failed with the category context_error.
func (l *ToolLoop) report(call ToolCall, started time.Time, res ToolResult, err error) {
	if err != nil {
		res = ToolResult{CallID: call.ID, Status: ToolFailed, ErrorCategory: "context_error", StartedAt: started, EndedAt: l.cfg.Now()}
	}
	l.emit(&core.ToolResultFrame{Turn: call.TurnID, Gen: call.Generation, Result: cloneToolResult(res), Timestamp: l.cfg.Now()})
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
		roundResults := make([]ToolResult, 0, len(calls))
		batch := calls
		truncated := len(batch) > l.cfg.MaxCalls-result.Calls
		if truncated {
			batch = batch[:l.cfg.MaxCalls-result.Calls]
		}
		slots := func() []slot {
			// AGENTS.md says never block under a lock; this read lock is a deliberate exception.
			// It already spans the executors, and announce/report (which can wait up to 2 s in
			// Session.Reenter) run inside the round, in call order, from the executor goroutines.
			// Collecting the frames to post after RUnlock would deliver every `started` after
			// the tools finished and need a second ordering layer for the exactly-once settle.
			// It cannot deadlock: only injectAsync takes the lock exclusively, on a background
			// goroutine, so the worst case is a delayed async injection.
			// Ceiling: in the parallel claim phase each announce can block up to 2 s on a full
			// data queue (16 calls = 32 s) before any tool runs, with the round timer ticking.
			l.async.round.RLock()
			defer l.async.round.RUnlock()
			l.history.BeginToolRound() // plain messages queue until the results are written
			defer l.history.EndToolRound()
			return l.runRound(roundCtx, batch)
		}()
		for _, s := range slots {
			if !s.ran {
				break
			}
			result.Calls++
			if s.err != nil {
				cancelRound()
				return result, s.err
			}
			toolResult := s.res
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
		if truncated {
			cancelRound()
			return result, ErrMaxToolCalls
		}
		// A generation that moved on after the round still aborts the loop, but
		// results already committed to history are returned as recorded: history
		// results are immutable, so rewriting them here would contradict context.
		for _, call := range calls {
			if !l.isCurrent(call) {
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
		if len(next) == 0 { // nothing left to call: the loop is complete
			result.Done = true
			return result, nil
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

// execute is begin, announce and finish with no commit gate (sequential path).
// abandonGrace is how long a cancelled executor may take to return before
// its round stops waiting for it.
const abandonGrace = time.Second

func (l *ToolLoop) execute(roundCtx context.Context, call ToolCall) (ToolResult, error) {
	res, done, started, err := l.begin(call)
	if done {
		return res, err
	}
	return l.finish(roundCtx, call, started, l.announce(call, started), nil)
}

// begin checks and claims a call. Its only history write is the claim (the
// tool call itself), so a parallel round can claim every call up front and the
// provider sees one contiguous call batch. done reports that res/err is final.
func (l *ToolLoop) begin(call ToolCall) (ToolResult, bool, time.Time, error) {
	if call.ID == "" || call.Name == "" || call.TurnID == "" || call.Generation == 0 || call.SessionID == "" {
		return ToolResult{}, true, time.Time{}, ErrMalformedToolCall
	}
	if len(call.Arguments) > l.cfg.MaxArgumentBytes {
		return ToolResult{}, true, time.Time{}, ErrMalformedToolCall
	}
	started := l.cfg.Now()
	prior, claimed, err := l.history.ClaimToolCall(call, started)
	if err != nil {
		return ToolResult{}, true, started, err
	}
	if !claimed {
		if !l.isCurrent(call) {
			return staleToolResult(call, prior.StartedAt, l.cfg.Now()), true, started, nil
		}
		if _, category := normalizePayload(prior.Result, l.cfg.MaxResultBytes, l.cfg.MaxJSONDepth); category != "" {
			return ToolResult{}, true, started, ErrReplayResultOutOfBounds
		}
		return cloneToolResult(prior), true, started, nil
	}
	return ToolResult{}, false, started, nil
}

// finish validates a claimed call, runs the executor (or acknowledges an async tool) and commits
// the result; an announced call is settled when finish returns. gate, when non-nil, is called
// before every history write so parallel rounds commit in call order.
func (l *ToolLoop) finish(roundCtx context.Context, call ToolCall, started time.Time, announced bool, gate func()) (res ToolResult, err error) {
	async := false // startAsync settled the call elsewhere (background run, or cancelled on close)
	if announced {
		defer func() {
			if !async {
				l.report(call, started, res, err)
			}
		}()
	}
	if gate == nil {
		gate = func() {}
	}
	commit := func(status ToolStatus, payload json.RawMessage, category string) (ToolResult, error) {
		gate()
		return l.commit(call, status, payload, category, started)
	}
	if !toolNamePattern.MatchString(call.Name) {
		return commit(ToolFailed, errorPayload("invalid_tool_name"), "invalid_tool_name")
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return commit(ToolFailed, errorPayload("invalid_arguments"), "invalid_arguments")
	}
	if !validJSONDepth(call.Arguments, l.cfg.MaxJSONDepth) {
		return commit(ToolFailed, errorPayload("arguments_too_deep"), "arguments_too_deep")
	}
	if _, ok := l.definitions[call.Name]; !ok {
		return commit(ToolFailed, errorPayload("unknown_tool"), "unknown_tool")
	}
	if !l.isCurrent(call) {
		return commit(ToolCancelled, errorPayload("stale_generation"), "stale_generation")
	}
	if err := roundCtx.Err(); err != nil {
		category := "timeout"
		if errors.Is(err, context.Canceled) {
			category = "cancelled"
		}
		return commit(ToolCancelled, errorPayload(category), category)
	}
	if def := l.definitions[call.Name]; def.Policy.Async {
		gate() // in call order, so MaxAsyncInFlight rejects deterministically
		res, async, err = l.startAsync(call, def, started, announced, commit)
		return res, err
	}
	def := l.definitions[call.Name]
	timeout := def.Policy.Timeout
	if timeout <= 0 || timeout > l.cfg.DefaultCallTimeout {
		timeout = l.cfg.DefaultCallTimeout
	}
	callCtx, cancel := context.WithTimeout(roundCtx, timeout)
	defer cancel()
	callCtx = core.ContextWithTraceContext(callCtx, l.cfg.Tracer, l.cfg.TracePolicy)
	var toolResult ToolResult
	var execErr error
	event := core.NewTraceEvent("tool.execute", started, call.SessionID, call.TurnID, call.Generation, l.cfg.TracePolicy, map[string]any{
		"tool.arguments": string(call.Arguments),
	})
	event.ToolCallID = call.ID
	span := core.StartSpanSafe(callCtx, l.cfg.Tracer, event)
	callCtx = core.ContextWithSpan(callCtx, span)
	traceEnded := false
	endTrace := func(category string) {
		if traceEnded {
			return
		}
		traceEnded = true
		if category == "" {
			span.End(nil)
			return
		}
		// Only canonical loop categories reach tracing, never executor text.
		span.End(errors.New(category))
	}
	// The executor runs aside so one that ignores ctx cannot hold the round (and
	// the history queue behind it) open: once callCtx is done the outcome is
	// cancelled/timeout whatever it returns, and after abandonGrace it is left
	// behind. ponytail: an abandoned goroutine lives until it returns itself.
	type outcome struct {
		res ToolResult
		err error
	}
	out := make(chan outcome, 1)
	go func() {
		var o outcome
		defer func() {
			if recover() != nil {
				o.err = errExecutorPanic
			}
			out <- o
		}()
		o.res, o.err = l.executor.Execute(callCtx, cloneCall(call))
	}()
	select {
	case o := <-out:
		toolResult, execErr = o.res, o.err
	case <-callCtx.Done():
		select { // a cooperative executor unwinds before the round ends
		case <-out:
		case <-time.After(abandonGrace):
		}
	}
	ended := l.cfg.Now()
	if !l.isCurrent(call) {
		endTrace("stale_generation")
		return commit(ToolCancelled, errorPayload("stale_generation"), "stale_generation")
	}
	if callCtx.Err() != nil {
		category := "timeout"
		if errors.Is(roundCtx.Err(), context.Canceled) {
			category = "cancelled"
		}
		endTrace(category)
		return commit(ToolCancelled, errorPayload(category), category)
	}
	if execErr != nil {
		category := "executor_error"
		if errors.Is(execErr, errExecutorPanic) {
			category = "executor_panic"
		}
		endTrace(category)
		return commit(ToolFailed, errorPayload(category), category)
	}
	toolResult.CallID, toolResult.StartedAt, toolResult.EndedAt = call.ID, started, ended
	if toolResult.Status == "" {
		toolResult.Status = ToolSucceeded
	}
	if toolResult.Status != ToolSucceeded && toolResult.Status != ToolFailed && toolResult.Status != ToolCancelled {
		endTrace("invalid_status")
		return commit(ToolFailed, errorPayload("invalid_status"), "invalid_status")
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
	traceCategory := ""
	if toolResult.Status == ToolFailed {
		traceCategory = "tool_failed"
	} else if toolResult.Status == ToolCancelled {
		traceCategory = "cancelled"
	}
	if category != "" {
		toolResult.Status, toolResult.ErrorCategory = ToolFailed, category
		traceCategory = category
	}
	toolResult.Result = payload
	if !l.isCurrent(call) {
		endTrace("stale_generation")
		return commit(ToolCancelled, errorPayload("stale_generation"), "stale_generation")
	}
	gate()
	if err := l.history.AddToolResultStatus(string(call.TurnID), uint64(call.Generation), call.ID, toolResult.Status, payload, toolResult.ErrorCategory, ended); err != nil {
		if errors.Is(err, core.ErrContextLimitExceeded) {
			// The call is already in history; give it a small result that fits
			// rather than leaving a call with no result.
			endTrace("result_too_large")
			return commit(ToolFailed, errorPayload("result_too_large"), "result_too_large")
		}
		endTrace("context_error")
		return ToolResult{}, err
	}
	endTrace(traceCategory)
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
