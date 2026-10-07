package core

import (
	"context"
	"testing"
	"time"
	"unicode/utf8"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestHeardPrefixWithWordTimings(t *testing.T) {
	h := NewHeardText()
	h.AddUnit(1, "Hello there, my friend.")
	h.Played(1, ms(300), []WordTiming{{"Hello", 0, ms(250)}, {"there,", ms(300), ms(550)}}, false)
	h.Played(1, ms(300), []WordTiming{{"my", ms(600), ms(700)}, {"friend.", ms(750), ms(1100)}}, false)
	for _, tc := range []struct {
		at   time.Duration
		want string
	}{
		{0, ""}, {ms(100), ""}, {ms(260), "Hello"}, {ms(560), "Hello there,"},
		{ms(660), "Hello there, my"}, {ms(950), "Hello there, my friend."}, // past half of "friend."
		{ms(760), "Hello there, my"}, // under half of "friend."
	} {
		if got := h.HeardPrefix(1, tc.at); got != tc.want {
			t.Errorf("at %v: %q want %q", tc.at, got, tc.want)
		}
	}
	h.Played(1, ms(100), nil, true)
	if got := h.HeardPrefix(1, ms(10_000)); got != "Hello there, my friend." {
		t.Fatalf("finished unit: %q", got)
	}
}

func TestHeardPrefixProportionalFallback(t *testing.T) {
	h := NewHeardText()
	h.AddUnit(1, "one two three four five six seven eight nine ten")
	h.Played(1, ms(5000), nil, true) // finished: total is exactly 5s
	if got := h.HeardPrefix(1, ms(2500)); got != "one two three four five" {
		t.Fatalf("half: %q", got)
	}
	if got := h.HeardPrefix(1, ms(2300)); got != "one two three four" { // whole words only
		t.Fatalf("whole words: %q", got)
	}
	// Unfinished unit: rate is calibrated from the finished one (49 chars / 5s).
	h.AddUnit(2, "alpha beta gamma delta epsilon zeta eta theta")
	if got := h.HeardPrefix(2, ms(2500)); got == "" || got == "alpha beta gamma delta epsilon zeta eta theta" {
		t.Fatalf("calibrated estimate: %q", got)
	}
	// Unicode: never splits a rune.
	h.AddUnit(3, "héllo wörld ünï cödé")
	h.Played(3, ms(1000), nil, true)
	if got := h.HeardPrefix(3, ms(600)); got != "héllo wörld" {
		t.Fatalf("unicode: %q", got)
	}
	h.AddUnit(4, "今天天气很好")
	h.Played(4, ms(600), nil, true)
	if got := h.HeardPrefix(4, ms(300)); got != "今天天" {
		t.Fatalf("cjk: %q", got)
	}
}

// Barge-in mid-sentence: context keeps the delivered unit plus only the heard
// prefix of the one being spoken, marked partial.
func TestInterruptKeepsOnlyHeardPrefix(t *testing.T) {
	for _, withTimings := range []bool{true, false} {
		store := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
		p := NewAssistantContextProcessor(store, AssistantContextConfig{MinUnitLength: 2, MaxUnitLength: 100})
		e := &captureEmitter{}
		ctx := context.Background()
		for _, f := range []Frame{
			&UserMessageFrame{Turn: "t", Gen: 1},
			&LLMTextDeltaFrame{Turn: "t", Gen: 1, Delta: "First sentence. Second part is long and wordy. Third."},
			&WriteCompletionFrame{Turn: "t", Gen: 1, UnitID: 1, IsFinal: true, Media: ms(1000)},
		} {
			if err := p.ProcessFrame(ctx, f, Downstream, e); err != nil {
				t.Fatal(err)
			}
		}
		wc := &WriteCompletionFrame{Turn: "t", Gen: 1, UnitID: 2, Media: ms(600)}
		if withTimings {
			wc.WordTimings = []WordTiming{{"Second", 0, ms(250)}, {"part", ms(300), ms(550)}, {"is", ms(600), ms(700)}}
		}
		if err := p.ProcessFrame(ctx, wc, Downstream, e); err != nil {
			t.Fatal(err)
		}
		if err := p.ProcessFrame(ctx, &InterruptionFrame{Turn: "t", Gen: 1}, Downstream, e); err != nil {
			t.Fatal(err)
		}
		msgs := store.Messages()
		if len(msgs) != 1 || !msgs[0].Partial {
			t.Fatalf("timings=%v: %#v", withTimings, msgs)
		}
		got := msgs[0].Content
		if withTimings && got != "First sentence. Second part" {
			t.Fatalf("timed: %q", got)
		}
		if !withTimings && (got == "First sentence. Second part is long and wordy." || len(got) <= len("First sentence.")) {
			t.Fatalf("estimated: %q", got)
		}
	}
}

// A unit that never started playing contributes nothing.
func TestInterruptBeforeAnyPlaybackKeepsNothing(t *testing.T) {
	store := NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000})
	p := NewAssistantContextProcessor(store, AssistantContextConfig{MinUnitLength: 2, MaxUnitLength: 100})
	e := &captureEmitter{}
	for _, f := range []Frame{&UserMessageFrame{Turn: "t", Gen: 1}, &LLMTextDeltaFrame{Turn: "t", Gen: 1, Delta: "Hello there friend."}, &InterruptionFrame{Turn: "t", Gen: 1}} {
		if err := p.ProcessFrame(context.Background(), f, Downstream, e); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(store.Messages()); n != 0 {
		t.Fatalf("messages: %d", n)
	}
}

// Alignment never over-reports: no jump to a far repeat of a word, no match
// inside a longer word, and the punctuation glue never cuts a rune in half.
func TestTimedPrefixAlignment(t *testing.T) {
	for _, tc := range []struct {
		text  string
		words []string
		want  string
	}{
		{"The cat sat on the mat.", []string{"the"}, "The"},              // case differs; not "...on the"
		{"One two three four five six.", []string{"One", "six."}, "One"}, // too far ahead: skipped
		{"Scatter a cat.", []string{"Scatter", "a"}, "Scatter a"},        // "a" not inside "Scatter"
		{"Voilà tout.", []string{"Voil"}, "Voilà"},                       // à is C3 A0; A0 is not a space here
		{"It costs $5.99 today, ok.", []string{"It", "costs", "five", "dollars", "today,"}, "It costs $5.99 today,"},
		{"你好，今天天气很好。", []string{"你", "好", "今", "天"}, "你好，今天"}, // no spaces: glue punctuation only
	} {
		var ts []WordTiming
		for i, w := range tc.words {
			ts = append(ts, WordTiming{w, ms(100 * i), ms(100*i + 80)})
		}
		got := timedPrefix(tc.text, ts, ms(10_000))
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("%q %q: got %q want %q", tc.text, tc.words, got, tc.want)
		}
	}
}
