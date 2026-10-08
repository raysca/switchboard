package provtest

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"govoice/core"
)

const (
	chunkBytes   = clipRate * 2 / 50 // 20 ms
	hangoverTail = 25                // 500 ms of silence before the VAD stop
	maxFinalWait = 8 * time.Second
	maxFinalLat  = 5 * time.Second
)

// clipRun is what one streamed turn produced.
type clipRun struct {
	turn            core.TurnID
	gen             uint64 // generation at the end of the turn
	t0, stopAt      time.Time
	rebindAt        time.Time
	cands           []*core.TranscriptCandidateFrame
	at              []time.Time
	errs            []error
	ended           bool // an end-of-utterance or final arrived after the stop
	text            string
	finalAt         time.Time
	firstInterimLat time.Duration
}

func (r *run) pace(t0 time.Time, n int) {
	if r.o.Fast {
		time.Sleep(time.Millisecond) // dry mode: not real time, but the session inbox must not overflow
		return
	}
	if d := time.Until(t0.Add(time.Duration(n) * 20 * time.Millisecond)); d > 0 {
		time.Sleep(d)
	}
}

var turnSeq uint64

// streamClip plays pcm as one user turn the way the live pipeline does:
// UserStarted + TurnEventStarted, real-time 20 ms audio, a silence hangover,
// then UserStopped + TurnEventStopped/Completed. rebindAt (0..1) bumps the
// generation mid-clip with a same-turn TurnEventStarted (barge-in).
func (r *run) streamClip(g *rig, pcm []byte, rebindAt float64) clipRun {
	turnSeq++
	seq := turnSeq
	gen := g.gen()
	cr := clipRun{turn: core.TurnID(fmt.Sprintf("turn-%d-%d", gen, seq))}
	mark := len(g.sink.snapshot())
	now := time.Now()
	_ = g.sess.Post(&core.UserStartedSpeakingFrame{Seq: seq, Gen: gen, Timestamp: now})
	_ = g.sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: cr.turn, Gen: core.Generation(gen), At: now})
	cr.t0 = time.Now()
	n, rebound, failed := 0, rebindAt <= 0, false
	send := func(b []byte) {
		// Stop streaming once the provider failed (a bad credential): the error
		// latency must not include the rest of the clip.
		if failed = failed || n%5 == 0 && len(g.errs()) > 0; failed {
			return
		}
		_ = g.sess.Post(&core.InputAudioFrame{Samples: b, Encoding: core.EncodingPCM16LE, SampleRate: clipRate, Channels: 1, Gen: gen, Seq: uint64(n), Timestamp: time.Now()})
		n++
		r.pace(cr.t0, n)
	}
	for off := 0; off < len(pcm); off += chunkBytes {
		if !rebound && float64(off) >= rebindAt*float64(len(pcm)) {
			rebound = true
			gen = g.sess.IncrementGeneration()
			cr.rebindAt = time.Now()
			_ = g.sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStarted, Turn: cr.turn, Gen: core.Generation(gen), At: time.Now()})
		}
		end := min(off+chunkBytes, len(pcm))
		b := make([]byte, chunkBytes) // a short tail is zero padded
		copy(b, pcm[off:end])
		send(b)
	}
	for i := 0; i < hangoverTail; i++ {
		send(make([]byte, chunkBytes))
	}
	cr.stopAt = time.Now()
	_ = g.sess.Post(&core.UserStoppedSpeakingFrame{Seq: seq, Gen: gen, Timestamp: cr.stopAt})
	_ = g.sess.Post(&core.TurnEventFrame{Kind: core.TurnEventStopped, Turn: cr.turn, Gen: core.Generation(gen), At: cr.stopAt, Reason: core.TurnReasonSilence})
	_ = g.sess.Post(&core.TurnEventFrame{Kind: core.TurnEventCompleted, Turn: cr.turn, Gen: core.Generation(gen), At: time.Now(), Reason: core.TurnReasonSilence})
	cr.gen = gen

	ended := func() bool {
		hasFinal, last := false, cr.stopAt
		for _, s := range g.sink.snapshot()[mark:] {
			if c, ok := s.f.(*core.TranscriptCandidateFrame); ok {
				// Only an utterance end after the VAD stop ends the wait: one before it is the first
				// sentence of a clip with a pause, and a chunk final (xAI: is_final without
				// speech_final) is followed by the utterance final up to ~0.6 s later. A provider
				// that only sends finals is settled by the quiet rule below.
				if c.EndOfUtterance && !s.at.Before(cr.stopAt) {
					return true
				}
				hasFinal = hasFinal || c.Final // finalized by server endpointing before the stop
				if s.at.After(last) {
					last = s.at
				}
			}
		}
		// quiet for 1.5 s after the stop with a final already delivered: nothing is pending
		return hasFinal && time.Since(last) > 1500*time.Millisecond || len(g.errs()) > 0
	}
	waitFor(min(maxFinalWait, r.o.Timeout), ended)
	time.Sleep(150 * time.Millisecond) // an EndOfUtterance may trail the final
	for _, s := range g.sink.snapshot()[mark:] {
		if c, ok := s.f.(*core.TranscriptCandidateFrame); ok {
			cr.cands, cr.at = append(cr.cands, c), append(cr.at, s.at)
		}
	}
	cr.errs = g.errs()
	cr.summarize()
	return cr
}

