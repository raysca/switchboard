package main

import (
	"fmt"
	"sort"
	"strings"
)

// Tolerance is the allowed regression for one metric: limit = base*(1+RelPct/100) + Abs.
// Higher is worse for every metric. Abs is in the metric's own unit.
type Tolerance struct {
	RelPct float64
	Abs    float64
	Why    string
}

// Tolerances is the whole gate policy; edit values here, nothing else encodes them.
// Values come from docs/benchmarks/README.md "Tolerances": run-to-run
// spread of 5-11 runs on the baseline machine, widened for CI noise (PERF_SCALE), and kept
// well below the 100 ms regression the self-test injects. v2v rows are absolute
// because client v2v is ~940 ms of mostly fixed VAD hangover (0.7 s) + clip margin
// (0.1 s): 25% of that would let ~235 ms of real regression through.
var Tolerances = map[string]Tolerance{
	"v2v_p50_ms":             {0, 25, "client v2v includes the VAD hangover; absolute, not relative. 11 runs spread 1.5 ms"},
	"v2v_p95_ms":             {0, 25, "same; 11 runs spread 1.9 ms. Below one 32 ms VAD window, so a one-window slip fails"},
	"v2v_p99_ms":             {0, 40, "tail of n~30 samples is the max; 11 runs spread 2 ms"},
	"server_stage_p95_ms":    {0, 100, "server histogram buckets are 50/100 ms wide: one bucket is the resolution"},
	"cpu_s_per_session_min":  {30, 0.05, "CPU is shared with the load generator and other host work"},
	"mb_per_session":         {25, 2, "RSS above idle; allocator noise"},
	"error_pct":              {0, 1, "percentage points"},
	"rss_growth_pct":         {0, 10, "percentage points, first vs last window"},
	"goroutines_after_drain": {0, 5, "absolute cap above baseline idle count"},
	"goroutine_growth":       {0, 10, "absolute cap, steady windows"},
	"fds_after_drain":        {0, 10, "absolute cap"},
}

// CompareRow is one metric of a baseline-vs-current comparison.
type CompareRow struct {
	Metric   string
	Baseline float64
	Current  float64
	Limit    float64
	Rule     string
	Pass     bool
}

// Compare checks current against baseline using Tolerances scaled by scale
// (1 = as tabulated; >1 loosens, for noisier hosts). Anything that makes the
// comparison unsound (different machine class or run config, missing metric,
// the run's own verdict failing) is a failure, never a warning.
func Compare(base, cur *Report, scale float64) (rows []CompareRow, warnings []string, ok bool) {
	ok = true
	fail := func(msg string) { ok = false; warnings = append(warnings, "FAIL: "+msg) }
	bm, cm := base.Machine, cur.Machine
	if bm.OS != cm.OS || bm.Arch != cm.Arch || bm.CPUs != cm.CPUs || bm.CPUModel != cm.CPUModel { // unknown on one side is a mismatch too
		fail(fmt.Sprintf("machine class differs (baseline %s/%s %d CPUs %q, current %s/%s %d CPUs %q): never compare across machines, generate a baseline for this one",
			bm.OS, bm.Arch, bm.CPUs, bm.CPUModel, cm.OS, cm.Arch, cm.CPUs, cm.CPUModel))
	}
	bc, cc := base.Config, cur.Config
	if bc.Sessions != cc.Sessions || bc.Clip != cc.Clip || bc.TurnManager != cc.TurnManager || bc.VADMode != cc.VADMode || bc.Mode != cc.Mode {
		fail("run config differs from baseline (mode/sessions/clip/turn manager/vad mode)")
	}
	add := func(name, rule string, b, c float64) {
		t := Tolerances[rule]
		lim := b*(1+t.RelPct*scale/100) + t.Abs*scale
		row := CompareRow{name, round2(b), round2(c), round2(lim), fmt.Sprintf("+%g%% +%g", t.RelPct*scale, t.Abs*scale), c <= lim}
		ok = ok && row.Pass
		rows = append(rows, row)
	}
	missing := func(name string, b float64) {
		ok = false
		rows = append(rows, CompareRow{name + " (missing)", round2(b), 0, 0, "", false})
	}
	bv, cv := base.Results.V2V, cur.Results.V2V
	if cv.N == 0 {
		fail("current run has no completed turns")
	} else {
		add("v2v_p50_ms", "v2v_p50_ms", bv.P50, cv.P50)
		add("v2v_p95_ms", "v2v_p95_ms", bv.P95, cv.P95)
		add("v2v_p99_ms", "v2v_p99_ms", bv.P99, cv.P99)
	}
	stages := make([]string, 0, len(base.Results.Server))
	for k := range base.Results.Server {
		stages = append(stages, k)
	}
	sort.Strings(stages)
	for _, k := range stages {
		b := base.Results.Server[k]
		if b.N == 0 {
			continue
		}
		if c := cur.Results.Server[k]; c.N > 0 {
			add("server_"+k+"_p95_ms", "server_stage_p95_ms", b.P95, c.P95)
		} else {
			missing("server_"+k+"_p95_ms", b.P95)
		}
	}
	br, cr := base.Results.Res, cur.Results.Res
	for _, m := range []struct {
		name string
		b, c float64
	}{{"cpu_s_per_session_min", br.CPUSPerSessMin, cr.CPUSPerSessMin}, {"mb_per_session", br.MBPerSession, cr.MBPerSession}} {
		if m.b > 0 && m.c <= 0 { // sampling failed: zero is not "great", it is absent
			missing(m.name, m.b)
		} else {
			add(m.name, m.name, m.b, m.c)
		}
	}
	add("error_pct", "error_pct", base.Results.ErrorPct, cur.Results.ErrorPct)
	add("goroutines_after_drain", "goroutines_after_drain", float64(br.GoroutinesEnd), float64(cr.GoroutinesEnd))
	add("fds_after_drain", "fds_after_drain", float64(br.FDsEnd), float64(cr.FDsEnd))
	if g := base.Results.Growth; g.Valid {
		if !cur.Results.Growth.Valid {
			missing("growth windows", 0)
		} else {
			add("rss_growth_pct", "rss_growth_pct", g.RSSPct, cur.Results.Growth.RSSPct)
			add("goroutine_growth", "goroutine_growth", float64(g.Goroutines), float64(cur.Results.Growth.Goroutines))
		}
	}
	if !cur.Verdict.Pass {
		fail("current run's own verdict failed")
	}
	return rows, warnings, ok
}

func renderCompare(rows []CompareRow, warnings []string, scale float64, ok bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Comparison vs baseline (tolerance table x%g): %s\n\n| metric | baseline | current | limit | rule | |\n|---|---|---|---|---|---|\n", scale, passWord(ok))
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %g | %g | %g | %s | %s |\n", r.Metric, r.Baseline, r.Current, r.Limit, r.Rule, passWord(r.Pass))
	}
	for _, w := range warnings {
		fmt.Fprintf(&b, "\n%s\n", w)
	}
	return b.String()
}
