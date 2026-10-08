package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

type slackRig struct {
	t   *testing.T
	p   *AssistantContextProcessor
	e   *captureEmitter
	now time.Time
}

func newSlackRig(t *testing.T, mut func(*SlackMergeConfig)) *slackRig {
	r := &slackRig{t: t, e: &captureEmitter{}, now: time.Unix(1000, 0)}
	sc, _ := SlackDefaults("xai")
	sc.Now = func() time.Time { return r.now }
	if mut != nil {
		mut(&sc)
	}
	cfg := DefaultAssistantContextConfig()
	cfg.SlackMerge = &sc
	r.p = NewAssistantContextProcessor(NewConversationContext(ContextConfig{MaxMessages: 8, MaxTokens: 1000}), cfg)
	r.feed(&UserMessageFrame{Turn: "t1"})
	return r
}

func (r *slackRig) feed(f Frame) {
	if err := r.p.ProcessFrame(context.Background(), f, Downstream, r.e); err != nil {
		r.t.Fatal(err)
	}
}

// at advances the fake clock to offset d and delivers a text delta.
func (r *slackRig) at(d time.Duration, text string) {
	r.now = time.Unix(1000, 0).Add(d)
	r.feed(&LLMTextDeltaFrame{Delta: text, Turn: "t1"})
}

func (r *slackRig) units() (u []*SynthesisUnitFrame) {
	for _, f := range r.e.frames {
		if s, ok := f.(*SynthesisUnitFrame); ok {
			u = append(u, s)
		}
	}
	return
}

const (
	s60 = "The first sentence is a fairly long one with sixty characters."
	s30 = " A second sentence of thirty chars."
)

func TestSlackMergeHoldsThenSendsAtLastSafeMoment(t *testing.T) {
	r := newSlackRig(t, nil)
	r.at(0, s60) // first unit goes at once: ready 1.3s, audio runs to ~4.9s
	r.at(200*time.Millisecond, s30)
	r.at(400*time.Millisecond, s30)
	if n := len(r.units()); n != 1 {
		t.Fatalf("units = %d, want only the first (rest held)", n)
	}
	r.at(2*time.Second, "") // still safe: 2.0+0.8+0.5+0.3 < 4.9
	if n := len(r.units()); n != 1 {
		t.Fatalf("flushed too early: %d units", n)
	}
	r.at(3500*time.Millisecond, "") // 3.5+0.8+0.5+0.3 > 4.9: send now
	u := r.units()
	if len(u) != 2 || strings.Count(u[1].Text, "second sentence") != 2 {
		t.Fatalf("want one merged second unit, got %+v", u)
	}
}

func TestSlackMergeFlushesOnPlaybackTickWhenLLMStalls(t *testing.T) {
	r := newSlackRig(t, nil)
	r.at(0, s60)
	r.at(200*time.Millisecond, s30) // held; then the LLM goes quiet (e.g. a tool call)
	r.now = time.Unix(1000, 0).Add(2 * time.Second)
	r.feed(&WriteCompletionFrame{Turn: "t1", UnitID: 1, Media: 20 * time.Millisecond})
	if n := len(r.units()); n != 1 {
		t.Fatalf("flushed too early: %d units", n)
	}
	r.now = time.Unix(1000, 0).Add(3500 * time.Millisecond)
	r.feed(&WriteCompletionFrame{Turn: "t1", UnitID: 1, Media: 20 * time.Millisecond})
	if u := r.units(); len(u) != 2 || u[1].Text != strings.TrimSpace(s30) || u[1].IsFinal {
		t.Fatalf("held text not sent on tick: %+v", u)
	}
}

func TestSlackMergeFlushesAtReplyEnd(t *testing.T) {
	r := newSlackRig(t, nil)
	r.at(0, s60)
	r.at(100*time.Millisecond, s30)
	r.feed(&LLMTextFrame{Text: s60 + s30, Turn: "t1"})
	u := r.units()
	if len(u) != 2 || !u[1].IsFinal || u[0].IsFinal || u[1].Text != strings.TrimSpace(s30) {
		t.Fatalf("units = %+v", u)
	}
}

func TestSlackMergeCapSplits(t *testing.T) {
	r := newSlackRig(t, func(c *SlackMergeConfig) { c.Cap = 70 })
	r.at(0, s60)
	r.at(10*time.Millisecond, s30)     // held (30)
	r.at(20*time.Millisecond, " "+s60) // 30+60+1 > 70: send the held one, hold the new
	u := r.units()
	if len(u) != 2 || u[1].Text != strings.TrimSpace(s30) {
		t.Fatalf("units = %+v", u)
	}
}

func TestSlackMergeBargeInDropsHeldText(t *testing.T) {
	r := newSlackRig(t, nil)
	r.at(0, s60)
	r.at(100*time.Millisecond, s30)
	r.feed(&InterruptionFrame{Turn: "t1"})
	if r.p.slack.pending != "" {
		t.Fatal("held text survived barge-in")
	}
	r.feed(&UserMessageFrame{Turn: "t2"})
	r.at(time.Second, "")
	if n := len(r.units()); n != 1 {
		t.Fatalf("held text was sent after barge-in: %d units", n)
	}
}

func TestSlackMergeFirstUnitIsNotHeldAndClauseApplies(t *testing.T) {
	r := newSlackRig(t, nil)
	r.p.cfg.FirstClauseMinLength = 40
	r.at(0, "Well, this opening clause is long enough to go, and then more text follows")
	u := r.units()
	if len(u) != 1 || u[0].Text != "Well, this opening clause is long enough to go," {
		t.Fatalf("units = %+v", u)
	}
}

func TestSlackMergeLearnsOverhead(t *testing.T) {
	r := newSlackRig(t, nil)
	r.at(0, s60)
	before := r.p.slack.overhead
	r.now = r.now.Add(3 * time.Second) // first audio written 3s after the send
	r.feed(&WriteCompletionFrame{Turn: "t1", UnitID: 1, Media: 20 * time.Millisecond})
	if r.p.slack.overhead <= before {
		t.Fatalf("overhead not learned: %v -> %v", before, r.p.slack.overhead)
	}
}

func TestSlackOffIsUnchanged(t *testing.T) {
	p := NewAssistantContextProcessor(NewConversationContext(ContextConfig{MaxMessages: 8, MaxTokens: 1000}), DefaultAssistantContextConfig())
	e := &captureEmitter{}
	for _, f := range []Frame{&UserMessageFrame{Turn: "t1"}, &LLMTextDeltaFrame{Delta: s60 + s30, Turn: "t1"}} {
		_ = p.ProcessFrame(context.Background(), f, Downstream, e)
	}
	n := 0
	for _, f := range e.frames {
		if _, ok := f.(*SynthesisUnitFrame); ok {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("units = %d, want one per sentence", n)
	}
}
