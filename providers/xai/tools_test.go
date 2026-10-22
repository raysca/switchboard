package xai

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

	"govoice/core"
	"govoice/core/resilience"
	coretools "govoice/core/tools"
)

func TestMapToolDefinitionsCopiesJSONSchemaAndMapsRequired(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)
	got, err := MapToolDefinitions([]core.ToolDefinition{{Name: "weather", Description: "Find weather", InputSchema: schema}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "function" || got[0].Name != "weather" || got[0].Description != "Find weather" || string(got[0].Parameters) != string(schema) {
		t.Fatalf("unexpected mapping: %+v", got)
	}
	got[0].Parameters[0] = ' '
	if schema[0] != '{' {
		t.Fatal("provider schema aliases application memory")
	}
}

func TestNewProcessorRequiresLoopAndCopiesToolDefinitions(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	definitions := []core.ToolDefinition{{Name: "weather", InputSchema: schema}}
	if _, err := NewProcessor(Config{APIKey: "xai-test-key", ToolDefinitions: definitions}); err == nil {
		t.Fatal("definitions without shared ToolLoop accepted")
	}
	loop, err := coretools.NewToolLoop(definitions, core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) { return core.ToolResult{}, nil }), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	proc, err := NewProcessor(Config{APIKey: "xai-test-key", ToolDefinitions: definitions, ToolLoop: loop})
	if err != nil {
		t.Fatal(err)
	}
	schema[0] = ' '
	if proc.cfg.ToolDefinitions[0].InputSchema[0] != '{' {
		t.Fatal("processor config aliases caller's schema bytes")
	}
}

func TestParseXAIToolCallsKeepsOrderAndMalformedPayload(t *testing.T) {
	items := []XAIResponseOutputItem{
		{Type: "function_call", CallID: "one", Name: "a", Arguments: `{"n":1}`},
		{Type: "function_call", CallID: "two", Name: "b", Arguments: `{bad`},
	}
	calls := ParseXAIToolCalls(items, "session", "turn", 7)
	if len(calls) != 2 || calls[0].ID != "one" || calls[0].Name != "a" || calls[1].ID != "two" || string(calls[0].Arguments) != `{"n":1}` || string(calls[1].Arguments) != `{bad` {
		t.Fatalf("calls were not preserved for ToolLoop validation: %+v", calls)
	}
	if calls[0].SessionID != core.SessionID("session") || calls[0].TurnID != core.TurnID("turn") || calls[0].Generation != 7 {
		t.Fatalf("identity mismatch: %+v", calls[0])
	}
}

func TestParseXAIToolCallRequiresProviderCallID(t *testing.T) {
	calls := ParseXAIToolCalls([]XAIResponseOutputItem{{Type: "function_call", ID: "fc-item", Name: "x", Arguments: `{}`}}, "s", "t", 1)
	if len(calls) != 1 || calls[0].ID != "" {
		t.Fatalf("item id must not be mistaken for Responses call_id: %+v", calls)
	}
}

func TestMergeXAIOutputItemDeduplicatesExactEventReplayAndRejectsConflict(t *testing.T) {
	item := XAIResponseOutputItem{Type: "function_call", ID: "fc1", CallID: "call1", Name: "lookup", Arguments: `{}`}
	items, err := mergeOutputItem(nil, item)
	if err != nil {
		t.Fatal(err)
	}
	items, err = mergeOutputItem(items, item)
	if err != nil || len(items) != 1 {
		t.Fatalf("exact replay was not idempotent: items=%v err=%v", items, err)
	}
	conflict := item
	conflict.Arguments = `{"x":1}`
	if _, err := mergeOutputItem(items, conflict); err == nil {
		t.Fatal("conflicting call identity accepted")
	}
}

func TestParseXAIToolCallsDoesNotAliasProviderOutput(t *testing.T) {
	items := []XAIResponseOutputItem{{Type: "function_call", CallID: "one", Name: "a", Arguments: `{"x":1}`}}
	calls := ParseXAIToolCalls(items, "s", "t", 1)
	items[0].Arguments = "{}"
	if reflect.DeepEqual(calls[0].Arguments, json.RawMessage(`{"x":1}`)) == false {
		t.Fatalf("call arguments alias provider output: %s", calls[0].Arguments)
	}
}

