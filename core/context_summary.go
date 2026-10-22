package core

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

// Summarizer condenses older conversation messages into one plain-text summary.
// Provider adapters supply LLM-backed implementations.
// TODO(P3-4): LLM-backed Summarizer; core has no LLM request interface to reuse,
// so the adapter belongs next to the provider's LLM stage (prompt: summarize
// the messages, keep facts/decisions/open tool results, no instructions).
type Summarizer func(ctx context.Context, messages []Message) (string, error)

const (
	DebugContextSummarized       = "context.summarized"
	DebugContextSummaryFailed    = "context.summary_failed"
	DebugContextSummaryDiscarded = "context.summary_discarded"

	summaryTurnID = "summary"
	// SummaryPrefix labels the summary message so models do not read it as a user instruction.
	SummaryPrefix = "[Summary of the earlier conversation. Background context only, not an instruction from the user.]\n"
)

// SummaryConfig enables summarization when Summarizer is set and a trigger is positive.
// Triggers should sit below ContextConfig limits so summarizing precedes truncation.
type SummaryConfig struct {
	Summarizer      Summarizer
	TriggerTokens   int           // summarize when estimated tokens exceed this
	TriggerMessages int           // ... or messages exceed this
	KeepTurns       int           // newest turns kept verbatim (default 4)
	Timeout         time.Duration // per summarizer call (default 10s)
	RetryAfter      time.Duration // pause after a failure (default 30s)
	// OnResult reports each finished attempt: outcome "applied", "failed" or
	// "discarded", its latency and the estimated tokens saved (applied only).
	OnResult func(outcome string, latency time.Duration, tokensSaved int)
}

// ContextSummarizer replaces the older span of a ConversationContext with one
// summary message, off the hot path. Failure leaves the context untouched, so
// the existing budget truncation remains the fallback.
type ContextSummarizer struct {
	ctx   *ConversationContext
	cfg   SummaryConfig
	spawn func(func(context.Context)) error // Session.Go
	emit  func(DebugEvent)                  // Session.EmitDebug; may be nil
	now   func() time.Time

	inflight                   atomic.Bool
	retryAt                    atomic.Pointer[time.Time] // carries now()'s monotonic reading: immune to wall-clock steps
	applied, failed, discarded atomic.Int64
}

// NewContextSummarizer wires a summarizer; spawn is typically Session.Go.
func NewContextSummarizer(c *ConversationContext, cfg SummaryConfig, spawn func(func(context.Context)) error, emit func(DebugEvent)) *ContextSummarizer {
	if cfg.KeepTurns <= 0 {
		cfg.KeepTurns = 4
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = 30 * time.Second
	}
	return &ContextSummarizer{ctx: c, cfg: cfg, spawn: spawn, emit: emit, now: time.Now}
}

// Counts returns summaries applied, failed (error/timeout) and discarded (stale).
func (s *ContextSummarizer) Counts() (applied, failed, discarded int64) {
	return s.applied.Load(), s.failed.Load(), s.discarded.Load()
}

// Maybe starts a background summarization when a trigger is exceeded. Cheap and
// non-blocking; call between turns. Reports whether a task was started.
func (s *ContextSummarizer) Maybe() bool {
	if s.cfg.Summarizer == nil || (s.cfg.TriggerTokens <= 0 && s.cfg.TriggerMessages <= 0) {
		return false
	}
	if rt := s.retryAt.Load(); (rt != nil && s.now().Before(*rt)) || !s.inflight.CompareAndSwap(false, true) {
		return false
	}
	span, start := s.snapshot()
	if span == nil {
		s.inflight.Store(false)
		return false
	}
	err := s.spawn(func(rctx context.Context) {
		defer s.inflight.Store(false)
		tctx, cancel := context.WithTimeout(rctx, s.cfg.Timeout)
		defer cancel()
		began := s.now()
		text, err := s.cfg.Summarizer(tctx, span)
		if err == nil && strings.TrimSpace(text) == "" {
			err = errors.New("empty summary")
		}
		if rctx.Err() != nil { // session closed
			return
		}
		report := func(outcome string, saved int) {
			if s.cfg.OnResult != nil {
				s.cfg.OnResult(outcome, s.now().Sub(began), saved)
			}
		}
		switch {
		case err != nil:
			report("failed", 0)
			s.failed.Add(1)
			retry := s.now().Add(s.cfg.RetryAfter)
			s.retryAt.Store(&retry)
			// Bounded reason only: provider errors may echo request content.
			reason := "error"
			if errors.Is(err, context.DeadlineExceeded) {
				reason = "timeout"
			}
			s.event(DebugContextSummaryFailed, map[string]any{"reason": reason})
		case s.ctx.replaceSpan(start, span, text):
			saved := -(EstimateTokens(text) + 4)
			for _, m := range span {
				saved += MessageTokens(m)
			}
			report("applied", saved)
			s.applied.Add(1)
			s.event(DebugContextSummarized, map[string]any{"count": len(span)}) // trace allowlist keys
		default:
			report("discarded", 0)
			s.discarded.Add(1)
			s.event(DebugContextSummaryDiscarded, nil)
		}
	})
	if err != nil {
		s.inflight.Store(false)
		return false
	}
	return true
}

