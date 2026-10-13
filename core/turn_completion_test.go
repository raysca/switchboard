package core_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/coretest"
)

// scriptLLM answers each user message with the chunks script(turn, content)
// returns, as deltas followed by the final LLMTextFrame. A chunk "<tool>"
// emits a ToolCallFrame instead of text.
type scriptLLM struct {
	sess   core.SessionController
	index  int
	script func(turn, content string) []string
	mu     sync.Mutex
	seen   []string
}

func (l *scriptLLM) Attach(s core.SessionController, i int) error { l.sess, l.index = s, i; return nil }

func (l *scriptLLM) turns() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.seen...)
}

func (l *scriptLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	um, ok := f.(*core.UserMessageFrame)
	if !ok {
		return e.PushDirection(f, dir)
	}
	l.mu.Lock()
	l.seen = append(l.seen, um.Turn)
	l.mu.Unlock()
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	gen := l.sess.CurrentGeneration()
	chunks := l.script(um.Turn, um.Content)
	if chunks == nil { // the generation never answers (test drives frames by hand)
		return nil
	}
	var full strings.Builder
	for _, c := range chunks {
		if c == "<tool>" {
			_ = l.sess.Reenter(&core.ToolCallFrame{Call: core.ToolCall{ID: "c1", Name: "t"}}, core.Downstream, l.index+1, gen)
			continue
		}
		full.WriteString(c)
		_ = l.sess.Reenter(&core.LLMTextDeltaFrame{Delta: c, Turn: um.Turn, Gen: gen}, core.Downstream, l.index+1, gen)
	}
	return l.sess.Reenter(&core.LLMTextFrame{Text: full.String(), Turn: um.Turn, Gen: gen}, core.Downstream, l.index+1, gen)
}

type complRig struct {
	sess     *core.Session
	llm      *scriptLLM
	asst     *core.AssistantContextProcessor
	ctx      *core.ConversationContext
	tail     *coretest.PassthroughProcessor
	clk      *fakeIdleClock
	mu       sync.Mutex
	verdicts []string
}

func newComplRig(t *testing.T, script func(turn, content string) []string) *complRig {
	t.Helper()
	r := &complRig{clk: &fakeIdleClock{now: time.Unix(1000, 0)}, tail: &coretest.PassthroughProcessor{}}
	r.ctx = core.NewConversationContext(core.ContextConfig{MaxMessages: 50, MaxTokens: 10000})
	r.llm = &scriptLLM{script: script}
	cfg := core.DefaultAssistantContextConfig()
	cfg.Completion = &core.TurnCompletionConfig{Clock: r.clk, OnVerdict: func(v string) { r.mu.Lock(); r.verdicts = append(r.verdicts, v); r.mu.Unlock() }}
	r.asst = core.NewAssistantContextProcessor(r.ctx, cfg)
	pipe, err := core.NewPipeline(r.llm, r.asst, r.tail)
	if err != nil {
		t.Fatal(err)
	}
	r.sess, err = core.NewSession(7, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.sess.Close() })
	return r
}

func (r *complRig) say(turn, text string) {
	_ = r.ctx.AddMessage(core.Message{Role: core.RoleUser, Content: text, TurnID: turn})
	_ = r.sess.Post(&core.UserMessageFrame{Turn: turn, Content: text})
}

func (r *complRig) verdictList() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.verdicts...)
}

// shape is the downstream frame sequence: D=text delta, U:<unit text>, T=final text.
func (r *complRig) shape() []string {
	var out []string
	for _, f := range r.tail.GetFrames() {
		switch v := f.(type) {
		case *core.LLMTextDeltaFrame:
			out = append(out, "D:"+v.Delta)
		case *core.SynthesisUnitFrame:
			out = append(out, "U:"+v.Text)
		}
	}
	return out
}

func units(shape []string) (u []string) {
	for _, s := range shape {
		if strings.HasPrefix(s, "U:") {
			u = append(u, s[2:])
		}
	}
	return
}

