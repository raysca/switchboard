package main

import (
	"encoding/json"
	"fmt"
	coreturn "govoice/core/turn"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
)

// Report is everything one run produced: results.json is this struct.
type Report struct {
	Started   time.Time       `json:"started"`
	Commit    string          `json:"commit"`
	Dirty     bool            `json:"dirty"`
	GoVersion string          `json:"go_version"`
	Tags      []string        `json:"build_tags"`
	Modes     []string        `json:"modes"`
	Scenarios []string        `json:"scenarios"`
	Repeat    int             `json:"repeat"`
	Estimate  int             `json:"estimated_provider_calls"` // upper bound
	Actual    int             `json:"actual_provider_requests"` // measured at the provider boundary + clip synthesis
	Selftest  bool            `json:"selftest,omitempty"`
	Merged    bool            `json:"merged,omitempty"` // docs copy: results of several runs (Result.At says which)
	Profiles  []ProfileReport `json:"profiles"`
	Results   []*Result       `json:"results"`
	Warnings  []string        `json:"server_log_warnings,omitempty"`
}

// ProfileReport is a profile's effective (env-expanded) config; Skipped says why it did not run.
type ProfileReport struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Skipped     string            `json:"skipped,omitempty"`
	STT         string            `json:"stt"`
	LLM         string            `json:"llm"`
	TTS         string            `json:"tts"`
	Tags        []string          `json:"build_tags,omitempty"`
	Infra       map[string]string `json:"infra,omitempty"`
	Interims    string            `json:"interims,omitempty"`
	Notes       string            `json:"notes,omitempty"`
}

func stageText(s Stage) string {
	parts := []string{s.Provider}
	for _, v := range []string{s.Model, s.Voice, s.Language} {
		if v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " ")
}

func profileReport(p Profile, skipped string) ProfileReport {
	pr := ProfileReport{Name: p.Name, Description: p.Description, Skipped: skipped, STT: stageText(p.Stages.STT), LLM: stageText(p.Stages.LLM),
		TTS: stageText(p.Stages.TTS), Tags: p.tags(), Interims: p.Interims, Notes: p.Notes}
	for k, m := range p.Infra {
		for kk, v := range m {
			if pr.Infra == nil {
				pr.Infra = map[string]string{}
			}
			pr.Infra[k+"."+kk] = v
		}
	}
	return pr
}

// sourceVersion is the short commit of the tree under test and whether it has local changes.
func sourceVersion(root string) (string, bool) {
	run := func(args ...string) string {
		c := exec.Command("git", args...)
		c.Dir = root
		b, _ := c.Output()
		return strings.TrimSpace(string(b))
	}
	commit := run("rev-parse", "--short=10", "HEAD")
	if commit == "" {
		commit = "unknown"
	}
	return commit, run("status", "--porcelain") != ""
}

func newReport(root string) *Report {
	commit, dirty := sourceVersion(root)
	return &Report{Started: time.Now().UTC().Truncate(time.Second), Commit: commit, Dirty: dirty, GoVersion: runtime.Version()}
}

// formatMetrics renders k=v pairs on one line.
func formatMetrics(r *Result) string {
	var parts []string
	for _, kv := range r.Metrics {
		v := fmt.Sprint(kv.V)
		if _, ok := kv.V.(string); ok {
			v = fmt.Sprintf("%q", kv.V)
		}
		parts = append(parts, kv.K+"="+v)
	}
	return strings.Join(parts, " ")
}

// headline is the one number a matrix cell shows for a scenario.
func headline(r *Result) string {
	f := func(key string, unit string, div float64) string {
		if v, ok := r.num(key); ok {
			return fmt.Sprintf("%.1f%s", v/div, unit)
		}
		return ""
	}
	switch r.Scenario {
	case "S1":
		return f("v2v_ms", "s", 1000)
	case "S2":
		return f("audio_stop_ms", "ms", 1)
	case "S4":
		if v, ok := r.num("user_turns"); ok {
			return fmt.Sprintf("%.0f turn", v)
		}
	case "S5":
		if v, ok := r.num("first_prompt_audio_s"); ok {
			return fmt.Sprintf("%.1fs", v)
		}
	case "S7":
		return f("text_to_audio_ms", "s", 1000)
	case "S8":
		if v, ok := r.num("silent_for_s"); ok {
			return fmt.Sprintf("%.0fs", v)
		}
	}
	return ""
}

