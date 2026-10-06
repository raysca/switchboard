package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"
	"govoice/core"
	"govoice/core/tools"
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
	calls, err := GoogleToolCallsAt(resp, core.ToolCall{SessionID: "s", TurnID: "t", Generation: 3}, 0)
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
	contents := GoogleContentsWithNativeExchanges(history, nil)
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
	native        [][]GoogleNativeExchange
	responses     [][]LLMStreamChunk
	requestErrors []error
}

func (c *googleToolClient) GenerateStream(ctx context.Context, model, prompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
	return c.GenerateStreamWithNativeTools(ctx, model, prompt, history, nil, nil, core.ToolCall{})
}

func (c *googleToolClient) GenerateStreamWithNativeTools(_ context.Context, _, _ string, history []core.Message, native []GoogleNativeExchange, _ []core.ToolDefinition, _ core.ToolCall) (<-chan LLMStreamChunk, <-chan error, error) {
	c.mu.Lock()
	i := len(c.requests)
	c.requests = append(c.requests, append([]core.Message(nil), history...))
	c.native = append(c.native, append([]GoogleNativeExchange(nil), native...))
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
	contents := GoogleContentsWithNativeExchanges(client.requests[2], client.native[2])
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

func TestGoogleToolSuccessPayloadPreservesArbitraryJSON(t *testing.T) {
	for _, raw := range []string{`[1,2]`, `"ok"`, `123`, `{"id":9007199254740993}`} {
		t.Run(raw, func(t *testing.T) {
			history := []core.Message{
				{Role: core.RoleAssistant, Kind: core.MessageToolCall, Content: `{}`, ToolCallID: "c", ToolName: "lookup"},
				{Role: core.RoleTool, Kind: core.MessageToolResult, Content: raw, ToolCallID: "c", ToolStatus: core.ToolSucceeded},
			}
			contents := GoogleContentsWithNativeExchanges(history, nil)
			payload, err := json.Marshal(contents[1].Parts[0].FunctionResponse.Response["output"])
			if err != nil {
				t.Fatal(err)
			}
			if string(payload) != raw {
				t.Fatalf("successful payload changed: want=%s got=%s", raw, payload)
			}
			mapped, err := GoogleFunctionResponse(core.ToolCall{ID: "c", Name: "lookup"}, core.ToolResult{CallID: "c", Status: core.ToolSucceeded, Result: json.RawMessage(raw)})
			if err != nil {
				t.Fatal(err)
			}
			publicPayload, err := json.Marshal(mapped.Response["output"])
			if err != nil {
				t.Fatal(err)
			}
			if string(publicPayload) != raw {
				t.Fatalf("public result mapping changed payload: want=%s got=%s", raw, publicPayload)
			}
		})
	}
}

func TestGoogleProcessorOwnsToolDefinitions(t *testing.T) {
	definitions := []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	processor, err := NewLLMProcessor(LLMConfig{ToolDefinitions: definitions, ToolExecutor: core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) { return core.ToolResult{}, nil }), ToolHistory: core.NewConversationContext(core.ContextConfig{})})
	if err != nil {
		t.Fatal(err)
	}
	definitions[0].Name = "changed"
	definitions[0].InputSchema[2] = 'X'
	if processor.cfg.ToolDefinitions[0].Name != "lookup" || string(processor.cfg.ToolDefinitions[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("configuration aliases caller-owned definitions: %+v", processor.cfg.ToolDefinitions)
	}
}

type task12Session struct {
	generation atomic.Uint64
	wg         sync.WaitGroup
}

func (s *task12Session) ID() uint64                { return 1 }
func (s *task12Session) Context() context.Context  { return context.Background() }
func (s *task12Session) CurrentGeneration() uint64 { return s.generation.Load() }
func (s *task12Session) Go(fn func(context.Context)) error {
	s.wg.Add(1)
	go func() { defer s.wg.Done(); fn(context.Background()) }()
	return nil
}
func (s *task12Session) Reenter(core.Frame, core.Direction, int, uint64) error { return nil }
func (s *task12Session) IncrementGeneration() uint64                           { return s.generation.Add(1) }

type task12Emitter struct{}

func (task12Emitter) Push(core.Frame) error                          { return nil }
func (task12Emitter) PushDirection(core.Frame, core.Direction) error { return nil }

type failedStreamClient struct{ requests atomic.Int32 }

