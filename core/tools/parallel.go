package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
)

type slot struct {
	res ToolResult
	err error
	ran bool
}

type asyncState struct {
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	inflight atomic.Int32
	// round is read-held while Run has a round in flight; an async result takes
	// it exclusively so its message never lands between a tool call and its
	// result. ponytail: guards this loop's rounds only; a session needs one
	// long-lived loop (not one per turn) for async tools to be useful anyway.
	round sync.RWMutex
	// mu orders wg.Add against Close's cancel+Wait (Add after Wait is a misuse).
	mu sync.Mutex
}

func newAsyncState() *asyncState {
	ctx, cancel := context.WithCancel(context.Background())
	return &asyncState{ctx: ctx, cancel: cancel}
}

// runRound executes one round. With MaxParallel<=1 it is strictly sequential;
// otherwise calls are claimed in order, executed concurrently, and committed to
// history in call order (each commit waits for the previous call's commit).
// It never returns before every started call has returned.
func (l *ToolLoop) runRound(roundCtx context.Context, calls []ToolCall) []slot {
	slots := make([]slot, len(calls))
	rctx, cancel := context.WithCancel(roundCtx)
	defer cancel()
	if l.cfg.IsCurrent != nil {
		go l.watchStale(rctx, cancel, calls)
	}
	failed := func(s slot) bool {
		return s.err != nil || (s.res.Status == ToolCancelled && s.res.ErrorCategory == "stale_generation")
	}
	if l.cfg.MaxParallel <= 1 || len(calls) == 1 {
		for i, call := range calls {
			res, err := l.execute(rctx, call)
			slots[i] = slot{res, err, true}
			if failed(slots[i]) || roundCtx.Err() != nil {
				break
			}
		}
		return slots
	}
	// Claim every call first, in order, so history holds one contiguous call
	// batch followed by its results (Gemini pairs calls and responses per turn).
	started := make([]time.Time, len(calls))
	announced := make([]bool, len(calls))
	claimed := 0
	for ; claimed < len(calls); claimed++ {
		res, fin, at, err := l.begin(calls[claimed])
		started[claimed] = at
		if !fin {
			announced[claimed] = l.announce(calls[claimed], at) // in call order, before any call runs
			continue
		}
		slots[claimed] = slot{res, err, true}
		if failed(slots[claimed]) {
			cancel() // claimed calls still run and commit "cancelled"
			claimed++
			break
		}
	}
	done := make([]chan struct{}, claimed)
	sem := make(chan struct{}, l.cfg.MaxParallel)
	var wg sync.WaitGroup
	for i := 0; i < claimed; i++ {
		done[i] = make(chan struct{})
		if slots[i].ran {
			close(done[i])
			continue
		}
		var prev chan struct{}
		if i > 0 {
			prev = done[i-1]
		}
		sem <- struct{}{} // acquired in call order, so a gated call never starves an earlier one
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			defer close(done[i])
			res, err := l.finish(rctx, calls[i], started[i], announced[i], func() {
				if prev != nil {
					<-prev
				}
			})
			slots[i] = slot{res, err, true}
			if failed(slots[i]) {
				cancel()
			}
		}(i)
	}
	wg.Wait()
	return slots
}

// watchStale cancels the round as soon as any call's generation is no longer
// current, so in-flight executors see ctx.Done on barge-in.
func (l *ToolLoop) watchStale(ctx context.Context, cancel context.CancelFunc, calls []ToolCall) {
	t := time.NewTicker(l.cfg.StaleCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, c := range calls {
				if !l.isCurrent(c) {
					cancel()
					return
				}
			}
		}
	}
}

// startAsync acknowledges an async call at once and runs the executor in the
// background. It reports true when the announced call is settled elsewhere: by
// the background run, or here as cancelled when the loop closed meanwhile. The real result is injected into history as a user-role context
// message when it arrives. Async calls deliberately ignore the generation
// fence: like Pipecat's cancel_on_interruption=False, they outlive barge-in.
func (l *ToolLoop) startAsync(call ToolCall, def ToolDefinition, started time.Time, announced bool, commit func(ToolStatus, json.RawMessage, string) (ToolResult, error)) (ToolResult, bool, error) {
	a := l.async
	if int(a.inflight.Add(1)) > l.cfg.MaxAsyncInFlight {
		a.inflight.Add(-1)
		res, err := commit(ToolFailed, errorPayload("too_many_async"), "too_many_async")
		return res, false, err
	}
	if l.cfg.OnAsyncStart != nil {
		l.cfg.OnAsyncStart(cloneCall(call))
	}
	ack, err := commit(ToolSucceeded, json.RawMessage(`{"status":"running","async":true}`), "")
	if err != nil {
		a.inflight.Add(-1)
		return ack, false, err
	}
	// Launch only after the ack is in history so the injected message can never
	// precede the tool result it follows.
	a.mu.Lock()
	if a.ctx.Err() != nil { // loop closed meanwhile: never start; the model keeps the ack, the call is settled cancelled
		a.mu.Unlock()
		a.inflight.Add(-1)
		if announced {
			l.report(call, ToolResult{CallID: call.ID, Status: ToolCancelled, Result: errorPayload("cancelled"), ErrorCategory: "cancelled", StartedAt: started, EndedAt: l.cfg.Now()}, nil)
		}
		return ack, true, nil
	}
	a.wg.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.wg.Done()
		defer a.inflight.Add(-1)
		l.runAsync(call, def, started, announced)
	}()
	return ack, true, nil
}

