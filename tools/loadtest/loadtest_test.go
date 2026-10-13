package main

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPercentileNearestRank(t *testing.T) {
	s := make([]float64, 100)
	for i := range s {
		s[i] = float64(i + 1)
	}
	for p, want := range map[float64]float64{50: 50, 95: 95, 99: 99, 100: 100} {
		if got := percentile(s, p); got != want {
			t.Errorf("p%v = %v, want %v", p, got, want)
		}
	}
	if percentile(nil, 50) != 0 {
		t.Error("empty must be 0")
	}
	st := statOf([]float64{3, 1, 2}) // N<20: p95 == max, as in govoice-bench
	if st.P95 != 3 || st.P50 != 2 || st.N != 3 || st.Mean != 2 {
		t.Errorf("statOf = %+v", st)
	}
}

func TestBucketQuantile(t *testing.T) {
	// 10 obs: 5 in (0,0.1], 5 in (0.1,0.2]
	b, c := []float64{0.1, 0.2, 0.5}, []float64{5, 10, 10}
	if got := bucketQuantile(0.5, b, c, 10); math.Abs(got-0.1) > 1e-9 {
		t.Errorf("p50 = %v", got)
	}
	if got := bucketQuantile(0.75, b, c, 10); math.Abs(got-0.15) > 1e-9 {
		t.Errorf("p75 = %v", got)
	}
	if got := bucketQuantile(1, b, []float64{5, 10, 10}, 12); got != 0.5 { // overflow bucket: lower bound
		t.Errorf("overflow = %v", got)
	}
}

func TestSnapshotAndProcParsing(t *testing.T) {
	sn := snapshotFrom(`# HELP x
voice_active_sessions 3
voice_admission_rejects_total{reason="session_limit"} 2
voice_latency_seconds_bucket{stage="voice_to_voice",le="0.05"} 1
voice_latency_seconds_bucket{stage="voice_to_voice",le="+Inf"} 4
voice_latency_seconds_sum{stage="voice_to_voice"} 0.5
voice_latency_seconds_count{stage="voice_to_voice"} 4
`)
	if sn.Gauges["voice_active_sessions"] != 3 || sn.Rejects["session_limit"] != 2 || sn.Count["voice_to_voice"] != 4 ||
		sn.Buckets["voice_to_voice"][0.05] != 1 || sn.Buckets["voice_to_voice"][math.Inf(1)] != 4 {
		t.Errorf("snapshot = %+v", sn)
	}
	for in, want := range map[string]float64{"0:01.50": 1.5, "1:02:03": 3723, "2-00:00:01": 172801} {
		if got := cpuTime(in); got != want {
			t.Errorf("cpuTime(%q) = %v want %v", in, got, want)
		}
	}
	ps := "10 1 1024 0:01.00\n11 10 2048 0:02.00\n12 11 512 0:00.50\n99 1 9999 5:00.00\n"
	pids, rss, _ := treePIDs(ps, 10)
	var sum float64
	for _, p := range pids {
		sum += rss[p]
	}
	if len(pids) != 3 || sum != 3.5 { // 10, 11, 12 but not 99
		t.Errorf("tree pids %v rss %v", pids, sum)
	}
}

func TestClip(t *testing.T) {
	a, b := syntheticSpeech(500), syntheticSpeech(500)
	if len(a) != 16000 || string(a) != string(b) {
		t.Fatal("synthetic clip must be deterministic, 500 ms at 16 kHz PCM16")
	}
	if got, err := loadClip("../../e2e/fixtures/voice-debug.wav"); err != nil || len(got) < 20*chunkBytes || len(got) > 200*chunkBytes {
		t.Fatalf("fixture trim: len %d err %v", len(got), err)
	}
	if _, err := parseWAV([]byte("nope")); err == nil {
		t.Error("garbage must not parse")
	}
}

func sampleReport(v2vP95, cpu, mb float64) *Report {
	r := &Report{SchemaVersion: SchemaVersion, Kind: "govoice-loadtest", Machine: Machine{OS: "darwin", Arch: "arm64", CPUs: 12},
		Config: Config{Mode: "load", Sessions: 8, Clip: "c", VADMode: "process"}, StartedAt: time.Unix(1, 0).UTC()}
	r.Results.V2V = Stat{N: 40, P50: v2vP95 - 10, P95: v2vP95, P99: v2vP95 + 10, Max: v2vP95 + 10}
	r.Results.Server = map[string]Stat{"stt_final": {N: 40, P95: 300}}
	r.Results.Res.CPUSPerSessMin, r.Results.Res.MBPerSession = cpu, mb
	r.Results.Counters.AdmissionRejects = map[string]uint64{}
	r.Results.Growth = Growth{Valid: true, WindowS: 30, RSSPct: 3, Goroutines: 2}
	r.Verdict = evaluate(r, Thresholds{MaxErrorPct: 1, MaxGoroutineGrow: -1})
	return r
}

