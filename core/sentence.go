package core

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// splitRules are the segmentation limits, all counted in runes.
type splitRules struct {
	min    int  // runes of text before a sentence terminator
	max    int  // soft cap: split at the next whitespace/comma after this many runes
	clause int  // >0: first unit of a turn may end at a clause boundary after this many runes
	first  bool // no unit emitted yet this turn
	last   bool // the LLM text is complete; nothing more will arrive
}

// abbreviations never end a sentence; the next word belongs to the same unit.
// ponytail: English only and deliberately short, a missed one just merges two
// sentences into one unit, which is harmless. Extend per language if it matters.
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "mx": true, "dr": true, "prof": true, "sr": true, "jr": true,
	"st": true, "mt": true, "vs": true, "etc": true, "inc": true, "ltd": true, "corp": true, "co": true,
	"approx": true, "dept": true, "fig": true, "gen": true, "gov": true, "sen": true, "rep": true,
	"lt": true, "col": true, "capt": true, "sgt": true,
}

const (
	closers = "\"'”’)]»」』" // may trail a terminator and belong to its sentence
	cjkTerm = "。！？؟।"      // terminators that need no following space (CJK, Arabic, Devanagari)
)

// nextSplit returns the byte index at which buf's next synthesis unit ends, or
// -1 to wait for more text. It never splits inside a number, URL, e-mail,
// abbreviation, code span/fence or <tag>.
//
// The same text yields the same units however the deltas are chunked
// (FuzzNextSplit), except that a terminator ending a delta splits at once
// rather than stall a reply that pauses for a tool call.
func nextSplit(buf string, c splitRules) int {
	var fence, code, tag bool
	n, tagAt := 0, 0 // runes before position i; rune index of the open '<'
	// Leading whitespace (left by the previous split) counts for nothing.
	for i := len(buf) - len(strings.TrimLeftFunc(buf, unicode.IsSpace)); i < len(buf); {
		r, w := utf8.DecodeRuneInString(buf[i:])
		next := i + w
		if n >= 6*c.max {
			return i // no whitespace in sight (a long token): cut anyway
		}
		// Protection is bounded so an unclosed fence or a '<' meaning
		// less-than cannot hold audio forever.
		protect := n < 4*c.max
		if r == '`' {
			if !c.last && len(buf)-i < 3 && strings.HasPrefix("```", buf[i:]) {
				return -1 // may be the start of a fence
			}
			if strings.HasPrefix(buf[i:], "```") {
				fence = !fence
				next, n = i+3, n+3
			} else {
				if !fence {
					code = !code
				}
				n++
			}
			i = next
			continue
		}
		if r2, _ := utf8.DecodeRuneInString(buf[next:]); r == '<' && (r2 == '/' || r2 == '!' || unicode.IsLetter(r2)) {
			tag, tagAt = true, n
		} else if r == '>' || n-tagAt > c.max {
			tag = false
		}
		if protect && (fence || code || tag) {
			i = next
			n++
			continue
		}

		switch {
		case n >= c.min && (r == '.' || r == '!' || r == '?' || r == '…' || r == '\n' || strings.ContainsRune(cjkTerm, r)):
			if end := sentenceEnd(buf, i, r, next, c.last); end >= 0 {
				return end
			}
		case c.first && c.clause > 0 && n >= c.clause && n >= c.min && strings.ContainsRune(",;:—，；：、", r):
			if cjk := r == '，' || r == '；' || r == '：' || r == '、' || r == '—'; cjk || (next < len(buf) && isSpace(buf[next:])) {
				return skipSpaces(buf, next)
			}
		case n >= c.max:
			if unicode.IsSpace(r) {
				return next
			}
			if r == ',' && next < len(buf) && isSpace(buf[next:]) {
				return skipSpaces(buf, next)
			}
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
				return next // CJK has no spaces to break on
			}
		}
		i = next
		n++
	}
	if c.last && len(buf) > 0 {
		return len(buf)
	}
	return -1
}

// sentenceEnd decides whether the terminator r at i really ends a sentence and
// returns the split index, or -1.
func sentenceEnd(buf string, i int, r rune, next int, last bool) int {
	end := next
	for end < len(buf) {
		r2, w2 := utf8.DecodeRuneInString(buf[end:])
		if r2 != '.' && r2 != '!' && r2 != '?' && r2 != '…' && !strings.ContainsRune(closers, r2) {
			break
		}
		end += w2
	}
	cjk := strings.ContainsRune(cjkTerm, r)
	switch {
	case r == '\n' || cjk:
		return skipSpaces(buf, end)
	case end == len(buf):
		// "3." may continue as "3.5" in the next delta; anything else ends here.
		// "Dr." may continue as "Dr. Smith". Other terminators split at once.
		if prev, _ := utf8.DecodeLastRuneInString(buf[:i]); !last && r == '.' && (unicode.IsDigit(prev) || endsAbbreviation(buf[:i])) {
			return -1
		}
		return end
	case !isSpace(buf[end:]):
		return -1 // "example.com", "v1.2", "a@b.co"
	case r == '.' && endsAbbreviation(buf[:i]):
		return -1
	case end > next && strings.ContainsRune(closers, lastRune(buf[:end])) && continuesLowercase(buf[end:], last):
		return -1 // `He said "stop!" and left.`
	}
	return skipSpaces(buf, end)
}

func lastRune(s string) rune { r, _ := utf8.DecodeLastRuneInString(s); return r }

func isSpace(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsSpace(r)
}

// skipSpaces advances over spaces and tabs (not newlines, which are boundaries).
func skipSpaces(buf string, i int) int {
	for i < len(buf) && (buf[i] == ' ' || buf[i] == '\t') {
		i++
	}
	return i
}

// endsAbbreviation reports whether the word before a '.' is an abbreviation,
// an initial, a dotted acronym ("U.S", "e.g") or a list number ("2.").
func endsAbbreviation(before string) bool {
	start := strings.LastIndexFunc(before, unicode.IsSpace) + 1
	tok := strings.TrimLeft(before[start:], "([\"'“‘")
	if tok == "" {
		return false
	}
	if abbreviations[strings.ToLower(tok)] || (strings.ContainsRune(strings.Trim(tok, "."), '.') && strings.IndexFunc(tok, unicode.IsLetter) >= 0) {
		return true
	}
	if utf8.RuneCountInString(tok) == 1 {
		r, _ := utf8.DecodeRuneInString(tok)
		return unicode.IsLetter(r) && r != 'I' // "I." ends sentences, "J. Smith" does not
	}
	if strings.Trim(tok, "0123456789") == "" {
		return strings.TrimSpace(before[:start]) == "" || strings.HasSuffix(strings.TrimRight(before[:start], " \t"), "\n")
	}
	return false
}

// speakable reports whether s has anything a TTS can pronounce.
func speakable(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

// continuesLowercase reports whether the next word starts lowercase, which
// means the sentence went on past a closing quote. At the end of the buffer it
// answers true (wait for more text) unless the text is complete.
func continuesLowercase(rest string, last bool) bool {
	rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
	if rest == "" {
		return !last
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsLower(r)
}
