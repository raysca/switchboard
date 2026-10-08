package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"govoice/core/resilience"
)

// SummarizeText runs one plain, tool-less, non-streaming completion. It is
// off the call's hot path and deliberately bypasses the processor's circuit
// breaker and retries: a failed summary must never open the live LLM breaker
// (the caller has its own cooldown). ctx bounds the request.
func (p *Processor) SummarizeText(ctx context.Context, system, user string) (string, error) {
	if p.cfg.CircuitBreaker.WouldRefuse() { // the live breaker says the provider is down: don't burn a call on it
		return "", resilience.ErrCircuitOpen
	}
	body, err := json.Marshal(map[string]any{
		"model":  p.cfg.Model,
		"stream": false,
		"messages": []wireMsg{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	resp, err := p.cfg.HTTPClient.Do(req)
	if errors.Is(err, ErrBlockedAddress) {
		return "", &ProviderError{Message: ErrBlockedAddress.Error()}
	}
	if err != nil {
		return "", errors.New(SanitizeString(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", newHTTPError(resp, string(b), p.cfg.Model)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("openai: summary response has no choices")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