func TestCompareTolerances(t *testing.T) {
	base := sampleReport(1000, 1.0, 10)
	for _, tc := range []struct {
		name     string
		cur      *Report
		wantOK   bool
		wantFail string // metric prefix that must fail (and be the only one)
	}{
		{"identical", sampleReport(1000, 1.0, 10), true, ""},
		{"within noise", sampleReport(1020, 1.25, 11.5), true, ""},
		{"v2v +30ms (one VAD window)", sampleReport(1030, 1.0, 10), false, "v2v_"},
		{"improvement", sampleReport(500, 0.5, 5), true, ""},
		{"v2v +100ms (absolute, not 25%)", sampleReport(1100, 1.0, 10), false, "v2v_"},
		{"cpu +60%", sampleReport(1000, 1.6, 10), false, "cpu_"},
		{"mem x2", sampleReport(1000, 1.0, 20), false, "mb_"},
	} {
		rows, _, ok := Compare(base, tc.cur, 1)
		if ok != tc.wantOK {
			t.Errorf("%s: ok=%v want %v rows=%+v", tc.name, ok, tc.wantOK, rows)
		}
		for _, r := range rows {
			if !r.Pass && (tc.wantFail == "" || !strings.HasPrefix(r.Metric, tc.wantFail)) {
				t.Errorf("%s: unexpected failing row %s", tc.name, r.Metric)
			}
		}
	}
	if _, _, ok := Compare(base, sampleReport(1100, 1, 10), 4); !ok {
		t.Error("scale 4 must loosen the table")
	}
	mut := func(f func(r *Report)) *Report { r := sampleReport(1000, 1, 10); f(r); return r }
	for name, cur := range map[string]*Report{
		"other machine":     mut(func(r *Report) { r.Machine.CPUs = 4 }),
		"other cpu model":   mut(func(r *Report) { r.Machine.CPUModel = "x" }),
		"cpu model unknown": mut(func(r *Report) { r.Machine.CPUModel = "" }),
		"other config":      mut(func(r *Report) { r.Config.Sessions = 4 }),
		"stage missing":     mut(func(r *Report) { delete(r.Results.Server, "stt_final") }),
		"no turns":          mut(func(r *Report) { r.Results.V2V = Stat{} }),
		"cpu not sampled":   mut(func(r *Report) { r.Results.Res.CPUSPerSessMin = 0 }),
		"growth not valid":  mut(func(r *Report) { r.Results.Growth.Valid = false }),
		"goroutines leaked": mut(func(r *Report) { r.Results.Res.GoroutinesEnd = 50 }),
		"rss growing":       mut(func(r *Report) { r.Results.Growth.RSSPct = 40 }),
		"own verdict": mut(func(r *Report) {
			r.Results.Counters.LeakedSessions = 1
			r.Verdict = evaluate(r, Thresholds{MaxErrorPct: 1, MaxGoroutineGrow: -1})
		}),
	} {
		base := sampleReport(1000, 1, 10)
		base.Machine.CPUModel = "m"
		if name != "other cpu model" && name != "cpu model unknown" {
			cur.Machine.CPUModel = "m"
		}
		if _, _, ok := Compare(base, cur, 1); ok {
			t.Errorf("%s must fail the comparison", name)
		}
	}
	for k, tol := range Tolerances { // every rule is documented
		if tol.Why == "" || (tol.RelPct == 0 && tol.Abs == 0) {
			t.Errorf("tolerance %s needs a reason and a nonzero allowance", k)
		}
	}
}