func TestCompletion_CompleteStripsMarkerSameTTSStart(t *testing.T) {
	marked := newComplRig(t, func(string, string) []string { return []string{"●", " Hello there. ", "How are you today?"} })
	plain := newComplRig(t, func(string, string) []string { return []string{"Hello there. ", "How are you today?"} })
	marked.say("turn-1", "hello")
	plain.say("turn-1", "hello")
	waitFor(t, "units", func() bool { return len(units(marked.shape())) >= 1 && len(units(plain.shape())) >= 1 })
	m, p := marked.shape(), plain.shape()
	for _, s := range m {
		if strings.ContainsAny(s, "●◐○") {
			t.Fatalf("marker leaked: %v", m)
		}
	}
	// Identical TTS start: exactly one text delta precedes the first unit in both.
	firstUnit := func(s []string) int {
		for i, x := range s {
			if strings.HasPrefix(x, "U:") {
				return i
			}
		}
		return -1
	}
	if firstUnit(m) != firstUnit(p) || units(m)[0] != "Hello there." {
		t.Fatalf("marked %v vs plain %v", m, p)
	}
	if got := marked.verdictList(); len(got) != 1 || got[0] != core.VerdictComplete {
		t.Fatalf("verdicts = %v", got)
	}
}

func TestCompletion_HistoryNeverHoldsMarker(t *testing.T) {
	r := newComplRig(t, func(string, string) []string { return []string{"●", " Hi there."} })
	r.say("turn-1", "hello")
	waitFor(t, "unit", func() bool { return len(units(r.shape())) == 1 })
	_ = r.sess.Post(&core.WriteCompletionFrame{Turn: "turn-1", UnitID: 1, IsFinal: true})
	waitFor(t, "commit", func() bool { return len(r.ctx.Messages()) == 2 })
	if m := r.ctx.Messages()[1]; m.Role != core.RoleAssistant || m.Content != "Hi there." {
		t.Fatalf("assistant message = %+v", m)
	}
}

func TestCompletion_IncompleteHoldsThenNudges(t *testing.T) {
	for _, tc := range []struct {
		marker  string
		wait    time.Duration
		verdict string
	}{{"◐", 5 * time.Second, core.VerdictIncompleteShort}, {"○", 10 * time.Second, core.VerdictIncompleteLong}} {
		r := newComplRig(t, func(turn, _ string) []string {
			if strings.HasPrefix(turn, "completion-") {
				return []string{"● Go ahead, I'm listening."}
			}
			return []string{tc.marker}
		})
		r.say("turn-1", "i'd go to")
		waitFor(t, "hold", r.asst.Held)
		if r.asst.Active() {
			t.Fatal("incomplete turn must not stay Active")
		}
		r.clk.parkedAt(t, r.clk.Now().Add(tc.wait))
		r.clk.Advance(tc.wait - time.Millisecond)
		time.Sleep(20 * time.Millisecond)
		if n := len(r.llm.turns()); n != 1 || len(units(r.shape())) != 0 || !r.asst.Held() {
			t.Fatalf("%s: before the wait: turns=%d shape=%v", tc.marker, n, r.shape())
		}
		for _, s := range r.shape() {
			t.Fatalf("incomplete turn emitted %q", s)
		}
		r.clk.Advance(time.Millisecond)
		waitFor(t, "nudge speech", func() bool { return len(units(r.shape())) == 1 })
		if u := units(r.shape())[0]; u != "Go ahead, I'm listening." {
			t.Fatalf("unit = %q", u)
		}
		if r.asst.Held() {
			t.Fatal("hold not released")
		}
		msgs := r.ctx.Messages()
		if len(msgs) != 2 || msgs[1].Role != core.RoleUser || !strings.Contains(msgs[1].Content, "not spoken by the user") {
			t.Fatalf("history = %+v", msgs)
		}
		for _, m := range msgs {
			if m.Role == core.RoleAssistant {
				t.Fatalf("marker/assistant text recorded: %+v", m)
			}
		}
		if v := r.verdictList(); v[0] != tc.verdict || v[len(v)-1] != core.VerdictComplete {
			t.Fatalf("verdicts = %v", v)
		}
	}
}