func (c *failedStreamClient) GenerateStream(ctx context.Context, model, prompt string, history []core.Message) (<-chan LLMStreamChunk, <-chan error, error) {
	return c.GenerateStreamWithNativeTools(ctx, model, prompt, history, nil, nil, core.ToolCall{})
}
func (c *failedStreamClient) GenerateStreamWithNativeTools(context.Context, string, string, []core.Message, []GoogleNativeExchange, []core.ToolDefinition, core.ToolCall) (<-chan LLMStreamChunk, <-chan error, error) {
	c.requests.Add(1)
	chunks := make(chan LLMStreamChunk, 1)
	errs := make(chan error, 1)
	chunks <- LLMStreamChunk{ToolCalls: []core.ToolCall{{ID: "failed", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}
	errs <- errors.New("stream failed after tool call")
	close(chunks)
	close(errs)
	return chunks, errs, nil
}

func TestGoogleFailedStreamNeverExecutesBufferedToolCalls(t *testing.T) {
	for i := 0; i < 20; i++ {
		client := &failedStreamClient{}
		var executed atomic.Bool
		processor, err := NewLLMProcessor(LLMConfig{Client: client, ToolDefinitions: []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolExecutor: core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) {
			executed.Store(true)
			return core.ToolResult{Result: json.RawMessage(`{}`)}, nil
		}), ToolHistory: core.NewConversationContext(core.ContextConfig{}), RetryPolicy: RetryPolicy{MaxAttempts: 1}})
		if err != nil {
			t.Fatal(err)
		}
		session := &task12Session{}
		session.generation.Store(1)
		_ = processor.Attach(session, 0)
		_ = processor.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "turn", Gen: 1, Content: "hi"}, core.Downstream, task12Emitter{})
		session.wg.Wait()
		if executed.Load() {
			t.Fatalf("tool executed despite terminal stream error at iteration %d", i)
		}
	}
}

func TestGoogleApplicationGenerationPredicateIsComposed(t *testing.T) {
	client := &googleToolClient{responses: [][]LLMStreamChunk{{{ToolCalls: []core.ToolCall{{ID: "predicate", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}}}}
	var executed atomic.Bool
	cfg := LLMConfig{Client: client, ToolDefinitions: []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolExecutor: core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) {
		executed.Store(true)
		return core.ToolResult{Result: json.RawMessage(`{}`)}, nil
	}), ToolHistory: core.NewConversationContext(core.ContextConfig{})}
	cfg.ToolLoopConfig = tools.DefaultToolLoopConfig()
	cfg.ToolLoopConfig.IsCurrent = func(core.ToolCall) bool { return false }
	processor, err := NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	session := &task12Session{}
	session.generation.Store(1)
	_ = processor.Attach(session, 0)
	_ = processor.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "turn", Gen: 1, Content: "hi"}, core.Downstream, task12Emitter{})
	session.wg.Wait()
	client.mu.Lock()
	requests := len(client.requests)
	client.mu.Unlock()
	if executed.Load() || requests != 1 {
		t.Fatalf("caller predicate was ignored: executed=%v requests=%d", executed.Load(), requests)
	}
}

func TestGoogleGenerationFlipDuringToolExecutionStopsContinuation(t *testing.T) {
	client := &googleToolClient{responses: [][]LLMStreamChunk{{{ToolCalls: []core.ToolCall{{ID: "generation", Name: "lookup", Arguments: json.RawMessage(`{}`)}}}}}}
	started, release := make(chan struct{}), make(chan struct{})
	processor, err := NewLLMProcessor(LLMConfig{Client: client, ToolDefinitions: []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolExecutor: core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) {
		close(started)
		<-release
		return core.ToolResult{Result: json.RawMessage(`{}`)}, nil
	}), ToolHistory: core.NewConversationContext(core.ContextConfig{})})
	if err != nil {
		t.Fatal(err)
	}
	session := &task12Session{}
	session.generation.Store(1)
	_ = processor.Attach(session, 0)
	_ = processor.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "turn", Gen: 1, Content: "hi"}, core.Downstream, task12Emitter{})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	session.IncrementGeneration()
	close(release)
	session.wg.Wait()
	client.mu.Lock()
	requests := len(client.requests)
	client.mu.Unlock()
	if requests != 1 {
		t.Fatalf("stale generation issued %d provider continuation requests", requests-1)
	}
}

