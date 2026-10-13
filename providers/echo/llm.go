package echo

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"govoice/core"
	"govoice/providers/google"
)

// LLMOptions configure the simulated model.
type LLMOptions struct {
	Figures    LLMFigures
	ExtraDelay time.Duration     // added to every word (the perf-gate self-test)
	Label      string            // prefixes every reply, e.g. "[front desk] "
	Phrases    map[string]string // lower-case phrase -> tool name ("transfer to sales" -> transfer_to_sales)
	Spoken     map[string]string // tool name -> the name its reply uses; these take {json} arguments from the message and say what came back
}

// LLMClient is a simulated Google model backend: the real google LLM processor (retries, breaker,
// usage, tracing, tool loop) streams from it. It repeats the caller, word by word, and calls the
// tool a known phrase asks for.
type LLMClient struct {
	o      LLMOptions
	nextID atomic.Uint64
}

func NewLLMClient(o LLMOptions) *LLMClient { return &LLMClient{o: o} }

func (c *LLMClient) GenerateStream(ctx context.Context, _, _ string, history []core.Message) (<-chan google.LLMStreamChunk, <-chan error, error) {
	return c.stream(ctx, history, false)
}

func (c *LLMClient) GenerateStreamWithNativeTools(ctx context.Context, _, _ string, history []core.Message, _ []google.GoogleNativeExchange, defs []core.ToolDefinition, _ core.ToolCall) (<-chan google.LLMStreamChunk, <-chan error, error) {
	return c.stream(ctx, history, len(defs) > 0)
}

func (c *LLMClient) stream(ctx context.Context, history []core.Message, hasTools bool) (<-chan google.LLMStreamChunk, <-chan error, error) {
	chunks := make(chan google.LLMStreamChunk, 8)
	errs := make(chan error, 1)
	go func() {
		defer close(chunks)
		defer close(errs)
		send := func(ch google.LLMStreamChunk) bool {
			select {
			case chunks <- ch:
				return true
			case <-ctx.Done():
				return false
			}
		}
		promptTokens := wordCount(history)
		if call, ok := c.toolCall(history, hasTools); ok {
			send(google.LLMStreamChunk{ToolCalls: []core.ToolCall{call}, PromptTokens: promptTokens, TotalTokens: promptTokens})
			return
		}
		words := c.pieces(history)
		if err := wait(ctx, c.o.Figures.FirstToken); err != nil {
			return
		}
		for i, w := range words {
			if i > 0 {
				if err := wait(ctx, c.o.Figures.TokenDelay+c.o.ExtraDelay); err != nil {
					return
				}
			}
			if !send(google.LLMStreamChunk{Text: w}) {
				return
			}
		}
		send(google.LLMStreamChunk{IsDone: true, FinishReason: "STOP", PromptTokens: promptTokens, OutputTokens: len(words), TotalTokens: promptTokens + len(words)})
	}()
	return chunks, errs, nil
}

// pieces is what the model says, in streaming order: the label, then the answer to a tool result or
// the caller's words repeated; one word each with WordChunks, else one phrase each.
func (c *LLMClient) pieces(history []core.Message) []string {
	var parts []string
	if c.o.Label != "" {
		parts = append(parts, c.o.Label)
	}
	if n := len(history); n > 0 && history[n-1].Kind == core.MessageToolResult {
		parts = append(parts, c.toolAnswer(history[n-1]))
	} else {
		parts = append(parts, "You said: ", lastUser(history))
	}
	if !c.o.Figures.WordChunks {
		return parts
	}
	var words []string
	for _, p := range parts {
		words = append(words, splitWords(p)...)
	}
	return words
}

func (c *LLMClient) toolAnswer(m core.Message) string {
	spoken, say := c.o.Spoken[m.ToolName]
	if !say {
		return "Connecting you."
	}
	if m.ToolStatus != "" && m.ToolStatus != core.ToolSucceeded {
		why := m.ToolErrorCategory
		if why == "" {
			why = string(m.ToolStatus)
		}
		return "Tool " + spoken + " failed: " + why
	}
	return "Tool " + spoken + " returned " + m.Content
}

// toolCall is the call the last user message asks for (first matching phrase in sorted order, so
// the choice is deterministic), unless the model is already answering a tool result.
func (c *LLMClient) toolCall(history []core.Message, hasTools bool) (core.ToolCall, bool) {
	if !hasTools || len(history) == 0 || history[len(history)-1].Kind == core.MessageToolResult {
		return core.ToolCall{}, false
	}
	msg := lastUser(history)
	lower := strings.ToLower(msg)
	phrases := make([]string, 0, len(c.o.Phrases))
	for p := range c.o.Phrases {
		phrases = append(phrases, p)
	}
	sort.Strings(phrases)
	for _, p := range phrases {
		if strings.Contains(lower, p) {
			name := c.o.Phrases[p]
			args := json.RawMessage(`{}`)
			if _, say := c.o.Spoken[name]; say {
				args = toolArguments(msg)
			}
			return core.ToolCall{ID: "echo-call-" + strconv.FormatUint(c.nextID.Add(1), 10), Name: name, Arguments: args}, true
		}
	}
	return core.ToolCall{}, false
}

func lastUser(history []core.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == core.RoleUser && (history[i].Kind == "" || history[i].Kind == core.MessageText) {
			return history[i].Content
		}
	}
	return ""
}

func wordCount(history []core.Message) int {
	n := 0
	for _, m := range history {
		n += len(strings.Fields(m.Content))
	}
	return n
}

// splitWords splits s after each space, so the pieces concatenate back to s.
func splitWords(s string) []string {
	parts := strings.SplitAfter(s, " ")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// toolArguments is the first balanced {...} in msg if it is a JSON object, else {}.
func toolArguments(msg string) json.RawMessage {
	start := strings.IndexByte(msg, '{')
	if start < 0 {
		return json.RawMessage(`{}`)
	}
	depth := 0
	for i := start; i < len(msg); i++ {
		switch msg[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var obj map[string]any
				if json.Unmarshal([]byte(msg[start:i+1]), &obj) == nil {
					return json.RawMessage(msg[start : i+1])
				}
				return json.RawMessage(`{}`)
			}
		}
	}
	return json.RawMessage(`{}`)
}
