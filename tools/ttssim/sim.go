// Command ttssim is an offline simulator for choosing TTS unit-sizing parameters for UNARY
// providers (xAI HTTP, Google): one request per unit, the whole unit's audio returned before
// playback of it can start. No network, no keys. See docs/benchmarks/tts-unit-sizing.md.
package main

import (
	"math/rand"
	"strings"
	"unicode/utf8"

	"govoice/core"
)

// Provider models synthesis latency = OverheadMS + chars/Rate, requests strictly in order.
type Provider struct {
	Name       string
	OverheadMS float64
	Rate       float64 // chars synthesized per second (excluding overhead)
	AudioPerCh float64 // seconds of speech per char (measured: ~0.062)
	MaxChars   int     // provider text limit per request
}

func (p Provider) synth(chars int) float64 { return p.OverheadMS/1000 + float64(chars)/p.Rate }
func (p Provider) audio(chars int) float64 { return float64(chars) * p.AudioPerCh }

// Strategy decides unit boundaries. Min/FirstClause feed the real splitter; Slack enables the
// deadline-aware merger on top of it.
type Strategy struct {
	Name        string
	Min         int // splitter min runes before a terminator (production MinUnitLength)
	FirstClause int // production FirstClauseMinLength; 0 off
	Slack       bool
	MarginMS    float64 // slack: required headroom between a unit's ready time and the audio running out
	CapChars    int     // slack: max merged unit chars (0 = provider limit)
}

// LLM streams text at CPS after TTFT; deltas arrive every DeltaMS (a few tokens).
type LLM struct{ CPS, TTFTMS, DeltaMS float64 }

// Result is one simulated reply. Times are seconds from the end of the user's turn.
type Result struct {
	FirstAudio float64
	Gaps       int     // audible gaps (> GapMinS) between units
	GapS       float64 // total silence inserted between units
	Requests   int
	Chars      int
	End        float64 // playback end
}

const GapMinS = 0.1

var pool = []string{
	"Sure.", "Of course!", "Got it.", "Absolutely, I can help with that.", "Let me check on that for you.",
	"Your order shipped yesterday and should arrive on Friday.", "The meeting is at three o'clock tomorrow afternoon.",
	"I found two flights that match your dates, one leaving at nine and one at noon.",
	"Would you like me to book the earlier one or keep looking?",
	"The cheaper fare doesn't include a checked bag, but the other one does and costs about forty dollars more.",
	"Just to confirm, you want to move the appointment from Tuesday to Thursday morning, correct?",
	"It's currently sixty eight degrees and sunny, with a light breeze from the west.",
	"There are a few options here, and the best one depends on how much time you have and whether you need it in writing.",
	"Thanks for waiting.", "Is there anything else I can do for you today?",
	"I'm sorry about that, let me fix it right away.",
	"First, open the settings page; second, choose the billing tab; and finally, press the update button next to your card.",
	"That should take about five minutes.",
}

// Reply builds a 2-6 sentence reply.
func Reply(r *rand.Rand) string {
	n := 2 + r.Intn(5)
	s := make([]string, n)
	for i := range s {
		s[i] = pool[r.Intn(len(pool))]
	}
	return strings.Join(s, " ")
}

// Run simulates one reply under strategy st on provider p with LLM l.
func Run(text string, l LLM, p Provider, st Strategy) Result {
	const dt = 0.01
	var (
		res        Result
		buf        string
		fed        int // runes delivered
		runes      = []rune(text)
		pending    string // slack: merged text not yet sent
		firstSent  bool
		synthFree  float64 // when the provider finishes its queue
		audioEnd   float64 // when queued audio finishes playing
		started    bool
		cap        = st.CapChars
		margin     = st.MarginMS / 1000
		nextDeltaT = l.TTFTMS / 1000
	)
	if cap <= 0 || cap > p.MaxChars {
		cap = p.MaxChars
	}
	if len(runes) == 0 {
		return res // nothing would ever be fed; the loop below would not end
	}
	send := func(now float64, u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		c := utf8.RuneCountInString(u)
		pieces := (c + p.MaxChars - 1) / p.MaxChars // a unit over the provider limit is split into requests
		for i := 0; i < pieces; i++ {
			pc := c / pieces
			if i < c%pieces {
				pc++ // keep every char counted
			}
			start := maxf(now, synthFree)
			ready := start + p.synth(pc)
			synthFree = ready
			if !started {
				started, res.FirstAudio, audioEnd = true, ready, ready
			} else if ready > audioEnd {
				if ready-audioEnd > GapMinS {
					res.Gaps++
				}
				res.GapS += ready - audioEnd
				audioEnd = ready
			}
			audioEnd += p.audio(pc)
			res.Requests++
			res.Chars += pc
		}
		res.End = audioEnd
	}
	// estimate when `chars` sent at `now` would be ready, and the queued audio's end
	readyEst := func(now float64, chars int) float64 { return maxf(now, synthFree) + p.synth(chars) }
	flush := func(now float64) {
		if pending != "" {
			send(now, pending)
			pending = ""
		}
	}
	candidate := func(now float64, u string) { // a sentence-level unit from the splitter
		if !st.Slack {
			send(now, u)
			firstSent = true
			return
		}
		if !firstSent { // first unit: as soon as the splitter yields it
			send(now, u)
			firstSent = true
			return
		}
		merged := strings.TrimSpace(pending + " " + u)
		mc := utf8.RuneCountInString(merged)
		if pending != "" && (mc > cap || readyEst(now, mc)+margin > audioEnd) {
			flush(now) // merging would stall playback (or bust the cap): send what we have
			merged = u
		}
		pending = merged
		if readyEst(now, utf8.RuneCountInString(pending))+margin > audioEnd {
			flush(now) // even alone it is already late
		}
	}
	drain := func(now float64, last bool) {
		for buf != "" {
			i := core.SplitNext(buf, st.Min, 150, st.FirstClause, !firstSent, last)
			if i < 0 {
				break
			}
			u := strings.TrimSpace(buf[:i])
			buf = buf[i:]
			if u != "" {
				candidate(now, u)
			}
		}
	}
	for now := 0.0; ; now += dt {
		if fed < len(runes) && now >= nextDeltaT {
			n := int(l.CPS * l.DeltaMS / 1000)
			if n < 1 {
				n = 1
			}
			if fed+n > len(runes) {
				n = len(runes) - fed
			}
			buf += string(runes[fed : fed+n])
			fed += n
			nextDeltaT += l.DeltaMS / 1000
			last := fed >= len(runes)
			drain(now, last)
			if last {
				flush(now)
				return res
			}
		}
		// slack: send pending at the last safe moment instead of waiting for the next sentence
		if st.Slack && pending != "" && firstSent && readyEst(now, utf8.RuneCountInString(pending))+margin+0.5*dt >= audioEnd {
			flush(now)
		}
	}
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
