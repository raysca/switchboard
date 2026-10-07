// Package dtmf is provider-neutral DTMF support: a Goertzel detector for
// inbound PCM16 audio, a dual-tone generator for outbound digits, and the
// pipeline processors and send_dtmf tool built on them. Pure Go, no cgo.
package dtmf

import "strings"

// Digits is every valid keypad symbol (the 4x4 matrix including A-D).
const Digits = "0123456789*#ABCD"

var (
	lowFreqs  = [4]float64{697, 770, 852, 941}
	highFreqs = [4]float64{1209, 1336, 1477, 1633}
	keypad    = [4][4]byte{
		{'1', '2', '3', 'A'},
		{'4', '5', '6', 'B'},
		{'7', '8', '9', 'C'},
		{'*', '0', '#', 'D'},
	}
)

// Valid reports whether b is a DTMF symbol.
func Valid(b byte) bool { return strings.IndexByte(Digits, b) >= 0 }

// digitFreqs returns the low/high frequencies of a valid digit.
func digitFreqs(b byte) (lo, hi float64, ok bool) {
	for r := range keypad {
		for c := range keypad[r] {
			if keypad[r][c] == b {
				return lowFreqs[r], highFreqs[c], true
			}
		}
	}
	return 0, 0, false
}
