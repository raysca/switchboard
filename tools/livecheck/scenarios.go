package main

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// opts are the knobs scenarios read (flags).
type opts struct {
	s1Turns    int
	pause      time.Duration
	werMax     float64       // max word error rate of what the STT heard
	v2vMax     time.Duration // S1 voice-to-voice ceiling
	stopMax    time.Duration // S2 bot audio must stop this fast after the user starts speaking
	mcpURL     string        // public HTTPS MCP server exposing get_time (S6)
	toolExpect *regexp.Regexp
	trace      bool
}

// Result statuses.
const (
	stPass = "pass"
	stFail = "fail"
	stNA   = "n/a"   // cannot apply to this profile/mode (reason says why)
	stSkip = "skip"  // not run: profile env missing, run aborted
	stErr  = "error" // the harness or server failed before an assertion could be made
	stInfo = "info"  // measured, nothing to assert in this mode
)

// KV is one measurement; values are numbers or short strings.
type KV struct {
	K string `json:"k"`
	V any    `json:"v"`
}

type Check struct {
	Name string `json:"name"`
	Pass bool   `json:"pass"`
	Note string `json:"note,omitempty"`
}

// Result is one scenario run of one profile in one mode.
type Result struct {
	Profile  string  `json:"profile"`
	Mode     string  `json:"mode"`
	Scenario string  `json:"scenario"`
	Repeat   int     `json:"repeat"`
	Status   string  `json:"status"`
	Reason   string  `json:"reason,omitempty"`
	Metrics  []KV    `json:"metrics,omitempty"`
	Checks   []Check `json:"checks,omitempty"`
	Seconds  float64 `json:"seconds,omitempty"`
	// Estimate is the upper-bound provider requests planned for the scenario; Requests is what
	// the server child actually sent (voice_provider_requests_total delta), ReqBy per provider/stage.
	Estimate int            `json:"estimated_requests,omitempty"`
	Requests int            `json:"actual_requests,omitempty"`
	ReqBy    map[string]int `json:"actual_requests_by_stage,omitempty"`
	At       string         `json:"at,omitempty"` // run stamp "date commit", set in the merged docs copy
}

func (r *Result) m(k string, v any) { r.Metrics = append(r.Metrics, KV{k, v}) }

func (r *Result) check(name string, ok bool, note string) {
	r.Checks = append(r.Checks, Check{name, ok, note})
}

// num returns the first numeric metric named k.
func (r *Result) num(k string) (float64, bool) {
	for _, kv := range r.Metrics {
		if kv.K == k {
			switch v := kv.V.(type) {
			case int:
				return float64(v), true
			case int64:
				return float64(v), true
			case float64:
				return v, true
			}
		}
	}
	return 0, false
}

func (r *Result) na(reason string)    { r.Status, r.Reason = stNA, reason }
func (r *Result) abort(reason string) { r.Status, r.Reason = stErr, reason }

// finalize sets the status from the checks unless it was already decided.
func (r *Result) finalize() {
	if r.Status != "" {
		return
	}
	r.Status = stInfo
	for _, c := range r.Checks {
		if !c.Pass {
			r.Status = stFail
			return
		}
		r.Status = stPass
	}
}

func msv(d time.Duration) int64 { return d.Milliseconds() }

// sctx is what a scenario gets: a connected client plus the run settings.
type sctx struct {
	c     *client
	srv   *server
	clips clips
	p     Profile
	mode  mode
	o     *opts
	fake  bool
}

// manager reports whether the turn manager is on in this mode.
func (x *sctx) manager() bool { return x.mode.turn != "" }

// minWords reports whether the mode sets turn.min_words.
func (x *sctx) minWords() bool {
	return x.mode.turn != "" && regexp.MustCompile(`min_words`).MatchString(x.mode.turn)
}

// content says whether transcript/answer content can be asserted (not with the fake STT/LLM).
func (x *sctx) content() bool { return !x.fake }