func TestMapToolDefinitionsRejectsInvalidDeepAndOversizedSchemas(t *testing.T) {
	for name, schema := range map[string][]byte{
		"invalid":         []byte(`{`),
		"non-object-root": []byte(`{"type":"string"}`),
		"oversized":       append([]byte(`{"type":"object","description":"`), append(make([]byte, 32<<10), []byte(`"}`)...)...),
		"too-deep":        []byte(`{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"object","properties":{"c":{"type":"object","properties":{"d":{"type":"object","properties":{"e":{"type":"object","properties":{"f":{"type":"object","properties":{"g":{"type":"object","properties":{"h":{"type":"object","properties":{"i":{"type":"object","properties":{"j":{"type":"object","properties":{"k":{"type":"object","properties":{"l":{"type":"object","properties":{"m":{"type":"object","properties":{"n":{"type":"object","properties":{"o":{"type":"object","properties":{"p":{"type":"object","properties":{"q":{"type":"string"}}}}}}}}}}}}}}}}}}}}}}}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := MapToolDefinitions([]core.ToolDefinition{{Name: "bad", InputSchema: schema}}); err == nil {
				t.Fatal("invalid schema accepted")
			}
		})
	}
}

func TestMapToolDefinitionsSupportsObjectUnionsAndToolLimit(t *testing.T) {
	union := json.RawMessage(`{"oneOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"integer"}}}]}`)
	if _, err := MapToolDefinitions([]core.ToolDefinition{{Name: "union", InputSchema: union}}); err != nil {
		t.Fatalf("object union rejected: %v", err)
	}
	// This task transports schemas and bounds their representation; it does not
	// locally evaluate required/property conformance for provider arguments.
	if _, err := MapToolDefinitions([]core.ToolDefinition{{Name: "unvalidated", InputSchema: json.RawMessage(`{"type":"object","required":["not-declared"],"properties":{}}`)}}); err != nil {
		t.Fatalf("nested schema was locally validated: %v", err)
	}
	defs := make([]core.ToolDefinition, 350)
	for i := range defs {
		defs[i] = core.ToolDefinition{Name: fmt.Sprintf("tool-%d", i), InputSchema: json.RawMessage(`{"type":"object"}`)}
	}
	if _, err := MapToolDefinitions(defs); err != nil {
		t.Fatalf("350 tools rejected: %v", err)
	}
	defs = append(defs, core.ToolDefinition{Name: "tool-extra", InputSchema: json.RawMessage(`{"type":"object"}`)})
	if _, err := MapToolDefinitions(defs); err == nil {
		t.Fatal("tool count above xAI cap accepted")
	}
}

func TestXAIToolResultOutputPreservesStatusAndSanitizesCategory(t *testing.T) {
	cases := []struct {
		name   string
		result core.ToolResult
		want   string
	}{
		{"success", core.ToolResult{CallID: "c", Status: core.ToolSucceeded, Result: json.RawMessage(`{"ok":true}`)}, `{"output":{"ok":true}}`},
		{"failure", core.ToolResult{CallID: "c", Status: core.ToolFailed, ErrorCategory: "denied", Result: json.RawMessage(`{"data":"partial"}`)}, `{"error":"denied"}`},
		{"cancelled", core.ToolResult{CallID: "c", Status: core.ToolCancelled, ErrorCategory: "private detail", Result: json.RawMessage(`{"ok":true}`)}, `{"error":"cancelled"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := XAIToolResultOutput(tc.result)
			if err != nil || got != tc.want {
				t.Fatalf("got=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

func TestXAIToolResultOutputPreservesLargeNumbersAndRejectsInvalidJSON(t *testing.T) {
	got, err := XAIToolResultOutput(core.ToolResult{Status: core.ToolSucceeded, Result: json.RawMessage(`{"id":9007199254740993}`)})
	if err != nil || got != `{"output":{"id":9007199254740993}}` {
		t.Fatalf("numeric result changed: output=%s err=%v", got, err)
	}
	for _, raw := range []string{`null`, `42`, `"text"`, `[1,9007199254740993]`} {
		got, err := XAIToolResultOutput(core.ToolResult{Status: core.ToolSucceeded, Result: json.RawMessage(raw)})
		if err != nil || !strings.Contains(got, raw) {
			t.Fatalf("result %s was not preserved: output=%s err=%v", raw, got, err)
		}
	}
	if _, err := XAIToolResultOutput(core.ToolResult{Status: core.ToolSucceeded, Result: json.RawMessage(`{"invalid":`)}); err == nil {
		t.Fatal("malformed successful result accepted")
	}
}

func TestXAIToolResultOutputHonorsConfiguredToolLoopResultBound(t *testing.T) {
	payload, err := json.Marshal(strings.Repeat("x", 70<<10))
	if err != nil {
		t.Fatal(err)
	}
	ctx := core.NewConversationContext(core.ContextConfig{MaxTokens: 1000000})
	cfg := coretools.DefaultToolLoopConfig()
	cfg.MaxResultBytes = 128 << 10
	loop, err := coretools.NewToolLoop([]core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) {
		return core.ToolResult{Result: append(json.RawMessage(nil), payload...)}, nil
	}), ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Run(context.Background(), []core.ToolCall{{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`), SessionID: "s", TurnID: "t", Generation: 1}}, func(_ context.Context, results []core.ToolResult) ([]core.ToolCall, bool, error) {
		if len(results) != 1 || results[0].Status != core.ToolSucceeded {
			t.Fatalf("configured result bound was not honored by ToolLoop: %+v", results)
		}
		wire, err := XAIToolResultOutput(results[0])
		if err == nil && !strings.Contains(wire, string(payload)) {
			t.Fatalf("provider continuation changed %d-byte result", len(payload))
		}
		return nil, true, err
	})
	if err != nil {
		t.Fatalf("accepted %d-byte result failed provider mapping: %v", len(payload), err)
	}
	items := xaiHistoryItems(ctx.Messages())
	if len(items) != 2 {
		t.Fatalf("history did not retain tool call and result: items=%d", len(items))
	}
	if items[1].Output == "" || !strings.Contains(items[1].Output, string(payload)) {
		t.Fatalf("history did not preserve configured-size result: output-bytes=%d", len(items[1].Output))
	}
}

func TestXAIStreamCorrelatesCallItemAndIndexAliases(t *testing.T) {
	wire := `data: {"type":"response.function_call_arguments.delta","output_index":7,"item_id":"fc1","delta":"{\"x\":"}

data: {"type":"response.output_item.added","output_index":7,"item":{"type":"function_call","id":"fc1","name":"lookup","arguments":"","status":"in_progress"}}

data: {"type":"response.function_call_arguments.delta","item_id":"fc1","delta":"1}"}

data: {"type":"response.output_item.done","output_index":7,"item":{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","status":"completed"}}

data: {"type":"response.completed","response":{"output":[{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","status":"completed"}]}}

data: [DONE]

`
	calls, err := runXAIToolStream(t, wire)
	if err != nil || len(calls) != 1 || calls[0].ID != "c1" || string(calls[0].Arguments) != `{"x":1}` {
		t.Fatalf("aliases failed to correlate: calls=%+v err=%v", calls, err)
	}
}

func TestXAIStreamRejectsConflictingArgumentsDoneAndCompleted(t *testing.T) {
	wire := `data: {"type":"response.function_call_arguments.done","item_id":"fc1","arguments":"{\"x\":1}"}

data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","arguments":"","status":"in_progress"}}

data: {"type":"response.output_item.done","item":{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","arguments":"{\"x\":2}","status":"completed"}}

data: [DONE]

`
	if _, err := runXAIToolStream(t, wire); err == nil {
		t.Fatal("conflicting completed arguments accepted")
	}
}

func TestXAIStreamRejectsArgumentsDoneVsCompletedConflict(t *testing.T) {
	wire := `data: {"type":"response.function_call_arguments.done","item_id":"fc1","arguments":"{\"x\":1}"}

data: {"type":"response.completed","response":{"output":[{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","arguments":"{\"x\":2}","status":"completed"}]}}

data: [DONE]

`
	if _, err := runXAIToolStream(t, wire); err == nil {
		t.Fatal("completed output conflicting with arguments.done accepted")
	}
}

func TestXAIStreamDuplicateOutputItemsRemainSingleCall(t *testing.T) {
	item := `{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","arguments":"{}","status":"completed"}`
	wire := `data: {"type":"response.output_item.added","output_index":0,"item":` + item + `}

data: {"type":"response.output_item.done","output_index":0,"item":` + item + `}

data: {"type":"response.output_item.done","output_index":0,"item":` + item + `}

data: [DONE]

`
	calls, err := runXAIToolStream(t, wire)
	if err != nil || len(calls) != 1 {
		t.Fatalf("duplicate output items changed call count: calls=%+v err=%v", calls, err)
	}
}

func runXAIToolStream(t *testing.T, wire string) ([]core.ToolCall, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, wire)
	}))
	defer srv.Close()
	proc, err := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: srv.URL, HTTPClient: srv.Client(), RetryPolicy: resilience.Policy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	session := newMockSessionController(1, 1)
	if err = proc.Attach(session, 0); err != nil {
		t.Fatal(err)
	}
	proc.activeGen = 1
	var text strings.Builder
	calls, _, _, _, err := proc.requestRound(context.Background(), context.Background(), responsesRequest{}, 1, "t", session, &text)
	return coreCalls(calls), err
}

