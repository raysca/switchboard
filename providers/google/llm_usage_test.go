package google

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"govoice/core"
)

// A tool-loop turn makes several model requests; its usage is their sum.
func TestGoogleLLMToolLoopUsageSumsEveryRequest(t *testing.T) {
	history := core.NewConversationContext(core.ContextConfig{})
	client := &googleToolClient{responses: [][]LLMStreamChunk{
		{{ToolCalls: []core.ToolCall{{ID: "c1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}, PromptTokens: 10, OutputTokens: 3, TotalTokens: 13}},
		{{Text: "do", PromptTokens: 20, OutputTokens: 1, TotalTokens: 21}, {Text: "ne", PromptTokens: 20, OutputTokens: 5, TotalTokens: 25}},
	}}
	proc, err := NewLLMProcessor(LLMConfig{
		Client:          client,
		ToolDefinitions: []core.ToolDefinition{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolExecutor: core.ToolExecutorFunc(func(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
			return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{}`)}, nil
		}),
		ToolHistory:     history,
		ContextProvider: history.Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipe, _ := core.NewPipeline(&core.ContextAggregator{Context: history}, proc)
	session, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(session, 1)
	usage := make(chan *core.UsageMetricsFrame, 1)
	session.OnOutput = func(f core.Frame) {
		if u, ok := f.(*core.UsageMetricsFrame); ok {
			usage <- u
		}
	}
	_ = session.Start()
	defer session.Close()
	_ = session.Post(&core.UserMessageFrame{Turn: "turn-1", Gen: 1, Content: "find"})

	select {
	case u := <-usage:
		if *u.PromptTokens != 30 || *u.CompletionTokens != 8 || *u.TotalTokens != 38 || *u.Requests != 2 {
			t.Fatalf("usage = prompt %d, output %d, total %d, requests %d; want 30, 8, 38, 2", *u.PromptTokens, *u.CompletionTokens, *u.TotalTokens, *u.Requests)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no usage frame")
	}
}
