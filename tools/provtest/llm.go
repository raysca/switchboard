package provtest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"govoice/core"
)

const (
	llmQuestion = "What is two plus two? Answer in one short sentence."
	llmLong     = "Write a long, detailed story about a lighthouse keeper, at least 400 words."
	llmWeather  = "What's the weather in Paris right now?"
)

func (r *run) llmWait() time.Duration { return max(r.o.Timeout, 45*time.Second) }

// ask posts one user message and waits for the complete reply or an error.
func (r *run) ask(g *rig, text string) (t0 time.Time) {
	t0 = time.Now()
	_ = g.sess.Post(&core.UserMessageFrame{Turn: "t1", Gen: g.gen(), Content: text})
	waitFor(r.llmWait(), func() bool {
		f, _ := frames[*core.LLMTextFrame](g.sink)
		return len(f) > 0 || len(g.errs()) > 0
	})
	return t0
}

func (r *run) llmSuite() {
	base := settleGoroutines(200 * time.Millisecond)
	p, err := r.t.Build(Variant{})
	if err != nil {
		r.add("build", false, "%v", Redact(err.Error()))
		r.skip("stream", "build failed")
	} else {
		r.add("build", true, "")
		if g, gerr := newRig(p); gerr != nil {
			r.add("build", false, "rig: %v", gerr)
		} else {
			r.llmStream(g)
			r.leakAfter(g, base)
		}
	}
	r.llmTool()
	r.llmCancel()
	r.badCredential(func(g *rig) error {
		r.ask(g, llmQuestion)
		if e := g.errs(); len(e) > 0 {
			return e[0]
		}
		return nil
	})
	r.llmBadModel()
}

func (r *run) llmStream(g *rig) {
	if !r.spend(1, "stream") {
		return
	}
	t0 := r.ask(g, llmQuestion)
	total := time.Since(t0)
	time.Sleep(300 * time.Millisecond) // usage may trail the text
	deltas, at := frames[*core.LLMTextDeltaFrame](g.sink)
	full, _ := frames[*core.LLMTextFrame](g.sink)
	errs := g.errs()
	r.add("no_error", len(errs) == 0, "%d errors; first: %v", len(errs), firstErr(errs))
	if len(full) == 0 || len(deltas) == 0 {
		r.add("stream", false, "%d deltas, %d complete texts", len(deltas), len(full))
		return
	}
	var sb strings.Builder
	for _, d := range deltas {
		sb.WriteString(d.Delta)
	}
	text := full[0].Text
	lower := strings.ToLower(text)
	r.add("stream", strings.TrimSpace(sb.String()) == strings.TrimSpace(text) && (strings.Contains(lower, "four") || strings.Contains(text, "4")),
		"%d deltas, deltas==text: %v, answer %q", len(deltas), strings.TrimSpace(sb.String()) == strings.TrimSpace(text), trunc(text, 60))
	ttft := at[0].Sub(t0)
	r.metric("ttft", ttft < 10*time.Second, float64(ttft.Milliseconds()), "ms", "first token after %v (plausible < 10s)", ttft.Round(time.Millisecond))
	r.metric("total_time", total < 60*time.Second, total.Seconds(), "s", "reply complete after %v", total.Round(time.Millisecond))
	us, _ := frames[*core.UsageMetricsFrame](g.sink)
	var prompt, comp int64
	for _, u := range us {
		if u.Service == "llm" {
			if u.PromptTokens != nil {
				prompt += *u.PromptTokens
			}
			if u.CompletionTokens != nil {
				comp += *u.CompletionTokens
			}
		}
	}
	r.add("usage", prompt > 0 && comp > 0, "%d usage frames, prompt tokens %d, completion tokens %d", len(us), prompt, comp)
}

// weatherKit is the trivial tool: one call, fixed answer.
type weatherKit struct {
	mu   sync.Mutex
	args []string
	kit  *ToolKit
}

