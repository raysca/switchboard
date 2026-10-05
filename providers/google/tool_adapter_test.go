package google

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"
	"govoice/core"
)

func TestGoogleToolAdapterMapsSchemasAndCopiesBytes(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"},"count":{"type":"integer"}},"required":["q"]}`)
	defs := []core.ToolDefinition{{Name: "search", Description: "Find things", InputSchema: schema}}
	got, err := GoogleToolDeclarations(defs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "search" || got[0].Description != "Find things" {
		t.Fatalf("declaration = %#v", got)
	}
	encoded, err := json.Marshal(got[0].ParametersJsonSchema)
	if err != nil {
		t.Fatal(err)
	}
	var gotSchema, wantSchema any
	if err := json.Unmarshal(encoded, &gotSchema); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(schema, &wantSchema); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotSchema, wantSchema) {
		t.Fatalf("schema = %s", encoded)
	}
	defs[0].InputSchema = json.RawMessage(strings.Replace(string(schema), `"string"`, `"number"`, 1))
	encoded, _ = json.Marshal(got[0].ParametersJsonSchema)
	if !strings.Contains(string(encoded), `"string"`) || strings.Contains(string(encoded), `"number"`) {
		t.Fatal("provider schema retained caller-owned bytes")
	}
}

func TestGoogleToolAdapterParsesOrderedCallsWithStableIdentity(t *testing.T) {
	resp := &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{
		{FunctionCall: &genai.FunctionCall{ID: "provider-1", Name: "search", Args: map[string]any{"q": "one"}}},
		{FunctionCall: &genai.FunctionCall{Name: "lookup", Args: map[string]any{"id": json.Number("42")}}},
	}}}}}
	calls, err := GoogleToolCalls(resp, core.ToolCall{SessionID: "s", TurnID: "t", Generation: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].ID != "provider-1" || calls[0].Name != "search" || calls[1].Name != "lookup" {
		t.Fatalf("calls = %#v", calls)
	}
	if string(calls[0].Arguments) != `{"q":"one"}` || string(calls[1].Arguments) != `{"id":42}` {
		t.Fatalf("args = %s / %s", calls[0].Arguments, calls[1].Arguments)
	}
	if calls[1].ID == "" || calls[1].ID == calls[0].ID {
		t.Fatalf("generated ID = %q", calls[1].ID)
	}
}

func TestGoogleToolAdapterRejectsInvalidOrOversizedDefinitions(t *testing.T) {
	for _, def := range []core.ToolDefinition{
		{Name: "bad", InputSchema: json.RawMessage(`{`)},
		{Name: "deep", InputSchema: json.RawMessage(`{"a":{"b":{"c":{"d":{"e":{"f":{"g":{"h":{"i":{"j":{"k":{"l":{"m":{"n":{"o":{"p":{"q":1}}}}}}}}}}}}}}}}}`)},
		{Name: "large", InputSchema: json.RawMessage(`{"type":"object","description":"` + strings.Repeat("x", 33<<10) + `"}`)},
	} {
		if _, err := GoogleToolDeclarations([]core.ToolDefinition{def}); err == nil {
			t.Fatalf("accepted invalid schema %q", def.Name)
		}
	}
}

func TestGoogleToolAdapterMapsResultStatusWithoutLeakingError(t *testing.T) {
	call := core.ToolCall{ID: "id", Name: "search"}
	for _, tc := range []struct {
		status core.ToolStatus
		want   map[string]any
	}{
		{core.ToolSucceeded, map[string]any{"output": map[string]any{"ok": true}}},
		{core.ToolFailed, map[string]any{"error": "tool_failed"}},
		{core.ToolCancelled, map[string]any{"error": "cancelled"}},
	} {
		got, err := GoogleFunctionResponse(call, core.ToolResult{CallID: "id", Status: tc.status, Result: json.RawMessage(`{"ok":true}`), ErrorCategory: "secret token"})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != "id" || got.Name != "search" {
			t.Fatalf("identity = %#v", got)
		}
		encoded, _ := json.Marshal(got.Response)
		want, _ := json.Marshal(tc.want)
		if string(encoded) != string(want) {
			t.Fatalf("response = %s; want %s", encoded, want)
		}
	}
}

