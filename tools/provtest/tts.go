package provtest

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"govoice/core"
)

const (
	pipelineRate = 24000                        // server defaultTTSRate
	frameBytes   = pipelineRate * 2 * 20 / 1000 // 20 ms PCM16 mono
	maxInterrupt = 300 * time.Millisecond
)

var ttsSentences = []string{
	"The quick brown fox jumps over the lazy dog.",
	"Please turn left at the next intersection.",
	"Thank you for calling, and have a wonderful day.",
}

const ttsLong = "Once upon a time a lighthouse keeper lived on a rocky island far from shore. Every evening he climbed the spiral stairs to light the great lamp, and every morning he wrote the weather in his journal. One night a storm rolled in from the north, and the waves rose higher than the old stone wall. He kept the lamp burning until the sky turned grey and the ships were safe in harbour."

func (r *run) ttsSuite() {
	base := settleGoroutines(200 * time.Millisecond)
	p, err := r.t.Build(Variant{})
	if err != nil {
		r.add("build", false, "%v", Redact(err.Error()))
		r.skip("synth", "build failed")
	} else {
		r.add("build", true, "")
		g, gerr := newRig(p)
		if gerr != nil {
			r.add("build", false, "rig: %v", gerr)
			return
		}
		r.ttsSynth(g)
		r.leakAfter(g, base)
	}
	r.ttsInterrupt()
	r.badCredential(func(g *rig) error {
		_ = g.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: g.gen(), UnitID: 1, Text: ttsSentences[0], IsFinal: true})
		waitFor(r.o.Timeout, func() bool {
			a, _ := frames[*core.OutputAudioFrame](g.sink)
			return len(g.errs()) > 0 || len(a) > 0
		})
		if e := g.errs(); len(e) > 0 {
			return e[0]
		}
		return nil
	})
}

type unitRun struct {
	post, first, stop time.Time
	frames            []*core.OutputAudioFrame
}