func newWeatherKit() *weatherKit {
	w := &weatherKit{}
	w.kit = &ToolKit{
		Defs: []core.ToolDefinition{{Name: "get_weather", Description: "Get the current weather for a city.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)}},
		Exec: core.ToolExecutorFunc(func(_ context.Context, c core.ToolCall) (core.ToolResult, error) {
			w.mu.Lock()
			w.args = append(w.args, string(c.Arguments))
			w.mu.Unlock()
			return core.ToolResult{CallID: c.ID, Status: core.ToolSucceeded, Result: json.RawMessage(`{"temp_c":21,"sky":"sunny"}`)}, nil
		}),
		History: core.NewConversationContext(core.ContextConfig{}),
	}
	return w
}

func (r *run) llmTool() {
	const name = "tool_round_trip"
	w := newWeatherKit()
	p, err := r.t.Build(Variant{Tools: w.kit})
	if err != nil {
		r.add(name, false, "build with tools: %v", Redact(err.Error()))
		return
	}
	g, err := newRig(p)
	if err != nil {
		r.add(name, false, "rig: %v", err)
		return
	}
	defer g.close()
	if !r.spend(2, name) {
		return
	}
	r.ask(g, llmWeather)
	w.mu.Lock()
	args := append([]string(nil), w.args...)
	w.mu.Unlock()
	full, _ := frames[*core.LLMTextFrame](g.sink)
	text := ""
	if len(full) > 0 {
		text = full[len(full)-1].Text
	}
	var call struct {
		City string `json:"city"`
	}
	// The arguments must be valid JSON matching the schema, not just mention Paris.
	paris := len(args) > 0 && json.Unmarshal([]byte(args[0]), &call) == nil && strings.Contains(strings.ToLower(call.City), "paris")
	used := strings.Contains(text, "21") || strings.Contains(strings.ToLower(text), "sunny")
	r.add(name, len(args) >= 1 && paris && used && len(g.errs()) == 0, "tool executed %d time(s) (args %.60v, valid city=paris: %v), reply uses the result: %v, errors=%d, reply %q", len(args), args, paris, used, len(g.errs()), trunc(text, 70))
}

func (r *run) llmCancel() {
	const name = "cancel"
	p, err := r.t.Build(Variant{})
	if err != nil {
		r.skip(name, "build failed")
		return
	}
	g, err := newRig(p)
	if err != nil {
		r.add(name, false, "rig: %v", err)
		return
	}
	defer g.close()
	if !r.spend(1, name) {
		return
	}
	_ = g.sess.Post(&core.UserMessageFrame{Turn: "t1", Gen: g.gen(), Content: llmLong})
	if !waitFor(r.llmWait(), func() bool { d, _ := frames[*core.LLMTextDeltaFrame](g.sink); return len(d) > 0 || len(g.errs()) > 0 }) || len(g.errs()) > 0 {
		r.add(name, false, "no streamed text before the interruption; errors=%v", firstErr(g.errs()))
		return
	}
	time.Sleep(150 * time.Millisecond)
	if done, _ := frames[*core.LLMTextFrame](g.sink); len(done) > 0 {
		r.skip(name, "reply completed before the interruption (nothing to cancel)")
		return
	}
	t := time.Now()
	_ = g.sess.Post(&core.InterruptionFrame{Turn: "t1", Gen: g.gen()})
	time.Sleep(maxInterrupt + 700*time.Millisecond)
	_, at := frames[*core.LLMTextDeltaFrame](g.sink)
	var last time.Duration
	for _, a := range at {
		if a.After(t) {
			last = max(last, a.Sub(t))
		}
	}
	_, errAt := frames[*core.ErrorFrame](g.sink)
	lateErr := false
	for _, e := range errAt {
		lateErr = lateErr || e.After(t)
	}
	r.metric(name, last <= maxInterrupt && !lateErr && g.sess.Err() == nil, float64(last.Milliseconds()), "ms",
		"last delta %v after interruption (max %v), error-after-cancel=%v", last.Round(time.Millisecond), maxInterrupt, lateErr || g.sess.Err() != nil)
}

func (r *run) llmBadModel() {
	const name = "bad_model"
	if !r.spend(1, name) {
		return
	}
	start := time.Now()
	var got error
	p, err := r.t.Build(Variant{BadModel: true})
	if err != nil {
		got = err
	} else {
		g, gerr := newRig(p)
		if gerr != nil {
			r.add(name, false, "rig: %v", gerr)
			return
		}
		r.ask(g, llmQuestion)
		if e := g.errs(); len(e) > 0 {
			got = e[0]
		}
		g.close()
	}
	r.errorClass(name, got, start)
}