// summarize derives the turn's transcript: the last end-of-utterance candidate,
// else the last final, else the last hypothesis.
func (cr *clipRun) summarize() {
	pick := -1
	for i, c := range cr.cands {
		if c.EndOfUtterance {
			pick = i
		}
	}
	if pick < 0 {
		for i, c := range cr.cands {
			if c.Final {
				pick = i
			}
		}
	}
	if pick < 0 && len(cr.cands) > 0 {
		pick = len(cr.cands) - 1
	}
	if pick >= 0 {
		cr.text, cr.finalAt = cr.cands[pick].Text, cr.at[pick]
		cr.ended = cr.cands[pick].Final || cr.cands[pick].EndOfUtterance
	}
	for i, c := range cr.cands {
		if !c.Final && !c.EndOfUtterance {
			cr.firstInterimLat = cr.at[i].Sub(cr.t0)
			break
		}
	}
}

// dump lists the turn's candidates (+ms after the stop, I/F/E flags, text) for failure details.
func (cr *clipRun) dump() string {
	var parts []string
	for i, c := range cr.cands {
		fl := "I"
		if c.Final {
			fl = "F"
		}
		if c.EndOfUtterance {
			fl += "E"
		}
		parts = append(parts, fmt.Sprintf("%+dms %s %q", cr.at[i].Sub(cr.stopAt).Milliseconds(), fl, trunc(c.Text, 40)))
	}
	if len(parts) > 8 {
		parts = append(parts[:2], parts[len(parts)-6:]...)
	}
	return strings.Join(parts, " | ")
}

func (cr *clipRun) finalLatency() time.Duration {
	if !cr.ended {
		return maxFinalWait
	}
	return max(0, cr.finalAt.Sub(cr.stopAt))
}

func (r *run) sttSuite() {
	if len(r.o.Clips) == 0 {
		r.skip("clips", "no clips loaded")
		return
	}
	base := settleGoroutines(200 * time.Millisecond)
	p, err := r.t.Build(Variant{})
	if err != nil {
		r.add("build", false, "%v", Redact(err.Error()))
		r.skip("wer", "build failed")
		r.sttBadCredential()
		return
	}
	r.add("build", true, "")
	g, err := newRig(p)
	if err != nil {
		r.add("build", false, "rig: %v", err)
		return
	}
	var runs []clipRun
	for i, c := range r.o.Clips {
		if !r.spend(1, "wer") {
			break
		}
		if i == 1 {
			time.Sleep(r.o.Idle) // idle gap: the first turn's stream is long closed
		}
		runs = append(runs, r.streamClip(g, c.PCM, 0))
		if len(runs[len(runs)-1].errs) > 0 {
			break
		}
	}
	r.sttAnalyse(runs)
	r.leakAfter(g, base)
	r.sttRebind()
	r.sttBadCredential()
}

