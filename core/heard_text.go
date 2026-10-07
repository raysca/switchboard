package core

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MediaDuration is the audio length of f: Duration when set, else derived from
// PCM16 samples, else 0.
func (f *OutputAudioFrame) MediaDuration() time.Duration {
	if f.Duration > 0 {
		return f.Duration
	}
	if f.SampleRate > 0 && f.Channels > 0 {
		return time.Duration(len(f.Samples)/(2*f.Channels)) * time.Second / time.Duration(f.SampleRate)
	}
	return 0
}

// estimatedCharsPerSecond is normal speech (~180 wpm) and is used for a unit
// whose total audio length is not yet known and no earlier unit calibrates it.
const estimatedCharsPerSecond = 15.0

// HeardText answers "what text has the listener actually heard?" for the
// synthesis units of one assistant turn.
//
// "Played" is the media time the paced writer has released (the sum of
// successfully written frame durations). That is the best signal the server
// has: it can lead true client playback by the client's own buffer, so the
// result is an upper bound of what was heard.
//
// Not safe for concurrent use; the AssistantContextProcessor owns one.
type HeardText struct {
	units map[uint64]*heardUnit
	chars int           // runes of finished units, for the speaking-rate estimate
	dur   time.Duration // their audio duration
}

type heardUnit struct {
	text    string
	timings []WordTiming
	played  time.Duration
	done    bool // final frame written: played is the unit's total length
}

func NewHeardText() *HeardText { return &HeardText{units: make(map[uint64]*heardUnit)} }

// Reset forgets every unit (new turn).
func (h *HeardText) Reset() {
	h.units = make(map[uint64]*heardUnit)
	h.chars, h.dur = 0, 0
}

// AddUnit registers the text sent to TTS for unit id.
func (h *HeardText) AddUnit(id uint64, text string) { h.units[id] = &heardUnit{text: text} }

// Played records one released audio frame of unit id: its duration, any word
// timings it carried (offsets from the unit's start) and whether it was the
// unit's last frame.
func (h *HeardText) Played(id uint64, media time.Duration, timings []WordTiming, final bool) {
	u := h.units[id]
	if u == nil {
		return
	}
	u.played += media
	u.timings = append(u.timings, timings...)
	if final && !u.done {
		u.done = true
		h.chars += utf8.RuneCountInString(u.text)
		h.dur += u.played
	}
}

// Elapsed is the media time released so far for unit id.
func (h *HeardText) Elapsed(id uint64) time.Duration {
	if u := h.units[id]; u != nil {
		return u.played
	}
	return 0
}

// Heard is HeardPrefix at the unit's own released media time.
func (h *HeardText) Heard(id uint64) string { return h.HeardPrefix(id, h.Elapsed(id)) }

// HeardPrefix returns the leading part of unit id's text that is spoken by
// playedDuration into its audio, always ending on a word boundary. It uses the
// provider's word timings when present and otherwise estimates proportionally
// to characters.
func (h *HeardText) HeardPrefix(id uint64, played time.Duration) string {
	u := h.units[id]
	if u == nil || played <= 0 {
		return ""
	}
	if u.done && played >= u.played {
		return u.text
	}
	if len(u.timings) > 0 {
		return timedPrefix(u.text, u.timings, played)
	}
	return h.proportionalPrefix(u, played)
}

// timedPrefix keeps words that have finished, or are at least half spoken.
func timedPrefix(text string, timings []WordTiming, played time.Duration) string {
	cursor, end := 0, 0
	for _, w := range timings {
		if w.Start >= played || (w.End > played && played-w.Start < (w.End-w.Start)/2) {
			break
		}
		if at := findWord(text, cursor, w.Word); at >= 0 {
			cursor = at + len(w.Word)
			end = cursor
		} // else the provider tokenised differently ("5" spoken as "five"): skip, never guess
	}
	if end == 0 {
		return ""
	}
	// Finish the word and keep punctuation glued to it (CJK: punctuation only).
	for end < len(text) {
		r, w := utf8.DecodeRuneInString(text[end:])
		if unicode.IsSpace(r) || (!spaceDelimited(r) && (unicode.IsLetter(r) || unicode.IsDigit(r))) {
			break
		}
		end += w
	}
	return strings.TrimSpace(text[:end])
}

// findWord returns the byte index of word in text at or after from, or -1.
// The match must start a word (so "a" is not found inside "cat"), may differ
// in ASCII case ("the" matches "The"), and may skip at most two unheard words,
// so one misaligned token cannot jump the cursor to a far-off repeat.
func findWord(text string, from int, word string) int {
	if word == "" {
		return -1
	}
	hay, needle := asciiLower(text[from:]), asciiLower(word)
	for off := 0; ; {
		at := strings.Index(hay[off:], needle)
		if at < 0 {
			return -1
		}
		at += off
		if skipped := strings.FieldsFunc(hay[:at], func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }); len(skipped) > 2 {
			return -1
		}
		prev, _ := utf8.DecodeLastRuneInString(hay[:at])
		first, _ := utf8.DecodeRuneInString(needle)
		if at == 0 || !(unicode.IsLetter(prev) || unicode.IsDigit(prev)) || !spaceDelimited(first) {
			return from + at
		}
		_, w := utf8.DecodeRuneInString(hay[at:])
		off = at + w
	}
}

// spaceDelimited is false for scripts written without spaces between words.
func spaceDelimited(r rune) bool {
	return !unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai)
}

// asciiLower lower-cases ASCII letters only, so byte offsets are unchanged.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func (h *HeardText) proportionalPrefix(u *heardUnit, played time.Duration) string {
	runes := []rune(u.text)
	total := time.Duration(float64(len(runes)) / estimatedCharsPerSecond * float64(time.Second))
	if u.done {
		total = u.played
	} else if h.dur > 0 {
		total = time.Duration(float64(len(runes)) / (float64(h.chars) / h.dur.Seconds()) * float64(time.Second))
	}
	if total <= 0 || played >= total {
		return u.text
	}
	k := int(float64(len(runes)) * float64(played) / float64(total))
	for k > 0 && !unicode.IsSpace(runes[k]) && spaceDelimited(runes[k-1]) { // whole words only (CJK: any rune)
		k--
	}
	return strings.TrimSpace(string(runes[:k]))
}
