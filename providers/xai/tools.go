package xai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"govoice/core"
)

var errInvalidToolDefinition = errors.New("xai: invalid tool definition")

var xaiToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// xAIToolDefinition is the Responses API function tool shape.
type XAIToolDefinition struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// MapToolDefinitions converts the provider-neutral contract to xAI's
// Responses API schema while keeping caller-owned JSON isolated.
func MapToolDefinitions(definitions []core.ToolDefinition) ([]XAIToolDefinition, error) {
	if len(definitions) > 350 {
		return nil, fmt.Errorf("%w: too many tools", errInvalidToolDefinition)
	}
	out := make([]XAIToolDefinition, 0, len(definitions))
	seen := make(map[string]struct{}, len(definitions))
	for _, def := range definitions {
		if !xaiToolNamePattern.MatchString(def.Name) || len(def.Description) > 4096 || len(def.InputSchema) == 0 || len(def.InputSchema) > 32<<10 || !json.Valid(def.InputSchema) {
			return nil, fmt.Errorf("%w: invalid name or schema", errInvalidToolDefinition)
		}
		if _, ok := seen[def.Name]; ok {
			return nil, fmt.Errorf("%w: duplicate name", errInvalidToolDefinition)
		}
		seen[def.Name] = struct{}{}
		var root map[string]json.RawMessage
		if err := json.Unmarshal(def.InputSchema, &root); err != nil || !validRootSchema(root) || jsonDepth(def.InputSchema) > 16 {
			return nil, fmt.Errorf("%w: unsupported parameters schema", errInvalidToolDefinition)
		}
		out = append(out, XAIToolDefinition{Type: "function", Name: def.Name, Description: def.Description, Parameters: append(json.RawMessage(nil), def.InputSchema...)})
	}
	return out, nil
}

// XAIToolResultOutput encodes statuses so successful payloads and failures
// remain distinguishable even when a failed handler returned useful JSON.
func XAIToolResultOutput(result core.ToolResult) (string, error) {
	var envelope map[string]any
	switch result.Status {
	case core.ToolSucceeded:
		var value any
		if len(result.Result) == 0 || json.Unmarshal(result.Result, &value) != nil {
			return "", errors.New("xai: invalid successful tool result")
		}
		envelope = map[string]any{"output": value}
	case core.ToolFailed:
		envelope = map[string]any{"error": safeXAIErrorCategory(result.ErrorCategory, "tool_failed")}
	case core.ToolCancelled:
		envelope = map[string]any{"error": safeXAIErrorCategory(result.ErrorCategory, "cancelled")}
	default:
		return "", errors.New("xai: invalid tool result status")
	}
	encoded, err := json.Marshal(envelope)
	return string(encoded), err
}

func safeXAIErrorCategory(category, fallback string) string {
	if len(category) == 0 || len(category) > 64 {
		return fallback
	}
	for _, char := range category {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.') {
			return fallback
		}
	}
	return category
}

func xaiItemRawWithArguments(item XAIResponseOutputItem, arguments string) json.RawMessage {
	if len(item.Raw) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(item.Raw, &fields) != nil {
		return append(json.RawMessage(nil), item.Raw...)
	}
	fields["arguments"], _ = json.Marshal(arguments)
	raw, err := json.Marshal(fields)
	if err != nil {
		return append(json.RawMessage(nil), item.Raw...)
	}
	return raw
}

func validRootSchema(root map[string]json.RawMessage) bool {
	var typ string
	_ = json.Unmarshal(root["type"], &typ)
	if typ == "object" {
		return true
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		var branches []map[string]json.RawMessage
		if err := json.Unmarshal(root[key], &branches); err == nil && len(branches) > 0 {
			valid := true
			for _, branch := range branches {
				var branchType string
				_ = json.Unmarshal(branch["type"], &branchType)
				if branchType != "object" {
					valid = false
					break
				}
			}
			if valid {
				return true
			}
		}
	}
	return false
}

func jsonDepth(raw []byte) int {
	dec := json.NewDecoder(bytes.NewReader(raw))
	depth, maxDepth := 0, 0
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
			if depth > maxDepth {
				maxDepth = depth
			}
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return maxDepth
}

type XAIResponseOutputItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type xaiFunctionCallAssembly struct {
	item      XAIResponseOutputItem
	arguments strings.Builder
}