func coreCalls(items []XAIResponseOutputItem) []core.ToolCall {
	return ParseXAIToolCalls(items, "s", "t", 1)
}

func TestXAIStreamingLifecycleAssemblesArgumentsWithoutSpeakingThem(t *testing.T) {
	wire := `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","arguments":"","status":"in_progress"}}

data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc1","delta":"{"}

data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc1","delta":"}"}

data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc1","arguments":"{}"}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","arguments":"{}","status":"completed"}}

data: {"type":"response.reasoning_text.delta","delta":"private reasoning"}

data: {"delta":"private untyped payload"}

data: {"type":"response.completed","response":{"output":[{"type":"function_call","id":"fc1","call_id":"c1","name":"lookup","arguments":"{}","status":"completed"}]}}

data: [DONE]

`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, wire)
	}))
	defer srv.Close()
	proc, err := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: srv.URL, HTTPClient: srv.Client(), RetryPolicy: resilience.Policy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	session := newMockSessionController(1, 1)
	_ = proc.Attach(session, 0)
	proc.activeGen = 1
	var text strings.Builder
	calls, _, _, _, err := proc.requestRound(context.Background(), context.Background(), responsesRequest{}, 1, "t", session, &text)
	if err != nil || len(calls) != 1 || calls[0].Arguments != "{}" {
		t.Fatalf("calls=%+v err=%v", calls, err)
	}
	if text.Len() != 0 || len(session.reentered) != 0 {
		t.Fatalf("non-text payload reached assistant: text=%q frames=%d", text.String(), len(session.reentered))
	}
}