func (r *run) ttsSynth(g *rig) {
	if !r.spend(len(ttsSentences), "synth") {
		return
	}
	gen := g.gen()
	units := make([]unitRun, len(ttsSentences))
	t0 := time.Now()
	for i, s := range ttsSentences {
		id := uint64(i + 1)
		units[i].post = time.Now()
		if err := g.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: gen, UnitID: id, Text: s, IsFinal: i == len(ttsSentences)-1}); err != nil {
			r.add("synth", false, "post: %v", err)
			return
		}
		waitFor(r.o.Timeout, func() bool {
			st, _ := frames[*core.TTSStoppedFrame](g.sink)
			return len(st) > i || len(g.errs()) > 0
		})
		if len(g.errs()) > 0 {
			break
		}
	}
	total := time.Since(t0)
	time.Sleep(300 * time.Millisecond) // usage frames re-enter after the stop
	audio, at := frames[*core.OutputAudioFrame](g.sink)
	for i, f := range audio {
		if f.UnitID >= 1 && int(f.UnitID) <= len(units) {
			u := &units[f.UnitID-1]
			u.frames = append(u.frames, f)
			if u.first.IsZero() {
				u.first = at[i]
			}
		}
	}
	stops, stopAt := frames[*core.TTSStoppedFrame](g.sink)
	for i, s := range stops {
		if s.UnitID >= 1 && int(s.UnitID) <= len(units) {
			units[s.UnitID-1].stop = stopAt[i]
		}
	}
	errs := g.errs()
	r.add("no_error", len(errs) == 0, "%d error frames; first: %v", len(errs), firstErr(errs))
	if len(audio) == 0 {
		r.add("synth", false, "no audio frames from %d units", len(units))
		return
	}
	r.add("synth", true, "%d frames", len(audio))

	// format: PCM16 mono at the pipeline rate, 20 ms framing.
	var bad []string
	for i, u := range units {
		for j, f := range u.frames {
			last := j == len(u.frames)-1
			switch {
			case f.Encoding != core.EncodingPCM16LE && f.Encoding != "":
				bad = append(bad, fmt.Sprintf("unit%d encoding %q", i+1, f.Encoding))
			case f.SampleRate != pipelineRate || f.Channels != 1:
				bad = append(bad, fmt.Sprintf("unit%d %d Hz %d ch", i+1, f.SampleRate, f.Channels))
			case len(f.Samples)%2 != 0 || len(f.Samples) == 0:
				bad = append(bad, fmt.Sprintf("unit%d frame %d has %d bytes", i+1, j, len(f.Samples)))
			case !last && len(f.Samples) != frameBytes, last && len(f.Samples) > frameBytes:
				bad = append(bad, fmt.Sprintf("unit%d frame %d is %d bytes (want %d)", i+1, j, len(f.Samples), frameBytes))
			}
		}
	}
	r.add("format", len(bad) == 0, "PCM16 mono %d Hz, %d-byte (20 ms) frames; violations: %.200s", pipelineRate, frameBytes, strings.Join(bad, "; "))

	// final / turn-final flags and ordering.
	bad = nil
	for i, u := range units {
		finals := 0
		for j, f := range u.frames {
			if f.IsFinal {
				finals++
				if j != len(u.frames)-1 {
					bad = append(bad, fmt.Sprintf("unit%d IsFinal on frame %d of %d", i+1, j, len(u.frames)))
				}
			}
			if f.TurnFinal && !(i == len(units)-1 && f.IsFinal) {
				bad = append(bad, fmt.Sprintf("unit%d TurnFinal on a non-final frame/unit", i+1))
			}
		}
		if len(u.frames) > 0 && finals != 1 {
			bad = append(bad, fmt.Sprintf("unit%d has %d IsFinal frames", i+1, finals))
		}
		if len(u.frames) == 0 {
			bad = append(bad, fmt.Sprintf("unit%d has no audio", i+1))
		}
	}
	if n := len(units[len(units)-1].frames); n > 0 && !units[len(units)-1].frames[n-1].TurnFinal {
		bad = append(bad, "last unit's final frame is not TurnFinal")
	}
	r.add("final_flags", len(bad) == 0, "one IsFinal per unit as its last frame; TurnFinal only on the turn's last; violations: %.200s", strings.Join(bad, "; "))

	// TTSStopped per unit, after that unit's audio.
	bad = nil
	for i, u := range units {
		switch {
		case u.stop.IsZero():
			bad = append(bad, fmt.Sprintf("unit%d no TTSStopped", i+1))
		case !u.first.IsZero() && u.stop.Before(u.first):
			bad = append(bad, fmt.Sprintf("unit%d TTSStopped before audio", i+1))
		}
	}
	r.add("stopped", len(bad) == 0, "TTSStopped after each unit's audio; violations: %.200s", strings.Join(bad, "; "))

	// usage: characters billed per unit.
	us, _ := frames[*core.UsageMetricsFrame](g.sink)
	var chars, want int64
	for _, s := range ttsSentences {
		want += int64(utf8.RuneCountInString(s))
	}
	for _, u := range us {
		if u.Service == "tts" && u.Characters != nil {
			chars += *u.Characters
		}
	}
	r.metric("usage_chars", chars == want, float64(chars), "chars", "usage characters %d, text characters %d", chars, want)

	// time to first audio per unit; the first is the cold one.
	var ttfa []float64
	for _, u := range units {
		if !u.first.IsZero() {
			ttfa = append(ttfa, float64(u.first.Sub(u.post).Milliseconds()))
		}
	}
	if len(ttfa) > 0 {
		r.metric("ttfa", slices.Max(ttfa) < 10000, ttfa[0], "ms", "per unit ms %v (value = first, cold); plausible < 10000", ttfa)
	}

	// audio duration vs text length; non-silent; no WAV header.
	bad = nil
	var totalAudio time.Duration
	var silent, riff []string
	for i, u := range units {
		var pcm []byte
		for _, f := range u.frames {
			pcm = append(pcm, f.Samples...)
		}
		d := time.Duration(len(pcm)/2) * time.Second / pipelineRate
		totalAudio += d
		perChar := d.Seconds() / float64(utf8.RuneCountInString(ttsSentences[i]))
		if perChar < 0.02 || perChar > 0.25 {
			bad = append(bad, fmt.Sprintf("unit%d %.2fs for %d chars", i+1, d.Seconds(), utf8.RuneCountInString(ttsSentences[i])))
		}
		if rms := RMS(pcm); rms < 100 {
			silent = append(silent, fmt.Sprintf("unit%d rms %.0f", i+1, rms))
		}
		if len(pcm) >= 4 && string(pcm[:4]) == "RIFF" {
			riff = append(riff, fmt.Sprintf("unit%d", i+1))
		}
	}
	r.metric("duration_plausible", len(bad) == 0, totalAudio.Seconds(), "s", "%.2fs of audio for %d chars (0.02-0.25 s/char); violations: %.160s", totalAudio.Seconds(), totalChars(), strings.Join(bad, "; "))
	r.add("non_silent", len(silent) == 0, "rms >= 100/32768 per unit; %s", strings.Join(silent, "; "))
	r.add("no_riff", len(riff) == 0, "raw PCM, no WAV header; RIFF in %v", riff)
	r.metric("total_time", total < 60*time.Second, total.Seconds(), "s", "3 sentences in %v for %.2fs of audio", total.Round(time.Millisecond), totalAudio.Seconds())
}

