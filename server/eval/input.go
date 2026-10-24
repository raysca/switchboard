package eval

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"
)

// Input is what a scorer sees of one call, built from the stored rows (BuildInput), so a suite run
// and a live call are scored alike. ReplyMS, ClientErrors and Dropped are client facts: they are
// zero for a call nobody dialed.
type Input struct {
	Turns           []Line // caller and agent text from the stored turns
	Path            []string
	StepNames       map[string]string
	Tools           []ToolRow
	HandoffFailures int
	EndReason       string
	TurnCount       int
	Prompts         map[string]string // step id -> system prompt
	Case            *Case
	ReplyMS         []int
	ClientErrors    int
	Dropped         int
}

// ToolRow is one tool call by name and outcome. Arguments and Result stay empty: names and outcomes
// only (docs/superpowers/specs/2026-10-24-agent-evaluation-design.md, Privacy).
type ToolRow struct {
	Name, Status, Step string
	StartedAt, EndedAt time.Time
	Arguments, Result  string
}

// BuildInput reads the stored rows of one call. a.Start is the first path entry; c is the case the
// call served, or nil for a live call.
func BuildInput(a Agent, o Observed, c *Case) Input {
	st := o.Stored
	in := Input{StepNames: a.StepNames, Prompts: a.Prompts, Case: c, EndReason: st.Session.EndReason, TurnCount: st.Session.TurnCount,
		ReplyMS: o.ReplyMS, ClientErrors: o.ClientErrors, Dropped: o.Dropped, Path: []string{a.Start}}
	type hop struct {
		at time.Time
		to string
	}
	hops := make([]hop, 0, len(st.Spans))
	spans := slices.Clone(st.Spans)
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].StartedAt.Before(spans[j].StartedAt) })
	for _, sp := range spans {
		switch sp.Name {
		case "handoff.failed":
			in.HandoffFailures++
		case "handoff.active":
			var at struct {
				To string `json:"to_step"`
			}
			if json.Unmarshal([]byte(sp.AttributesJSON), &at) != nil || at.To == "" {
				continue
			}
			in.Path = append(in.Path, at.To)
			hops = append(hops, hop{sp.StartedAt, at.To})
		}
	}
	calls := slices.Clone(st.Tools)
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].StartedAt.Before(calls[j].StartedAt) })
	for _, tc := range calls {
		step := a.Start
		for _, h := range hops {
			if !h.at.After(tc.StartedAt) {
				step = h.to
			}
		}
		in.Tools = append(in.Tools, ToolRow{Name: tc.RemoteName, Status: tc.Status, Step: step, StartedAt: tc.StartedAt, EndedAt: tc.EndedAt})
	}
	for _, t := range st.Turns {
		if t.Transcript != "" {
			in.Turns = append(in.Turns, Line{"caller", t.Transcript})
		}
		if t.AssistantText != "" {
			in.Turns = append(in.Turns, Line{"agent", t.AssistantText})
		}
	}
	return in
}

// ToolStrings lists the tools as "name:status" in call order.
func (in Input) ToolStrings() []string {
	out := make([]string, len(in.Tools))
	for i, t := range in.Tools {
		out[i] = t.Name + ":" + t.Status
	}
	return out
}

// replies is every agent line, newline-joined.
func (in Input) replies() string {
	var r []string
	for _, l := range in.Turns {
		if l.Who == "agent" {
			r = append(r, l.Text)
		}
	}
	return strings.Join(r, "\n")
}

// MedianMS is the upper median of ms (0 for none).
func MedianMS(ms []int) int {
	if len(ms) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(ms))
	return s[len(s)/2]
}

// Truncate cuts s to n runes, adding "..." when it cut.
func Truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
