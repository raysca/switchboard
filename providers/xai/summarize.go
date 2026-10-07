package xai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"govoice/core/resilience"
)

const maxSummaryResponse = 1 << 20

// SummarizeText runs one plain, tool-less, non-streaming completion. It is
// off the call's hot path and deliberately bypasses the processor's circuit
// breaker and retries: a failed summary must never open the live LLM breaker
// (the caller has its own cooldown). ctx bounds the request.
func (p *Processor) SummarizeText(ctx context.Context, system, user string) (string, error) {
	if p.cfg.CircuitBreaker.WouldRefuse() { // the live breaker says the provider is down: don't burn a call on it
		return "", resilience.ErrCircuitOpen
	}
	msgs := []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}}
	req := responsesRequest{Model: p.cfg.Model, Store: false}
	if strings.Contains(p.cfg.Endpoint, "chat/completions") {
		req.Messages = msgs
	} else {
		req.Input = msgs
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	hr.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		hr.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	resp, err := p.cfg.HTTPClient.Do(hr)
	if err != nil {
		return "", MapError("llm", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", MapHTTPError("llm", resp, string(b), nil)
	}
	var out struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSummaryResponse)).Decode(&out); err != nil {
		return "", MapError("llm", err)
	}
	text := out.OutputText // when present it already joins the output items
	for _, o := range out.Output {
		for _, c := range o.Content {
			if out.OutputText == "" && c.Type == "output_text" { // never reasoning text
				text += c.Text
			}
		}
	}
	if text == "" && len(out.Choices) > 0 {
		text = out.Choices[0].Message.Content
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("xai: summary response is empty")
	}
	return strings.TrimSpace(text), nil
}
