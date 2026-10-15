package api

import (
	"encoding/json"
	"slices"
)

// medianReplyMS is the median reply time over a call's turns, from each turn's stored latencies: the
// voice-to-voice time of a spoken turn, else model plus voice for a typed one. Turns with neither (and
// malformed rows) are skipped; nil when no turn was measured. The UI's Call detail uses the same rule.
func medianReplyMS(latencies []string) *int {
	var times []int
	for _, raw := range latencies {
		var l struct {
			V2V  *int `json:"voice_to_voice_ms"`
			TTFT *int `json:"llm_ttft_ms"`
			TTFB *int `json:"tts_ttfb_ms"`
		}
		if json.Unmarshal([]byte(raw), &l) != nil {
			continue
		}
		switch {
		case l.V2V != nil:
			times = append(times, *l.V2V)
		case l.TTFT != nil && l.TTFB != nil:
			times = append(times, *l.TTFT+*l.TTFB)
		}
	}
	if len(times) == 0 {
		return nil
	}
	slices.Sort(times)
	mid := len(times) / 2
	m := times[mid]
	if len(times)%2 == 0 {
		m = (times[mid-1] + times[mid] + 1) / 2
	}
	return &m
}