type scenario struct {
	id, title string
	class     serverClass
	// clips are the audio clips the scenario streams (only these are synthesized when missing).
	clips []string
	// shape feeds the upper-bound provider-request estimate (estimate.go).
	shape func(o *opts) shape
	// na is the static reason this scenario cannot apply (known before any call), or "".
	na  func(p Profile, m mode, o *opts) string
	run func(x *sctx, r *Result)
}

const naFake = "fake providers: bot audio is too short to barge into"

var scenarios = []scenario{
	{id: "S1", title: "baseline turn", class: classBase, clips: []string{"question"},
		shape: func(o *opts) shape { return shape{turns: o.s1Turns} }, run: s1},
	{id: "S2", title: "barge-in", class: classBase, clips: []string{"mars", "question"},
		shape: func(*opts) shape { return shape{turns: 2, extraSTT: 1} },
		na: func(p Profile, _ mode, _ *opts) string {
			if p.Fake {
				return naFake
			}
			return ""
		}, run: s2},
	{id: "S3", title: "backchannel (min_words)", class: classBase, clips: []string{"mars", "backchannel"},
		shape: func(*opts) shape { return shape{turns: 2, extraSTT: 1} },
		na: func(p Profile, m mode, _ *opts) string {
			switch {
			case p.Fake:
				return naFake
			case p.Interims == "no":
				return "STT sends no interim results (min_words only acts on finals)"
			}
			return ""
		}, run: s3},
	{id: "S4", title: "mid-utterance pause", class: classBase, clips: []string{"pauseA", "pauseB"},
		shape: func(*opts) shape { return shape{turns: 2, extraSTT: 1} }, run: s4},
	{id: "S5", title: "idle prompt and hang-up", class: classIdle,
		shape: func(*opts) shape { return shape{turns: 1} }, run: s5},
	{id: "S6", title: "tool call", class: classBase,
		shape: func(*opts) shape { return shape{turns: 1, extraLLM: 1} },
		na: func(p Profile, _ mode, o *opts) string {
			switch {
			case p.Fake:
				return "fake LLM has no tools"
			case o.mcpURL == "":
				return "no -mcp-url: tools come from MCP servers and the server accepts only public HTTPS endpoints"
			}
			return ""
		}, run: s6},
	{id: "S7", title: "text-only turn", class: classBase,
		shape: func(*opts) shape { return shape{turns: 1, noSTT: true} }, run: s7},
	{id: "S8", title: "muted-client liveness", class: classPing,
		shape: func(*opts) shape { return shape{} }, run: s8},
	{id: "S9", title: "long reply (TTS requests, first audio, gaps)", class: classBase, clips: []string{"mars"},
		shape: func(*opts) shape { return shape{turns: 1, long: true} },
		na: func(p Profile, _ mode, _ *opts) string {
			if p.Fake {
				return "fake providers: reply is not multi-sentence"
			}
			return ""
		}, run: s9},
}

func scenarioByID(id string) (scenario, bool) {
	for _, s := range scenarios {
		if s.id == id {
			return s, true
		}
	}
	return scenario{}, false
}

// reply is the bot's streamed reply text after t, truncated for display.
func reply(c *client, t time.Time, n int) string {
	return truncate(c.texts(t, "bot-llm-text", false), n)
}

