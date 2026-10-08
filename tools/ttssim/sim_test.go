package main

import "testing"

// A slack-aware merge never needs more requests than the current rule, and a one-sentence reply is one request.
func TestSlackNoMoreRequests(t *testing.T) {
	p := Provider{OverheadMS: 800, Rate: 129, AudioPerCh: 0.062, MaxChars: 250}
	l := LLM{CPS: 80, TTFTMS: 600, DeltaMS: 60}
	txt := "Sure. The meeting is at three o'clock tomorrow afternoon. Is there anything else I can do for you today?"
	cur := Run(txt, l, p, Strategy{Min: 10})
	sl := Run(txt, l, p, Strategy{Min: 10, FirstClause: 30, Slack: true, MarginMS: 300})
	if sl.Requests > cur.Requests || sl.Chars < len(txt)-4 {
		t.Fatalf("cur=%+v slack=%+v", cur, sl)
	}
	if one := Run("Hello there, how are you?", l, p, Strategy{Min: 10, Slack: true}); one.Requests != 1 || one.Gaps != 0 {
		t.Fatalf("%+v", one)
	}
	if e := Run("", l, p, Strategy{Min: 10, Slack: true}); e.Requests != 0 { // must return, not spin
		t.Fatalf("%+v", e)
	}
}
