package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"govoice/server/api"
	"govoice/tools/internal/stack"
)

// Observed is everything one call showed: the server's stored view of it (session row, handoff
// spans, tool rows) and what the client saw (RTVI events, typed turns).
type Observed struct {
	SessionID, AgentVersionID, ConfigHash, EndReason string
	TurnCount                                        int
	Path                                             []string // step ids: the start step, then each handoff target
	Tools                                            []string // "name:status" in call order
	HandoffFailures, ErrorFrames                     int
	ReplyMS                                          []int // typed turn -> first bot-llm-text
	Transcript                                       []Line
	Replies                                          string // every agent line, newline-joined
	Dropped                                          int    // client events past the cap: the call is only partly seen
}

// Line is one transcript line; Who is caller or agent.
type Line struct {
	Who  string `json:"who"`
	Text string `json:"text"`
}

// Sent is one typed caller turn.
type Sent struct {
	At   time.Time
	Text string
}

// Observe combines the stored session and traces (API DTOs, so an API rename breaks the build)
// with the client's events and typed turns. start is the agent's start step id.
func Observe(sess api.SessionDTO, tr api.SessionTracesDTO, start string, ev []stack.Event, sent []Sent) Observed {
	o := Observed{SessionID: sess.ID, AgentVersionID: sess.AgentVersionID, ConfigHash: sess.ConfigHash,
		EndReason: sess.EndReason, TurnCount: sess.TurnCount, Path: []string{start}}

	spans := slices.Clone(tr.Spans)
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].StartedAt.Before(spans[j].StartedAt) })
	for _, sp := range spans {
		switch sp.Name {
		case "handoff.failed":
			o.HandoffFailures++
		case "handoff.active":
			var a struct {
				From string `json:"from_step"`
				To   string `json:"to_step"`
			}
			if json.Unmarshal(sp.Attributes, &a) != nil || a.To == "" {
				continue
			}
			if len(o.Path) == 1 && o.Path[0] == start && a.From != "" {
				o.Path[0] = a.From
			}
			o.Path = append(o.Path, a.To)
		}
	}
	calls := slices.Clone(tr.ToolCalls)
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].StartedAt.Before(calls[j].StartedAt) })
	for _, c := range calls {
		o.Tools = append(o.Tools, c.RemoteName+":"+c.Status)
	}

	for _, e := range ev {
		// The server closes a call with an error message that carries the end reason: not a fault.
		if e.Kind == "error" && (sess.EndReason == "" || e.Text != sess.EndReason) {
			o.ErrorFrames++
		}
	}
	// agentText joins the bot text that arrived in [from, to); a new LLM reply starts a new sentence.
	agentText := func(from, to time.Time) string {
		var b strings.Builder
		for _, e := range ev {
			if e.At.Before(from) || (!to.IsZero() && !e.At.Before(to)) {
				continue
			}
			switch e.Kind {
			case "bot-llm-started":
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
			case "bot-llm-text":
				b.WriteString(e.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	add := func(who, text string) {
		if text != "" {
			o.Transcript = append(o.Transcript, Line{who, text})
		}
	}
	if len(sent) > 0 {
		add("agent", agentText(time.Time{}, sent[0].At))
	} else {
		add("agent", agentText(time.Time{}, time.Time{}))
	}
	for i, s := range sent {
		var next time.Time
		if i+1 < len(sent) {
			next = sent[i+1].At
		}
		add("caller", s.Text)
		add("agent", agentText(s.At, next))
		for _, e := range ev {
			if e.Kind == "bot-llm-text" && !e.At.Before(s.At) && (next.IsZero() || e.At.Before(next)) {
				o.ReplyMS = append(o.ReplyMS, int(e.At.Sub(s.At).Milliseconds()))
				break
			}
		}
	}
	var replies []string
	for _, l := range o.Transcript {
		if l.Who == "agent" {
			replies = append(replies, l.Text)
		}
	}
	o.Replies = strings.Join(replies, "\n")
	return o
}

// medianMS is the upper median of ms (0 for none).
func medianMS(ms []int) int {
	if len(ms) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(ms))
	return s[len(s)/2]
}

// Score checks the call against the case; stepNames maps step id to name. It returns one line per
// failed assertion, none for a pass. Zero RTVI error messages and zero failed handoffs are always required.
func Score(c Case, o Observed, stepNames map[string]string) (fails []string) {
	fail := func(f string, a ...any) { fails = append(fails, fmt.Sprintf(f, a...)) }
	e := c.Expect
	if o.ErrorFrames > 0 {
		fail("errors: want 0 RTVI error messages, got %d", o.ErrorFrames)
	}
	if o.HandoffFailures > 0 {
		fail("handoff: want 0 failed handoffs, got %d", o.HandoffFailures)
	}
	if o.Dropped > 0 {
		fail("events: %d client events were dropped past the cap, the call was not fully seen", o.Dropped)
	}
	if len(e.Path) > 0 {
		ok := len(e.Path) == len(o.Path)
		for i := 0; ok && i < len(o.Path); i++ {
			ok = e.Path[i] == o.Path[i] || e.Path[i] == stepNames[o.Path[i]]
		}
		if !ok {
			fail("path: want %v, got %v", e.Path, o.Path)
		}
	}
	for _, t := range e.ToolsCalled {
		if !strings.Contains(t, ":") {
			t += ":succeeded"
		}
		if !slices.Contains(o.Tools, t) {
			fail("tools_called: want %q, got %v", t, o.Tools)
		}
	}
	for _, t := range e.ToolsNotCalled {
		if slices.ContainsFunc(o.Tools, func(got string) bool { return got == t || strings.HasPrefix(got, t+":") }) {
			fail("tools_not_called: want %q not called, got %v", t, o.Tools)
		}
	}
	for _, p := range e.Say {
		if !regexp.MustCompile(p).MatchString(o.Replies) { // validated by LoadCases
			fail("say: want a match for %q", p)
		}
	}
	for _, p := range e.NeverSay {
		if m := regexp.MustCompile(p).FindString(o.Replies); m != "" {
			fail("never_say: want no match for %q, matched %q", p, truncate(m, 80))
		}
	}
	if e.EndReason != "" && o.EndReason != e.EndReason {
		fail("end_reason: want %q, got %q", e.EndReason, o.EndReason)
	}
	if e.MaxTurns > 0 && o.TurnCount > e.MaxTurns {
		fail("max_turns: want at most %d, got %d", e.MaxTurns, o.TurnCount)
	}
	if e.MedianReplyMSMax > 0 {
		if len(o.ReplyMS) == 0 {
			fail("median_reply_ms_max: no reply was measured")
		} else if m := medianMS(o.ReplyMS); m > e.MedianReplyMSMax {
			fail("median_reply_ms_max: want at most %d, got %d", e.MedianReplyMSMax, m)
		}
	}
	return fails
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