// s1: n normal turns. v2v is end of speech to first bot audio.
func s1(x *sctx, r *Result) {
	c, q := x.c, x.clips["question"]
	before := x.srv.stages()
	var v2vs []float64
	for i := 0; i < x.o.s1Turns; i++ {
		c.quiet(400 * time.Millisecond)
		mark := time.Now()
		_, tEnd, err := c.stream(q)
		if err != nil {
			r.abort(redact(err))
			return
		}
		c.wait(15*time.Second, func() bool { return c.has(mark, "audio") })
		pfx := fmt.Sprintf("t%d.", i+1)
		e, ok := c.firstAfter(mark, "audio")
		r.check(pfx+"bot audio", ok, "")
		if ok {
			v2v := e.t.Sub(tEnd)
			v2vs = append(v2vs, float64(v2v.Milliseconds()))
			r.m(pfx+"v2v_ms", msv(v2v))
			r.check(pfx+"v2v <= "+x.o.v2vMax.String(), v2v <= x.o.v2vMax, "")
		}
		if e, ok := c.firstAfter(tEnd.Add(-300*time.Millisecond), "user-stopped-speaking"); ok {
			r.m(pfx+"vad_stop_ms", msv(e.t.Sub(tEnd)))
		}
		for _, e := range c.snap() {
			if e.t.After(mark) && e.kind == "user-transcription" && e.final {
				r.m(pfx+"final_tr_ms", msv(e.t.Sub(tEnd)))
				break
			}
		}
		c.wait(25*time.Second, func() bool { return c.has(tEnd, "bot-stopped-speaking") })
		if ok {
			s1Breakdown(x, r, pfx, tEnd, e.t)
		}
		heard, said := c.texts(mark, "user-transcription", true), c.texts(mark, "bot-llm-text", false)
		r.m(pfx+"heard", heard)
		r.m(pfx+"reply", truncate(said, 90))
		r.check(pfx+"reply text", said != "", "")
		if x.content() {
			w := wer(x.clips.text("question"), heard)
			r.m(pfx+"wer", fmt.Sprintf("%.2f", w))
			r.check(pfx+fmt.Sprintf("wer <= %.2f", x.o.werMax), w <= x.o.werMax, "")
			r.check(pfx+"answer is four", fourRe.MatchString(said), "")
		}
		c.quiet(time.Second)
	}
	if len(v2vs) > 0 {
		sort.Float64s(v2vs)
		r.m("v2v_ms", int64(v2vs[len(v2vs)/2]))
	}
	d := stageDelta(before, x.srv.stages())
	for _, stage := range []string{"stt_final", "llm_ttft", "tts_ttfb"} {
		if v, ok := d[stage]; ok {
			r.m(stage+"_ms", int64(v[0]))
		}
	}
}

// s1Breakdown splits a turn's v2v into additive stages from the server timeline (same host,
// same wall clock, no extra provider calls): vad (clip end to server speech_stop), eot (speech_stop to
// the user message reaching the LLM (stt_result): STT final + turn commit), ttft, unit_wait (first token to first TTS unit dispatched),
// tts (unit to first audio), tail (server audio to client receipt). Each stage is the earliest event of
// its type at or after the previous one (not the i-th: tts_unit fires per unit, speech_stop per pause,
// the ring evicts, and latency events are recorded out of order), so the parts sum to v2v exactly.
func s1Breakdown(x *sctx, r *Result, pfx string, tEnd, clientAudio time.Time) {
	evs := x.srv.timelineEvents(x.c.n)
	prev := tEnd.Add(-300 * time.Millisecond) // same slack as vad_stop_ms: the VAD may stop inside the clip's 100 ms margin
	from := tEnd
	for _, st := range [][2]string{{"speech_stop", "bd_vad_ms"}, {"stt_result", "bd_eot_ms"}, {"llm_first_token", "bd_ttft_ms"}, {"tts_unit", "bd_unit_wait_ms"}, {"latency:tts_ttfb", "bd_tts_ms"}} {
		var t time.Time
		for _, e := range evs {
			k := e.Type
			if k == "latency" {
				k += ":" + e.Details // observer events carry the event time in Timestamp
			}
			if k == st[0] && !e.Timestamp.Before(prev) && (t.IsZero() || e.Timestamp.Before(t)) {
				t = e.Timestamp
			}
		}
		if t.IsZero() {
			return
		}
		r.m(pfx+st[1], msv(t.Sub(from)))
		prev, from = t, t
	}
	r.m(pfx+"bd_tail_ms", msv(clientAudio.Sub(prev)))
}

var fourRe = regexp.MustCompile(`(?i)\b(four|4)\b`)

// bargeSetup streams the Mars question and returns when the bot has been audible for 2 s.
func bargeSetup(x *sctx, r *Result) (ok bool) {
	c := x.c
	c.quiet(400 * time.Millisecond)
	_, tEnd, _ := c.stream(x.clips["mars"])
	if !c.wait(20*time.Second, func() bool { return c.has(tEnd, "audio") }) {
		r.abort("no bot audio for the setup question")
		return false
	}
	c.quiet(2 * time.Second)
	if !c.audioNow(300 * time.Millisecond) {
		r.abort("bot not audible 2 s after first audio (reply too short to barge into)")
		return false
	}
	return true
}