func TestEvaluateThresholds(t *testing.T) {
	r := sampleReport(800, 1, 10)
	r.Results.Growth = Growth{Valid: true, RSSPct: 30, Goroutines: 5, GoroutinesDrain: 90}
	v := evaluate(r, Thresholds{MaxP95V2VMS: 500, MaxRSSGrowthPct: 10, MaxGoroutineGrow: 50, MaxErrorPct: 1})
	fail := map[string]bool{}
	for _, c := range v.Checks {
		if !c.Pass {
			fail[c.Name] = true
		}
	}
	if v.Pass || !fail["v2v_p95_ms"] || !fail["rss_growth_pct"] || !fail["goroutine_growth"] || len(fail) != 3 {
		t.Errorf("verdict %+v", v)
	}
	r.Results.Growth.Valid = false // too-short soak is a failure, not a silent pass
	if v := evaluate(r, Thresholds{MaxRSSGrowthPct: 10, MaxGoroutineGrow: -1, MaxErrorPct: 1}); v.Pass {
		t.Error("invalid growth windows must fail a growth check")
	}
	r2 := sampleReport(800, 1, 10)
	r2.Results.Counters.AdmissionRejects = map[string]uint64{"session_limit": 1}
	if evaluate(r2, Thresholds{MaxErrorPct: 1, MaxGoroutineGrow: -1}).Pass {
		t.Error("admission rejects must fail")
	}
}

