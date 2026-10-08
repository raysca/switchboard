package core

import (
	"strings"
	"time"
	"unicode/utf8"
)

// SlackMergeConfig turns on slack-aware unit merging for UNARY TTS providers
// (xAI HTTP, Google), where a unit's whole audio returns before playback of it
// starts. After the first unit, sentences are held and merged while a request
// sent now would still finish at least Margin before the queued audio runs
// out; the merged unit goes out at the last safe moment, at Cap, or at reply
// end. Fewer requests, same characters (docs/benchmarks/tts-unit-sizing.md).
type SlackMergeConfig struct {
	Overhead    time.Duration // seed for per-request overhead; learned online
	CharsPerSec float64       // synthesis rate (fixed)
	Margin      time.Duration // required headroom before the audio runs out
	Cap         int           // max runes per merged unit (the provider limit)
	Now         func() time.Time
}

// SlackDefaults returns the seeds for a unary provider (ok false for others).
func SlackDefaults(provider string) (c SlackMergeConfig, ok bool) {
	c = SlackMergeConfig{Margin: 300 * time.Millisecond, Cap: 250}
	switch provider {
	case "xai":
		c.Overhead, c.CharsPerSec = 800*time.Millisecond, 120
	case "google":
		c.Overhead, c.CharsPerSec = 800*time.Millisecond, 118
	default:
		return c, false
	}
	return c, true
}

type slackUnit struct {
	at    time.Time
	chars int
	media time.Duration
	seen  bool
}

type slackOut struct {
	text  string
	final bool
}

// slackMerger holds the estimator (kept across replies) and the per-reply queue model.
type slackMerger struct {
	cfg      SlackMergeConfig
	overhead float64 // seconds, EWMA
	apc      float64 // seconds of audio per char, EWMA

	pending             string
	synthFree, audioEnd time.Time
	units               map[uint64]*slackUnit
}

func newSlackMerger(c SlackMergeConfig) *slackMerger {
	if c.Now == nil {
		c.Now = time.Now
	}
	return &slackMerger{cfg: c, overhead: c.Overhead.Seconds(), apc: 0.06, units: map[uint64]*slackUnit{}}
}

func (m *slackMerger) reset() {
	m.pending, m.synthFree, m.audioEnd = "", time.Time{}, time.Time{}
	m.units = map[uint64]*slackUnit{}
}

func (m *slackMerger) synth(chars int) time.Duration {
	return time.Duration((m.overhead + float64(chars)/m.cfg.CharsPerSec) * float64(time.Second))
}

func (m *slackMerger) start() time.Time {
	if now := m.cfg.Now(); !m.synthFree.After(now) {
		return now
	}
	return m.synthFree
}

// late reports whether sending chars now would finish less than Margin before
// the queued audio runs out.
func (m *slackMerger) late(chars int) bool {
	return m.start().Add(m.synth(chars) + m.cfg.Margin).After(m.audioEnd)
}

// sent updates the queue model for a unit just sent.
func (m *slackMerger) sent(id uint64, chars int) {
	m.units[id] = &slackUnit{at: m.cfg.Now(), chars: chars}
	m.synthFree = m.start().Add(m.synth(chars))
	if m.synthFree.After(m.audioEnd) {
		m.audioEnd = m.synthFree
	}
	m.audioEnd = m.audioEnd.Add(time.Duration(float64(chars) * m.apc * float64(time.Second)))
}

// observe learns from played audio. ponytail: only the reply's first unit
// times the overhead (the provider is idle then, so first write ~ ready); the
// rate stays at its seed. Fit the rate by regression if the seed proves off.
func (m *slackMerger) observe(id, firstID uint64, media time.Duration, final bool) {
	u := m.units[id]
	if u == nil {
		return
	}
	const alpha = 0.3
	u.media += media
	if !u.seen {
		u.seen = true
		if id == firstID {
			obs := m.cfg.Now().Sub(u.at).Seconds() - float64(u.chars)/m.cfg.CharsPerSec
			m.overhead += alpha * (min(max(obs, 0), 5) - m.overhead)
		}
	}
	if final && u.chars > 0 && u.media > 0 {
		m.apc += alpha * (u.media.Seconds()/float64(u.chars) - m.apc)
	}
}

// admit takes one sentence-level unit and returns the units to send now.
// first: no unit has been sent this reply. final: it ends the reply.
func (m *slackMerger) admit(text string, first, final bool) []slackOut {
	if first {
		return []slackOut{{text, final}}
	}
	var out []slackOut
	merged := text
	if m.pending != "" {
		merged = m.pending + " " + text
		if n := utf8.RuneCountInString(merged); n > m.cfg.Cap || m.late(n) {
			out = append(out, slackOut{m.pending, false})
			merged = text
		}
	}
	m.pending = merged
	if n := utf8.RuneCountInString(merged); final || n >= m.cfg.Cap || m.late(n) {
		out = append(out, slackOut{merged, final})
		m.pending = ""
	}
	return out
}

// due flushes the held text when it must go now (last safe moment) or, with
// end set, because the reply is over.
func (m *slackMerger) due(end bool) []slackOut {
	if m.pending == "" || !(end || m.late(utf8.RuneCountInString(m.pending))) {
		return nil
	}
	t := strings.TrimSpace(m.pending)
	m.pending = ""
	return []slackOut{{t, end}}
}