func TestXAIHistoryUsesResponsesFunctionCallItems(t *testing.T) {
	history := []core.Message{
		{Role: core.RoleAssistant, Kind: core.MessageToolCall, Content: `{"city":"London"}`, ToolCallID: "c1", ToolName: "weather"},
		{Role: core.RoleTool, Kind: core.MessageToolResult, Content: `{"ok":true}`, ToolCallID: "c1", ToolStatus: core.ToolSucceeded},
		{Role: core.RoleAssistant, Kind: core.MessageToolCall, Content: `{"city":"Paris"}`, ToolCallID: "c2", ToolName: "weather"},
		{Role: core.RoleTool, Kind: core.MessageToolResult, Content: `{"data":"partial"}`, ToolCallID: "c2", ToolStatus: core.ToolFailed, ToolErrorCategory: "denied"},
		{Role: core.RoleUser, Content: "continue"},
	}
	items := xaiHistoryItems(history)
	if len(items) != 5 || items[0].Type != "function_call" || items[0].CallID != "c1" || items[0].Name != "weather" || items[0].Arguments != `{"city":"London"}` || items[1].Type != "function_call_output" || items[1].CallID != "c1" || items[1].Output != `{"output":{"ok":true}}` || items[2].CallID != "c2" || items[3].Output != `{"error":"denied"}` || items[4].Content != "continue" {
		t.Fatalf("history not kept in native Responses form: %+v", items)
	}
}

func TestXAIHistoryPreservesLargeResultNumber(t *testing.T) {
	history := []core.Message{{Role: core.RoleTool, Kind: core.MessageToolResult, Content: `{"id":9007199254740993}`, ToolCallID: "c1", ToolStatus: core.ToolSucceeded}}
	items := xaiHistoryItems(history)
	if len(items) != 1 || items[0].Output != `{"output":{"id":9007199254740993}}` {
		t.Fatalf("historical result number changed: %+v", items)
	}
}

