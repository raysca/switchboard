package api

import "testing"

func TestMedianReplyMS(t *testing.T) {
	v2v := func(n string) string { return `{"voice_to_voice_ms":` + n + `}` }
	typed := `{"llm_ttft_ms":300,"tts_ttfb_ms":200}`
	cases := []struct {
		name string
		in   []string
		want int // -1 = absent
	}{
		{"none", nil, -1},
		{"empty objects", []string{`{}`, `{}`}, -1},
		{"one spoken turn", []string{v2v("900")}, 900},
		{"odd count", []string{v2v("1000"), v2v("500"), v2v("700")}, 700},
		{"even count is the mean of the middle two", []string{v2v("500"), v2v("700"), v2v("900"), v2v("1100")}, 800},
		{"a typed turn counts model plus voice", []string{typed}, 500},
		{"spoken and typed mix", []string{v2v("900"), typed, v2v("100")}, 500},
		{"only part of a typed turn does not count", []string{`{"llm_ttft_ms":300}`, v2v("400")}, 400},
		{"malformed rows are skipped", []string{`not json`, v2v("600")}, 600},
		{"rounding", []string{v2v("1"), v2v("2")}, 2},
	}
	for _, c := range cases {
		got := medianReplyMS(c.in)
		switch {
		case c.want == -1 && got != nil:
			t.Errorf("%s: want absent, got %d", c.name, *got)
		case c.want != -1 && (got == nil || *got != c.want):
			t.Errorf("%s: want %d, got %v", c.name, c.want, got)
		}
	}
}