func TestCompletion_ContinuationCancelsWait(t *testing.T) {
	r := newComplRig(t, func(turn, content string) []string {
		if turn == "turn-1" {
			return []string{"◐"}
		}
		return []string{"● Japan is great."}
	})
	r.say("turn-1", "i'd go to")
	waitFor(t, "hold", r.asst.Held)
	r.say("turn-2", "japan because i love food")
	waitFor(t, "reply", func() bool { return len(units(r.shape())) == 1 })
	if r.asst.Held() {
		t.Fatal("continuation must cancel the wait")
	}
	r.clk.Advance(time.Minute)
	time.Sleep(30 * time.Millisecond)
	if got := r.llm.turns(); len(got) != 2 {
		t.Fatalf("no nudge expected after continuation, turns=%v", got)
	}
}

func TestCompletion_UserSpeechCancelsWait(t *testing.T) {
	r := newComplRig(t, func(string, string) []string { return []string{"○"} })
	r.say("turn-1", "hold on")
	waitFor(t, "hold", r.asst.Held)
	_ = r.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1})
	waitFor(t, "release", func() bool { return !r.asst.Held() })
	r.clk.Advance(time.Minute)
	time.Sleep(30 * time.Millisecond)
	if len(r.llm.turns()) != 1 {
		t.Fatal("nudge fired after the user started speaking")
	}
}

func TestCompletion_MarkerSplitAcrossDeltas(t *testing.T) {
	for _, chunks := range [][]string{{"[comp", "lete] Hi", " there."}, {" ", "●", "Hi there."}, {"(Complete)", " Hi there."}} {
		r := newComplRig(t, func(string, string) []string { return chunks })
		r.say("turn-1", "x")
		waitFor(t, "unit", func() bool { return len(units(r.shape())) == 1 })
		if u := units(r.shape())[0]; u != "Hi there." {
			t.Fatalf("%q -> %q (%v)", chunks, u, r.shape())
		}
	}
}

func TestCompletion_NoMarkerSpeaksImmediately(t *testing.T) {
	r := newComplRig(t, func(string, string) []string { return []string{"Hello there. ", "More."} })
	r.say("turn-1", "x")
	waitFor(t, "units", func() bool { return len(units(r.shape())) == 2 })
	if s := r.shape(); s[0] != "D:Hello there. " {
		t.Fatalf("first delta must pass unbuffered: %v", s)
	}
	if v := r.verdictList(); len(v) != 1 || v[0] != core.VerdictNoMarker {
		t.Fatalf("verdicts = %v", v)
	}
	// A bracketed non-marker is released whole.
	r2 := newComplRig(t, func(string, string) []string { return []string{"[note] Hi there."} })
	r2.say("turn-1", "x")
	waitFor(t, "unit", func() bool { return len(units(r2.shape())) == 1 })
	if u := units(r2.shape())[0]; u != "[note] Hi there." {
		t.Fatalf("unit = %q", u)
	}
}

func TestCompletion_ToolFirstAndSyntheticTurnsNeverHold(t *testing.T) {
	r := newComplRig(t, func(string, string) []string { return []string{"<tool>", "◐ Sunny today."} })
	r.say("turn-1", "weather?")
	waitFor(t, "unit", func() bool { return len(units(r.shape())) == 1 })
	if u := units(r.shape())[0]; u != "Sunny today." || r.asst.Held() {
		t.Fatalf("tool-first: %q held=%v", u, r.asst.Held())
	}
	r = newComplRig(t, func(string, string) []string { return []string{"○ Still here?"} })
	r.say("idle-1", "[idle notice]")
	waitFor(t, "unit", func() bool { return len(units(r.shape())) == 1 })
	if u := units(r.shape())[0]; u != "Still here?" || r.asst.Held() {
		t.Fatalf("synthetic: %q held=%v", u, r.asst.Held())
	}
}