func TestXAIResponsesToolLoopExecutesOnceAndContinues(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), body...))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\\\"London\\\"}\",\"extra\":{\"opaque\":true}}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_2\",\"type\":\"function_call\",\"call_id\":\"call_2\",\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Checking weather\"}]},{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\\\"London\\\"}\",\"extra\":{\"opaque\":true}},{\"id\":\"fc_2\",\"type\":\"function_call\",\"call_id\":\"call_2\",\"name\":\"weather\",\"arguments\":\"{\\\"city\\\":\\\"Paris\\\"}\"}]}}\n\n")
		} else {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"It is sunny.\"}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"output_text\":\"It is sunny.\"}}\n\n")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	ctx := core.NewConversationContext(core.ContextConfig{})
	var executes []string
	defs := []core.ToolDefinition{{Name: "weather", Description: "Get weather", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}}
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
		executes = append(executes, call.ID)
		if call.ID == "call_2" {
			return core.ToolResult{}, errors.New("private handler detail")
		}
		var args struct {
			City string `json:"city"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			t.Fatal(err)
		}
		return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{"weather":"sunny","city":"` + args.City + `","id":9007199254740993}`)}, nil
	}), ctx, coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	proc, err := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: server.URL, Model: "grok-test", HTTPClient: server.Client(), ToolDefinitions: defs, ToolLoop: loop})
	if err != nil {
		t.Fatal(err)
	}
	session := newMockSessionController(3, 4)
	if err := proc.Attach(session, 0); err != nil {
		t.Fatal(err)
	}
	if err := proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "turn-1", Gen: 4, Content: "weather"}, core.Downstream, &mockEmitter{}); err != nil {
		t.Fatal(err)
	}
	session.taskWg.Wait()
	if !reflect.DeepEqual(executes, []string{"call_1", "call_2"}) {
		t.Fatalf("provider call order changed or calls duplicated: %v", executes)
	}
	mu.Lock()
	gotBodies := append([][]byte(nil), bodies...)
	mu.Unlock()
	if len(gotBodies) != 2 {
		t.Fatalf("expected initial and continuation request, got %d", len(gotBodies))
	}
	var first, second map[string]any
	if err := json.Unmarshal(gotBodies[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(gotBodies[1], &second); err != nil {
		t.Fatal(err)
	}
	if first["store"] != false {
		t.Fatalf("store must remain false: %s", gotBodies[0])
	}
	tools, ok := first["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tool schema missing: %s", gotBodies[0])
	}
	input := second["input"].([]any)
	if len(input) < 5 {
		t.Fatalf("continuation omitted history/call/output: %s", gotBodies[1])
	}
	callOne, _ := input[len(input)-4].(map[string]any)
	callTwo, _ := input[len(input)-3].(map[string]any)
	resultOne, _ := input[len(input)-2].(map[string]any)
	resultTwo, _ := input[len(input)-1].(map[string]any)
	if callOne["type"] != "function_call" || callOne["call_id"] != "call_1" || callTwo["type"] != "function_call" || callTwo["call_id"] != "call_2" || resultOne["type"] != "function_call_output" || resultOne["call_id"] != "call_1" || !strings.Contains(resultOne["output"].(string), "London") || resultTwo["call_id"] != "call_2" || resultTwo["output"] != `{"error":"executor_error"}` {
		t.Fatalf("bad continuation mapping: %#v %#v %#v %#v", callOne, callTwo, resultOne, resultTwo)
	}
	if opaque, ok := callOne["extra"].(map[string]any); !ok || opaque["opaque"] != true {
		t.Fatalf("Responses item fields were not preserved: %#v", callOne)
	}
	priorMessage, _ := input[1].(map[string]any)
	if priorMessage["id"] != "msg_1" {
		t.Fatalf("stateless continuation dropped prior model output: %#v", input)
	}
	if strings.Contains(string(gotBodies[1]), "private handler detail") {
		t.Fatal("handler error leaked to provider continuation")
	}
	if !strings.Contains(string(gotBodies[1]), `9007199254740993`) {
		t.Fatalf("active continuation rounded result number: %s", gotBodies[1])
	}
	statuses := map[string]core.ToolStatus{}
	for _, message := range ctx.Messages() {
		if message.Role == core.RoleTool {
			statuses[message.ToolCallID] = message.ToolStatus
		}
	}
	if statuses["call_1"] != core.ToolSucceeded || statuses["call_2"] != core.ToolFailed {
		t.Fatalf("tool result statuses not committed: %v", statuses)
	}
	var finalText bool
	session.reenteredMu.Lock()
	for _, f := range session.reentered {
		if frame, ok := f.(*core.LLMTextFrame); ok && frame.Text == "It is sunny." {
			finalText = true
		}
	}
	session.reenteredMu.Unlock()
	if !finalText {
		t.Fatal("final assistant response missing")
	}
}