// cell summarises the repeats of one (profile, mode, scenario) for the matrix.
func cell(rs []*Result) string {
	if len(rs) == 0 {
		return "-"
	}
	count := map[string]int{}
	var head string
	for _, r := range rs {
		count[r.Status]++
		if head == "" && r.Status == stPass || head == "" && r.Status == stFail || head == "" && r.Status == stInfo {
			head = headline(r)
		}
	}
	n := len(rs)
	label := func(st, text string) string {
		s := text
		if n > 1 {
			s = fmt.Sprintf("%s %d/%d", text, count[st], n)
		}
		if head != "" {
			s += " " + head
		}
		return s
	}
	switch {
	case count[stErr] > 0:
		return "ERROR"
	case count[stFail] > 0:
		return label(stFail, "FAIL")
	case count[stPass] == n:
		return label(stPass, "PASS")
	case count[stNA] == n:
		return "N/A"
	case count[stSkip] == n:
		return "SKIP"
	case count[stInfo]+count[stPass] == n:
		s := "info"
		if head != "" {
			s += " " + head
		}
		return s
	}
	return "mixed"
}

func groupResults(rs []*Result) map[[3]string][]*Result {
	g := map[[3]string][]*Result{}
	for _, r := range rs {
		k := [3]string{r.Profile, r.Mode, r.Scenario}
		g[k] = append(g[k], r)
	}
	return g
}

// renderMarkdown is the matrix report (docs/benchmarks/flow-matrix.md).
func renderMarkdown(rep *Report) string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	dirty := ""
	if rep.Dirty {
		dirty = " (working tree had local changes)"
	}
	w("# Agent provider matrix\n\n")
	w("One standard agent (same system prompt and scenario set) run across provider profiles with `tools/livecheck` (`make agent-matrix`). Small-n timings, single machine: a regression signal and a capability map, not a benchmark.\n\n")
	if rep.Merged {
		w("- Accumulated over runs: each profile x mode x scenario keeps its latest run (`last run` below; `agent-matrix.json` has the stamp per result). Latest run: %s UTC, commit `%s`%s, %s\n", rep.Started.Format("2006-01-02 15:04"), rep.Commit, dirty, rep.GoVersion)
	} else {
		w("- Run: %s UTC, server commit `%s`%s, %s\n", rep.Started.Format("2006-01-02 15:04"), rep.Commit, dirty, rep.GoVersion)
	}
	w("- Server build tags: %s\n", codeList(rep.Tags))
	w("- Modes: %s; scenarios: %s; repeat: %d; provider requests: %d actual (upper-bound estimate %d)\n\n", codeList(rep.Modes), codeList(rep.Scenarios), rep.Repeat, rep.Actual, rep.Estimate)
	if rep.Selftest {
		w("> Offline self-test with fake providers: nothing here says anything about a real provider.\n\n")
	}

	w("## Profiles\n\n| profile | STT | LLM | TTS | build tags | state |\n|---|---|---|---|---|---|\n")
	for _, p := range rep.Profiles {
		state := "ran"
		if p.Skipped != "" {
			state = "SKIPPED: " + p.Skipped
		}
		if last := lastRun(rep, p.Name); rep.Merged && last != "" {
			state += ", last run " + last
		}
		w("| %s | %s | %s | %s | %s | %s |\n", p.Name, esc(p.STT), esc(p.LLM), esc(p.TTS), codeList(p.Tags), esc(state))
	}
	for _, p := range rep.Profiles {
		if p.Notes != "" || len(p.Infra) > 0 {
			extra := ""
			for _, k := range sortedKeys(p.Infra) {
				extra += fmt.Sprintf(" `%s=%s`", k, p.Infra[k])
			}
			w("\n- **%s**: %s%s", p.Name, p.Notes, extra)
		}
	}
	w("\n\n")

	g := groupResults(rep.Results)
	for _, m := range rep.Modes {
		w("## Mode `%s`\n\n| profile |", m)
		for _, s := range rep.Scenarios {
			w(" %s |", s)
		}
		w("\n|---|")
		for range rep.Scenarios {
			w("---|")
		}
		w("\n")
		for _, p := range rep.Profiles {
			if p.Skipped != "" {
				continue
			}
			w("| %s |", p.Name)
			for _, s := range rep.Scenarios {
				w(" %s |", cell(g[[3]string{p.Name, m, s}]))
			}
			w("\n")
		}
		w("\n")
	}

	w("## Voice-to-voice breakdown (S1)\n\nClient v2v is end of the clip to first bot audio; the stage columns are server means over the S1 turns (`voice_latency_seconds`).\n\n")
	w("| profile | mode | v2v | STT final | LLM first token | TTS first byte |\n|---|---|---|---|---|---|\n")
	for _, p := range rep.Profiles {
		for _, m := range rep.Modes {
			rs := g[[3]string{p.Name, m, "S1"}]
			if len(rs) == 0 || rs[0].Status == stNA {
				continue
			}
			w("| %s | %s | %s | %s | %s | %s |\n", p.Name, m, med(rs, "v2v_ms"), med(rs, "stt_final_ms"), med(rs, "llm_ttft_ms"), med(rs, "tts_ttfb_ms"))
		}
	}

	w("\nAdditive split of the first S1 turn (server timeline, same clock): clip end to speech_stop (vad), to the user message at the LLM (eot: STT final + turn commit), to first token (ttft), to first TTS unit (unit wait), to first TTS audio (tts), to client receipt (tail).\n\n")
	w("| profile | mode | vad | eot | ttft | unit wait | tts | tail |\n|---|---|---|---|---|---|---|---|\n")
	for _, p := range rep.Profiles {
		for _, m := range rep.Modes {
			rs := g[[3]string{p.Name, m, "S1"}]
			if len(rs) == 0 || rs[0].Status == stNA {
				continue
			}
			w("| %s | %s |", p.Name, m)
			for _, k := range []string{"vad", "eot", "ttft", "unit_wait", "tts", "tail"} {
				w(" %s |", med(rs, "t1.bd_"+k+"_ms"))
			}
			w("\n")
		}
	}

	w("\n## STT final vs built-in p99\n\nMedian clip end to final user transcription (S1) next to the p99 the speech timeout waits for (core/turn DefaultSTTP99); a measured value near or above the p99 means the table is stale.\n\n")
	w("| profile | mode | final (ms) | p99 (ms) |\n|---|---|---|---|\n")
	for _, p := range rep.Profiles {
		for _, m := range rep.Modes {
			rs := g[[3]string{p.Name, m, "S1"}]
			if len(rs) == 0 || rs[0].Status == stNA || p.STT == "" {
				continue
			}
			w("| %s | %s | %s | %d |\n", p.Name, m, med(rs, "t1.final_tr_ms"), coreturn.DefaultSTTP99(strings.Fields(p.STT)[0]).Milliseconds())
		}
	}

	renderRequests(w, rep)

	w("\n## Details\n\nFailed checks, errors, N/A reasons and skips.\n\n")
	var lines []string
	for _, r := range rep.Results {
		switch r.Status {
		case stFail:
			for _, c := range r.Checks {
				if !c.Pass {
					lines = append(lines, fmt.Sprintf("- `%s` %s %s: FAIL %s %s", r.Profile, r.Mode, r.Scenario, c.Name, c.Note))
				}
			}
		case stErr, stNA, stSkip:
			lines = append(lines, fmt.Sprintf("- `%s` %s %s: %s %s", r.Profile, r.Mode, r.Scenario, strings.ToUpper(r.Status), r.Reason))
		}
	}
	lines = uniq(lines)
	if len(lines) == 0 {
		w("None.\n")
	}
	for _, l := range lines {
		w("%s\n", esc(l))
	}
	if len(rep.Warnings) > 0 {
		w("\nServer WARN/ERROR log lines (redacted):\n\n")
		for _, l := range uniq(rep.Warnings) {
			w("- `%s`\n", strings.ReplaceAll(truncate(l, 200), "`", "'"))
		}
	}
	return b.String()
}

