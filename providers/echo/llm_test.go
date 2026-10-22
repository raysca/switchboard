package echo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
	"govoice/providers/google"
)

var (
	_ google.LLMStreamClient           = (*LLMClient)(nil)
	_ google.LLMNativeToolStreamClient = (*LLMClient)(nil)
)

func user(text string) []core.Message {
	return []core.Message{{Role: core.RoleUser, Content: text}}
}

func collect(t *testing.T, chunks <-chan google.LLMStreamChunk, errs <-chan error) []google.LLMStreamChunk {
	t.Helper()
	var out []google.LLMStreamChunk
	timeout := time.After(5 * time.Second)
	for chunks != nil || errs != nil {
		select {
		case c, ok := <-chunks:
			if !ok {
				chunks = nil
				continue
			}
			out = append(out, c)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				t.Fatalf("stream error: %v", err)
			}
		case <-timeout:
			t.Fatal("stream did not finish")
		}
	}
	return out
}

func text(chunks []google.LLMStreamChunk) (deltas []string, joined string) {
	for _, c := range chunks {
		if c.Text != "" {
			deltas = append(deltas, c.Text)
			joined += c.Text
		}
	}
	return
}

func TestEchoReplyRepeatsTheCallerWordByWord(t *testing.T) {
	c := NewLLMClient(LLMOptions{})
	ch, errs, err := c.GenerateStream(context.Background(), "m", "sys", user("hi there"), google.Sampling{})
	if err != nil {
		t.Fatal(err)
	}
	chunks := collect(t, ch, errs)
	deltas, joined := text(chunks)
	if joined != "You said: hi there" || len(deltas) != 4 || deltas[1] != "said: " {
		t.Fatalf("deltas=%q joined=%q", deltas, joined)
	}
	last := chunks[len(chunks)-1]
	if !last.IsDone || last.FinishReason != "STOP" || last.OutputTokens != 4 || last.PromptTokens == 0 || last.TotalTokens != last.PromptTokens+last.OutputTokens {
		t.Fatalf("closing chunk %+v", last)
	}
}

func TestEchoLabelPrefixesTheReply(t *testing.T) {
	c := NewLLMClient(LLMOptions{Label: "[front] "})
	ch, errs, _ := c.GenerateStream(context.Background(), "m", "", user("hi"), google.Sampling{})
	if _, joined := text(collect(t, ch, errs)); joined != "[front] You said: hi" {
		t.Fatalf("reply %q", joined)
	}
}

func TestEchoLabelShowsTheSamplingItWasGiven(t *testing.T) {
	temp, topP := 0.3, 0.9
	c := NewLLMClient(LLMOptions{Label: "[front] "})
	for name, want := range map[string]struct {
		s    google.Sampling
		said string
	}{
		"all":   {google.Sampling{Temperature: &temp, TopP: &topP, MaxOutputTokens: 200}, "[front] {temperature=0.3 top_p=0.9 max_output_tokens=200} You said: hi"},
		"zero":  {google.Sampling{Temperature: new(float64)}, "[front] {temperature=0} You said: hi"},
		"unset": {google.Sampling{}, "[front] You said: hi"},
	} {
		ch, errs, _ := c.GenerateStream(context.Background(), "m", "", user("hi"), want.s)
		if _, joined := text(collect(t, ch, errs)); joined != want.said {
			t.Errorf("%s: reply %q, want %q", name, joined, want.said)
		}
	}
	// No label, no sampling text: the unlabelled reply is unchanged.
	ch, errs, _ := NewLLMClient(LLMOptions{}).GenerateStream(context.Background(), "m", "", user("hi"), google.Sampling{MaxOutputTokens: 5})
	if _, joined := text(collect(t, ch, errs)); joined != "You said: hi" {
		t.Errorf("unlabelled reply %q", joined)
	}
}