func TestXAIResponsesToolLoopCancellationDoesNotContinue(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	ctx := core.NewConversationContext(core.ContextConfig{})
	defs := []core.ToolDefinition{{Name: "weather", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) {
		return core.ToolResult{}, errors.New("must not execute")
	}), ctx, coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	proc, _ := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: server.URL, HTTPClient: server.Client(), ToolDefinitions: defs, ToolLoop: loop})
	session := newMockSessionController(1, 1)
	_ = proc.Attach(session, 0)
	_ = proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t", Gen: 1, Content: "x"}, core.Downstream, &mockEmitter{})
	<-started
	_ = proc.ProcessFrame(context.Background(), &core.InterruptionFrame{}, core.Downstream, &mockEmitter{})
	session.taskWg.Wait()
}

func TestXAIResponsesStaleGenerationDoesNotRunTool(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-finish
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"weather\",\"arguments\":\"{}\"}]}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	defs := []core.ToolDefinition{{Name: "weather", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	var executed bool
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) {
		executed = true
		return core.ToolResult{}, nil
	}), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	proc, _ := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: server.URL, HTTPClient: server.Client(), ToolDefinitions: defs, ToolLoop: loop})
	session := newMockSessionController(2, 1)
	_ = proc.Attach(session, 0)
	_ = proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "turn", Gen: 1, Content: "query"}, core.Downstream, &mockEmitter{})
	<-started
	session.IncrementGeneration()
	close(finish)
	session.taskWg.Wait()
	if executed {
		t.Fatal("stale-generation function call executed")
	}
}

func TestXAIProviderFencePreservesApplicationToolFence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"lookup\",\"arguments\":\"{}\"}]}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	defs := []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	var executed bool
	cfg := coretools.DefaultToolLoopConfig()
	cfg.IsCurrent = func(core.ToolCall) bool { return false }
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) {
		executed = true
		return core.ToolResult{}, nil
	}), core.NewConversationContext(core.ContextConfig{}), cfg)
	if err != nil {
		t.Fatal(err)
	}
	proc, err := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: srv.URL, HTTPClient: srv.Client(), ToolDefinitions: defs, ToolLoop: loop})
	if err != nil {
		t.Fatal(err)
	}
	session := newMockSessionController(1, 1)
	_ = proc.Attach(session, 0)
	_ = proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t", Gen: 1, Content: "lookup"}, core.Downstream, &mockEmitter{})
	session.taskWg.Wait()
	if executed {
		t.Fatal("provider fence replaced an application generation rejection")
	}
}

func TestXAIProviderFenceStopsBatchWhenGenerationChangesDuringHandler(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"lookup\",\"arguments\":\"{}\"},{\"type\":\"function_call\",\"call_id\":\"c2\",\"name\":\"lookup\",\"arguments\":\"{}\"}]}}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	defs := []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	session := newMockSessionController(1, 1)
	var runs []string
	history := core.NewConversationContext(core.ContextConfig{})
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
		runs = append(runs, call.ID)
		if call.ID == "c1" {
			session.IncrementGeneration()
		}
		return core.ToolResult{Status: core.ToolSucceeded, Result: json.RawMessage(`{"secret":"old"}`)}, nil
	}), history, coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	proc, err := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: srv.URL, HTTPClient: srv.Client(), ToolDefinitions: defs, ToolLoop: loop})
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Attach(session, 0)
	_ = proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t", Gen: 1, Content: "lookup"}, core.Downstream, &mockEmitter{})
	session.taskWg.Wait()
	if !reflect.DeepEqual(runs, []string{"c1"}) {
		t.Fatalf("stale batch still executed: %v", runs)
	}
	if requests.Load() != 1 {
		t.Fatalf("stale batch continued with provider: %d requests", requests.Load())
	}
	for _, message := range history.Messages() {
		if message.Kind == core.MessageToolResult && message.ToolStatus != core.ToolCancelled {
			t.Fatalf("stale successful result committed: %+v", message)
		}
	}
}