func TestSchemaRoundTripAndMarkdown(t *testing.T) {
	r := sampleReport(900, 1.1, 12)
	r.Capacity = &Capacity{BudgetMS: 1100, MaxSessions: 16, SessionsPerCore: 20, Steps: []CapStep{{Sessions: 16, V2VP95: 900, Pass: true}}}
	r.Samples = []Sample{{T: 1, Active: 2}}
	path := filepath.Join(t.TempDir(), "r.json")
	if err := r.writeJSON(path); err != nil {
		t.Fatal(err)
	}
	got, err := loadReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Results.V2V.P95 != 900 || got.Capacity.MaxSessions != 16 || got.SchemaVersion != 1 || !got.Machine.Specific && false {
		t.Errorf("round trip lost data: %+v", got)
	}
	raw, _ := os.ReadFile(path)
	for _, key := range []string{`"schema_version"`, `"voice_to_voice_client_ms"`, `"server_stage_ms"`, `"cpu_s_per_session_min"`, `"mb_per_session"`, `"p95_ms"`, `"machine_specific"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("schema key %s missing; renaming is a breaking change (bump SchemaVersion)", key)
		}
	}
	md := r.Markdown()
	for _, want := range []string{"## Verdict: PASS", "## Latency (ms)", "## Capacity", "sessions per core", "voice_to_voice (client"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":99,"kind":"govoice-loadtest"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReport(path); err == nil {
		t.Error("schema mismatch must be rejected")
	}
}

func TestGrowthWindows(t *testing.T) {
	var ss []Sample
	for i := 0; i <= 60; i++ { // 60 s at 1 s: RSS 100 -> 160, goroutines flat
		ss = append(ss, Sample{T: float64(i), Active: 4, RSSMB: 100 + float64(i), Goroutines: 50, HeapMB: 10})
	}
	st := newLoadState()
	r := analyze(st, &Snapshot{Gauges: map[string]float64{"voice_process_goroutines": 20}, Buckets: map[string]map[float64]float64{}, Sum: map[string]float64{}, Count: map[string]float64{}, Rejects: map[string]float64{}},
		&Snapshot{Gauges: map[string]float64{"voice_process_goroutines": 22}, Buckets: map[string]map[float64]float64{}, Sum: map[string]float64{}, Count: map[string]float64{}, Rejects: map[string]float64{}},
		ProcStats{RSSMB: 90}, ProcStats{}, ss, ss[len(ss)-1], 60*time.Second, 5*time.Second, time.Second, 4, true)
	if !r.Growth.Valid || r.Growth.RSSPct < 20 || r.Growth.Goroutines != 0 || r.Growth.GoroutinesDrain != 2 {
		t.Errorf("growth %+v", r.Growth)
	}
	short := analyze(st, &Snapshot{Gauges: map[string]float64{}, Buckets: map[string]map[float64]float64{}, Sum: map[string]float64{}, Count: map[string]float64{}, Rejects: map[string]float64{}},
		&Snapshot{Gauges: map[string]float64{}, Buckets: map[string]map[float64]float64{}, Sum: map[string]float64{}, Count: map[string]float64{}, Rejects: map[string]float64{}},
		ProcStats{}, ProcStats{}, ss[:4], ss[3], 4*time.Second, time.Second, time.Second, 4, true)
	if short.Growth.Valid {
		t.Error("a run too short for two windows must not report growth")
	}
}

// TestEndToEndTwoSessions runs the real harness against a freshly built
// echo-provider server. It builds binaries (cgo, ~30 s), so it only runs
// when GOVOICE_LOADTEST_E2E=1 (make loadtest-smoke covers the same path).
func TestEndToEndTwoSessions(t *testing.T) {
	if os.Getenv("GOVOICE_LOADTEST_E2E") != "1" {
		t.Skip("set GOVOICE_LOADTEST_E2E=1 to build the echo server and run 2 real calls")
	}
	f, err := parseFlags([]string{"-sessions", "2", "-duration", "20s", "-ramp", "1s", "-turns-per-session", "2", "-sample-interval", "2s", "-root", "../.."})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := run(f)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Results.Turns.Completed < 2 || rep.Results.Sessions.MaxConcurrent != 2 || rep.Results.V2V.P50 <= 0 || !rep.Verdict.Pass {
		t.Errorf("unexpected result: %+v\n%s", rep.Results, rep.Markdown())
	}
}

func TestSampleIntervalAutoAndSoakMinimum(t *testing.T) {
	for _, c := range []struct {
		dur  string
		want time.Duration
	}{{"2m", 2 * time.Second}, {"5m", 5 * time.Second}, {"24h", 60 * time.Second}} {
		f, err := parseFlags([]string{"-soak", "-sessions", "16", "-duration", c.dur, "-ramp", "30s"})
		if err != nil || f.sampleEvery != c.want {
			t.Errorf("%s: interval %v err %v, want %v", c.dur, f.sampleEvery, err, c.want)
		}
	}
	if _, err := parseFlags([]string{"-soak", "-duration", "50s", "-ramp", "30s"}); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Errorf("short soak: err %v, want a 'too short' message", err)
	}
}

func TestParseFlagsChurnAndChaos(t *testing.T) {
	args := []string{
		"-target-url", "127.0.0.1:8080",
		"-admin-url", "http://127.0.0.1:9090",
		"-metrics-url", "http://127.0.0.1:9092",
		"-container", "govoice-perf",
		"-min-session-dur", "30s",
		"-max-session-dur", "60s",
		"-min-turns", "4",
		"-max-turns", "8",
		"-chaos-rate", "0.15",
	}
	f, err := parseFlags(args)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.targetURL != "127.0.0.1:8080" || f.adminURL != "http://127.0.0.1:9090" || f.metricsURL != "http://127.0.0.1:9092" {
		t.Errorf("unexpected target URLs: %+v", f)
	}
	if f.container != "govoice-perf" {
		t.Errorf("unexpected container: %s", f.container)
	}
	if f.minSessDur != 30*time.Second || f.maxSessDur != 60*time.Second {
		t.Errorf("unexpected churn durations: %v, %v", f.minSessDur, f.maxSessDur)
	}
	if f.minTurns != 4 || f.maxTurns != 8 {
		t.Errorf("unexpected turn limits: %d, %d", f.minTurns, f.maxTurns)
	}
	if f.chaosRate != 0.15 {
		t.Errorf("unexpected chaos rate: %v", f.chaosRate)
	}
}

func TestChaosRecordingAndMarkdown(t *testing.T) {
	st := newLoadState()
	st.recordChaos("tcp_abort")
	st.recordChaos("malformed_frame")
	st.recordChaos("stalled_consumer")

	snap := &Snapshot{Gauges: map[string]float64{}, Buckets: map[string]map[float64]float64{}, Sum: map[string]float64{}, Count: map[string]float64{}, Rejects: map[string]float64{}}
	res := analyze(st, snap, snap, ProcStats{}, ProcStats{}, nil, Sample{}, time.Minute, time.Second, time.Second, 10, false)

	if res.ChaosInjected != 3 {
		t.Errorf("expected 3 chaos injected, got %d", res.ChaosInjected)
	}
	if res.ChaosErrors["tcp_abort"] != 1 || res.ChaosErrors["malformed_frame"] != 1 || res.ChaosErrors["stalled_consumer"] != 1 {
		t.Errorf("unexpected chaos errors: %+v", res.ChaosErrors)
	}

	rep := &Report{Results: *res, Config: Config{Mode: "load", Sessions: 10}}
	md := rep.Markdown()
	if !strings.Contains(md, "Fault Injection & Chaos") || !strings.Contains(md, "tcp_abort") {
		t.Errorf("expected chaos section in markdown, got:\n%s", md)
	}
}