// s2: barge-in with a full utterance; checks flush time and that the second utterance is answered.
func s2(x *sctx, r *Result) {
	c := x.c
	if !bargeSetup(x, r) {
		return
	}
	onset, tEnd, _ := c.stream(x.clips["question"])
	// The barge-in can land just after a short clip ends (min_words waits for a 3-word interim):
	// look for the flush until stopMax + 1 s after the onset; the latency check below stays stopMax.
	var stop time.Time
	var paused bool
	c.wait(time.Until(onset.Add(x.o.stopMax+time.Second)), func() bool {
		stop, paused = c.audioRunEnd(onset, 250*time.Millisecond)
		return paused
	})
	r.check("bot audio flushed", paused, "")
	if paused {
		d := stop.Sub(onset)
		r.m("audio_stop_ms", msv(d))
		r.check("audio stops <= "+x.o.stopMax.String(), d <= x.o.stopMax, "")
	}
	if e, ok := c.firstAfter(onset, "bot-stopped-speaking"); ok {
		r.m("bot_stopped_evt_ms", msv(e.t.Sub(onset)))
	}
	if e, ok := c.firstAfter(onset, "user-started-speaking"); ok {
		r.m("user_started_evt_ms", msv(e.t.Sub(onset)))
	}
	c.wait(20*time.Second, func() bool { return c.has(tEnd, "audio") && c.has(tEnd, "bot-llm-started") })
	if e, ok := c.firstAfter(tEnd, "bot-llm-started"); ok {
		r.m("llm_start_after_end_ms", msv(e.t.Sub(tEnd)))
	}
	e, ok := c.firstAfter(tEnd, "audio")
	r.check("second utterance answered", ok, "")
	if ok {
		r.m("second_reply_v2v_ms", msv(e.t.Sub(tEnd)))
	}
	c.wait(25*time.Second, func() bool { return c.has(tEnd, "bot-stopped-speaking") })
	heard := c.texts(onset.Add(-500*time.Millisecond), "user-transcription", true)
	r.m("heard_after_onset", heard)
	r.m("reply_after_onset", reply(c, onset, 90))
	if x.content() {
		w := wer(x.clips.text("question"), heard)
		r.m("wer", fmt.Sprintf("%.2f", w))
		r.check(fmt.Sprintf("second utterance heard (wer <= %.2f)", x.o.werMax), w <= x.o.werMax, "")
	}
}

// s3: a backchannel while the bot speaks. With turn.min_words=3 it must not interrupt.
func s3(x *sctx, r *Result) {
	c := x.c
	if !bargeSetup(x, r) {
		return
	}
	if x.p.Interims != "yes" && c.interims(c.t0) == 0 { // probe: did the setup question produce interims?
		r.na("STT sent no interim results during the setup question (min_words only acts on finals)")
		return
	}
	onset, tEnd, _ := c.stream(x.clips["backchannel"])
	c.quiet(6 * time.Second) // long enough for a transcript, a model reply and its first audio
	newReply, stops := c.has(onset, "bot-llm-started"), c.count(onset, "bot-stopped-speaking")
	r.m("new_reply_started", newReply)
	r.m("bot_stopped_evts", stops)
	if stop, paused := c.audioRunEnd(onset, 250*time.Millisecond); paused {
		r.m("audio_gap_after_ms", msv(stop.Sub(onset)))
	} else {
		r.m("audio_gap_after_ms", "none")
	}
	r.m("heard", c.texts(onset.Add(-300*time.Millisecond), "user-transcription", true))
	if x.minWords() { // without min_words a backchannel interrupts by design: measured only
		r.check("backchannel did not start a new reply", !newReply, "")
		r.check("bot not interrupted", stops == 0, "")
	}
	c.wait(30*time.Second, func() bool {
		return c.has(tEnd.Add(3*time.Second), "bot-stopped-speaking") && !c.audioNow(1500*time.Millisecond)
	})
}

