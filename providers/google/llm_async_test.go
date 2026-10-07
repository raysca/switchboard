package google

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/tools"
)

// The processor's ToolLoop is session-lived: an async call acknowledged in one
// generation survives a barge-in and a second generation, its result is handed
// to the sink and injected after both, and Close cancels a call still running.
func TestGoogleAsyncToolOutlivesGenerationAndCloseCancels(t *testing.T) {
	history := core.NewConversationContext(core.ContextConfig{})
	client := &googleToolClient{responses: [][]LLMStreamChunk{
		{{ToolCalls: []core.ToolCall{{ID: "a", Name: "slow", Arguments: json.RawMessage(`{}`)}}}},
		{{Text: "On it."}},
		{{ToolCalls: []core.ToolCall{{ID: "b", Name: "slow", Arguments: json.RawMessage(`{}`)}}}},
		{{Text: "ok"}},
	}}
	release, cancelled := make(chan struct{}), make(chan string, 2)
	sunk := make(chan core.AsyncToolResult, 2)
	cfg := tools.DefaultToolLoopConfig()
	cfg.AsyncSink = func(r core.AsyncToolResult) { sunk <- r }
	proc, err := NewLLMProcessor(LLMConfig{
		Client:          client,
		ToolDefinitions: []core.ToolDefinition{{Name: "slow", InputSchema: json.RawMessage(`{"type":"object"}`), Policy: core.ToolPolicy{Async: true}}},
		ToolExecutor: core.ToolExecutorFunc(func(ctx context.Context, call core.ToolCall) (core.ToolResult, error) {
			if call.ID == "a" {
				<-release
				return core.ToolResult{Status: core.ToolSucceeded, Result: json.RawMessage(`{"answer":42}`)}, nil
			}
			<-ctx.Done()
			cancelled <- call.ID
			return core.ToolResult{}, ctx.Err()
		}),
		ToolHistory:     history,
		ToolLoopConfig:  cfg,
		ContextProvider: history.Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := core.NewPipeline(&core.ContextAggregator{Context: history}, proc)
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(1, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	texts := make(chan string, 4)
	session.OnOutput = func(f core.Frame) {
		if v, ok := f.(*core.LLMTextFrame); ok {
			texts <- v.Text
		}
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-texts:
			if got != want {
				t.Fatalf("text %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no %q", want)
		}
	}
	if err := session.Post(&core.UserMessageFrame{Turn: "t1", Gen: session.CurrentGeneration(), Content: "run it"}); err != nil {
		t.Fatal(err)
	}
	expect("On it.")
	gen := session.IncrementGeneration() // barge-in: the async call must survive it
	if err := session.Post(&core.UserMessageFrame{Turn: "t2", Gen: gen, Content: "and another"}); err != nil {
		t.Fatal(err)
	}
	expect("ok")
	close(release)
	select {
	case r := <-sunk:
		if r.Call.ID != "a" || !strings.Contains(r.Message.Content, `"answer":42`) {
			t.Fatalf("sunk %+v", r)
		}
		if err := r.Inject(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("async result never reached the sink")
	}
	msgs := history.Messages()
	if last := msgs[len(msgs)-1]; last.Metadata["async_tool_call_id"] != "a" {
		t.Fatalf("last message %+v, want the envelope", last)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-cancelled:
		if id != "b" {
			t.Fatalf("cancelled %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel the running async call")
	}
	if len(sunk) != 0 {
		t.Fatal("a cancelled call was delivered after Close")
	}
	if n := session.LeakedTasks(); n != 0 {
		t.Fatalf("leaked %d tasks", n)
	}
}