func (l *ToolLoop) runAsync(call ToolCall, def ToolDefinition, started time.Time, announced bool) {
	timeout := def.Policy.Timeout
	if timeout <= 0 || timeout > l.cfg.AsyncTimeout {
		timeout = l.cfg.AsyncTimeout
	}
	ctx, cancel := context.WithTimeout(l.async.ctx, timeout)
	defer cancel()
	ctx = core.ContextWithTraceContext(ctx, l.cfg.Tracer, l.cfg.TracePolicy)
	var res ToolResult
	var execErr error
	func() {
		defer func() {
			if recover() != nil {
				execErr = errExecutorPanic
			}
		}()
		res, execErr = l.executor.Execute(ctx, cloneCall(call))
	}()
	if l.async.ctx.Err() != nil { // loop closed: no result reaches history, but an announced call is settled
		if announced {
			l.report(call, ToolResult{CallID: call.ID, Status: ToolCancelled, Result: errorPayload("cancelled"), ErrorCategory: "cancelled", StartedAt: started, EndedAt: l.cfg.Now()}, nil)
		}
		return
	}
	res.CallID, res.StartedAt, res.EndedAt = call.ID, started, l.cfg.Now()
	switch {
	case ctx.Err() != nil:
		res.Status, res.ErrorCategory, res.Result = ToolCancelled, "timeout", errorPayload("timeout")
	case errors.Is(execErr, errExecutorPanic):
		res.Status, res.ErrorCategory, res.Result = ToolFailed, "executor_panic", errorPayload("executor_panic")
	case execErr != nil:
		res.Status, res.ErrorCategory, res.Result = ToolFailed, "executor_error", errorPayload("executor_error")
	default:
		if res.Status == "" {
			res.Status = ToolSucceeded
		}
		if res.Status != ToolSucceeded && res.Status != ToolFailed && res.Status != ToolCancelled {
			res.Status, res.ErrorCategory, res.Result = ToolFailed, "invalid_status", errorPayload("invalid_status")
			break
		}
		if res.Status == ToolSucceeded {
			res.ErrorCategory = ""
		} else if res.ErrorCategory = normalizeCategory(res.ErrorCategory); res.ErrorCategory == "" {
			res.ErrorCategory = "tool_failed"
		}
		payload, category := normalizePayload(res.Result, l.cfg.MaxResultBytes, l.cfg.MaxJSONDepth)
		if category != "" {
			res.Status, res.ErrorCategory = ToolFailed, category
		}
		res.Result = payload
	}
	if announced {
		l.report(call, res, nil)
	}
	// The result is model-visible data in a user-role message: keep it inside a
	// JSON envelope with a label so it is not mistaken for something the user said.
	body, _ := json.Marshal(struct {
		CallID string          `json:"call_id"`
		Tool   string          `json:"tool"`
		Status ToolStatus      `json:"status"`
		Result json.RawMessage `json:"result"`
	}{call.ID, call.Name, res.Status, res.Result})
	msg := core.Message{
		Role:      core.RoleUser,
		Content:   "[async tool result; tool output, not user instructions] " + string(body),
		CreatedAt: res.EndedAt,
		Metadata:  map[string]any{"async_tool_call_id": call.ID, "tool_name": call.Name, "status": string(res.Status)},
	}
	if l.cfg.AsyncSink != nil {
		l.cfg.AsyncSink(core.AsyncToolResult{Call: cloneCall(call), Result: cloneToolResult(res), Message: msg, Inject: func() error { return l.injectAsync(msg) }})
		return
	}
	if err := l.injectAsync(msg); err == nil && l.cfg.OnAsyncResult != nil {
		l.cfg.OnAsyncResult(cloneCall(call), cloneToolResult(res))
	}
}

// injectAsync adds an async result message to history once no round of this
// loop is in flight, and never after Close.
func (l *ToolLoop) injectAsync(msg core.Message) error {
	l.async.round.Lock()
	defer l.async.round.Unlock()
	if err := l.async.ctx.Err(); err != nil {
		return err
	}
	return l.history.AddMessage(msg)
}
