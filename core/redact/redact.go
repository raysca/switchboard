// Package redact masks sensitive content (card/SSN/account numbers, PIN and
// code contexts, DTMF digits) in text that is about to be stored or exported.
// It is applied at sinks only, never to the live LLM context.
//
// Precision over recall: every rule is a Go RE2 pattern (linear time, no
// backtracking) plus, where a cheap checksum exists, a validation step.
package redact

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"govoice/core/audio/dtmf"
)

// Config is the privacy.redaction policy.
type Config struct {
	// Enabled turns on the text rules. DTMF masking is independent of it.
	Enabled bool `yaml:"enabled"`
	// Rules selects text rules by name; empty means DefaultRules. email and
	// phone are only active when named.
	Rules []string `yaml:"rules"`
	// DTMF is "always" (default) or "off".
	DTMF string `yaml:"dtmf"`
}

// DefaultRules are the text rules used when Config.Rules is empty.
var DefaultRules = []string{"pin", "ssn", "iban", "card", "digits"}

const (
	tok   = `(?:(?:double|triple)\s+)?(?:zero|oh|one|two|three|four|five|six|seven|eight|nine|\d)`
	sep   = `[ ,.\-]+`
	spoke = `\b` + tok + `(?:` + sep + tok + `){8,}\b`
)

type rule struct {
	kind  string
	re    *regexp.Regexp
	valid func(match, group string) bool // nil = every match is masked
	group int                            // submatch to mask; 0 = whole match
}

