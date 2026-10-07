package redact

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func mk(t *testing.T, c Config) *Redactor {
	t.Helper()
	r, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRules(t *testing.T) {
	on := mk(t, Config{Enabled: true, Rules: append([]string{"email", "phone"}, DefaultRules...)})
	def := mk(t, Config{Enabled: true})
	cases := []struct {
		name, in, want string
		r              *Redactor
	}{
		{"card spaces", "card 4111 1111 1111 1111 ok", "card [REDACTED:card] ok", def},
		{"card dashes", "4111-1111-1111-1111", "[REDACTED:card]", def},
		{"card luhn-invalid spaced stays", "4111 1111 1111 1112", "4111 1111 1111 1112", def},
		{"card contiguous invalid caught by digits", "4111111111111112", "[REDACTED:digits]", def},
		{"ssn", "ssn 123-45-6789.", "ssn [REDACTED:ssn].", def},
		{"ssn invalid area stays", "000-12-3456", "000-12-3456", def},
		{"iban", "GB82 WEST 1234 5698 7654 32 thanks", "[REDACTED:iban] thanks", def},
		{"iban bad checksum stays", "GB83 WEST 1234 5698 7654 32", "GB83 WEST 1234 5698 7654 32", def},
		{"long digits", "account 123456789012", "account [REDACTED:digits]", def},
		{"spoken digits", "it is four one one one one one one one one", "it is [REDACTED:digits]", def},
		{"single digits", "I have 4 1 1 1 1 1 1 1 1 1 1 1 thanks", "I have [REDACTED:digits] thanks", def},
		{"pin spoken", "my pin is 1 2 3 4", "my pin is [REDACTED:pin]", def},
		{"pin digits", "CVV: 123", "CVV: [REDACTED:pin]", def},
		{"auth code words", "authentication code is five five two nine", "authentication code is [REDACTED:pin]", def},
		{"email on", "mail a.b@example.com now", "mail [REDACTED:email] now", on},
		{"phone on", "call (415) 555-0132", "call [REDACTED:phone]", on},
		{"card dots", "4111.1111.1111.1111", "[REDACTED:card]", def},
		{"arabic-indic digits", "٤١١١ ١١١١ ١١١١ ١١١١", "[REDACTED:card]", def},
		{"fullwidth digits", "４１１１ １１１１ １１１１ １１１１", "[REDACTED:card]", def},
		{"zero-width split", "4111\u200b1111\u200b1111\u202e1111", "[REDACTED:card]", def},
		{"otp code", "your code is 482913", "your code is [REDACTED:pin]", def},
		{"oh and double", "oh four double one one one one one one one", "[REDACTED:digits]", def},
		{"zip code stays", "zip code 90210, area code 415, error code 404", "zip code 90210, area code 415, error code 404", def},
		// negatives
		{"order number", "order 12345678 shipped", "order 12345678 shipped", def},
		{"phone not card", "call 415-555-0132 or 415 555 0132", "call 415-555-0132 or 415 555 0132", def},
		{"email off by default", "a@b.com", "a@b.com", def},
		{"year", "in 2024 and 1999", "in 2024 and 1999", def},
		{"timestamp", "at 2026-10-07 12:30:45", "at 2026-10-07 12:30:45", def},
		{"short counting", "one two three four five", "one two three four five", def},
		{"pin word only", "my pin is secret", "my pin is secret", def},
		{"unicode", "héllo 世界 🙂 4111 1111 1111 1111", "héllo 世界 🙂 [REDACTED:card]", def},
	}
	for _, c := range cases {
		if got := c.r.Redact(c.in); got != c.want {
			t.Errorf("%s: %q -> %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestDTMF(t *testing.T) {
	// text rules disabled: DTMF still masked by default.
	r := mk(t, Config{})
	if got, want := r.Redact("DTMF: 12#"), "DTMF: [REDACTED:dtmf 2]"; got != want {
		t.Fatalf("%q != %q", got, want)
	}
	if got := r.Redact("DTMF: 1234*5#"); got != "DTMF: [REDACTED:dtmf 6]" {
		t.Fatal(got)
	}
	if got := r.Redact("card 4111 1111 1111 1111"); !strings.Contains(got, "4111") {
		t.Fatal("text rules must be off:", got)
	}
	off := mk(t, Config{DTMF: "off"})
	if got := off.Redact("DTMF: 12#"); got != "DTMF: 12#" {
		t.Fatal(got)
	}
	var nilR *Redactor
	if nilR.Redact("DTMF: 1") != "DTMF: 1" {
		t.Fatal("nil must be a no-op")
	}
}

func TestValidate(t *testing.T) {
	if _, err := New(Config{Rules: []string{"nope"}}); err == nil {
		t.Fatal("unknown rule accepted")
	}
	if _, err := New(Config{DTMF: "sometimes"}); err == nil {
		t.Fatal("bad dtmf accepted")
	}
}

func TestLinearTime(t *testing.T) {
	r := mk(t, Config{Enabled: true, Rules: append([]string{"email", "phone"}, DefaultRules...)})
	for _, unit := range []string{"1 ", "4111", "one ", "a@", "pin ", "DTMF: 1", "GB82 ", "1-"} {
		in := strings.Repeat(unit, 1<<16/len(unit))
		start := time.Now()
		_ = r.Redact(in)
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%q x N took %v", unit, d)
		}
	}
}

func FuzzRedact(f *testing.F) {
	for _, s := range []string{"", "DTMF: 12#", "my pin is 1 2 3 4", "4111 1111 1111 1111", "\xff\xfe 123-45-6789", "GB82 WEST 1234 5698 7654 32", "pin 1234 and 5678", "pin 1 2 3 4 5 6 7 8 9 0"} {
		f.Add(s)
	}
	r, _ := New(Config{Enabled: true, Rules: append([]string{"email", "phone"}, DefaultRules...)})
	f.Fuzz(func(t *testing.T, s string) {
		out := r.Redact(s)
		if utf8.ValidString(s) && !utf8.ValidString(out) {
			t.Fatalf("invalid utf8 from valid input %q", s)
		}
		if again := r.Redact(out); again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, out, again)
		}
	})
}