func TestGoogleToolHistoryUsesFunctionParts(t *testing.T) {
	history := []core.Message{
		{Role: core.RoleAssistant, Kind: core.MessageToolCall, Content: `{"q":"one"}`, ToolCallID: "call-1", ToolName: "search"},
		{Role: core.RoleTool, Kind: core.MessageToolResult, Content: `{"ok":true}`, ToolCallID: "call-1", ToolStatus: core.ToolSucceeded},
	}
	contents := GoogleContents(history)
	if len(contents) != 2 || contents[0].Role != "model" || contents[0].Parts[0].FunctionCall == nil || contents[1].Role != "user" || contents[1].Parts[0].FunctionResponse == nil {
		t.Fatalf("contents = %#v", contents)
	}
	response := contents[1].Parts[0].FunctionResponse
	if response.Name != "search" || response.Response["output"] == nil {
		t.Fatalf("function result = %#v", response)
	}
}

type googleToolClient struct {
	mu            sync.Mutex
	requests      [][]core.Message
	responses     [][]LLMStreamChunk
	requestErrors []error
}

func (c *googleToolClient) GenerateStream(ctx context.Context, model, prompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
	return c.GenerateStreamWithTools(ctx, model, prompt, history, nil)
}

func (c *googleToolClient) GenerateStreamWithTools(_ context.Context, _, _ string, history []core.Message, _ []core.ToolDefinition) (<-chan LLMStreamChunk, <-chan error, error) {
	c.mu.Lock()
	i := len(c.requests)
	c.requests = append(c.requests, append([]core.Message(nil), history...))
	if i < len(c.requestErrors) && c.requestErrors[i] != nil {
		err := c.requestErrors[i]
		c.mu.Unlock()
		return nil, nil, err
	}
	var response []LLMStreamChunk
	if i < len(c.responses) {
		response = c.responses[i]
	}
	c.mu.Unlock()
	chunks := make(chan LLMStreamChunk, len(response))
	errCh := make(chan error)
	for _, chunk := range response {
		chunks <- chunk
	}
	close(chunks)
	close(errCh)
	return chunks, errCh, nil
}

