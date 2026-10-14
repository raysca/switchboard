package google

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/genai"
	"govoice/core"
)

var errInvalidGoogleTool = errors.New("google: invalid tool data")
var googleToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

const (
	maxGoogleSchemaBytes = 32 << 10
	maxGoogleSchemaDepth = 16
)

// GoogleToolDeclarations translates framework schemas into the current GenAI
// SDK's JSON Schema declaration field. The SDK marshals this value as
// parametersJsonSchema and the returned structure owns a copy of the schema.
func GoogleToolDeclarations(definitions []core.ToolDefinition) ([]*genai.FunctionDeclaration, error) {
	declarations := make([]*genai.FunctionDeclaration, 0, len(definitions))
	seen := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		if !googleToolNamePattern.MatchString(definition.Name) || len(definition.Description) > 4096 || len(definition.InputSchema) == 0 || len(definition.InputSchema) > maxGoogleSchemaBytes || !json.Valid(definition.InputSchema) || !googleJSONDepth(definition.InputSchema, maxGoogleSchemaDepth) {
			return nil, errInvalidGoogleTool
		}
		if _, exists := seen[definition.Name]; exists {
			return nil, errInvalidGoogleTool
		}
		seen[definition.Name] = struct{}{}
		var schema any
		decoder := json.NewDecoder(strings.NewReader(string(definition.InputSchema)))
		decoder.UseNumber()
		if decoder.Decode(&schema) != nil {
			return nil, errInvalidGoogleTool
		}
		object, ok := schema.(map[string]any)
		if !ok || object["type"] != "object" {
			return nil, errInvalidGoogleTool
		}
		declarations = append(declarations, &genai.FunctionDeclaration{
			Name: definition.Name, Description: definition.Description,
			ParametersJsonSchema: cloneGoogleJSON(object),
		})
	}
	return declarations, nil
}

// GoogleToolCallsAt uses a request-wide ordinal so missing provider IDs remain
// unique across streamed response chunks and stable when the same request is
// replayed from its first chunk.
func GoogleToolCallsAt(response *genai.GenerateContentResponse, identity core.ToolCall, streamOrdinal int) ([]core.ToolCall, error) {
	if response == nil {
		return nil, nil
	}
	var calls []*genai.FunctionCall
	for _, candidate := range response.Candidates {
		if candidate == nil || candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if part != nil && part.FunctionCall != nil {
				calls = append(calls, part.FunctionCall)
			}
		}
	}
	result := make([]core.ToolCall, 0, len(calls))
	for index, call := range calls {
		args, err := json.Marshal(call.Args)
		if err != nil {
			return nil, errInvalidGoogleTool
		}
		id := call.ID
		if id == "" {
			seed := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%d\x00%s", response.ResponseID, streamOrdinal+index, identity.SessionID, identity.TurnID, identity.Generation, call.Name)
			digest := sha256.Sum256([]byte(seed + "\x00" + string(args)))
			id = "google-" + hex.EncodeToString(digest[:12])
		}
		result = append(result, core.ToolCall{
			ID: id, Name: call.Name, Arguments: append(json.RawMessage(nil), args...),
			SessionID: identity.SessionID, TurnID: identity.TurnID, Generation: identity.Generation,
		})
	}
	return result, nil
}

// GoogleFunctionResponse creates the SDK-neutral function result envelope.
// Error details are canonical categories and never include handler text.
func GoogleFunctionResponse(call core.ToolCall, result core.ToolResult) (*genai.FunctionResponse, error) {
	if call.ID == "" || call.Name == "" || result.CallID != call.ID {
		return nil, errInvalidGoogleTool
	}
	response := &genai.FunctionResponse{ID: call.ID, Name: call.Name}
	switch result.Status {
	case core.ToolSucceeded:
		value, err := decodeGoogleJSON(result.Result)
		if err != nil {
			return nil, errInvalidGoogleTool
		}
		response.Response = map[string]any{"output": value}
	case core.ToolFailed:
		response.Response = map[string]any{"error": googleSafeCategory(result.ErrorCategory, "tool_failed")}
	case core.ToolCancelled:
		category := googleSafeCategory(result.ErrorCategory, "cancelled")
		response.Response = map[string]any{"error": category}
	default:
		return nil, errInvalidGoogleTool
	}
	return response, nil
}

func googleTextOnly(c *genai.Content) bool {
	for _, p := range c.Parts {
		if p == nil || p.FunctionCall != nil || p.FunctionResponse != nil {
			return false
		}
	}
	return true
}

