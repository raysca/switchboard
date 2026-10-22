package echo

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"govoice/core"
	"govoice/providers/google"
)

// LLMOptions configure the simulated model.
type LLMOptions struct {
	Figures    LLMFigures
	ExtraDelay time.Duration     // added between pieces, after the first (the perf-gate self-test)
	Label      string            // prefixes every reply, e.g. "[front desk] "
	Phrases    map[string]string // lower-case phrase -> tool name ("transfer to sales" -> transfer_to_sales)
	Spoken     map[string]string // tool name -> the name its reply uses; these take {json} arguments from the message and say what came back
}

// LLMClient is a simulated Google model backend: the real google LLM processor (retries, breaker,
// usage, tracing, tool loop) streams from it. It repeats the caller, word by word, and calls the
// tool a known phrase asks for.
type LLMClient struct {
	o       LLMOptions
	phrases []string // o.Phrases keys, sorted once: the first match wins, deterministically
}

// callSeq makes tool call IDs unique across every client of the process: each step of a call builds
// its own client, but the steps share one tool ledger, which rejects a reused ID.
var callSeq atomic.Uint64

func NewLLMClient(o LLMOptions) *LLMClient {
	c := &LLMClient{o: o, phrases: make([]string, 0, len(o.Phrases))}
	for p := range o.Phrases {
		c.phrases = append(c.phrases, p)
	}
	sort.Strings(c.phrases)
	return c
}

func (c *LLMClient) GenerateStream(ctx context.Context, _, _ string, history []core.Message, sampling google.Sampling) (<-chan google.LLMStreamChunk, <-chan error, error) {
	return c.stream(ctx, history, false, sampling)
}

func (c *LLMClient) GenerateStreamWithNativeTools(ctx context.Context, _, _ string, history []core.Message, _ []google.GoogleNativeExchange, defs []core.ToolDefinition, _ core.ToolCall, sampling google.Sampling) (<-chan google.LLMStreamChunk, <-chan error, error) {
	return c.stream(ctx, history, len(defs) > 0, sampling)
}

func (c *LLMClient) stream(ctx context.Context, history []core.Message, hasTools bool, sampling google.Sampling) (<-chan google.LLMStreamChunk, <-chan error, error) {
	chunks := make(chan google.LLMStreamChunk, 8)
	errs := make(chan error, 1)
	go func() {
		defer close(chunks)
		defer close(errs)
		defer func() { // the processor reads two closed channels as a finished reply: report the panic
			if recover() != nil {
				select {
				case errs <- errors.New("echo llm: stream panicked"):
				default:
				}
			}
		}()
		send := func(ch google.LLMStreamChunk) bool {
			select {
			case chunks <- ch:
				return true
			case <-ctx.Done():
				return false
			}
		}
		promptTokens := wordCount(history)
		if calls := c.toolCalls(history, hasTools); len(calls) > 0 {
			if err := ctx.Err(); err != nil { // cancelled before the call: report it, emit nothing
				errs <- err
				return
			}
			if !send(google.LLMStreamChunk{ToolCalls: calls, PromptTokens: promptTokens, TotalTokens: promptTokens}) {
				errs <- ctx.Err()
			}
			return
		}
		// A cancelled or timed-out request is reported, not just ended: the processor reads two closed
		// channels as a finished reply, and a timeout would pass for a silent empty answer.
		fail := func(err error) { errs <- err }
		words := c.pieces(history, sampling)
		if err := wait(ctx, c.o.Figures.FirstToken); err != nil {
			fail(err)
			return
		}
		for i, w := range words {
			if i > 0 {
				if err := wait(ctx, c.o.Figures.TokenDelay+c.o.ExtraDelay); err != nil {
					fail(err)
					return
				}
			}
			if !send(google.LLMStreamChunk{Text: w}) {
				fail(ctx.Err())
				return
			}
		}
		send(google.LLMStreamChunk{IsDone: true, FinishReason: "STOP", PromptTokens: promptTokens, OutputTokens: len(words), TotalTokens: promptTokens + len(words)})
	}()
	return chunks, errs, nil
}

// pieces is what the model says, in streaming order: the label, then the answer to a tool result or
// the caller's words repeated; one word per piece, as a model streams.
func (c *LLMClient) pieces(history []core.Message, sampling google.Sampling) []string {
	var parts []string
	if c.o.Label != "" {
		parts = append(parts, c.o.Label)
		if s := samplingLabel(sampling); s != "" { // proves the flow's sampling reached the model call
			parts = append(parts, s)
		}
	}
	if n := len(history); n > 0 && history[n-1].Kind == core.MessageToolResult {
		parts = append(parts, c.toolAnswer(history[n-1]))
	} else {
		parts = append(parts, "You said: ", lastUser(history))
	}
	var out []string
	for _, p := range parts {
		for _, w := range splitWords(p) { // none for an empty part
			out = append(out, capPiece(w)...) // a long unbroken word is still cut
		}
	}
	return out
}

