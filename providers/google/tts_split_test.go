package google

import (
	"strings"
	"testing"
)

// A split never yields a whitespace-only piece (TTS rejects empty input).
func TestSplitUnitSkipsBlankPieces(t *testing.T) {
	text := strings.Repeat("a", 250) + "  "
	for _, piece := range splitUnit(text, 250) {
		if strings.TrimSpace(piece) == "" {
			t.Fatalf("blank piece in %q", splitUnit(text, 250))
		}
	}
	if got := splitUnit("hello world", 250); len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("short unit split: %q", got)
	}
}