// GoogleContentsWithNativeExchanges maps history while keeping each completed
// model response attached to the call batch it produced. A later response's
// leading signed/text parts therefore cannot move ahead of an earlier batch's
// tool result.
func GoogleContentsWithNativeExchanges(history []core.Message, exchanges []GoogleNativeExchange) []*genai.Content {
	contents := make([]*genai.Content, 0, len(history)+len(exchanges))
	toolNames := make(map[string]string)
	for _, message := range history {
		if message.Kind == core.MessageToolCall && message.ToolCallID != "" && message.ToolName != "" {
			toolNames[message.ToolCallID] = message.ToolName
		}
	}
	exchangeIndex := 0
	for i := 0; i < len(history); i++ {
		message := history[i]
		if message.Role == core.RoleSystem {
			continue
		}
		if message.Kind == core.MessageToolCall {
			batch := make([]core.Message, 0, 2)
			for i < len(history) && history[i].Kind == core.MessageToolCall {
				batch = append(batch, history[i])
				i++
			}
			i--
			if exchangeIndex < len(exchanges) && googleExchangeMatchesBatch(exchanges[exchangeIndex], batch) {
				exchange := exchanges[exchangeIndex]
				exchangeIndex++
				matched := make(map[string]int, len(batch))
				modelParts := make([]*genai.Part, 0, len(batch))
				for _, native := range exchange.Contents {
					copy := cloneGoogleContent(native)
					if copy == nil {
						continue
					}
					for _, part := range copy.Parts {
						if part != nil && part.FunctionCall != nil {
							matched[part.FunctionCall.ID]++
						}
						if part != nil {
							modelParts = append(modelParts, part)
						}
					}
				}
				for _, call := range batch {
					if matched[call.ToolCallID] > 0 {
						matched[call.ToolCallID]--
						continue
					}
					modelParts = append(modelParts, googleReconstructedCall(call).Parts...)
				}
				contents = append(contents, &genai.Content{Role: "model", Parts: modelParts})
			} else {
				for _, call := range batch {
					contents = append(contents, googleReconstructedCall(call))
				}
			}
			continue
		}
		if message.Kind == core.MessageToolResult || message.Role == core.RoleTool {
			parts := make([]*genai.Part, 0, 2)
			for i < len(history) && (history[i].Kind == core.MessageToolResult || history[i].Role == core.RoleTool) {
				result := history[i]
				response, err := GoogleFunctionResponse(core.ToolCall{ID: result.ToolCallID, Name: toolNames[result.ToolCallID]}, core.ToolResult{CallID: result.ToolCallID, Status: result.ToolStatus, Result: json.RawMessage(result.Content), ErrorCategory: result.ToolErrorCategory})
				if err != nil {
					response = &genai.FunctionResponse{ID: result.ToolCallID, Name: toolNames[result.ToolCallID], Response: map[string]any{"error": "invalid_result"}}
				}
				parts = append(parts, &genai.Part{FunctionResponse: response})
				i++
			}
			contents = append(contents, &genai.Content{Role: "user", Parts: parts})
			i--
			continue
		}
		role := "user"
		if message.Role == core.RoleAssistant {
			role = "model"
		}
		// Consecutive same-role text (e.g. user fragments the turn-completion
		// protocol held without a reply) joins one Content: Gemini expects
		// user/model turns to alternate.
		if n := len(contents); n > 0 && contents[n-1].Role == role && googleTextOnly(contents[n-1]) {
			contents[n-1].Parts = append(contents[n-1].Parts, &genai.Part{Text: message.Content})
			continue
		}
		contents = append(contents, &genai.Content{Role: role, Parts: []*genai.Part{{Text: message.Content}}})
	}
	// Gemini expects the conversation to start with a user turn: a call that opens with the agent's own
	// greeting would send the model turn first.
	if len(contents) > 0 && contents[0].Role == "model" && googleTextOnly(contents[0]) {
		contents = append([]*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "[The call has just connected.]"}}}}, contents...)
	}
	return contents
}

func googleExchangeMatchesBatch(exchange GoogleNativeExchange, batch []core.Message) bool {
	if len(exchange.CallIDs) != len(batch) {
		return false
	}
	for i := range batch {
		if exchange.CallIDs[i] != batch[i].ToolCallID {
			return false
		}
	}
	return true
}

func googleReconstructedCall(call core.Message) *genai.Content {
	var args map[string]any
	if json.Unmarshal([]byte(call.Content), &args) != nil || args == nil {
		args = map[string]any{}
	}
	return &genai.Content{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: call.ToolCallID, Name: call.ToolName, Args: args}}}}
}

func decodeGoogleJSON(raw []byte) (any, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, errInvalidGoogleTool
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errInvalidGoogleTool
	}
	return value, nil
}

func cloneGoogleContent(content *genai.Content) *genai.Content {
	if content == nil {
		return nil
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return nil
	}
	var cloned genai.Content
	if json.Unmarshal(encoded, &cloned) != nil {
		return nil
	}
	return &cloned
}

func googleNativeContents(response *genai.GenerateContentResponse, calls []core.ToolCall) []*genai.Content {
	if response == nil {
		return nil
	}
	contents := make([]*genai.Content, 0, len(response.Candidates))
	callIndex := 0
	for _, candidate := range response.Candidates {
		if candidate == nil || candidate.Content == nil {
			continue
		}
		content := cloneGoogleContent(candidate.Content)
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil || part.FunctionCall == nil {
				continue
			}
			if part.FunctionCall.ID == "" && callIndex < len(calls) {
				part.FunctionCall.ID = calls[callIndex].ID
			}
			callIndex++
		}
		contents = append(contents, content)
	}
	return contents
}

func googleJSONDepth(raw []byte, maximum int) bool {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	var walk func(any, int) bool
	walk = func(item any, depth int) bool {
		if depth > maximum {
			return false
		}
		switch typed := item.(type) {
		case map[string]any:
			for _, child := range typed {
				if !walk(child, depth+1) {
					return false
				}
			}
		case []any:
			for _, child := range typed {
				if !walk(child, depth+1) {
					return false
				}
			}
		}
		return true
	}
	return walk(value, 0)
}

func cloneGoogleJSON(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	var copied any
	_ = decoder.Decode(&copied)
	return copied
}

func cloneGoogleToolDefinitions(definitions []core.ToolDefinition) []core.ToolDefinition {
	cloned := make([]core.ToolDefinition, len(definitions))
	copy(cloned, definitions)
	for i := range cloned {
		cloned[i].InputSchema = append(json.RawMessage(nil), definitions[i].InputSchema...)
	}
	return cloned
}

func googleSafeCategory(category, fallback string) string {
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