func totalChars() int {
	n := 0
	for _, s := range ttsSentences {
		n += utf8.RuneCountInString(s)
	}
	return n
}

func firstErr(es []error) string {
	if len(es) == 0 {
		return "none"
	}
	return Redact(es[0].Error())
}

// ttsInterrupt: barge-in must stop audio within maxInterrupt and the cancelled unit must not complete or fail.
func (r *run) ttsInterrupt() {
	const name = "interrupt"
	p, err := r.t.Build(Variant{})
	if err != nil {
		r.skip(name, "build failed")
		return
	}
	g, err := newRig(p)
	if err != nil {
		r.add(name, false, "rig: %v", err)
		return
	}
	defer g.close()
	if !r.spend(1, name) {
		return
	}
	_ = g.sess.Post(&core.SynthesisUnitFrame{Turn: "t1", Gen: g.gen(), UnitID: 1, Text: ttsLong, IsFinal: true})
	if !waitFor(r.o.Timeout, func() bool { a, _ := frames[*core.OutputAudioFrame](g.sink); return len(a) > 0 || len(g.errs()) > 0 }) {
		r.add(name, false, "no audio within %v", r.o.Timeout)
		return
	}
	if len(g.errs()) > 0 {
		r.add(name, false, "error before interruption: %s", firstErr(g.errs()))
		return
	}
	t := time.Now()
	if st, _ := frames[*core.TTSStoppedFrame](g.sink); len(st) > 0 {
		r.skip(name, "unit completed before the interruption (nothing to cancel)")
		return
	}
	_ = g.sess.Post(&core.InterruptionFrame{Turn: "t1", Gen: g.gen()})
	time.Sleep(maxInterrupt + 500*time.Millisecond)
	_, at := frames[*core.OutputAudioFrame](g.sink)
	_, stopAt := frames[*core.TTSStoppedFrame](g.sink)
	_, errAt := frames[*core.ErrorFrame](g.sink)
	var lastAudio time.Duration
	for _, a := range at {
		if a.After(t) {
			lastAudio = max(lastAudio, a.Sub(t))
		}
	}
	lateStop := slices.ContainsFunc(stopAt, func(s time.Time) bool { return s.After(t) })
	lateErr := slices.ContainsFunc(errAt, func(s time.Time) bool { return s.After(t) })
	r.metric(name, lastAudio <= maxInterrupt && !lateStop && !lateErr, float64(lastAudio.Milliseconds()), "ms",
		"last audio %v after interruption (max %v), completed-after-cancel=%v, error-after-cancel=%v", lastAudio.Round(time.Millisecond), maxInterrupt, lateStop, lateErr)
}
