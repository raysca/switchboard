//go:build integration

package openai

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	coretools "govoice/core/tools"
)

// Live NVIDIA NIM check through this processor (NVIDIA_API_KEY; optional
// NVIDIA_LLM_MODEL). A handful of tiny completions. The key is never printed.
func nimCfg(t *testing.T) Config {
	key := os.Getenv("NVIDIA_API_KEY")
	if key == "" {
		t.Skip("NVIDIA_API_KEY not set")
	}
	m := os.Getenv("NVIDIA_LLM_MODEL")
	if m == "" {
		m = "openai/gpt-oss-20b"
	}
	return Config{Provider: "nvidia", BaseURL: "https://integrate.api.nvidia.com/v1", Model: m, APIKey: key, Timeout: 30 * time.Second}
}

func TestLiveNIM_StreamAndUsage(t *testing.T) {
	cfg := nimCfg(t)
	cfg.SystemPrompt = "Answer in one short sentence."
	t0 := time.Now()
	s := run(t, cfg)
	var text string
	var first time.Duration
	var u *core.UsageMetricsFrame
	n := 0
	for _, f := range s.got {
		switch f := f.(type) {
		case *core.LLMTextDeltaFrame:
			n++
		case *core.LLMTextFrame:
			text = f.Text
		case *core.UsageMetricsFrame:
			u = f
		case *core.ErrorFrame:
			t.Fatalf("error: %v", f.Err)
		}
	}
	_ = first
	t.Logf("total %v, %d deltas, text=%q usage=%v", time.Since(t0).Round(time.Millisecond), n, text, u != nil)
	if u != nil {
		t.Logf("usage prompt=%v completion=%v", *u.PromptTokens, *u.CompletionTokens)
	}
	if text == "" || n == 0 {
		t.Fatal("no streamed text")
	}
}

func TestLiveNIM_ToolRoundTrip(t *testing.T) {
	cfg := nimCfg(t)
	defs := []core.ToolDefinition{{Name: "get_weather", Description: "Get the current weather for a city.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}}
	var executed atomic.Int32
	var args string
	loop, err := coretools.NewToolLoop(defs, core.ToolExecutorFunc(func(_ context.Context, c core.ToolCall) (core.ToolResult, error) {
		executed.Add(1)
		args = string(c.Arguments)
		return core.ToolResult{CallID: c.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{"temp_c":21,"sky":"sunny"}`)}, nil
	}), core.NewConversationContext(core.ContextConfig{}), coretools.DefaultToolLoopConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ToolDefinitions, cfg.ToolLoop = defs, loop
	cfg.SystemPrompt = "You must call get_weather to answer weather questions, then reply in one short sentence."
	s := runMsg(t, cfg, "What's the weather in Paris right now?")
	for _, f := range s.got {
		if f, ok := f.(*core.ErrorFrame); ok {
			t.Fatalf("error: %v", f.Err)
		}
		if f, ok := f.(*core.LLMTextFrame); ok {
			t.Logf("final text: %q", f.Text)
		}
	}
	t.Logf("tool executed %d times, args=%s", executed.Load(), args)
	if executed.Load() == 0 {
		t.Fatal("model did not call the tool")
	}
}

func runMsg(t *testing.T, cfg Config, msg string) *fakeSession {
	t.Helper()
	p, err := NewProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := newSession()
	_ = p.Attach(s, 0)
	if err := p.ProcessFrame(context.Background(), &core.UserMessageFrame{Turn: "t1", Gen: 1, Content: msg}, core.Downstream, sink{}); err != nil {
		t.Fatal(err)
	}
	s.wg.Wait()
	return s
}