func (r *run) sttAnalyse(runs []clipRun) {
	if len(runs) == 0 {
		return
	}
	var errs []error
	var details, noEnd, cum, bind []string
	var wers, lat, first []float64
	var over []string
	sawInterim := false
	for i, cr := range runs {
		c := r.o.Clips[i]
		errs = append(errs, cr.errs...)
		w := WER(c.Text, cr.text)
		wers = append(wers, w)
		if w > r.limit(c) {
			over = append(over, c.ID)
		}
		details = append(details, fmt.Sprintf("%s=%.2f/%.2f (%q)", c.ID, w, r.limit(c), trunc(cr.text, 60)))
		if !cr.ended {
			noEnd = append(noEnd, c.ID)
		}
		lat = append(lat, float64(cr.finalLatency().Milliseconds()))
		if cr.firstInterimLat > 0 {
			sawInterim = true
			first = append(first, float64(cr.firstInterimLat.Milliseconds()))
		}
		// cumulative text: each final extends the previous final.
		prev := ""
		for _, cand := range cr.cands {
			if !cand.Final {
				continue
			}
			if prev != "" && !hasWordPrefix(prev, cand.Text) {
				cum = append(cum, fmt.Sprintf("%s: %q then %q", c.ID, trunc(prev, 30), trunc(cand.Text, 30)))
			}
			prev = cand.Text
		}
		for _, cand := range cr.cands {
			if cand.Turn != cr.turn || uint64(cand.Gen) != cr.gen {
				bind = append(bind, fmt.Sprintf("%s: turn=%q gen=%d want %q/%d", c.ID, cand.Turn, cand.Gen, cr.turn, cr.gen))
				break
			}
		}
	}
	r.add("no_error", len(errs) == 0, "%d errors; first: %v", len(errs), firstErr(errs))
	r.metric("wer", len(over) == 0 && len(noEnd) == 0, mean(wers), "wer", "mean %.2f; over the clip limit: %v; no final in time: %v; clip=wer/limit: %s", mean(wers), over, noEnd, strings.Join(details, "; "))
	if sawInterim {
		// interim candidates must arrive before the turn's final
		var late []string
		for i, cr := range runs {
			firstFinal, firstInterim := time.Time{}, time.Time{}
			for j, cand := range cr.cands {
				if cand.Final && firstFinal.IsZero() {
					firstFinal = cr.at[j]
				}
				if !cand.Final && !cand.EndOfUtterance && firstInterim.IsZero() {
					firstInterim = cr.at[j]
				}
			}
			if !firstInterim.IsZero() && !firstFinal.IsZero() && firstInterim.After(firstFinal) {
				late = append(late, r.o.Clips[i].ID)
			}
		}
		r.metric("interim_before_final", len(late) == 0, median(first), "ms", "first interim median %.0f ms after speech start; interims after the first final: %v", median(first), late)
	} else {
		r.skip("interim_before_final", "provider emitted no interim results")
	}
	r.metric("final_latency", median(lat) <= float64(maxFinalLat.Milliseconds()), median(lat), "ms", "final after the VAD stop, median %.0f ms (max %v); per clip %v", median(lat), maxFinalLat, lat)
	r.add("cumulative", len(cum) == 0, "per-turn finals extend each other; violations: %.200s", strings.Join(cum, "; "))
	r.add("turn_binding", len(bind) == 0, "candidates carry the started turn id and generation; violations: %.200s", strings.Join(bind, "; "))
	// Resumed speech: the clip with a pause between sentences, in one turn.
	for i, c := range r.o.Clips {
		if len(c.Segments) < 2 || i >= len(runs) {
			continue
		}
		cr := runs[i]
		cov := make([]float64, len(c.Segments))
		for k, seg := range c.Segments {
			cov[k] = coverage(seg, cr.text)
		}
		ok := WER(c.Text, cr.text) <= r.limit(c) && slices.Min(cov) >= 0.6
		r.metric("resumed_speech", ok, WER(c.Text, cr.text), "wer", "one turn with a pause: word coverage per sentence %v, WER %.2f, text %q; candidates: %s", fmtFloats(cov), WER(c.Text, cr.text), trunc(cr.text, 80), cr.dump())
	}
	// Reuse after an idle gap: the second turn of the same processor (clip index 1).
	if len(runs) > 1 {
		ok := runs[1].ended && len(runs[1].errs) == 0 && WER(r.o.Clips[1].Text, runs[1].text) <= r.limit(r.o.Clips[1])
		r.add("reuse_after_idle", ok, "second turn after %v idle: final=%v wer=%.2f errors=%d", r.o.Idle, runs[1].ended, WER(r.o.Clips[1].Text, runs[1].text), len(runs[1].errs))
	}
}