func (a *xaiFunctionCallAssembly) mergeIdentity(item XAIResponseOutputItem) error {
	if a.item.Type != "" && a.item.Type != item.Type {
		return errors.New("xai: conflicting function call type")
	}
	if a.item.ID != "" && item.ID != "" && a.item.ID != item.ID {
		return errors.New("xai: conflicting function call id")
	}
	if a.item.CallID != "" && item.CallID != "" && a.item.CallID != item.CallID {
		return errors.New("xai: conflicting function call id")
	}
	if a.item.Name != "" && item.Name != "" && a.item.Name != item.Name {
		return errors.New("xai: conflicting function call name")
	}
	if a.item.Arguments != "" && item.Arguments != "" && a.item.Arguments != item.Arguments {
		return errors.New("xai: conflicting function call arguments")
	}
	if item.Arguments != "" && a.arguments.Len() > 0 && a.arguments.String() != item.Arguments {
		return errors.New("xai: assembled function call arguments conflict with final item")
	}
	if item.Type != "" {
		a.item.Type = item.Type
	}
	if item.ID != "" {
		a.item.ID = item.ID
	}
	if item.CallID != "" {
		a.item.CallID = item.CallID
	}
	if item.Name != "" {
		a.item.Name = item.Name
	}
	if item.Arguments != "" {
		a.item.Arguments = item.Arguments
	}
	if len(item.Raw) > 0 {
		a.item.Raw = append(json.RawMessage(nil), item.Raw...)
	}
	return nil
}

func xaiOutputItemKey(item XAIResponseOutputItem, outputIndex int) string {
	if item.CallID != "" {
		return "call:" + item.CallID
	}
	if item.ID != "" {
		return "item:" + item.ID
	}
	return fmt.Sprintf("output:%d", outputIndex)
}