func TestXAIToolLoopFramesReachTheSession(t *testing.T) {
	var n atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n.Add(1) == 1 {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\",\"arguments\":\"{}\"}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\",\"arguments\":\"{}\"}]}}\n\n")
		} else {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Sunny.\"}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"output_text\":\"Sunny.\"}}\n\n")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	defs := []core.ToolDefinition{{Name: "weather", Description: "Get weather", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
		return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{"weather":"sunny"}`)}, nil
	}), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	proc, err := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: server.URL, Model: "grok-test", HTTPClient: server.Client(), ToolDefinitions: defs, ToolLoop: loop})
	if err != nil {
		t.Fatal(err)
	}
	session := newMockSessionController(3, 4)
	if err := proc.Attach(session, 0); err != nil {
		t.Fatal(err)
	}
	if err := proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "turn-1", Gen: 4, Content: "weather"}, core.Downstream, &mockEmitter{}); err != nil {
		t.Fatal(err)
	}
	session.taskWg.Wait()
	var got []string
	session.reenteredMu.Lock()
	for _, f := range session.reentered {
		switch f := f.(type) {
		case *core.ToolCallFrame:
			got = append(got, "call:"+f.Call.ID)
		case *core.ToolResultFrame:
			got = append(got, "result:"+f.Result.CallID+":"+string(f.Result.Status))
		}
	}
	session.reenteredMu.Unlock()
	if strings.Join(got, ",") != "call:call_1,result:call_1:succeeded" {
		t.Fatalf("frames %v", got)
	}
}

// Sampling is part of the request that is re-sent after a tool round, not only the first.
func TestXAISamplingReachesTheSecondToolRoundRequest(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		bodies = append(bodies, m)
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\",\"arguments\":\"{}\"}}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"weather\",\"arguments\":\"{}\"}]}}\n\n")
		} else {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Sunny.\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"output_text\":\"Sunny.\"}}\n\n")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	defs := []core.ToolDefinition{{Name: "weather", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(_ context.Context, c core.ToolCall) (core.ToolResult, error) {
		return core.ToolResult{CallID: c.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{"w":"sunny"}`)}, nil
	}), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	temp, topP := 0.3, 0.9
	proc, err := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: server.URL, Model: "grok-test", HTTPClient: server.Client(), ToolDefinitions: defs, ToolLoop: loop,
		Temperature: &temp, TopP: &topP, MaxOutputTokens: 200})
	if err != nil {
		t.Fatal(err)
	}
	session := newMockSessionController(3, 4)
	if err := proc.Attach(session, 0); err != nil {
		t.Fatal(err)
	}
	if err := proc.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "turn-1", Gen: 4, Content: "weather"}, core.Downstream, &mockEmitter{}); err != nil {
		t.Fatal(err)
	}
	session.taskWg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	for i, b := range bodies {
		if b["temperature"] != 0.3 || b["top_p"] != 0.9 || b["max_output_tokens"] != 200.0 {
			t.Fatalf("request %d lost sampling: %v", i+1, b)
		}
	}
}

// The tool round appends its calls and outputs to the Responses-style Input, so tools must never be
// accepted on a chat/completions endpoint (which sends Messages): the guard keeps that path unreachable.
func TestNewProcessorRejectsToolsOnChatCompletions(t *testing.T) {
	definitions := []core.ToolDefinition{{Name: "weather", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	loop, err := coretools.NewToolLoop(definitions, core.ToolExecutorFunc(func(context.Context, core.ToolCall) (core.ToolResult, error) { return core.ToolResult{}, nil }), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewProcessor(Config{APIKey: "xai-test-key", Endpoint: "https://api.x.ai/v1/chat/completions", ToolDefinitions: definitions, ToolLoop: loop}); err == nil {
		t.Fatal("tool definitions accepted on a chat/completions endpoint")
	}
}