// lastRun is the newest run stamp among a profile's results.
func lastRun(rep *Report, profile string) (last string) {
	for _, r := range rep.Results {
		if r.Profile == profile && r.At > last {
			last = r.At
		}
	}
	return last
}

// mergeRuns folds rep into prev (the docs copy) so -write-docs accumulates: a (profile, mode,
// scenario) that ran now replaces all its earlier repeats, everything else is kept with its stamp.
func mergeRuns(prev, rep *Report) *Report {
	stamp := rep.Started.Format("2006-01-02 15:04Z") + " " + rep.Commit
	if rep.Dirty {
		stamp += "+dirty"
	}
	out := *rep
	out.Merged, out.Results, out.Profiles, out.Warnings = true, nil, nil, rep.Warnings
	ran := map[[3]string]bool{}
	for _, r := range rep.Results {
		if r.Status != stSkip { // a skip (missing env, interrupted, -max-calls) never hides an older real result
			ran[[3]string{r.Profile, r.Mode, r.Scenario}] = true
		}
	}
	if prev != nil {
		for _, r := range prev.Results {
			if !ran[[3]string{r.Profile, r.Mode, r.Scenario}] {
				out.Results = append(out.Results, r)
			}
		}
		out.Modes, out.Scenarios = union(prev.Modes, rep.Modes), union(prev.Scenarios, rep.Scenarios)
		sort.Strings(out.Scenarios)
	}
	for _, r := range rep.Results {
		if ran[[3]string{r.Profile, r.Mode, r.Scenario}] {
			c := *r
			c.At = stamp
			out.Results = append(out.Results, &c)
		}
	}
	has := func(name string) bool {
		for _, r := range out.Results {
			if r.Profile == name {
				return true
			}
		}
		return false
	}
	for _, p := range rep.Profiles { // this run's effective config wins
		if p.Skipped == "" || !has(p.Name) {
			out.Profiles = append(out.Profiles, p)
		}
	}
	if prev != nil {
		for _, p := range prev.Profiles {
			if !slices.ContainsFunc(out.Profiles, func(o ProfileReport) bool { return o.Name == p.Name }) {
				out.Profiles = append(out.Profiles, p)
			}
		}
	}
	return &out
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, s := range b {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func med(rs []*Result, key string) string {
	var v []float64
	for _, r := range rs {
		if x, ok := r.num(key); ok {
			v = append(v, x)
		}
	}
	if len(v) == 0 {
		return "-"
	}
	sort.Float64s(v)
	return fmt.Sprintf("%.0f ms", v[len(v)/2])
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func codeList(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return "`" + strings.Join(l, "`, `") + "`"
}

func esc(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// writeReport writes results.json and flow-matrix.md into dir (every byte redacted), and
// returns the markdown path.
func writeReport(rep *Report, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	md := filepath.Join(dir, "agent-matrix.md")
	return md, writeFiles(rep, filepath.Join(dir, "results.json"), md)
}

// mergeDocs merges rep into the committed docs copy (docsDir/flow-matrix.json) and re-renders
// docsDir/flow-matrix.md from the merged results.
func mergeDocs(rep *Report, docsDir string) error {
	j := filepath.Join(docsDir, "agent-matrix.json")
	var prev *Report
	if b, err := os.ReadFile(j); err == nil {
		prev = &Report{}
		if err := json.Unmarshal(b, prev); err != nil {
			return fmt.Errorf("%s: %w (fix or delete it)", j, err)
		}
	}
	return writeFiles(mergeRuns(prev, rep), j, filepath.Join(docsDir, "agent-matrix.md"))
}

func writeFiles(rep *Report, jsonPath, mdPath string) error {
	raw, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	var tree any
	_ = json.Unmarshal(raw, &tree)
	j, err := json.MarshalIndent(redactTree(tree), "", " ") // redact string values, not the JSON text (patterns eat quotes)
	if err != nil {
		return err
	}
	if err := os.WriteFile(jsonPath, append(j, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(mdPath, []byte(theRedactor.String(renderMarkdown(rep))), 0o644)
}

func redactTree(v any) any {
	switch x := v.(type) {
	case string:
		return theRedactor.String(x)
	case []any:
		for i := range x {
			x[i] = redactTree(x[i])
		}
	case map[string]any:
		for k, e := range x {
			x[k] = redactTree(e)
		}
	}
	return v
}

// renderRequests is the estimate-vs-actual table: per profile and scenario (all modes and repeats
// summed), then a per-profile total. Fake profiles spend nothing and are left out.
func renderRequests(w func(string, ...any), rep *Report) {
	type agg struct{ est, act int }
	cells := map[[2]string]*agg{}
	totals := map[string]*agg{}
	stages := map[string]map[string]int{}
	var profs []string
	for _, r := range rep.Results {
		if r.Estimate == 0 && r.Requests == 0 {
			continue
		}
		if totals[r.Profile] == nil {
			totals[r.Profile], stages[r.Profile] = &agg{}, map[string]int{}
			profs = append(profs, r.Profile)
		}
		k := [2]string{r.Profile, r.Scenario}
		if cells[k] == nil {
			cells[k] = &agg{}
		}
		for _, a := range []*agg{cells[k], totals[r.Profile]} {
			a.est += r.Estimate
			a.act += r.Requests
		}
		for s, n := range r.ReqBy {
			stages[r.Profile][s] += n
		}
	}
	if len(profs) == 0 {
		return
	}
	w("\n## Provider requests: upper-bound estimate vs actual\n\nActual is measured at the provider boundary (`voice_provider_requests_total`: HTTP calls, WebSocket dials, gRPC opens, retries included); the estimate is a deliberately pessimistic upper bound used for the `-max-calls` pre-check. Summed over modes and repeats.\n\n")
	w("| profile | scenario | upper-bound estimate | actual |\n|---|---|---|---|\n")
	for _, p := range profs {
		for _, s := range rep.Scenarios {
			if a := cells[[2]string{p, s}]; a != nil {
				w("| %s | %s | %d | %d |\n", p, s, a.est, a.act)
			}
		}
		w("| **%s** | **total** | **%d** | **%d** |\n", p, totals[p].est, totals[p].act)
	}
	w("\nActual by provider/stage:")
	for _, p := range profs {
		var parts []string
		for _, k := range sortedKeys(stages[p]) {
			parts = append(parts, fmt.Sprintf("`%s` %d", k, stages[p][k]))
		}
		w(" **%s**: %s.", p, strings.Join(parts, ", "))
	}
	w("\n")
}