func TestGoogleNativeToolContinuationPreservesThoughtSignature(t *testing.T) {
	var requests atomic.Int32
	continuationBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			fmt.Fprint(w, "data: {\"responseId\":\"resp-1\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"thoughtSignature\":\"AQID\",\"functionCall\":{\"id\":\"call-1\",\"name\":\"lookup\",\"args\":{}}}]},\"finishReason\":\"STOP\"}] }\n\n")
		} else {
			continuationBody <- body
			fmt.Fprint(w, "data: {\"responseId\":\"resp-2\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"done\"}]},\"finishReason\":\"STOP\"}] }\n\n")
		}
	}))
	defer server.Close()
	sdk, err := genai.NewClient(context.Background(), &genai.ClientConfig{Backend: genai.BackendGeminiAPI, APIKey: "review-key", HTTPClient: server.Client(), HTTPOptions: genai.HTTPOptions{BaseURL: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	processor, err := NewLLMProcessor(LLMConfig{Client: &RealGoogleLLMClient{client: sdk}, ToolDefinitions: []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolExecutor: core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) {
		return core.ToolResult{Result: json.RawMessage(`{}`)}, nil
	}), ToolHistory: core.NewConversationContext(core.ContextConfig{})})
	if err != nil {
		t.Fatal(err)
	}
	session := &task12Session{}
	session.generation.Store(1)
	_ = processor.Attach(session, 0)
	_ = processor.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "turn", Gen: 1, Content: "x"}, core.Downstream, task12Emitter{})
	session.wg.Wait()
	select {
	case body := <-continuationBody:
		if !strings.Contains(string(body), `"thoughtSignature":"AQID"`) {
			t.Fatalf("continuation discarded original native content: %s", body)
		}
	default:
		t.Fatalf("continuation not issued: requests=%d errors=%d", requests.Load(), processor.Errors.Load())
	}
}

func TestGoogleFallbackIDsUseStreamWideOrdinal(t *testing.T) {
	response := &genai.GenerateContentResponse{ResponseID: "response", Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{Name: "lookup", Args: map[string]any{}}}}}}}}
	first, err := GoogleToolCallsAt(response, core.ToolCall{TurnID: "turn", Generation: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GoogleToolCallsAt(response, core.ToolCall{TurnID: "turn", Generation: 1}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].ID == second[0].ID {
		t.Fatalf("separate streamed calls collided on fallback ID %q", first[0].ID)
	}
}

func TestGoogleNativeExchangesStayAfterPriorToolResults(t *testing.T) {
	call1 := core.Message{Role: core.RoleAssistant, Kind: core.MessageToolCall, ToolCallID: "c1", ToolName: "lookup", Content: `{}`}
	result1 := core.Message{Role: core.RoleTool, Kind: core.MessageToolResult, ToolCallID: "c1", ToolName: "lookup", ToolStatus: core.ToolSucceeded, Content: `{}`}
	call2 := core.Message{Role: core.RoleAssistant, Kind: core.MessageToolCall, ToolCallID: "c2", ToolName: "lookup", Content: `{}`}
	result2 := core.Message{Role: core.RoleTool, Kind: core.MessageToolResult, ToolCallID: "c2", ToolName: "lookup", ToolStatus: core.ToolSucceeded, Content: `{}`}
	exchanges := []GoogleNativeExchange{
		{CallIDs: []string{"c1"}, Contents: []*genai.Content{{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "lookup"}, ThoughtSignature: []byte{1}}}}}},
		{CallIDs: []string{"c2"}, Contents: []*genai.Content{
			{Role: "model", Parts: []*genai.Part{{Text: "second-round-prefix", ThoughtSignature: []byte{2}}}},
			{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c2", Name: "lookup"}, ThoughtSignature: []byte{3}}}},
		}},
	}
	got := GoogleContentsWithNativeExchanges([]core.Message{call1, result1, call2, result2}, exchanges)
	result1Index, prefixIndex := -1, -1
	for index, content := range got {
		for _, part := range content.Parts {
			if part.FunctionResponse != nil && part.FunctionResponse.ID == "c1" {
				result1Index = index
			}
			if part.Text == "second-round-prefix" {
				prefixIndex = index
				if len(part.ThoughtSignature) != 1 || part.ThoughtSignature[0] != 2 {
					t.Fatalf("second response signature changed: %#v", part)
				}
			}
		}
	}
	if result1Index < 0 || prefixIndex <= result1Index {
		t.Fatalf("response boundary lost: result1 index=%d prefix index=%d contents=%#v", result1Index, prefixIndex, got)
	}
}
