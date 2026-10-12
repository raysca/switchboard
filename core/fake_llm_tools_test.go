package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type captureTTS struct {
	mu   sync.Mutex
	text []string
}

func (c *captureTTS) ProcessFrame(_ context.Context, f Frame, d Direction, e Emitter) error {
	if t, ok := f.(*LLMTextFrame); ok {
		c.mu.Lock()
		c.text = append(c.text, t.Text)
		c.mu.Unlock()
	}
	return e.PushDirection(f, d)
}
func (c *captureTTS) replies() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.text...)
}

func runFakeLLM(t *testing.T, cfg FakeLLMConfig, say string) *captureTTS {
	t.Helper()
	sink := &captureTTS{}
	pipe, err := NewPipeline(NewFakeLLMProcessor(cfg), sink)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewSession(1, pipe, DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.Reenter(&UserMessageFrame{Turn: "t1", Content: say, Timestamp: time.Now()}, Downstream, 0, 0); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); len(sink.replies()) == 0 && time.Now().Before(end); time.Sleep(time.Millisecond) {
	}
	return sink
}

func TestFakeLLMCallsAToolOnItsPhraseAndSaysAfterTool(t *testing.T) {
	var calls atomic.Int32
	var name atomic.Value
	cfg := FakeLLMConfig{
		Label: "[front] ", ResponseChunks: []string{"You said: "}, EchoUser: true,
		ToolOnPhrase: map[string]string{"transfer to sales": "transfer_to_sales"}, AfterTool: "Connecting you.",
		Executor: ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
			calls.Add(1)
			name.Store(c.Name)
			return ToolResult{CallID: c.ID, Status: ToolSucceeded}, nil
		}),
	}
	got := runFakeLLM(t, cfg, "Please TRANSFER TO SALES now").replies()
	if calls.Load() != 1 || name.Load() != "transfer_to_sales" {
		t.Fatalf("calls=%d name=%v", calls.Load(), name.Load())
	}
	if len(got) != 1 || got[0] != "[front] Connecting you." {
		t.Fatalf("replies = %q", got)
	}
}

func TestFakeLLMWithoutAMatchingPhraseAnswersNormally(t *testing.T) {
	var calls atomic.Int32
	cfg := FakeLLMConfig{
		Label: "[front] ", ResponseChunks: []string{"You said: "}, EchoUser: true,
		ToolOnPhrase: map[string]string{"transfer to sales": "transfer_to_sales"},
		Executor:     ToolExecutorFunc(func(context.Context, ToolCall) (ToolResult, error) { calls.Add(1); return ToolResult{}, nil }),
	}
	got := runFakeLLM(t, cfg, "hello there").replies()
	if calls.Load() != 0 || len(got) != 1 || !strings.HasPrefix(got[0], "[front] You said: hello there") {
		t.Fatalf("calls=%d replies=%q", calls.Load(), got)
	}
}

func TestFakeLLMWithoutLabelOrToolsIsUnchanged(t *testing.T) {
	got := runFakeLLM(t, FakeLLMConfig{ResponseChunks: []string{"You said: "}, EchoUser: true}, "hi").replies()
	if len(got) != 1 || got[0] != "You said: hi" {
		t.Fatalf("replies = %q", got)
	}
}

func TestFakeLLMPassesArgumentsAndSaysTheToolResult(t *testing.T) {
	var args atomic.Value
	cfg := FakeLLMConfig{
		ResponseChunks: []string{"You said: "}, EchoUser: true,
		ToolOnPhrase: map[string]string{"call tool lookup_order": "mcp__abcd1234_lookup_order0a1b2c3d"},
		SayResult:    map[string]string{"mcp__abcd1234_lookup_order0a1b2c3d": "lookup_order"},
		Executor: ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
			args.Store(string(c.Arguments))
			return ToolResult{CallID: c.ID, Status: ToolSucceeded, Result: json.RawMessage(`{"status":"shipped"}`)}, nil
		}),
	}
	got := runFakeLLM(t, cfg, `please call tool lookup_order {"order_id":"42"} thanks`).replies()
	if args.Load() != `{"order_id":"42"}` {
		t.Fatalf("arguments = %v", args.Load())
	}
	if len(got) != 1 || got[0] != `Tool lookup_order returned {"status":"shipped"}` {
		t.Fatalf("replies = %q", got)
	}
}

func TestFakeLLMSaysWhyTheToolFailed(t *testing.T) {
	for name, exec := range map[string]ToolExecutorFunc{
		"error": func(context.Context, ToolCall) (ToolResult, error) {
			return ToolResult{}, errors.New("connection failed")
		},
		"category": func(context.Context, ToolCall) (ToolResult, error) {
			return ToolResult{Status: ToolFailed, ErrorCategory: "timeout"}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := FakeLLMConfig{
				ToolOnPhrase: map[string]string{"call tool slow_lookup": "mcp__x_slow_lookup00000000"},
				SayResult:    map[string]string{"mcp__x_slow_lookup00000000": "slow_lookup"},
				Executor:     exec,
			}
			got := runFakeLLM(t, cfg, "call tool slow_lookup").replies()
			want := map[string]string{"error": "Tool slow_lookup failed: connection failed", "category": "Tool slow_lookup failed: timeout"}[name]
			if len(got) != 1 || got[0] != want {
				t.Fatalf("replies = %q, want %q", got, want)
			}
		})
	}
}

func TestFakeLLMArgumentsFallBackToAnEmptyObject(t *testing.T) {
	var args atomic.Value
	cfg := FakeLLMConfig{
		ToolOnPhrase: map[string]string{"call tool echo": "echo"},
		SayResult:    map[string]string{"echo": "echo"},
		Executor: ToolExecutorFunc(func(_ context.Context, c ToolCall) (ToolResult, error) {
			args.Store(string(c.Arguments))
			return ToolResult{Status: ToolSucceeded}, nil
		}),
	}
	runFakeLLM(t, cfg, "call tool echo {not json}")
	if args.Load() != `{}` {
		t.Fatalf("arguments = %v", args.Load())
	}
}

// An interruption cancels the turn, and with it the tool the turn is waiting on, as the
// production tool loop does.
func TestFakeLLMInterruptionCancelsTheRunningTool(t *testing.T) {
	cancelled := make(chan struct{})
	started := make(chan struct{})
	cfg := FakeLLMConfig{
		ToolOnPhrase: map[string]string{"call tool slow": "slow"},
		SayResult:    map[string]string{"slow": "slow"},
		Executor: ToolExecutorFunc(func(ctx context.Context, c ToolCall) (ToolResult, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return ToolResult{}, ctx.Err()
		}),
	}
	p := NewFakeLLMProcessor(cfg)
	pipe, err := NewPipeline(p, &captureTTS{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewSession(1, pipe, DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.Reenter(&UserMessageFrame{Turn: "t1", Content: "call tool slow", Timestamp: time.Now()}, Downstream, 0, 0); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := sess.Reenter(&InterruptionFrame{Turn: "t1"}, Downstream, 0, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the interruption did not cancel the running tool")
	}
}
