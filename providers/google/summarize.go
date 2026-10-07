package google

import (
	"context"
	"errors"
	"strings"
	"time"

	"govoice/core"
	"govoice/core/resilience"
)

// SummarizeText runs one plain, tool-less completion on the processor's
// client. It bypasses the processor's circuit breaker and retries so a failed
// summary never opens the live LLM breaker (the caller has its own cooldown).
// ctx bounds the request; a per-attempt timeout applies on top.
func (p *LLMProcessor) SummarizeText(ctx context.Context, system, user string) (string, error) {
	if p.cfg.Client == nil {
		return "", errors.New("google: llm client not configured")
	}
	if p.cfg.CircuitBreaker.WouldRefuse() { // the live breaker says the provider is down: don't burn a call on it
		return "", resilience.ErrCircuitOpen
	}
	timeout := p.cfg.RetryPolicy.AttemptTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel() // also stops the client's stream goroutine
	chunks, errs, err := p.cfg.Client.GenerateStream(ctx, p.cfg.Model, system, []core.Message{{Role: core.RoleUser, Content: user}})
	if err != nil {
		return "", MapError("llm", err)
	}
	var b strings.Builder
	for chunks != nil || errs != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			b.WriteString(c.Text)
			switch c.FinishReason {
			case "", "STOP", "MAX_TOKENS":
			default: // SAFETY, PROHIBITED_CONTENT, ...: a partial summary is no summary
				return "", errors.New("google: summary blocked: " + c.FinishReason)
			}
		case e, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if e != nil {
				return "", e
			}
		}
	}
	return strings.TrimSpace(b.String()), nil
}