// sttRebind: a same-turn TurnEventStarted at a newer generation (barge-in) must
// re-bind the open stream so results keep flowing under the new generation.
func (r *run) sttRebind() {
	const name = "barge_in_rebind"
	var clip *Clip
	for i := range r.o.Clips {
		if len(r.o.Clips[i].Segments) == 0 && (clip == nil || len(r.o.Clips[i].PCM) > len(clip.PCM)) {
			clip = &r.o.Clips[i]
		}
	}
	if clip == nil {
		r.skip(name, "no single-sentence clip")
		return
	}
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
	cr := r.streamClip(g, clip.PCM, 0.5)
	var wrong, post int
	inflight := 300 * time.Millisecond // results already on the wire may still carry the old generation
	if r.o.Fast {
		inflight = 0
	}
	for i, c := range cr.cands {
		if cr.at[i].After(cr.rebindAt.Add(inflight)) {
			post++
			if uint64(c.Gen) != cr.gen || c.Turn != cr.turn {
				wrong++
			}
		}
	}
	w := WER(clip.Text, cr.text)
	ok := len(cr.errs) == 0 && cr.ended && post > 0 && wrong == 0 && w <= r.limit(*clip)+0.35 // the rebind may cost a word or two
	r.metric(name, ok, w, "wer", "%d candidates after the rebind, %d with a stale turn/generation (want 0), final=%v, errors=%d, wer=%.2f (%q)", post, wrong, cr.ended, len(cr.errs), w, trunc(cr.text, 60))
}

func (r *run) sttBadCredential() {
	r.badCredential(func(g *rig) error {
		pcm := r.o.Clips[0].PCM
		cr := r.streamClip(g, pcm[:min(len(pcm), clipRate*2)], 0)
		if len(cr.errs) > 0 {
			return cr.errs[0]
		}
		return nil
	})
}

// limit is the clip's WER limit (clips.json max_wer), else Options.MaxWER.
func (r *run) limit(c Clip) float64 {
	if c.MaxWER > 0 {
		return c.MaxWER
	}
	return r.o.MaxWER
}

func coverage(ref, hyp string) float64 {
	r, h := Words(ref), map[string]bool{}
	for _, w := range Words(hyp) {
		h[w] = true
	}
	if len(r) == 0 {
		return 1
	}
	n := 0
	for _, w := range r {
		if h[w] {
			n++
		}
	}
	return float64(n) / float64(len(r))
}

func fmtFloats(v []float64) string {
	s := make([]string, len(v))
	for i, x := range v {
		s[i] = fmt.Sprintf("%.2f", x)
	}
	return "[" + strings.Join(s, " ") + "]"
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := slices.Clone(v)
	sort.Float64s(c)
	return c[len(c)/2]
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