var rules = map[string]rule{
	"pin": {kind: "pin", group: 1, valid: notOwnPlaceholder, re: regexp.MustCompile(
		`(?i)(?:\bREDACTED:)?\b(?:pin|cvv2?|cvc|passcode|otp|one[- ]time password|(?:my|the|your|a|this|that|access|door|gate|security|verification|confirmation|auth(?:entication)?|one[- ]time) code)\b[^0-9\n[]{0,24}?\b(\d{3,8}\b|` +
			tok + `(?:` + sep + tok + `){2,7}\b)`)},
	"ssn":  {kind: "ssn", re: regexp.MustCompile(`\b\d{3}[ -]\d{2}[ -]\d{4}\b`), valid: validSSN},
	"iban": {kind: "iban", re: regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){3,7}(?: ?[A-Z0-9]{1,3})?\b`), valid: validIBAN},
	"card": {kind: "card", re: regexp.MustCompile(`\b\d(?:[ .-]?\d){12,18}\b`), valid: validCard},
	// digits: contiguous run of 9+ digits, or 9+ spoken/single digit tokens.
	"digits": {kind: "digits", re: regexp.MustCompile(`\b\d{9,}\b|(?i)` + spoke)},
	"email":  {kind: "email", re: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)},
	"phone":  {kind: "phone", re: regexp.MustCompile(`(?:\+?\d{1,3}[ .-]?)?\(?\d{3}\)?[ .-]\d{3}[ .-]\d{4}\b`)},
}

// ruleOrder is the application order (most specific first).
var ruleOrder = []string{"pin", "ssn", "iban", "card", "digits", "email", "phone"}

// dtmfRe masks the keys after a "DTMF:" marker; the key class is the whole
// keypad alphabet (dtmf.Digits, A-D included), upper case like dtmf.Valid.
var dtmfRe = regexp.MustCompile(`(?i:\bDTMF:)[ \t]*[` + dtmf.Digits + `][` + dtmf.Digits + ` \-]*`)

// Redactor is immutable and safe for concurrent use. A nil *Redactor is a
// no-op.
type Redactor struct {
	rules []rule
	dtmf  bool
}

// New validates cfg and builds a Redactor.
func New(cfg Config) (*Redactor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	r := &Redactor{dtmf: cfg.DTMF != "off"}
	if cfg.Enabled {
		names := cfg.Rules
		if len(names) == 0 {
			names = DefaultRules
		}
		on := map[string]bool{}
		for _, n := range names {
			on[n] = true
		}
		for _, n := range ruleOrder {
			if on[n] {
				r.rules = append(r.rules, rules[n])
			}
		}
	}
	return r, nil
}

// Validate rejects unknown rule names and dtmf modes.
func (c Config) Validate() error {
	switch c.DTMF {
	case "", "always", "off":
	default:
		return fmt.Errorf("privacy.redaction.dtmf must be always or off, got %q", c.DTMF)
	}
	for _, n := range c.Rules {
		if _, ok := rules[n]; !ok {
			return fmt.Errorf("privacy.redaction.rules: unknown rule %q", n)
		}
	}
	return nil
}

// Redact returns s with sensitive spans replaced by [REDACTED:kind]. DTMF
// transcripts keep their marker and key count: "DTMF: [REDACTED:dtmf 4]".
func (r *Redactor) Redact(s string) string {
	if r == nil || s == "" {
		return s
	}
	if r.dtmf {
		s = dtmfRe.ReplaceAllStringFunc(s, func(m string) string {
			i := strings.IndexByte(m, ':')
			keys := 0
			for _, c := range m[i:] {
				if c != '#' && c < 0x80 && dtmf.Valid(byte(c)) {
					keys++
				}
			}
			head := m[:i+1]
			return head + " [REDACTED:dtmf " + strconv.Itoa(keys) + "]"
		})
	}
	if len(r.rules) == 0 {
		return s
	}
	s = fold(s)
	// A rejected match (e.g. Luhn-invalid) hides valid spans inside it until
	// a neighbour is masked, so repeat to a fixpoint: Redact is idempotent.
	// Every pass that changes s removes digits/'@', so this terminates.
	for range 8 {
		prev := s
		for _, ru := range r.rules {
			s = ru.apply(s)
		}
		if s == prev {
			break
		}
	}
	return s
}

// fold maps non-ASCII decimal digits (Arabic-Indic, fullwidth, ...) to ASCII
// and drops invisible characters (zero-width, bidi controls, soft hyphen) so
// they cannot split a number past the rules. The stored text changes only in
// those characters.
func fold(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return strings.Map(func(c rune) rune {
				switch {
				case c < 0x80:
					return c
				case c == 0xAD || c == 0x200B || c == 0x200E || c == 0x200F || c == 0xFEFF ||
					c >= 0x202A && c <= 0x202E || c >= 0x2060 && c <= 0x2069:
					return -1
				case unicode.IsDigit(c):
					return '0' + digitValue(c)
				}
				return c
			}, s)
		}
	}
	return s
}

// digitValue: Unicode Nd digits come in runs of whole 0-9 blocks.
func digitValue(c rune) rune {
	z := c
	for unicode.IsDigit(z - 1) {
		z--
	}
	return (c - z) % 10
}

func (ru rule) apply(s string) string {
	idx := ru.re.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		lo, hi := m[2*ru.group], m[2*ru.group+1]
		if lo < last || lo < 0 || (ru.valid != nil && !ru.valid(s[m[0]:m[1]], s[lo:hi])) {
			continue
		}
		b.WriteString(s[last:lo])
		b.WriteString("[REDACTED:" + ru.kind + "]")
		last = hi
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func digitsOf(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// notOwnPlaceholder keeps Redact idempotent: "[REDACTED:pin]" contains the
// pin keyword, so the regex consumes an optional "REDACTED:" prefix (RE2 has
// no lookbehind) and such matches are skipped.
func notOwnPlaceholder(match, _ string) bool { return !strings.HasPrefix(match, "REDACTED:") }

func validCard(_, s string) bool {
	d := digitsOf(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum, dbl := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if dbl {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
		dbl = !dbl
	}
	return sum%10 == 0
}

func validSSN(_, s string) bool {
	d := digitsOf(s)
	area := d[:3]
	return area != "000" && area != "666" && area[0] != '9' && d[3:5] != "00" && d[5:] != "0000"
}

func validIBAN(_, s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	s = s[4:] + s[:4]
	rem := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A') + 10) % 97
		default:
			return false
		}
	}
	return rem == 1
}