func TestCompletion_BargeInWhileBuffering(t *testing.T) {
	r := newComplRig(t, func(string, string) []string { return nil })
	r.say("turn-1", "x")
	waitFor(t, "active", r.asst.Active)
	_ = r.sess.Post(&core.LLMTextDeltaFrame{Delta: "[comp", Turn: "turn-1", Gen: r.sess.CurrentGeneration()})
	_ = r.sess.Post(&core.InterruptionFrame{})
	waitFor(t, "idle", func() bool { return !r.asst.Active() })
	_ = r.sess.Post(&core.LLMTextDeltaFrame{Delta: "lete] Leaked.", Turn: "turn-1", Gen: r.sess.CurrentGeneration()})
	time.Sleep(30 * time.Millisecond)
	for _, s := range r.shape() {
		t.Fatalf("output after barge-in: %v", s)
	}
}

func TestCompletion_RegenerationResetsMarkerState(t *testing.T) {
	// A failover replay restarts the same turn: a half-read marker from the
	// abandoned attempt must not corrupt the next one.
	n := 0
	r := newComplRig(t, func(string, string) []string {
		n++
		if n == 1 {
			return nil
		}
		return []string{"● Hi there."}
	})
	r.say("turn-1", "x")
	waitFor(t, "active", r.asst.Active)
	_ = r.sess.Post(&core.LLMTextDeltaFrame{Delta: "[comp", Turn: "turn-1", Gen: r.sess.CurrentGeneration()})
	_ = r.sess.Post(&core.UserMessageFrame{Turn: "turn-1", Content: "x"})
	waitFor(t, "unit", func() bool { return len(units(r.shape())) == 1 })
	if u := units(r.shape())[0]; u != "Hi there." {
		t.Fatalf("unit = %q", u)
	}
}

func TestCompletion_OffIsUntouched(t *testing.T) {
	ctx := core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	asst := core.NewAssistantContextProcessor(ctx, core.DefaultAssistantContextConfig())
	llm := &scriptLLM{script: func(string, string) []string { return []string{"◐ Odd but spoken."} }}
	tail := &coretest.PassthroughProcessor{}
	pipe, _ := core.NewPipeline(llm, asst, tail)
	sess, _ := core.NewSession(8, pipe, core.DefaultResourceBudget())
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_ = sess.Post(&core.UserMessageFrame{Turn: "turn-1", Content: "x"})
	waitFor(t, "unit", func() bool {
		for _, f := range tail.GetFrames() {
			if u, ok := f.(*core.SynthesisUnitFrame); ok && strings.HasPrefix(u.Text, "◐") {
				return true
			}
		}
		return false
	})
}

// Replies that merely look like a marker prefix are released verbatim, and a
// marker repeated mid-reply (a second response round after a tool result or a
// failover restart, or a bullet list) is never spoken.
func TestCompletion_LookalikesAndRepeatedMarkers(t *testing.T) {
	for _, tc := range []struct {
		chunks []string
		want   string
	}{
		{[]string{"(Pausing", ") Sure, go ahead."}, "(Pausing) Sure, go ahead."},
		{[]string{"[Link] is below."}, "[Link] is below."},
		{[]string{"(this is a long aside without a closer"}, "(this is a long aside without a closer"},
		{[]string{"● Let me check.", "● It is sunny."}, "Let me check. It is sunny."},
		{[]string{"● one\n", "● two"}, "one\n two"},
	} {
		r := newComplRig(t, func(string, string) []string { return tc.chunks })
		r.say("turn-1", "x")
		waitFor(t, tc.want, func() bool {
			return strings.ReplaceAll(strings.Join(units(r.shape()), " "), " ", "") == strings.ReplaceAll(tc.want, " ", "")
		})
		for _, s := range r.shape() {
			if strings.ContainsAny(s, "●◐○") {
				t.Fatalf("%q: marker leaked: %v", tc.chunks, r.shape())
			}
		}
		if r.asst.Held() {
			t.Fatalf("%q: held", tc.chunks)
		}
	}
}
