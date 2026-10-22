package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/core/resilience"
	"govoice/server/flow"
)

// Defaults for failover.llm_budget (how long into a generation a new candidate
// may still be started) and failover.llm_first_token (how long a silent
// primary is waited on). Providers alone give up only after ~90 s (3 attempts x
// 30 s), which is why the first-token deadline is what lets failover rescue a hang.
const (
	defaultLLMFailoverBudget = 60 * time.Second
	defaultLLMFirstToken     = 8 * time.Second
)

// resolveLLMTiming applies the defaults to zero values.
func resolveLLMTiming(budget, firstToken time.Duration) (time.Duration, time.Duration) {
	if budget <= 0 {
		budget = defaultLLMFailoverBudget
	}
	if firstToken <= 0 {
		firstToken = defaultLLMFirstToken
	}
	return budget, firstToken
}

// llmFailoverTiming resolves the configured values (nil cfg = defaults).
func llmFailoverTiming(cfg *Config) (budget, firstToken time.Duration) {
	if cfg == nil {
		return resolveLLMTiming(0, 0)
	}
	return resolveLLMTiming(cfg.Failover.LLMBudget, cfg.Failover.LLMFirstToken)
}

// validateLLMFailoverTiming rejects values where the first-token deadline
// could never fire before the budget closes, which would make failover a no-op
// for a hung primary. Zero means default.
func validateLLMFailoverTiming(budget, firstToken time.Duration) error {
	if budget < 0 || firstToken < 0 {
		return errors.New("failover.llm_budget and failover.llm_first_token must not be negative")
	}
	if b, f := resolveLLMTiming(budget, firstToken); f >= b {
		return fmt.Errorf("failover.llm_first_token (%v) must be below failover.llm_budget (%v), or a hung primary can never fail over", f, b)
	}
	return nil
}

// llmClassify is the failover class of a failed LLM candidate ("" = none).
// Providers expose ClientError() on their ProviderError.
func llmClassify(err error) string {
	var ce interface{ ClientError() bool }
	switch {
	case errors.Is(err, core.ErrTooManyTasks), errors.Is(err, core.ErrSessionClosed):
		return "" // the session's own limits or teardown: another model would not help
	case errors.Is(err, core.ErrFirstTokenTimeout):
		return flow.ClassTimeout
	}
	return flow.ClassOf(err, resilience.Verdict{ClientError: errors.As(err, &ce) && ce.ClientError()})
}

// llmBreaker is the breaker an LLM stage of this provider uses (nil if unknown).
func (r *Registry) llmBreaker(spec StageSpec) *resilience.Breaker {
	switch spec.Provider {
	case "google":
		return r.googleLLMBreaker
	case "echo":
		return r.echoLLMBreaker
	case "xai":
		return r.xaiLLMBreaker
	case "offline":
		return r.offlineLLMBreaker
	case "nvidia":
		return r.nvidiaBreaker("llm|" + nvidiaNIMBaseURL)
	case "openai":
		if _, baseURL, err := openAICredential(spec.Secret); err == nil {
			br, _ := r.openaiBreakers.LoadOrStore(baseURL, resilience.NewBreaker(resilience.DefaultConfig()))
			return br.(*resilience.Breaker)
		}
	}
	return nil
}

// countingExecutor counts tool executions so a failed generation that already
// ran a tool is never replayed on another model.
type countingExecutor struct {
	core.ToolExecutor
	n *atomic.Uint64
}

func (c countingExecutor) Execute(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
	c.n.Add(1)
	return c.ToolExecutor.Execute(ctx, call)
}

// buildLLM builds the LLM stage: the primary alone (the pre-failover pipeline,
// unchanged) or, with fallbacks, a core.LLMFailover over the whole chain.
// Fallbacks that cannot be built are skipped (counted in voice_llm_fallbacks_skipped_total).
func (r *Registry) buildLLM(cfg *Config, call CallSpec, spec StageSpec, build func(string, StageSpec) (core.Processor, error)) (core.Processor, error) {
	if len(call.LLMFallbacks) == 0 {
		return build("llm", spec)
	}
	var effects atomic.Uint64
	specs := append([]StageSpec{spec}, call.LLMFallbacks...)
	var cands []core.LLMCandidate
	for i, sp := range specs {
		sp.History = spec.History
		// Count an async call when it is acknowledged, not when its goroutine
		// first runs: a failure in between must not look side-effect free.
		sp.OnAsyncStart = func(core.ToolCall) { effects.Add(1) }
		if sp.ToolExecutor != nil {
			sp.ToolExecutor = countingExecutor{sp.ToolExecutor, &effects}
		}
		p, err := build("llm", sp)
		if err != nil {
			if i == 0 {
				closeAll(cands)
				return nil, err
			}
			if r.onLLMFallbackSkipped != nil {
				r.onLLMFallbackSkipped()
			}
			continue
		}
		c := core.LLMCandidate{Provider: sp.Provider, Proc: p}
		if br := r.llmBreaker(sp); br != nil {
			c.Open = br.WouldRefuse
			c.OnFirstTokenTimeout = br.RecordFailure // a hang is a provider failure the breaker should see
		}
		cands = append(cands, c)
	}
	if len(cands) == 1 {
		return cands[0].Proc, nil
	}
	budget, firstToken := llmFailoverTiming(cfg) // pair validated by ValidateConfig (server.New, LoadConfigProfile)
	fo, err := core.NewLLMFailover(core.LLMFailoverConfig{
		Candidates: cands, Classify: llmClassify, Allow: call.Failover.Allows,
		SideEffects: effects.Load, Budget: budget, FirstTokenTimeout: firstToken, OnFailover: r.onLLMFailover,
	})
	if err != nil {
		closeAll(cands)
	}
	return fo, err
}

func closeAll(cands []core.LLMCandidate) {
	for _, c := range cands {
		if cl, ok := c.Proc.(io.Closer); ok {
			_ = cl.Close()
		}
	}
}

// failoverCounts is the llm.failover{from,to,class} counter. Labels are bounded:
// registered provider ids and the four failover classes.
type failoverCounts struct {
	mu sync.Mutex
	n  map[[3]string]uint64
}

func (f *failoverCounts) add(from, to, class string) {
	f.mu.Lock()
	if f.n == nil {
		f.n = map[[3]string]uint64{}
	}
	f.n[[3]string{from, to, class}]++
	f.mu.Unlock()
}

func (f *failoverCounts) write(w io.Writer) {
	f.writeAs(w, "voice_llm_failover_total", "LLM generations moved to a fallback provider before the first token.")
}

func (f *failoverCounts) writeAs(w io.Writer, name, help string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	keys := make([][3]string, 0, len(f.n))
	for k := range f.n {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	for _, k := range keys {
		fmt.Fprintf(w, "%s{from=%q,to=%q,class=%q} %d\n", name, k[0], k[1], k[2], f.n[k])
	}
}
