package core

import (
	"context"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// units runs text through nextSplit the way segmentBuffer does.
func units(text string, c splitRules) []string {
	var out []string
	buf := text
	for len(buf) > 0 {
		c.first = len(out) == 0
		i := nextSplit(buf, c)
		if i < 0 {
			break
		}
		if u := strings.TrimSpace(buf[:i]); speakable(u) {
			out = append(out, u)
		}
		buf = buf[i:]
	}
	return out
}

func TestNextSplitSafeBoundaries(t *testing.T) {
	r := splitRules{min: 5, max: 150, last: true}
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"It costs 3.50 dollars today. Thanks!", []string{"It costs 3.50 dollars today.", "Thanks!"}},
		{"Ask Dr. Smith about it. Then leave.", []string{"Ask Dr. Smith about it.", "Then leave."}},
		{"Written by J. K. Rowling in the U.S. last year. Wow.", []string{"Written by J. K. Rowling in the U.S. last year.", "Wow."}},
		{"Visit example.com/a.b for more. Or mail bob@x.co now.", []string{"Visit example.com/a.b for more.", "Or mail bob@x.co now."}},
		{"1. First item\n2. Second item", []string{"1. First item", "2. Second item"}},
		{"He said \"stop now!\" and left. Done.", []string{"He said \"stop now!\" and left.", "Done."}},
		{"Run `a. b. c` please. Then ```x. y.\nz.``` finish.", []string{"Run `a. b. c` please.", "Then ```x. y.\nz.``` finish."}},
		{"Say <break time=\"1s\"/> now. Ok then.", []string{"Say <break time=\"1s\"/> now.", "Ok then."}},
		{"你好，今天天气很好。我们出去玩吧！", []string{"你好，今天天气很好。", "我们出去玩吧！"}},
		{"Wait... what? Really?! Yes.", []string{"Wait...", "what? Really?!", "Yes."}},
	} {
		got := units(tc.in, r)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%q\n got  %q\n want %q", tc.in, got, tc.want)
		}
	}
}

func TestNextSplitWaitsOnDecimalAtBufferEnd(t *testing.T) {
	r := splitRules{min: 5, max: 150}
	if i := nextSplit("The value is 3.", r); i != -1 {
		t.Fatalf("split inside a possible decimal at %d", i)
	}
	if i := nextSplit("The value is 3.5. ", r); i == -1 {
		t.Fatal("no split after a finished decimal sentence")
	}
	r.last = true
	if i := nextSplit("The value is 3.", r); i != len("The value is 3.") {
		t.Fatalf("last chunk: %d", i)
	}
}

func TestNextSplitFirstClause(t *testing.T) {
	text := "Well, that is a really good question, and here is why. It matters."
	// Disabled: first unit is the whole sentence.
	if got := units(text, splitRules{min: 5, max: 150, last: true}); got[0] != "Well, that is a really good question, and here is why." {
		t.Fatalf("clause off: %q", got)
	}
	// Enabled with N=20: skip the too-early "Well," and cut at the next comma.
	got := units(text, splitRules{min: 5, max: 150, clause: 20, last: true})
	if got[0] != "Well, that is a really good question," || got[1] != "and here is why." {
		t.Fatalf("clause on: %q", got)
	}
	// Numbers with thousands separators are not clauses.
	if got := units("The total is 1,000,000 dollars for sure. Ok.", splitRules{min: 5, max: 150, clause: 8, last: true}); got[0] != "The total is 1,000,000 dollars for sure." {
		t.Fatalf("number split: %q", got)
	}
}

func TestNextSplitMaxLengthNeverBreaksNumberOrCode(t *testing.T) {
	r := splitRules{min: 5, max: 10, last: true}
	got := units("aaaaaaaaa 1,000 bbbbbbbbbbbbbbbbbbbb", r)
	for _, u := range got {
		if strings.HasSuffix(u, "1,") || strings.HasPrefix(u, "000") {
			t.Fatalf("split inside number: %q", got)
		}
	}
	if strings.Join(got, " ") != "aaaaaaaaa 1,000 bbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("lost text: %q", got)
	}
}

func TestSegmentBufferFirstClauseEmitsEarly(t *testing.T) {
	cfg := DefaultAssistantContextConfig()
	cfg.FirstClauseMinLength = 20
	p := NewAssistantContextProcessor(NewConversationContext(ContextConfig{MaxMessages: 10, MaxTokens: 1000}), cfg)
	e := &captureEmitter{}
	ctx := context.Background()
	for _, f := range []Frame{&UserMessageFrame{Turn: "t", Gen: 1}, &LLMTextDeltaFrame{Turn: "t", Gen: 1, Delta: "Sure thing, I can help with that, "}} {
		if err := p.ProcessFrame(ctx, f, Downstream, e); err != nil {
			t.Fatal(err)
		}
	}
	var first *SynthesisUnitFrame
	for _, f := range e.frames {
		if u, ok := f.(*SynthesisUnitFrame); ok && first == nil {
			first = u
		}
	}
	if first == nil || first.Text != "Sure thing, I can help with that," {
		t.Fatalf("first unit = %+v", first)
	}
}