// s4: a ~1 s pause mid-utterance: one turn expected under the turn manager.
func s4(x *sctx, r *Result) {
	c := x.c
	c.quiet(400 * time.Millisecond)
	mark := time.Now()
	_, tEnd, err := c.stream(pauseClip(x.clips, x.o.pause))
	if err != nil {
		r.abort(redact(err))
		return
	}
	c.wait(20*time.Second, func() bool { return c.has(mark, "audio") })
	if e, ok := c.firstAfter(mark, "audio"); ok {
		r.m("first_audio_vs_end_ms", msv(e.t.Sub(tEnd))) // negative: the bot spoke before the user finished
	} else {
		r.m("first_audio_vs_end_ms", "NONE")
	}
	c.wait(25*time.Second, func() bool { return c.has(tEnd, "bot-stopped-speaking") })
	c.quiet(4 * time.Second) // late replies
	// A user turn is a committed (final) transcription: the manager joins VAD segments, and a
	// 1 s pause is longer than the VAD stop (800 ms), so raw VAD stops are 2 by design.
	turns, replies := c.count(mark, "user-transcription")-c.interims(mark), c.count(mark, "bot-llm-started")
	r.m("vad_stops", c.count(mark, "user-stopped-speaking"))
	r.m("user_turns", turns)
	r.m("bot_replies", replies)
	heard, said := c.texts(mark, "user-transcription", true), c.texts(mark, "bot-llm-text", false)
	r.m("heard", heard)
	r.m("reply", truncate(said, 110))
	if x.manager() && x.content() { // plain VAD turns may split at the pause: measured only; fake STT emits a word per frame
		r.check("one user turn", turns == 1, fmt.Sprintf("got %d", turns))
		r.check("one bot reply", replies == 1, fmt.Sprintf("got %d", replies))
		w := wer(x.clips.text("pauseA")+" "+x.clips.text("pauseB"), heard)
		r.m("wer", fmt.Sprintf("%.2f", w))
		r.check(fmt.Sprintf("heard whole sentence (wer <= %.2f)", x.o.werMax), w <= x.o.werMax, "")
	}
}

// s5: no speech at all: idle prompt then hang-up at short timings.
func s5(x *sctx, r *Result) {
	c, t0 := x.c, x.c.t0
	c.wait(25*time.Second, func() bool { return c.isClosed() })
	e, ok := c.firstAfter(t0, "audio")
	r.check("idle prompt audio", ok, "")
	if ok {
		r.m("first_prompt_audio_s", fmt.Sprintf("%.1f", e.t.Sub(t0).Seconds()))
	}
	if c.isClosed() {
		r.check("server hung up", true, "")
		r.m("closed_after_s", fmt.Sprintf("%.1f", c.closeAt.Sub(t0).Seconds()))
		r.m("close_status", int(websocket.CloseStatus(c.closeErr)))
	} else {
		r.check("server hung up", false, "not closed within 25 s")
	}
	r.m("bot_text", c.texts(t0, "bot-llm-text", false))
}

// s6: the model calls get_time and the answer carries its result.
func s6(x *sctx, r *Result) {
	c := x.c
	c.quiet(400 * time.Millisecond)
	mark := time.Now()
	if err := c.sendText("What time is it right now?"); err != nil {
		r.abort(redact(err))
		return
	}
	c.wait(30*time.Second, func() bool { return c.has(mark, "bot-stopped-speaking") })
	said := c.texts(mark, "bot-llm-text", false)
	r.m("reply", truncate(said, 110))
	// There is no client-visible tool event: a reply matching the tool's output is the evidence.
	r.check("bot replied with audio", c.has(mark, "audio"), "")
	r.check("answer mentions the tool result ("+x.o.toolExpect.String()+")", x.o.toolExpect.MatchString(said), "")
}