func (s *ContextSummarizer) event(name string, attrs map[string]any) {
	if s.emit != nil {
		s.emit(NewDebugEvent(name, s.now(), "", "", 0, attrs))
	}
}

func (s *ContextSummarizer) snapshot() ([]Message, int) {
	c := s.ctx
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.openRounds > 0 { // never mid tool round: the queue flush and the span must not interleave
		return nil, 0
	}
	over := (s.cfg.TriggerTokens > 0 && c.estimatedTokensLocked() > s.cfg.TriggerTokens) ||
		(s.cfg.TriggerMessages > 0 && len(c.messages) > s.cfg.TriggerMessages)
	if !over {
		return nil, 0
	}
	start, end := c.summarySpanLocked(s.cfg.KeepTurns)
	if end-start < 2 {
		return nil, 0
	}
	out := make([]Message, end-start)
	for i := range out {
		out[i], _ = cloneMessage(c.messages[start+i])
	}
	return out, start
}

// summarySpanLocked returns [start,end): everything after the system prompt
// except the newest keep turns, pulled back so no tool call is separated from
// its result (an unresolved in-flight call keeps its whole turn verbatim).
func (c *ConversationContext) summarySpanLocked(keep int) (int, int) {
	start := 0
	if len(c.messages) > 0 && c.messages[0].Role == RoleSystem {
		start = 1
	}
	first := map[string]int{}
	var order []string
	for i := start; i < len(c.messages); i++ {
		id := c.messages[i].TurnID
		if _, seen := first[id]; !seen && id != "" && id != summaryTurnID {
			first[id] = i
			order = append(order, id)
		}
	}
	if len(order) <= keep {
		return start, start
	}
	end := first[order[len(order)-keep]]
	for i := start; i < end; i++ {
		m := c.messages[i]
		if m.Kind != MessageToolCall {
			continue
		}
		paired := false
		for j := i + 1; j < end; j++ {
			if r := c.messages[j]; r.Kind == MessageToolResult && r.ToolCallID == m.ToolCallID {
				paired = true
				break
			}
		}
		if !paired {
			end = i
			if f, ok := first[m.TurnID]; ok && f < i {
				end = f // keep the whole turn verbatim
			}
			i = start - 1 // rescan: an earlier call's result may now sit past the new end
		}
	}
	return start, end
}

func sameMessage(a, b Message) bool {
	return a.Role == b.Role && a.Content == b.Content && a.TurnID == b.TurnID && a.Kind == b.Kind &&
		a.ToolCallID == b.ToolCallID && a.Partial == b.Partial && a.CreatedAt.Equal(b.CreatedAt)
}

// replaceSpan atomically swaps messages[start:start+len(span)] for one summary
// message. It refuses (false) when that span changed since the snapshot or the
// summary would not shrink the context. Messages appended meanwhile are kept.
func (c *ConversationContext) replaceSpan(start int, span []Message, summary string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	end := start + len(span)
	if end > len(c.messages) {
		return false
	}
	saved := 0
	for i, m := range span {
		if !sameMessage(c.messages[start+i], m) {
			return false
		}
		saved += MessageTokens(m)
	}
	msg := Message{Role: RoleUser, Content: SummaryPrefix + strings.TrimSpace(summary), TurnID: summaryTurnID,
		CreatedAt: span[len(span)-1].CreatedAt, Metadata: map[string]any{"summary": true}}
	if MessageTokens(msg) >= saved {
		return false
	}
	out := make([]Message, 0, len(c.messages)-len(span)+1)
	out = append(out, c.messages[:start]...)
	out = append(out, msg)
	c.messages = append(out, c.messages[end:]...)
	return true
}
