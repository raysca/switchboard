package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// tol is the allowed regression over baseline in absolute ms. Framework overhead
// is ~0.01-0.2 ms, so relative tolerances are meaningless; these allowances sit
// above scheduler noise on a loaded host and far below a 100 ms stage regression
// (docs/benchmarks/README.md).
var tol = map[string]float64{"overhead_p50_ms": 1, "overhead_p95_ms": 3, "overhead_p99_ms": 6}

const goroutineLeakCap = 2 // goroutines_after may exceed goroutines_before by this much

type row struct {
	Metric         string
	Base, Cur, Lim float64
	Pass           bool
}

// compare returns the rows and whether the run passes. Any difference in
// machine or run shape fails: the baseline would not describe this run.
func compare(base, cur Report) ([]row, []string, bool) {
	var why []string
	if base.Machine == "" || base.Machine != cur.Machine {
		why = append(why, fmt.Sprintf("machine differs: baseline %q, current %q", base.Machine, cur.Machine))
	}
	if base.Turns != cur.Turns || base.Sessions != cur.Sessions || base.SimSTT != cur.SimSTT || base.SimLLM != cur.SimLLM || base.SimTTS != cur.SimTTS {
		why = append(why, "run shape (turns/sessions/delays) differs from baseline")
	}
	if cur.Overhead.N == 0 || cur.Overhead.N < base.Overhead.N {
		why = append(why, fmt.Sprintf("current run completed %d turns, baseline %d", cur.Overhead.N, base.Overhead.N))
	}
	var rows []row
	for _, m := range []struct {
		name string
		b, c float64
	}{{"overhead_p50_ms", base.Overhead.P50, cur.Overhead.P50}, {"overhead_p95_ms", base.Overhead.P95, cur.Overhead.P95}, {"overhead_p99_ms", base.Overhead.P99, cur.Overhead.P99}} {
		rows = append(rows, row{m.name, m.b, m.c, m.b + tol[m.name], m.c <= m.b+tol[m.name]})
	}
	leak := float64(cur.GoroutinesAfter - cur.GoroutinesBefore)
	rows = append(rows, row{"goroutine_leak", 0, leak, goroutineLeakCap, leak <= goroutineLeakCap})
	ok := len(why) == 0
	for _, r := range rows {
		ok = ok && r.Pass
	}
	return rows, why, ok
}

func compareFile(path string, cur Report) int {
	raw, err := os.ReadFile(path)
	var base Report
	if err == nil {
		err = json.Unmarshal(raw, &base)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "govoice-bench: baseline:", err)
		return 2
	}
	rows, why, ok := compare(base, cur)
	fmt.Println("| metric | baseline | current | limit | |\n|---|---|---|---|---|")
	for _, r := range rows {
		fmt.Printf("| %s | %.3f | %.3f | %.3f | %v |\n", r.Metric, r.Base, r.Cur, r.Lim, map[bool]string{true: "pass", false: "FAIL"}[r.Pass])
	}
	for _, w := range why {
		fmt.Println("FAIL:", w)
	}
	fmt.Println("bench gate:", map[bool]string{true: "PASS", false: "FAIL"}[ok])
	if !ok {
		return 1
	}
	return 0
}
