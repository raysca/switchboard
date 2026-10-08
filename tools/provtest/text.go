package provtest

import (
	"strings"
	"unicode"
)

var numWords = strings.Fields("zero one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty")

// contractions expands the common ones (whole words) so "what's" and "what is" match.
var contractions = map[string]string{"what's": "what is", "it's": "it is", "that's": "that is", "there's": "there is",
	"i'm": "i am", "you're": "you are", "we're": "we are", "they're": "they are", "isn't": "is not", "don't": "do not",
	"can't": "cannot", "won't": "will not", "let's": "let us"}

// Words normalises text for WER: lower case, "+" as plus, small digits (0-20)
// spelled out, common contractions expanded, hyphens and other punctuation
// dropped (other apostrophes kept).
func Words(s string) []string {
	s = strings.NewReplacer("’", "'", "‘", "'", "+", " plus ").Replace(strings.ToLower(s))
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' {
			return r
		}
		return ' '
	}, s)
	var out []string
	for _, w := range strings.Fields(s) {
		if w = strings.Trim(w, "'"); w == "" { // quotes, not apostrophes
			continue
		}
		if x, ok := contractions[w]; ok {
			out = append(out, strings.Fields(x)...)
			continue
		}
		for n, name := range numWords {
			if w == itoa(n) {
				w = name
			}
		}
		out = append(out, w)
	}
	return out
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// WER is the word error rate of hyp against ref (edit distance / len(ref)).
func WER(ref, hyp string) float64 {
	r, h := Words(ref), Words(hyp)
	if len(r) == 0 {
		if len(h) == 0 {
			return 0
		}
		return 1
	}
	d := make([]int, len(h)+1)
	for j := range d {
		d[j] = j
	}
	for i := 1; i <= len(r); i++ {
		prev := d[0]
		d[0] = i
		for j := 1; j <= len(h); j++ {
			cur := d[j]
			cost := 1
			if r[i-1] == h[j-1] {
				cost = 0
			}
			d[j] = min(d[j]+1, d[j-1]+1, prev+cost)
			prev = cur
		}
	}
	return float64(d[len(h)]) / float64(len(r))
}

// hasWordPrefix reports whether b's words start with a's words.
func hasWordPrefix(a, b string) bool {
	wa, wb := Words(a), Words(b)
	if len(wa) > len(wb) {
		return false
	}
	for i := range wa {
		if wa[i] != wb[i] {
			return false
		}
	}
	return true
}
