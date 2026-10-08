package provtest

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// JSON renders the report with every secret-looking string redacted.
func (r Report) JSON() []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return []byte(Redact(string(b)) + "\n")
}

// WriteJSON writes results.json (mode 0600: it holds provider error text).
func (r Report) WriteJSON(path string) error { return os.WriteFile(path, r.JSON(), 0o600) }

// checkOrder is the column order per stage; unknown checks follow alphabetically.
var checkOrder = map[Stage][]string{
	STT: {"build", "no_error", "wer", "interim_before_final", "final_latency", "cumulative", "turn_binding", "resumed_speech", "reuse_after_idle", "barge_in_rebind", "bad_credential", "close_leaks"},
	TTS: {"build", "no_error", "synth", "format", "final_flags", "stopped", "usage_chars", "ttfa", "duration_plausible", "non_silent", "no_riff", "total_time", "interrupt", "bad_credential", "close_leaks"},
	LLM: {"build", "no_error", "stream", "ttft", "total_time", "usage", "tool_round_trip", "cancel", "bad_credential", "bad_model", "close_leaks"},
}

func cell(c *Check) string {
	if c == nil {
		return "-"
	}
	switch c.Status {
	case Skip:
		return "skip"
	case Fail:
		return "**FAIL**" + num(c)
	}
	return "pass" + num(c)
}

func num(c *Check) string {
	if c.Unit == "" || c.Value == 0 && c.Unit != "wer" { // a WER of 0.00 is a result
		return ""
	}
	switch c.Unit {
	case "ms":
		return fmt.Sprintf(" %.0f ms", c.Value)
	case "s":
		return fmt.Sprintf(" %.1f s", c.Value)
	case "wer":
		return fmt.Sprintf(" %.2f", c.Value)
	case "chars":
		return fmt.Sprintf(" %.0f", c.Value)
	}
	return fmt.Sprintf(" %.0f %s", c.Value, c.Unit)
}

// Markdown is the provider x check matrix, one table per stage, with failure details.
func (r Report) Markdown() string {
	var b strings.Builder
	limit := fmt.Sprintf("limit %d", r.MaxCalls)
	if r.MaxCalls == 0 {
		limit = "summed over merged runs"
	}
	fmt.Fprintf(&b, "Run %s, mode `%s`, %d planned provider calls (upper-bound estimate, %s), %d actual requests measured, repeat %d. Numbers are small-n timings from one machine (medians over repeats), not benchmarks. No secrets appear here.\n", r.At.Format("2006-01-02 15:04 UTC"), r.Mode, r.Calls, limit, r.Actual, r.Repeat)
	var details []string
	for _, st := range AllStages {
		var rows []Result
		for _, res := range r.Results {
			if res.Stage == st {
				rows = append(rows, res)
			}
		}
		if len(rows) == 0 {
			continue
		}
		cols := columns(st, rows)
		fmt.Fprintf(&b, "\n### %s\n\n| provider | result |", strings.ToUpper(string(st)))
		for _, c := range cols {
			fmt.Fprintf(&b, " %s |", c)
		}
		b.WriteString("\n|---|---|")
		for range cols {
			b.WriteString("---|")
		}
		b.WriteString("\n")
		for _, res := range rows {
			if res.Status == Skip {
				fmt.Fprintf(&b, "| %s | skip: %s |", res.Provider, res.Reason)
				for range cols {
					b.WriteString(" |")
				}
				b.WriteString("\n")
				continue
			}
			fmt.Fprintf(&b, "| %s | %s |", res.Provider, res.Status)
			for _, col := range cols {
				var found *Check
				for i := range res.Checks {
					if res.Checks[i].Name == col {
						found = &res.Checks[i]
					}
				}
				fmt.Fprintf(&b, " %s |", cell(found))
			}
			b.WriteString("\n")
			for _, c := range res.Checks {
				if c.Status == Fail {
					details = append(details, fmt.Sprintf("- `%s.%s` %s: %s", res.Provider, res.Stage, c.Name, c.Detail))
				}
			}
		}
	}
	if len(details) > 0 {
		b.WriteString("\n### Failures\n\n" + strings.Join(details, "\n") + "\n")
	}
	return Redact(b.String())
}