// stream feeds text to nextSplit in the given chunks the way segmentBuffer
// does and returns the raw split pieces plus what is left unsplit.
func stream(t *testing.T, chunks []string, c splitRules) (pieces []string, rest string) {
	emitted := 0
	split := func(last bool) {
		for len(rest) > 0 {
			c.first, c.last = emitted == 0, last
			i := nextSplit(rest, c)
			if i < 0 {
				return
			}
			if i == 0 || i > len(rest) {
				t.Fatalf("split %d of %q", i, rest)
			}
			pieces = append(pieces, rest[:i])
			if speakable(strings.TrimSpace(rest[:i])) {
				emitted++
			}
			rest = rest[i:]
		}
	}
	for _, ch := range chunks {
		rest += ch
		split(false)
	}
	split(true)
	return pieces, rest
}

func speakableUnits(pieces []string) []string {
	var out []string
	for _, p := range pieces {
		if u := strings.TrimSpace(p); speakable(u) {
			out = append(out, u)
		}
	}
	return out
}

// chunkAt cuts text at the given byte offsets, moved to rune starts and never
// directly after a terminator or closer (a terminator ending a delta splits
// early by design, see nextSplit).
func chunkAt(text string, cuts []byte) []string {
	var out []string
	prev := 0
	for _, b := range cuts {
		at := prev + int(b)%16 + 1
		for at < len(text) && !utf8.RuneStart(text[at]) {
			at++
		}
		if at >= len(text) {
			break
		}
		if r, _ := utf8.DecodeLastRuneInString(text[:at]); strings.ContainsRune(".!?…\n"+cjkTerm+closers, r) {
			continue
		}
		out = append(out, text[prev:at])
		prev = at
	}
	return append(out, text[prev:])
}

func checkSplit(t *testing.T, text string, cuts []byte, c splitRules) {
	t.Helper()
	pieces, rest := stream(t, chunkAt(text, cuts), c)
	if rest != "" {
		t.Fatalf("last chunk left %q unsplit", rest)
	}
	if got := strings.Join(pieces, ""); got != text {
		t.Fatalf("text lost or duplicated:\n in  %q\n out %q", text, got)
	}
	for _, p := range pieces {
		body := strings.TrimFunc(p, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(".!?…"+closers, r) })
		if n := utf8.RuneCountInString(body); n > 6*c.max+3 { // +3: a "```" fence is consumed whole
			t.Fatalf("unit of %d runes exceeds the hard cap %d", n, 6*c.max)
		}
	}
	oneShot, _ := stream(t, []string{text}, c)
	if a, b := speakableUnits(pieces), speakableUnits(oneShot); strings.Join(a, "\x00") != strings.Join(b, "\x00") {
		t.Fatalf("chunking changed units for %q:\n streamed %q\n one-shot %q", text, a, b)
	}
}

func TestNextSplitStreamingMatchesOneShot(t *testing.T) {
	texts := []string{
		"Ask Dr. Smith at 3.5 p.m. about v1.2.3, it costs $5.99. Mail bob@x.co or see example.com/a.b now!",
		"He said \"stop now!\" and left. Wait... what? 你好，今天天气很好。我们出去玩吧！ مرحبا؟ نعم.",
		"Run `a. b. c` please. Then ```x. y.\nz.``` finish. if a <b and c> d then. Done 🎉. ok",
		"Well, that is a really good question, and here is why. No. 5 is next. The value is 3.",
		strings.Repeat("x", 400) + " tail. " + "```" + strings.Repeat("never closed. ", 60),
	}
	for _, text := range texts {
		for seed := byte(0); seed < 40; seed++ {
			cuts := make([]byte, 64)
			for i := range cuts {
				cuts[i] = seed*7 + byte(i)*13
			}
			checkSplit(t, text, cuts, splitRules{min: 5, max: 40, clause: 20})
			checkSplit(t, text, cuts, splitRules{min: 10, max: 150})
		}
	}
}

func FuzzNextSplit(f *testing.F) {
	f.Add("Ask Dr. Smith about 3.5 now. Then leave!", []byte{3, 9, 1}, uint8(5), uint8(40), uint8(20))
	f.Add("```code. never closed "+strings.Repeat("a. ", 50), []byte{2}, uint8(1), uint8(10), uint8(0))
	f.Add("你好，今天天气很好。<b>x</b> a <b c. \"Hi!\" she said.", []byte{5, 5, 5}, uint8(2), uint8(8), uint8(4))
	f.Fuzz(func(t *testing.T, text string, cuts []byte, min, max, clause uint8) {
		if !utf8.ValidString(text) {
			return // LLM text is valid UTF-8
		}
		mx := int(max)%200 + 1
		checkSplit(t, text, cuts, splitRules{min: int(min) % (mx + 1), max: mx, clause: int(clause) % 100})
	})
}