// samplingLabel renders the set parameters, e.g. "{temperature=0.3 top_p=0.9 max_output_tokens=200} ".
func samplingLabel(s google.Sampling) string {
	var b []string
	if s.Temperature != nil {
		b = append(b, "temperature="+strconv.FormatFloat(*s.Temperature, 'g', -1, 64))
	}
	if s.TopP != nil {
		b = append(b, "top_p="+strconv.FormatFloat(*s.TopP, 'g', -1, 64))
	}
	if s.MaxOutputTokens > 0 {
		b = append(b, "max_output_tokens="+strconv.Itoa(s.MaxOutputTokens))
	}
	if len(b) == 0 {
		return ""
	}
	return "{" + strings.Join(b, " ") + "} "
}

// maxPiece bounds one streamed piece, as a real model's tokens are small: one huge delta would make
// the pipeline cut dozens of speech units out of a single frame and trip its per-frame emit budget.
const maxPiece = 120

// capPiece splits s into pieces of at most maxPiece runes, at spaces where it can.
func capPiece(s string) []string {
	var out []string
	for utf8.RuneCountInString(s) > maxPiece {
		r := []rune(s)
		cut := maxPiece
		for i := maxPiece; i > maxPiece/2; i-- {
			if r[i-1] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		s = string(r[cut:])
	}
	return append(out, s)
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
		if m.Content != "" { // the payload carries a tool_error's own message
			why += " " + m.Content
		}
		return "Tool " + spoken + " failed: " + why
	}
	return "Tool " + spoken + " returned " + m.Content
}

// toolCalls is what the user's own last message asks for, unless the model is already answering a
// tool result: every attached MCP tool whose phrase it contains, in the order the phrases appear, each
// with the {json} that follows its phrase (several make one parallel round); otherwise the first other
// phrase (a transfer, end_call) in sorted order, so the choice is deterministic.
// ponytail: offsets assume ToLower keeps byte lengths (ASCII messages); otherwise every call gets the
// message's first {json}.
func (c *LLMClient) toolCalls(history []core.Message, hasTools bool) []core.ToolCall {
	// Only the user's own last message asks for a tool: an older one must not call it again.
	if !hasTools || len(history) == 0 {
		return nil
	}
	if last := history[len(history)-1]; last.Role != core.RoleUser || last.Kind != "" && last.Kind != core.MessageText {
		return nil
	}
	msg := lastUser(history)
	lower := strings.ToLower(msg)
	type hit struct {
		start, at int // phrase start, and where its {json} begins
		name      string
	}
	var mcp []hit
	for _, p := range c.phrases {
		name := c.o.Phrases[p]
		if _, spoken := c.o.Spoken[name]; !spoken {
			continue
		}
		if at := strings.Index(lower, p); at >= 0 {
			mcp = append(mcp, hit{at, at + len(p), name})
		}
	}
	if len(mcp) > 0 {
		// The longest phrase wins where phrases overlap (`call tool get` inside `call tool get_order`).
		kept := mcp[:0:0]
		for _, h := range mcp {
			covered := false
			for _, o := range mcp {
				if o != h && o.start <= h.start && o.at >= h.at {
					covered = true
					break
				}
			}
			if !covered {
				kept = append(kept, h)
			}
		}
		mcp = kept
		sort.Slice(mcp, func(i, j int) bool {
			if mcp[i].at != mcp[j].at {
				return mcp[i].at < mcp[j].at
			}
			return mcp[i].name < mcp[j].name
		})
		calls := make([]core.ToolCall, 0, len(mcp))
		for _, h := range mcp {
			args := toolArguments(msg)
			if len(lower) == len(msg) {
				args = toolArguments(msg[h.at:])
			}
			calls = append(calls, c.call(h.name, args))
		}
		return calls
	}
	for _, p := range c.phrases {
		if strings.Contains(lower, p) {
			return []core.ToolCall{c.call(c.o.Phrases[p], json.RawMessage(`{}`))}
		}
	}
	return nil
}

func (c *LLMClient) call(name string, args json.RawMessage) core.ToolCall {
	return core.ToolCall{ID: "echo-call-" + strconv.FormatUint(callSeq.Add(1), 10), Name: name, Arguments: args}
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