func columns(st Stage, rows []Result) []string {
	have := map[string]bool{}
	for _, r := range rows {
		for _, c := range r.Checks {
			have[c.Name] = true
		}
	}
	var out []string
	for _, n := range checkOrder[st] {
		if have[n] {
			out = append(out, n)
			delete(have, n)
		}
	}
	var rest []string
	for n := range have {
		rest = append(rest, n)
	}
	slices.Sort(rest)
	return append(out, rest...)
}

// MergeFiles combines saved reports (later files win per provider+stage, a skip
// never replaces a real result) into one report; table rows no file covers are
// resolved against env only to explain why they were not run (no calls).
func MergeFiles(files []string, table []Provider, env Env) (Report, error) {
	out := Report{Mode: "live"}
	have := map[string]int{}
	for _, f := range files {
		b, err := os.ReadFile(strings.TrimSpace(f))
		if err != nil {
			return out, err
		}
		var r Report
		if err := json.Unmarshal(b, &r); err != nil {
			return out, err
		}
		out.Calls += r.Calls
		out.Actual += r.Actual
		out.Repeat = max(out.Repeat, r.Repeat)
		if out.At.Before(r.At) {
			out.At = r.At
		}
		for _, res := range r.Results {
			k := res.Provider + "." + string(res.Stage)
			i, ok := have[k]
			switch {
			case res.Status == Skip && ok:
			case ok:
				out.Results[i] = res
			default:
				have[k] = len(out.Results)
				out.Results = append(out.Results, res)
			}
		}
	}
	var ordered []Result
	for _, p := range table {
		res := Result{Provider: p.ID, Stage: p.Stage, Status: Skip}
		if i, ok := have[p.ID+"."+string(p.Stage)]; ok {
			res = out.Results[i]
		}
		if res.Status == Skip && (res.Reason == "" || res.Reason == "not selected") {
			if _, why := Live(p, env); why != "" {
				res.Reason = why
			} else {
				res.Reason = "not run"
			}
		}
		ordered = append(ordered, res)
	}
	out.Results = ordered
	return out, nil
}

// Legend explains the columns; it is written above the matrix in docs/benchmarks/provider-matrix.md.
const Legend = `## Checks

Numbers in a cell are the check's headline value (ms, s, WER, characters).

- **TTS**: synth (3 sentences as synthesis units), format (PCM16 mono at the 24 kHz pipeline rate, exact 20 ms frames), final_flags (one IsFinal per unit as its last frame, TurnFinal only on the turn's last), stopped (TTSStopped after each unit), usage_chars (billed characters equal text), ttfa (first audio per unit, value = the cold first unit), duration_plausible (0.02-0.25 s of audio per character), non_silent (RMS), no_riff (raw PCM, no WAV header), total_time, interrupt (barge-in stops audio within 300 ms, the cancelled unit neither completes nor errors; an explicit skip when the unit finished before the barge-in), bad_credential (client error class, no retry: under 10 s, secret absent from the error), close_leaks (session tasks and goroutines after Close).
- **STT** (16 kHz clips at real time with UserStarted/Stopped and TurnEvent Started/Stopped/Completed): wer (per clip against the known text after normalisation: case, punctuation, hyphens, small numerals, common contractions; per-clip limit max_wer in clips.json, 0.20-0.25 = one error on the short clips; Google and NVIDIA score 0.00 on every clip), interim_before_final (skip when the provider emits no interims), final_latency (final after the VAD stop), cumulative (each final extends the previous one), turn_binding (candidates carry the started turn and generation), resumed_speech (two sentences separated by a 1 s pause in one turn keep both), reuse_after_idle (second turn after an idle gap), barge_in_rebind (same-turn TurnEventStarted at a newer generation keeps results flowing under it), bad_credential, close_leaks.
- **LLM**: stream (deltas equal the complete text and answer 2+2), ttft, total_time, usage (prompt and completion tokens), tool_round_trip (get_weather executed with valid JSON arguments whose city is Paris, and the reply uses the result), cancel (deltas stop within 300 ms of a barge-in; an explicit skip when the reply completed first), bad_credential, bad_model (both client errors, redacted), close_leaks.

A row passes only when every check that ran passed; a row the call budget (-max-calls) cut short is a skip ("incomplete"), and once the budget is spent the remaining rows are not built or called.

Fixtures: tools/provtest/testdata/clips (Google TTS, resampled to 16 kHz, IMA ADPCM WAV, under 100 KB, decoded to PCM16 before streaming; the codec keeps about 34 dB SNR on these clips, so plain PCM would not change WER).

`
