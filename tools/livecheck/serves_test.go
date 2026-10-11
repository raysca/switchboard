package main

import "testing"

// A profile naming an unknown, empty or oddly-cased provider gets a validation
// error, never a panic.
func TestServesToleratesUnknownProviders(t *testing.T) {
	for _, tc := range []struct {
		prov, stage string
		want        bool
	}{
		{"nope", "stt", false}, {"", "llm", false}, {"  Google ", "tts", true}, {"GOOGLE", "stt", true},
	} {
		if got := serves(tc.prov, tc.stage); got != tc.want {
			t.Errorf("serves(%q,%q)=%v want %v", tc.prov, tc.stage, got, tc.want)
		}
	}
	p := Profile{Name: "x"}
	p.Stages.STT.Provider = "nope"
	if err := p.validate(); err == nil {
		t.Fatal("unknown provider validated")
	}
}
