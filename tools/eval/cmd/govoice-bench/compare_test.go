package main

import "testing"

func rep(p50, p95, p99 float64) Report {
	return Report{Machine: "m", Turns: 10, Sessions: 2, Overhead: Stat{N: 20, P50: p50, P95: p95, P99: p99}, GoroutinesBefore: 1, GoroutinesAfter: 1}
}

func TestCompare(t *testing.T) {
	base := rep(0.01, 0.2, 0.3)
	for name, tc := range map[string]struct {
		cur Report
		ok  bool
	}{
		"identical":      {rep(0.01, 0.2, 0.3), true},
		"within noise":   {rep(0.5, 1.5, 4), true},
		"improvement":    {rep(0, 0.1, 0.1), true},
		"p95 +5 ms":      {rep(0.01, 5.2, 0.3), false},
		"p50 +100 ms":    {rep(100, 100, 100), false},
		"other machine":  {func() Report { r := rep(0.01, 0.2, 0.3); r.Machine = "x"; return r }(), false},
		"other shape":    {func() Report { r := rep(0.01, 0.2, 0.3); r.Turns = 5; return r }(), false},
		"no samples":     {func() Report { r := rep(0, 0, 0); r.Overhead.N = 0; return r }(), false},
		"goroutine leak": {func() Report { r := rep(0.01, 0.2, 0.3); r.GoroutinesAfter = 9; return r }(), false},
	} {
		if _, _, ok := compare(base, tc.cur); ok != tc.ok {
			t.Errorf("%s: ok=%v want %v", name, ok, tc.ok)
		}
	}
}