func TestEchoFirstTokenAndWordDelays(t *testing.T) {
	c := NewLLMClient(LLMOptions{Figures: LLMFigures{FirstToken: 50 * time.Millisecond, TokenDelay: 10 * time.Millisecond}, ExtraDelay: 5 * time.Millisecond})
	start := time.Now()
	ch, errs, _ := c.GenerateStream(context.Background(), "m", "", user("a b"), google.Sampling{})
	first := <-ch
	if first.Text == "" || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("first word after %v: %+v", time.Since(start), first)
	}
	mark := time.Now()
	second := <-ch
	if second.Text == "" || time.Since(mark) < 15*time.Millisecond {
		t.Fatalf("second word after %v, want >= TokenDelay+ExtraDelay", time.Since(mark))
	}
	collect(t, ch, errs)
}

func TestEchoStopsOnContextCancel(t *testing.T) {
	c := NewLLMClient(LLMOptions{Figures: LLMFigures{TokenDelay: 200 * time.Millisecond}})
	ctx, cancel := context.WithCancel(context.Background())
	ch, errs, _ := c.GenerateStream(ctx, "m", "", user("a b c d e f"), google.Sampling{})
	<-ch
	cancel()
	deadline := time.After(2 * time.Second)
	for ch != nil || errs != nil {
		select {
		case _, ok := <-ch:
			if !ok {
				ch = nil
			}
		case _, ok := <-errs:
			if !ok {
				errs = nil
			}
		case <-deadline:
			t.Fatal("channels not closed after cancel")
		}
	}
}

var tools = []core.ToolDefinition{{Name: "transfer_to_sales", InputSchema: json.RawMessage(`{"type":"object"}`)}}

