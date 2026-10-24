package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"
)

const (
	reportSchema   = "govoice.agent-eval/v1"
	maxReportLine  = 500
	echoModeNote   = "note: echo mode checks the plumbing (handoffs, tools, hang-up), never prompts or model behaviour"
	reportFileMode = 0o600
)

// Report is the result of one eval run. It carries no timestamps, so two runs diff cleanly.
type Report struct {
	Schema  string       `json:"schema"`
	Mode    string       `json:"mode"`
	Results []CaseResult `json:"results"`
}

// CaseResult is one case over all its runs; Passed is the min-pass verdict.
type CaseResult struct {
	Case   string      `json:"case"`
	Passed bool        `json:"passed"`
	Runs   []RunResult `json:"runs"`
}

// RunResult is one call: what the server stored, what the client saw, and what failed.
type RunResult struct {
	SessionID      string   `json:"session_id"`
	AgentVersionID string   `json:"agent_version_id"`
	ConfigHash     string   `json:"config_hash"`
	Path           []string `json:"path"`
	Tools          []string `json:"tools"`
	EndReason      string   `json:"end_reason"`
	TurnCount      int      `json:"turn_count"`
	MedianReplyMS  int      `json:"median_reply_ms"`
	Errors         []string `json:"errors"` // harness problems: setup, timeouts
	Transcript     []Line   `json:"transcript"`
	Failures       []string `json:"failures"` // failed assertions
}

// OK reports whether the run had no harness error and no failed assertion.
func (r RunResult) OK() bool { return len(r.Errors) == 0 && len(r.Failures) == 0 }

// view returns a copy sorted by case name, with nil lists made empty and transcript lines bounded.
func (r Report) view() Report {
	out := Report{Schema: r.Schema, Mode: r.Mode, Results: slices.Clone(r.Results)}
	slices.SortFunc(out.Results, func(a, b CaseResult) int { return strings.Compare(a.Case, b.Case) })
	for i := range out.Results {
		out.Results[i].Runs = slices.Clone(out.Results[i].Runs)
		for j := range out.Results[i].Runs {
			run := &out.Results[i].Runs[j]
			run.Path, run.Tools = nonNil(run.Path), nonNil(run.Tools)
			run.Errors, run.Failures = nonNil(run.Errors), nonNil(run.Failures)
			tr := make([]Line, len(run.Transcript))
			for k, l := range run.Transcript {
				tr[k] = Line{l.Who, truncate(l.Text, maxReportLine)}
			}
			run.Transcript = tr
		}
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// visible writes control characters (an agent's text can carry terminal escapes) as \n, \t, \x1b, ...
func visible(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsControl(r) && r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.IsControl(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// WriteText prints the human report.
func WriteText(w io.Writer, r Report) {
	r = r.view()
	passed := 0
	for _, c := range r.Results {
		ok := 0
		for _, run := range c.Runs {
			if run.OK() {
				ok++
			}
		}
		verdict := "FAIL"
		if c.Passed {
			verdict, passed = "PASS", passed+1
		}
		fmt.Fprintf(w, "%s %s (%s, %d/%d runs)\n", verdict, c.Case, r.Mode, ok, len(c.Runs))
		for i, run := range c.Runs {
			fmt.Fprintf(w, "  run %d: session %s, agent version %s, config %s\n", i+1, run.SessionID, run.AgentVersionID, run.ConfigHash)
			tools := "none"
			if len(run.Tools) > 0 {
				tools = strings.Join(run.Tools, ", ")
			}
			fmt.Fprintf(w, "    path: %s\n    tools: %s\n", visible(strings.Join(run.Path, " > ")), visible(tools))
			fmt.Fprintf(w, "    end: %s, %d turns, median reply %d ms\n", visible(run.EndReason), run.TurnCount, run.MedianReplyMS)
			for _, l := range run.Transcript {
				fmt.Fprintf(w, "    %s: %s\n", l.Who, visible(l.Text))
			}
			for _, e := range run.Errors {
				fmt.Fprintf(w, "    ERROR %s\n", visible(e))
			}
			for _, f := range run.Failures {
				fmt.Fprintf(w, "    FAIL %s\n", visible(f))
			}
		}
	}
	if r.Mode == modeEcho {
		fmt.Fprintln(w, echoModeNote)
	}
	fmt.Fprintf(w, "agent-eval: %d passed, %d failed (%s)\n", passed, len(r.Results)-passed, r.Mode)
}

// WriteJSON writes the report to path with mode 0600 (it holds call transcripts).
func WriteJSON(path string, r Report) error {
	b, err := json.MarshalIndent(r.view(), "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, reportFileMode)
	if err != nil {
		return err
	}
	if err := f.Chmod(reportFileMode); err != nil { // OpenFile's mode only applies to a new file
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