func (item *XAIResponseOutputItem) UnmarshalJSON(data []byte) error {
	type plain XAIResponseOutputItem
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*item = XAIResponseOutputItem(decoded)
	item.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// ParseXAIToolCalls preserves provider ordering and malformed payloads so the
// shared ToolLoop can apply its common identity, JSON, size, and schema policy.
func ParseXAIToolCalls(items []XAIResponseOutputItem, session, turn string, generation uint64) []core.ToolCall {
	calls := make([]core.ToolCall, 0, len(items))
	for _, item := range items {
		if item.Type != "function_call" {
			continue
		}
		calls = append(calls, core.ToolCall{ID: item.CallID, Name: item.Name, Arguments: append(json.RawMessage(nil), item.Arguments...), SessionID: core.SessionID(session), TurnID: core.TurnID(turn), Generation: core.Generation(generation)})
	}
	return calls
}

func (p *Processor) requestRound(ctx, sessionCtx context.Context, req responsesRequest, gen uint64, turn string, s core.SessionController, text *strings.Builder) ([]XAIResponseOutputItem, int, int, int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, 0, 0, fmt.Errorf("marshal xai request: %w", err)
	}
	var committed bool
	var items []XAIResponseOutputItem
	var roundText strings.Builder
	callAssemblies := make(map[string]*xaiFunctionCallAssembly)
	var prompt, output, total int
	err = ExecuteWithRetry(ctx, p.cfg.CircuitBreaker, p.cfg.RetryPolicy, &committed, func(attempt int) error {
		if attempt > 0 {
			p.Retries.Add(1)
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.Endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		if p.cfg.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
		}
		resp, err := p.cfg.HTTPClient.Do(httpReq)
		if err != nil {
			return MapHTTPError("llm", nil, "", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return MapHTTPError("llm", resp, string(b), nil)
		}
		sse := NewSSEReader(resp.Body, p.cfg.MaxLineLength, p.cfg.MaxEventSize, p.cfg.MaxTotalSize)
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-sessionCtx.Done():
				return sessionCtx.Err()
			default:
			}
			event, err := sse.ReadEvent()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if event == nil || event.Data == "" {
				continue
			}
			var chunk chatChunkResponse
			if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidEvent, err)
			}
			usage := chunk.Usage
			if usage == nil && chunk.Response != nil {
				usage = chunk.Response.Usage
			}
			if usage != nil {
				if usage.PromptTokens > 0 {
					prompt = usage.PromptTokens
				} else if usage.InputTokens > 0 {
					prompt = usage.InputTokens
				}
				if usage.CompletionTokens > 0 {
					output = usage.CompletionTokens
				} else if usage.OutputTokens > 0 {
					output = usage.OutputTokens
				}
				if usage.TotalTokens > 0 {
					total = usage.TotalTokens
				} else {
					total = prompt + output
				}
			}
			if chunk.Type == "response.output_item.added" || chunk.Type == "response.output_item.done" {
				if chunk.Item != nil && chunk.Item.Type == "function_call" {
					key := xaiOutputItemKey(*chunk.Item, chunk.OutputIndex)
					assembly := callAssemblies[key]
					if assembly == nil {
						assembly = &xaiFunctionCallAssembly{}
						callAssemblies[key] = assembly
					}
					if err := assembly.mergeIdentity(*chunk.Item); err != nil {
						return err
					}
					if chunk.Type == "response.output_item.done" {
						completed := assembly.item
						if completed.Arguments == "" {
							completed.Arguments = assembly.arguments.String()
							completed.Raw = xaiItemRawWithArguments(completed, completed.Arguments)
						}
						items, err = mergeOutputItem(items, completed)
						if err != nil {
							return err
						}
						committed = true
					}
				}
			}
			if chunk.Type == "response.function_call_arguments.delta" || chunk.Type == "response.function_call_arguments.done" {
				key := chunk.ItemID
				if key == "" {
					key = fmt.Sprintf("output:%d", chunk.OutputIndex)
				}
				assembly := callAssemblies[key]
				if assembly == nil {
					assembly = &xaiFunctionCallAssembly{}
					callAssemblies[key] = assembly
				}
				if chunk.Type == "response.function_call_arguments.done" {
					assembly.arguments.Reset()
					assembly.arguments.WriteString(chunk.Arguments)
				} else {
					assembly.arguments.WriteString(chunk.Delta)
				}
				committed = true
			}
			if chunk.Response != nil {
				canonical := make([]XAIResponseOutputItem, 0, len(chunk.Response.Output))
				for index, item := range chunk.Response.Output {
					if item.Type == "function_call" {
						key := xaiOutputItemKey(item, index)
						assembly := callAssemblies[key]
						if assembly == nil {
							assembly = &xaiFunctionCallAssembly{}
							callAssemblies[key] = assembly
						}
						if err := assembly.mergeIdentity(item); err != nil {
							return err
						}
						if item.Arguments == "" {
							item.Arguments = assembly.arguments.String()
						}
						if len(item.Raw) > 0 && item.Arguments != "" {
							item.Raw = xaiItemRawWithArguments(item, item.Arguments)
						}
					}
					if _, err = mergeOutputItem(items, item); err != nil {
						return err
					}
					canonical, err = mergeOutputItem(canonical, item)
					if err != nil {
						return err
					}
					if item.Type == "function_call" {
						committed = true
					}
				}
				if len(canonical) > 0 {
					items = canonical
				}
			}
			if chunk.Type == "response.failed" {
				return errors.New("xai responses API returned response.failed")
			}
			if chunk.Type == "response.completed" || chunk.Type == "response.done" {
				if chunk.Response != nil && chunk.Response.OutputText != "" && roundText.Len() == 0 {
					roundText.WriteString(chunk.Response.OutputText)
					text.WriteString(chunk.Response.OutputText)
					if p.isCurrent(gen) {
						_ = s.Reenter(&core.LLMTextDeltaFrame{Delta: chunk.Response.OutputText, Turn: turn, Gen: gen}, core.Downstream, p.index, gen)
					}
				}
				continue
			}
			delta := ""
			if chunk.Type == "response.output_text.delta" || chunk.Type == "response.text.delta" {
				delta = chunk.Delta
				if delta == "" {
					delta = chunk.Text
				}
			}
			if delta == "" && chunk.Type == "" && len(chunk.Choices) > 0 {
				delta = chunk.Choices[0].Delta.Content
			}
			if delta != "" {
				committed = true
				roundText.WriteString(delta)
				text.WriteString(delta)
				if p.isCurrent(gen) {
					_ = s.Reenter(&core.LLMTextDeltaFrame{Delta: delta, Turn: turn, Gen: gen}, core.Downstream, p.index, gen)
				}
			}
		}
		if !sse.SawTerminal() {
			return ErrMissingTerminalEvent
		}
		return nil
	})
	return items, prompt, output, total, err
}

func mergeOutputItem(items []XAIResponseOutputItem, item XAIResponseOutputItem) ([]XAIResponseOutputItem, error) {
	for i, existing := range items {
		if (item.CallID != "" && existing.CallID == item.CallID) || (item.ID != "" && existing.ID == item.ID) {
			if item.Type != existing.Type {
				return nil, errors.New("xai: conflicting output item type")
			}
			if item.Type == "function_call" {
				if (item.ID != "" && existing.ID != "" && item.ID != existing.ID) || (item.CallID != "" && existing.CallID != "" && item.CallID != existing.CallID) || (item.Name != "" && existing.Name != "" && item.Name != existing.Name) || (item.Arguments != "" && existing.Arguments != "" && item.Arguments != existing.Arguments) {
					return nil, fmt.Errorf("xai: conflicting provider tool call identity")
				}
				if item.Arguments != "" || item.Name != "" {
					items[i] = item
				}
			}
			return items, nil
		}
	}
	item.Arguments = strings.Clone(item.Arguments)
	item.Raw = append(json.RawMessage(nil), item.Raw...)
	return append(items, item), nil
}