// s7: a text turn over RTVI send-text: no STT involved.
func s7(x *sctx, r *Result) {
	c := x.c
	c.quiet(400 * time.Millisecond)
	mark := time.Now()
	if err := c.sendText(x.clips.text("question")); err != nil {
		r.abort(redact(err))
		return
	}
	c.wait(30*time.Second, func() bool { return c.has(mark, "bot-stopped-speaking") })
	e, ok := c.firstAfter(mark, "audio")
	r.check("bot audio", ok, "")
	if ok {
		r.m("text_to_audio_ms", msv(e.t.Sub(mark)))
	}
	if e, ok := c.firstAfter(mark, "bot-llm-started"); ok {
		r.m("text_to_llm_ms", msv(e.t.Sub(mark)))
	}
	said := c.texts(mark, "bot-llm-text", false)
	r.m("reply", truncate(said, 90))
	r.check("reply text", said != "", "")
	if x.content() {
		r.check("answer is four", fourRe.MatchString(said), "")
	}
}

// s8: a client that sends nothing outlives the legacy idle timeout because the server's pings
// are answered (the harness client answers them while reading).
func s8(x *sctx, r *Result) {
	d := 3 * pingIdleTimeout
	alive := x.c.idle(d)
	r.m("silent_for_s", d.Seconds())
	r.check(fmt.Sprintf("session alive after %s of silence (idle_timeout %s)", d, pingIdleTimeout), alive, "")
}

// neededMissing keeps the missing clips some selected scenario streams.
func neededMissing(missing []string, scs []scenario) []string {
	var out []string
	for _, m := range missing {
		for _, s := range scs {
			if slices.Contains(s.clips, m) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// s9: one multi-sentence reply (the Mars clip). Measures TTS requests per reply, first audio, and
// underruns of a simulated real-time player (a chunk arriving after the queued audio ran out is a gap;
// robust to the server sending audio in bursts). Compare mode off against merge.
func s9(x *sctx, r *Result) {
	c := x.c
	ttsReqs := func() (n int) {
		for k, v := range x.srv.requests() {
			if strings.HasSuffix(k, "/tts") {
				n += v
			}
		}
		return
	}
	c.quiet(400 * time.Millisecond)
	before, bs := ttsReqs(), x.srv.stages()
	mark := time.Now()
	_, tEnd, err := c.stream(x.clips["mars"])
	if err != nil {
		r.abort(redact(err))
		return
	}
	c.wait(25*time.Second, func() bool { return c.has(mark, "audio") })
	c.wait(60*time.Second, func() bool { return c.has(tEnd, "bot-stopped-speaking") })
	c.quiet(time.Second)
	said := c.texts(mark, "bot-llm-text", false)
	var first, playEnd, last time.Time
	var gaps int
	var gapMs, audioMs int64
	for _, e := range c.snap() {
		if e.kind != "audio" || e.t.Before(mark) {
			continue
		}
		if first.IsZero() {
			first, playEnd = e.t, e.t
		}
		if e.t.After(playEnd.Add(100 * time.Millisecond)) {
			gaps++
			gapMs += e.t.Sub(playEnd).Milliseconds()
		}
		if e.t.After(playEnd) {
			playEnd = e.t
		}
		playEnd = playEnd.Add(e.dur)
		audioMs += e.dur.Milliseconds()
		last = e.t
	}
	r.check("bot audio", !first.IsZero(), "")
	if first.IsZero() {
		return
	}
	n := ttsReqs() - before
	r.m("first_audio_ms", msv(first.Sub(tEnd)))
	r.m("tts_requests", n)
	r.m("sentences_est", len(regexp.MustCompile(`[.!?]+(\s|$)`).FindAllString(said, -1)))
	r.m("reply_chars", len([]rune(said)))
	r.m("audio_ms", audioMs)
	r.m("gaps_gt100ms", gaps)
	r.m("gap_ms_total", gapMs)
	r.m("audio_span_ms", msv(last.Sub(first)))
	if v, ok := stageDelta(bs, x.srv.stages())["tts_ttfb"]; ok {
		r.m("tts_ttfb_ms", int64(v[0]))
	}
	r.m("reply", truncate(said, 90))
	r.check("reply text", said != "", "")
}
