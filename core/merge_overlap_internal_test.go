package core

import "testing"

func TestMergeOverlap(t *testing.T) {
	for _, tc := range []struct{ carry, text, want string }{
		{"book a table", "a table for two", "book a table for two"},
		{"I said no no", "no no no stop", "I said no no no stop"}, // longest seam wins
		{"I said no no no", "no no no stop", "I said no no no stop"},
		{"call 555 1234", "1234 now", "call 555 1234 now"},
		{"Book a table.", "table for two", "Book a table. for two"}, // punctuation and case ignored at the seam
		{"Hello, World", "world how are you", "Hello, World how are you"},
		{"bonjour à tous", "à tous les amis", "bonjour à tous les amis"},
		{"one two", "three four", "one two three four"}, // no overlap: gap kept
		{"", "three", "three"},
	} {
		if got := mergeOverlap(tc.carry, tc.text); got != tc.want {
			t.Errorf("mergeOverlap(%q, %q) = %q, want %q", tc.carry, tc.text, got, tc.want)
		}
	}
}