func TestEchoCallsTheToolNamedByAPhrase(t *testing.T) {
	c := NewLLMClient(LLMOptions{Phrases: map[string]string{"transfer to sales": "transfer_to_sales"}})
	ch, errs, _ := c.GenerateStreamWithNativeTools(context.Background(), "m", "", user("Please TRANSFER to sales now"), nil, tools, core.ToolCall{}, google.Sampling{})
	var calls []core.ToolCall
	for _, ck := range collect(t, ch, errs) {
		calls = append(calls, ck.ToolCalls...)
	}
	if len(calls) != 1 || calls[0].Name != "transfer_to_sales" || calls[0].ID == "" || string(calls[0].Arguments) != "{}" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestEchoPassesJSONArgumentsToSpokenTools(t *testing.T) {
	c := NewLLMClient(LLMOptions{Phrases: map[string]string{"call tool lookup_order": "mcp__x_lookup_order"}, Spoken: map[string]string{"mcp__x_lookup_order": "lookup_order"}})
	defs := []core.ToolDefinition{{Name: "mcp__x_lookup_order", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	args := func(msg string) string {
		ch, errs, _ := c.GenerateStreamWithNativeTools(context.Background(), "m", "", user(msg), nil, defs, core.ToolCall{}, google.Sampling{})
		for _, ck := range collect(t, ch, errs) {
			if len(ck.ToolCalls) > 0 {
				return string(ck.ToolCalls[0].Arguments)
			}
		}
		return "no call"
	}
	if got := args(`call tool lookup_order {"id":"7"}`); got != `{"id":"7"}` {
		t.Fatalf("args %s", got)
	}
	if got := args(`call tool lookup_order {not json}`); got != `{}` {
		t.Fatalf("malformed args %s", got)
	}
}

func TestEchoAnswersAfterToolResults(t *testing.T) {
	c := NewLLMClient(LLMOptions{Spoken: map[string]string{"mcp__x_lookup_order": "lookup_order"}})
	reply := func(m core.Message) string {
		h := append(user("go"), m)
		ch, errs, _ := c.GenerateStreamWithNativeTools(context.Background(), "m", "", h, nil, tools, core.ToolCall{}, google.Sampling{})
		_, joined := text(collect(t, ch, errs))
		return joined
	}
	if got := reply(core.Message{Role: core.RoleTool, Kind: core.MessageToolResult, ToolName: "transfer_to_sales", ToolStatus: core.ToolSucceeded, Content: "{}"}); got != "Connecting you." {
		t.Fatalf("transfer answer %q", got)
	}
	if got := reply(core.Message{Role: core.RoleTool, Kind: core.MessageToolResult, ToolName: "mcp__x_lookup_order", ToolStatus: core.ToolSucceeded, Content: `{"ok":true}`}); got != `Tool lookup_order returned {"ok":true}` {
		t.Fatalf("spoken success %q", got)
	}
	if got := reply(core.Message{Role: core.RoleTool, Kind: core.MessageToolResult, ToolName: "mcp__x_lookup_order", ToolStatus: core.ToolFailed, ToolErrorCategory: "timeout"}); got != "Tool lookup_order failed: timeout" {
		t.Fatalf("spoken failure %q", got)
	}
}

func TestEchoIgnoresPhrasesWithoutToolDefinitions(t *testing.T) {
	c := NewLLMClient(LLMOptions{Phrases: map[string]string{"transfer to sales": "transfer_to_sales"}})
	ch, errs, _ := c.GenerateStream(context.Background(), "m", "", user("transfer to sales"), google.Sampling{})
	for _, ck := range collect(t, ch, errs) {
		if len(ck.ToolCalls) != 0 {
			t.Fatalf("tool call without definitions: %+v", ck)
		}
	}
}

func TestRealLLMProcessorRunsTheToolLoopAgainstEcho(t *testing.T) {
	history := core.NewConversationContext(core.ContextConfig{})
	var ran atomic.Int32
	cfg := google.DefaultLLMConfig()
	cfg.Provider = "echo"
	cfg.Client = NewLLMClient(LLMOptions{Phrases: map[string]string{"transfer to sales": "transfer_to_sales"}})
	cfg.ToolDefinitions = tools
	cfg.ToolExecutor = core.ToolExecutorFunc(func(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
		ran.Add(1)
		return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{}`)}, nil
	})
	cfg.ToolHistory = history
	cfg.ContextProvider = history.Messages
	proc, err := google.NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	sess, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(sess, 0)
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := history.AddMessage(core.Message{Role: core.RoleUser, Content: "please transfer to sales", TurnID: "turn-1"}); err != nil {
		t.Fatal(err)
	}
	_ = sess.Post(&core.UserMessageFrame{Turn: "turn-1", Gen: 1, Content: "please transfer to sales"})
	var said string
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && !strings.Contains(said, "Connecting you."); time.Sleep(10 * time.Millisecond) {
		said = ""
		for _, f := range sink.GetFrames() {
			if v, ok := f.(*core.LLMTextDeltaFrame); ok {
				said += v.Delta
			}
		}
	}
	if ran.Load() != 1 || said != "Connecting you." {
		t.Fatalf("tool ran %d times, said %q", ran.Load(), said)
	}
}

func TestEchoNeverStreamsOneHugePiece(t *testing.T) {
	long := strings.Repeat("hello ", 500)
	c := NewLLMClient(LLMOptions{})
	ch, errs, _ := c.GenerateStream(context.Background(), "m", "", user(long), google.Sampling{})
	deltas, joined := text(collect(t, ch, errs))
	if joined != "You said: "+long {
		t.Fatalf("pieces must concatenate back to the reply")
	}
	for _, d := range deltas {
		if len([]rune(d)) > maxPiece {
			t.Fatalf("a piece of %d runes", len([]rune(d)))
		}
	}
}

func TestEchoCallIDsAreUniqueAcrossClients(t *testing.T) {
	// Every step of a call builds its own client but shares one tool ledger: a repeated ID with a
	// different call is a "conflicting tool call correlation" in the real tool loop.
	id := func() string {
		c := NewLLMClient(LLMOptions{Phrases: map[string]string{"go": "t"}})
		ch, errs, _ := c.GenerateStreamWithNativeTools(context.Background(), "m", "", user("go"), nil, tools, core.ToolCall{}, google.Sampling{})
		for _, ck := range collect(t, ch, errs) {
			if len(ck.ToolCalls) > 0 {
				return ck.ToolCalls[0].ID
			}
		}
		return ""
	}
	if a, b := id(), id(); a == "" || a == b {
		t.Fatalf("call ids %q and %q must differ across clients", a, b)
	}
}

func TestEchoNeverStreamsOneHugeWord(t *testing.T) {
	long := strings.Repeat("x", 5000) // typed text with no spaces, in the word-streaming mimic
	c := NewLLMClient(LLMOptions{})
	ch, errs, _ := c.GenerateStream(context.Background(), "m", "", user(long), google.Sampling{})
	deltas, joined := text(collect(t, ch, errs))
	if joined != "You said: "+long {
		t.Fatal("pieces must concatenate back to the reply")
	}
	for _, d := range deltas {
		if len([]rune(d)) > maxPiece {
			t.Fatalf("a piece of %d runes", len([]rune(d)))
		}
	}
}

// A cancelled or timed-out request must reach the processor as an error: both channels closing is a
// finished reply to it, which would turn a timeout into a silent empty answer.
func TestEchoReportsACancelledRequestOnTheErrorChannel(t *testing.T) {
	c := NewLLMClient(LLMOptions{Figures: LLMFigures{FirstToken: 5 * time.Second}})
	ctx, cancel := context.WithCancel(context.Background())
	ch, errs, _ := c.GenerateStream(ctx, "m", "", user("hi"), google.Sampling{})
	time.AfterFunc(20*time.Millisecond, cancel)
	var got error
	deadline := time.After(3 * time.Second)
	for ch != nil || errs != nil {
		select {
		case _, ok := <-ch:
			if !ok {
				ch = nil
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else {
				got = err
			}
		case <-deadline:
			t.Fatal("channels not closed")
		}
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("error channel carried %v, want context.Canceled", got)
	}
}

func TestEchoOnlyCallsAToolForTheUsersOwnLastMessage(t *testing.T) {
	c := NewLLMClient(LLMOptions{Phrases: map[string]string{"transfer to sales": "transfer_to_sales"}})
	history := append(user("please transfer to sales"), core.Message{Role: core.RoleAssistant, Content: "Sure."})
	ch, errs, _ := c.GenerateStreamWithNativeTools(context.Background(), "m", "", history, nil, tools, core.ToolCall{}, google.Sampling{})
	for _, ck := range collect(t, ch, errs) {
		if len(ck.ToolCalls) != 0 {
			t.Fatalf("an older user message triggered a call: %+v", ck.ToolCalls)
		}
	}
}

func TestEchoNeverStreamsAnEmptyPiece(t *testing.T) {
	c := NewLLMClient(LLMOptions{})
	ch, errs, _ := c.GenerateStream(context.Background(), "m", "", user(""), google.Sampling{})
	for _, ck := range collect(t, ch, errs) {
		if ck.Text == "" && !ck.IsDone {
			t.Fatalf("empty piece %+v", ck)
		}
	}
}

func TestEchoReportsACancelledToolRequestToo(t *testing.T) {
	c := NewLLMClient(LLMOptions{Phrases: map[string]string{"transfer to sales": "transfer_to_sales"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch, errs, _ := c.GenerateStreamWithNativeTools(ctx, "m", "", user("transfer to sales"), nil, tools, core.ToolCall{}, google.Sampling{})
	var got error
	for ch != nil || errs != nil {
		select {
		case ck, ok := <-ch:
			if !ok {
				ch = nil
			} else if len(ck.ToolCalls) != 0 {
				t.Fatal("a cancelled request must not emit a tool call")
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else {
				got = err
			}
		case <-time.After(2 * time.Second):
			t.Fatal("channels not closed")
		}
	}
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("error channel carried %v", got)
	}
}

func TestEchoCallsEveryMCPToolItsMessageNames(t *testing.T) {
	c := NewLLMClient(LLMOptions{
		Phrases: map[string]string{"call tool lookup_order": "mcp__x_lookup_order", "call tool echo": "mcp__x_echo", "transfer to sales": "transfer_to_sales"},
		Spoken:  map[string]string{"mcp__x_lookup_order": "lookup_order", "mcp__x_echo": "echo"},
	})
	defs := []core.ToolDefinition{{Name: "mcp__x_lookup_order", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	ch, errs, _ := c.GenerateStreamWithNativeTools(context.Background(), "m", "", user(`call tool lookup_order {"order_id":"42"} then call tool echo {"text":"hi"} and transfer to sales`), nil, defs, core.ToolCall{}, google.Sampling{})
	var calls []core.ToolCall
	for _, ck := range collect(t, ch, errs) {
		calls = append(calls, ck.ToolCalls...)
	}
	if len(calls) != 2 || calls[0].Name != "mcp__x_lookup_order" || string(calls[0].Arguments) != `{"order_id":"42"}` ||
		calls[1].Name != "mcp__x_echo" || string(calls[1].Arguments) != `{"text":"hi"}` || calls[0].ID == calls[1].ID {
		t.Fatalf("calls %+v", calls)
	}
}

func TestRealLLMProcessorReportsToolFramesBeforeItsAnswer(t *testing.T) {
	history := core.NewConversationContext(core.ContextConfig{})
	cfg := google.DefaultLLMConfig()
	cfg.Provider = "echo"
	cfg.Client = NewLLMClient(LLMOptions{Phrases: map[string]string{"transfer to sales": "transfer_to_sales"}})
	cfg.ToolDefinitions = tools
	cfg.ToolExecutor = core.ToolExecutorFunc(func(_ context.Context, call core.ToolCall) (core.ToolResult, error) {
		return core.ToolResult{CallID: call.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{}`)}, nil
	})
	cfg.ToolHistory = history
	cfg.ContextProvider = history.Messages
	proc, err := google.NewLLMProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(proc, sink)
	sess, _ := core.NewSession(1, pipe, core.DefaultResourceBudget())
	_ = proc.Attach(sess, 0)
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := history.AddMessage(core.Message{Role: core.RoleUser, Content: "please transfer to sales", TurnID: "turn-1"}); err != nil {
		t.Fatal(err)
	}
	_ = sess.Post(&core.UserMessageFrame{Turn: "turn-1", Gen: 1, Content: "please transfer to sales"})
	var got []string
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		got = got[:0]
		for _, f := range sink.GetFrames() {
			switch v := f.(type) {
			case *core.ToolCallFrame:
				got = append(got, "call:"+v.Call.Name)
			case *core.ToolResultFrame:
				got = append(got, "result:"+string(v.Result.Status))
			case *core.LLMTextFrame:
				got = append(got, "text")
			}
		}
		if len(got) >= 3 {
			break
		}
	}
	if strings.Join(got, ",") != "call:transfer_to_sales,result:succeeded,text" {
		t.Fatalf("frames %v", got)
	}
}