func TestGoogleLLMToolLoopClaimsOnceAndContinuesWithFunctionHistory(t *testing.T) {
	history := core.NewConversationContext(core.ContextConfig{})
	client := &googleToolClient{responses: [][]LLMStreamChunk{
		{{ToolCalls: []core.ToolCall{
			{ID: "provider-id", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)},
			{ID: "provider-id-2", Name: "lookup", Arguments: json.RawMessage(`{"key":"y"}`)},
		}}},
		nil,
		{{Text: "done"}},
	}, requestErrors: []error{nil, &ProviderError{Capability: "llm", BaseErr: ErrProviderUnavailable, StatusCode: 503, Retryable: true}}}
	var executed []string
	proc, err := NewLLMProcessor(LLMConfig{
		Client:          client,
		RetryPolicy:     RetryPolicy{InitialBackoff: time.Millisecond, MaxAttempts: 2},
		ToolDefinitions: []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}}}`)}},
		ToolExecutor: core.ToolExecutorFunc(func(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
			executed = append(executed, call.ID)
			return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{"value":1}`)}, nil
		}),
		ToolHistory:     history,
		ContextProvider: history.Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	aggregator := &core.ContextAggregator{Context: history}
	pipe, err := core.NewPipeline(aggregator, proc)
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Attach(session, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	outputErrors := make(chan error, 2)
	session.OnOutput = func(frame core.Frame) {
		if result, ok := frame.(*core.LLMTextFrame); ok && result.Text == "done" {
			close(done)
		}
		if result, ok := frame.(*core.ErrorFrame); ok {
			outputErrors <- result.Err
		}
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Post(&core.UserMessageFrame{Turn: "turn-1", Gen: 1, Content: "find"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		client.mu.Lock()
		requestCount := len(client.requests)
		client.mu.Unlock()
		select {
		case got := <-outputErrors:
			t.Fatalf("tool continuation did not finish: requests=%d retries=%d error=%v", requestCount, proc.Retries.Load(), got)
		default:
			t.Fatalf("tool continuation did not finish: requests=%d", requestCount)
		}
	}
	if !reflect.DeepEqual(executed, []string{"provider-id", "provider-id-2"}) {
		t.Fatalf("execution order = %v", executed)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.requests) != 3 {
		t.Fatalf("provider attempts = %d, want first call + failed continuation + retry", len(client.requests))
	}
	contents := GoogleContents(client.requests[2])
	foundCall, foundResult := false, false
	for _, content := range contents {
		for _, part := range content.Parts {
			foundCall = foundCall || part.FunctionCall != nil && part.FunctionCall.ID == "provider-id"
			foundResult = foundResult || part.FunctionResponse != nil && part.FunctionResponse.ID == "provider-id" && part.FunctionResponse.Response["output"] != nil
		}
	}
	if !foundCall || !foundResult {
		t.Fatalf("continuation history omitted call/result: %#v", contents)
	}
	if len(contents) != 3 || len(contents[1].Parts) != 2 || len(contents[2].Parts) != 2 {
		t.Fatalf("parallel calls/results were not kept in ordered Google content groups: %#v", contents)
	}
}

func TestGoogleLLMToolReplayDoesNotExecuteTwice(t *testing.T) {
	history := core.NewConversationContext(core.ContextConfig{})
	call := core.ToolCall{ID: "replayed", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}
	client := &googleToolClient{responses: [][]LLMStreamChunk{
		{{ToolCalls: []core.ToolCall{call}}},
		{{ToolCalls: []core.ToolCall{call}}},
		{{Text: "done"}},
	}}
	var executed int
	proc, err := NewLLMProcessor(LLMConfig{
		Client:          client,
		ToolDefinitions: []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolExecutor: core.ToolExecutorFunc(func(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
			executed++
			return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{"value":1}`)}, nil
		}),
		ToolHistory: history, ContextProvider: history.Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(&core.ContextAggregator{Context: history}, proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 1)
	done := make(chan struct{})
	session.OnOutput = func(frame core.Frame) {
		if result, ok := frame.(*core.LLMTextFrame); ok && result.Text == "done" {
			close(done)
		}
	}
	_ = session.Start()
	defer session.Close()
	_ = session.Post(&core.UserMessageFrame{Turn: "turn-replay", Gen: 1, Content: "find"})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("replay continuation did not finish")
	}
	if executed != 1 {
		t.Fatalf("replayed executor runs = %d", executed)
	}
}

func TestGoogleLLMInterruptionCancelsToolAndFencesContinuation(t *testing.T) {
	history := core.NewConversationContext(core.ContextConfig{})
	client := &googleToolClient{responses: [][]LLMStreamChunk{{
		{ToolCalls: []core.ToolCall{{ID: "slow", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}}},
	}}}
	started, canceled := make(chan struct{}), make(chan struct{})
	proc, err := NewLLMProcessor(LLMConfig{
		Client:          client,
		ToolDefinitions: []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolExecutor: core.ToolExecutorFunc(func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return core.ToolResult{}, ctx.Err()
		}),
		ToolHistory: history, ContextProvider: history.Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(&core.ContextAggregator{Context: history}, proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 1)
	_ = session.Start()
	defer session.Close()
	_ = session.Post(&core.UserMessageFrame{Turn: "turn-cancel", Gen: 1, Content: "find"})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("tool did not start")
	}
	session.IncrementGeneration()
	_ = session.Post(&core.InterruptionFrame{Turn: "turn-cancel", Gen: 2})
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("tool was not canceled")
	}
	time.Sleep(25 * time.Millisecond)
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.requests) != 1 {
		t.Fatalf("stale generation started %d continuation requests", len(client.requests)-1)
	}
}
