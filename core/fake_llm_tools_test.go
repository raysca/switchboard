package core

import (
	"context"
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
