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

// GoogleToolCalls converts every SDK function call in candidate/part order.
// Calls without provider IDs receive a deterministic response-scoped ID; core
// fills the session/turn/generation identity before handing them to ToolLoop.
func GoogleToolCalls(response *genai.GenerateContentResponse, identity core.ToolCall) ([]core.ToolCall, error) {
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
			seed := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%d", response.ResponseID, index, identity.TurnID, call.Name, identity.Generation)
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
		var value any
		if len(result.Result) == 0 || !json.Valid(result.Result) || json.Unmarshal(result.Result, &value) != nil {
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

// GoogleContents encodes framework history using the SDK's FunctionCall and
// FunctionResponse parts so a follow-up model request can continue the loop.
func GoogleContents(history []core.Message) []*genai.Content {
	contents := make([]*genai.Content, 0, len(history))
	toolNames := make(map[string]string)
	for _, message := range history {
		if message.Kind == core.MessageToolCall && message.ToolCallID != "" && message.ToolName != "" {
			toolNames[message.ToolCallID] = message.ToolName
		}
	}
	for i := 0; i < len(history); i++ {
		message := history[i]
		if message.Role == core.RoleSystem {
			continue
		}
		if message.Kind == core.MessageToolCall {
			parts := make([]*genai.Part, 0, 2)
			for i < len(history) && history[i].Kind == core.MessageToolCall {
				call := history[i]
				var args map[string]any
				if json.Unmarshal([]byte(call.Content), &args) != nil {
					args = map[string]any{}
				}
				parts = append(parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: call.ToolCallID, Name: call.ToolName, Args: args}})
				i++
			}
			contents = append(contents, &genai.Content{Role: "model", Parts: parts})
			i--
			continue
		}
		if message.Kind == core.MessageToolResult || message.Role == core.RoleTool {
			parts := make([]*genai.Part, 0, 2)
			for i < len(history) && (history[i].Kind == core.MessageToolResult || history[i].Role == core.RoleTool) {
				result := history[i]
				var payload map[string]any
				if json.Unmarshal([]byte(result.Content), &payload) != nil {
					payload = map[string]any{"error": "invalid_result"}
				}
				if result.ToolStatus == core.ToolSucceeded {
					payload = map[string]any{"output": payload}
				} else {
					payload = map[string]any{"error": googleSafeCategory(result.ToolErrorCategory, "tool_failed")}
				}
				parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: result.ToolCallID, Name: toolNames[result.ToolCallID], Response: payload}})
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
		contents = append(contents, &genai.Content{Role: role, Parts: []*genai.Part{{Text: message.Content}}})
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