// MCP tools sharing a prefix: the longer phrase owns its position, so one phrase makes one call.
func TestEchoLongestToolPhraseWins(t *testing.T) {
	c := NewLLMClient(LLMOptions{
		Phrases: map[string]string{"call tool get": "mcp__x_get", "call tool get_order": "mcp__x_get_order"},
		Spoken:  map[string]string{"mcp__x_get": "get", "mcp__x_get_order": "get_order"},
	})
	defs := []core.ToolDefinition{{Name: "mcp__x_get", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	for i := 0; i < 20; i++ { // map order must not matter
		ch, errs, _ := c.GenerateStreamWithNativeTools(context.Background(), "m", "", user(`call tool get_order {"id":"7"}`), nil, defs, core.ToolCall{}, google.Sampling{})
		var calls []core.ToolCall
		for _, ck := range collect(t, ch, errs) {
			calls = append(calls, ck.ToolCalls...)
		}
		if len(calls) != 1 || calls[0].Name != "mcp__x_get_order" || string(calls[0].Arguments) != `{"id":"7"}` {
			t.Fatalf("calls %+v", calls)
		}
	}
}

// A panic in the stream goroutine is reported as an error, never a crash or a
// silent empty reply.
func TestStreamPanicIsReportedAsError(t *testing.T) {
	var c *LLMClient // a nil client panics inside the goroutine
	chunks, errs, err := c.GenerateStream(t.Context(), "", "", nil, google.Sampling{})
	if err != nil {
		t.Fatal(err)
	}
	for range chunks {
	}
	if e := <-errs; e == nil {
		t.Fatal("want a stream error")
	}
}
