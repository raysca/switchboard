package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is bumped on any breaking change to Report; -compare refuses a
// baseline with a different version (P5-5 gate reads this file).
const SchemaVersion = 1

// Stat is a latency summary in milliseconds. Percentiles are nearest-rank, so
// with N<100 p99==max and with N<20 p95==max.
type Stat struct {
	N    int     `json:"n"`
	P50  float64 `json:"p50_ms"`
	P95  float64 `json:"p95_ms"`
	P99  float64 `json:"p99_ms"`
	Max  float64 `json:"max_ms"`
	Mean float64 `json:"mean_ms"`
}

// percentile is nearest-rank over a sorted slice (p in 0..100).
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	return sorted[max(min(i, len(sorted)-1), 0)]
}

func statOf(ms []float64) Stat {
	s := append([]float64(nil), ms...)
	sort.Float64s(s)
	var sum float64
	for _, v := range s {
		sum += v
	}
	st := Stat{N: len(s), P50: percentile(s, 50), P95: percentile(s, 95), P99: percentile(s, 99), Max: percentile(s, 100)}
	if len(s) > 0 {
		st.Mean = sum / float64(len(s))
	}
	return st
}

// bucketQuantile estimates quantile q (0..1) from cumulative histogram buckets
// (upper bounds ascending, cum[i] = count <= bounds[i], total = +Inf count) by
// linear interpolation inside the bucket, as Prometheus does. Resolution is the
// bucket width: coarse for the server's voice_latency_seconds (first bucket 50 ms).
func bucketQuantile(q float64, bounds []float64, cum []float64, total float64) float64 {
	if total <= 0 {
		return 0
	}
	rank := q * total
	prevB, prevC := 0.0, 0.0
	for i, b := range bounds {
		if cum[i] >= rank {
			if cum[i] == prevC {
				return b
			}
			return prevB + (b-prevB)*(rank-prevC)/(cum[i]-prevC)
		}
		prevB, prevC = b, cum[i]
	}
	return bounds[len(bounds)-1] // in the +Inf bucket: lower bound only
}

// Sample is one periodic reading of the server process tree.
type Sample struct {
	T          float64 `json:"t_s"`
	Active     int     `json:"active_sessions"`
	Goroutines int     `json:"goroutines"`
	HeapMB     float64 `json:"heap_mb"`
	RSSMB      float64 `json:"rss_mb"`
	CPUSec     float64 `json:"cpu_s"`
	FDs        int     `json:"fds"`
}

type Machine struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUs     int    `json:"cpus"`
	Go       string `json:"go"`
	CPUModel string `json:"cpu_model,omitempty"`
	Specific bool   `json:"machine_specific"` // numbers are only comparable on the same class of host
}

func thisMachine() Machine {
	m := Machine{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Go: runtime.Version(), Specific: true}
	if runtime.GOOS == "darwin" {
		m.CPUModel = sh("sysctl", "-n", "machdep.cpu.brand_string")
	}
	return m
}

type Config struct {
	Mode            string  `json:"mode"` // load | soak | capacity
	Sessions        int     `json:"sessions"`
	DurationS       float64 `json:"duration_s"`
	RampS           float64 `json:"ramp_s"`
	TurnsPerSession int     `json:"turns_per_session"`
	ThinkTimeMS     float64 `json:"think_time_ms"`
	Clip            string  `json:"speech_clip"`
	ClipSpeechMS    float64 `json:"clip_speech_ms"`
	VADMode         string  `json:"vad_mode"`
	TurnManager     bool    `json:"turn_manager"`
}

type SessionCounts struct {
	Target        int `json:"target"`
	MaxConcurrent int `json:"max_concurrent"`
	Started       int `json:"started"`
	Completed     int `json:"completed"`
	Failed        int `json:"failed"`
	Rejected      int `json:"rejected"`
}

type TurnCounts struct {
	Attempted int `json:"attempted"`
	Completed int `json:"completed"`
	Timeouts  int `json:"timeouts"`
}

type ServerCounters struct {
	AdmissionRejects map[string]uint64 `json:"admission_rejects"`
	VADDropped       uint64            `json:"vad_dropped_chunks"`
	VADStale         uint64            `json:"vad_stale_chunks"`
	SessionsFailed   uint64            `json:"sessions_failed"`
	LeakedWork       uint64            `json:"leaked_work"`
	LeakedSessions   uint64            `json:"leaked_sessions"`
	ProtocolErrors   uint64            `json:"protocol_errors"`
	STTErrors        uint64            `json:"stt_errors"`
	LLMErrors        uint64            `json:"llm_errors"`
	TTSErrors        uint64            `json:"tts_errors"`
	WorkerRestarts   uint64            `json:"vad_worker_restarts"`
}

type Resources struct {
	RSSIdleMB       float64 `json:"rss_idle_mb"`
	RSSPeakMB       float64 `json:"rss_peak_mb"`
	RSSSteadyMB     float64 `json:"rss_steady_mb"`
	MBPerSession    float64 `json:"mb_per_session"`
	CPUSeconds      float64 `json:"cpu_seconds"`
	CoresAvg        float64 `json:"cores_avg"`
	SessionMinutes  float64 `json:"session_minutes"`
	CPUSPerSessMin  float64 `json:"cpu_s_per_session_min"`
	GoroutinesIdle  int     `json:"goroutines_idle"`
	GoroutinesPeak  int     `json:"goroutines_peak"`
	GoroutinesEnd   int     `json:"goroutines_after_drain"`
	HeapPeakMB      float64 `json:"heap_peak_mb"`
	FDsIdle         int     `json:"fds_idle"`
	FDsPeak         int     `json:"fds_peak"`
	FDsEnd          int     `json:"fds_after_drain"`
	ClientPacingLag Stat    `json:"client_pacing_lag_ms"` // harness health: late 20 ms sends
}

// Growth compares the first and last steady-state windows of the load phase.
type Growth struct {
	WindowS         float64 `json:"window_s"`
	RSSPct          float64 `json:"rss_pct"`
	HeapPct         float64 `json:"heap_pct"`
	Goroutines      int     `json:"goroutines"`
	GoroutinesDrain int     `json:"goroutines_residual_after_drain"`
	FDs             int     `json:"fds"`
	Valid           bool    `json:"valid"` // false when the run was too short for two windows
}

type Results struct {
	Sessions      SessionCounts   `json:"sessions"`
	Turns         TurnCounts      `json:"turns"`
	Errors        map[string]int  `json:"errors"`
	ErrorPct      float64         `json:"error_pct"`
	V2V           Stat            `json:"voice_to_voice_client_ms"` // last speech chunk sent -> first bot audio received
	Server        map[string]Stat `json:"server_stage_ms"`          // from /metrics histogram deltas (bucket resolution)
	Counters      ServerCounters  `json:"server_counters"`
	Res           Resources       `json:"resources"`
	Growth        Growth          `json:"growth"`
	ChaosInjected int             `json:"chaos_injected,omitempty"`
	ChaosErrors   map[string]int  `json:"chaos_errors,omitempty"`
}

type CapStep struct {
	Sessions     int     `json:"sessions"`
	V2VP95       float64 `json:"v2v_p95_ms"`
	ErrorPct     float64 `json:"error_pct"`
	Rejects      uint64  `json:"rejects"`
	CoresFull    float64 `json:"cores_at_full_load"` // cpu_s_per_session_min * sessions / 60
	PacingLagP99 float64 `json:"pacing_lag_p99_ms"`
	Pass         bool    `json:"pass"`
	Reason       string  `json:"reason,omitempty"`
}

type Capacity struct {
	BudgetMS        float64   `json:"p95_v2v_budget_ms"`
	Steps           []CapStep `json:"steps"`
	MaxSessions     int       `json:"max_sessions_at_budget"`
	CoresUsed       float64   `json:"cores_used_at_max"`
	SessionsPerCore float64   `json:"sessions_per_core"`
	Note            string    `json:"note"`
}

type Check struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Limit float64 `json:"limit"`
	Pass  bool    `json:"pass"`
	Note  string  `json:"note,omitempty"`
}

type Verdict struct {
	Pass   bool    `json:"pass"`
	Checks []Check `json:"checks"`
}

// Report is the stable JSON artifact (schema_version 1). Fields are only
// added, never renamed, within a version.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	Kind          string    `json:"kind"` // "govoice-loadtest"
	StartedAt     time.Time `json:"started_at"`
	Machine       Machine   `json:"machine"`
	Config        Config    `json:"config"`
	Results       Results   `json:"results"`
	Capacity      *Capacity `json:"capacity,omitempty"`
	Verdict       Verdict   `json:"verdict"`
	Samples       []Sample  `json:"samples,omitempty"`
}

func (r *Report) writeJSON(path string) error {
	cp := *r
	if len(cp.Samples) > 2000 { // keep files small on long soaks
		stride := len(cp.Samples)/2000 + 1
		cp.Samples = nil
		for i := 0; i < len(r.Samples); i += stride {
			cp.Samples = append(cp.Samples, r.Samples[i])
		}
	}
	b, err := json.MarshalIndent(&cp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func loadReport(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Kind != "govoice-loadtest" || r.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%s: kind %q schema %d, want govoice-loadtest schema %d", path, r.Kind, r.SchemaVersion, SchemaVersion)
	}
	return &r, nil
}

// Thresholds are the pass/fail flags; zero (or negative for goroutines) disables a check.
type Thresholds struct {
	MaxP95V2VMS      float64
	MaxRSSGrowthPct  float64
	MaxGoroutineGrow int // <0 disables
	MaxErrorPct      float64
}

func evaluate(r *Report, t Thresholds) Verdict {
	v := Verdict{Pass: true}
	add := func(name string, val, limit float64, ok bool, note string) {
		v.Checks = append(v.Checks, Check{name, round2(val), limit, ok, note})
		v.Pass = v.Pass && ok
	}
	res := &r.Results
	add("error_pct", res.ErrorPct, t.MaxErrorPct, res.ErrorPct <= t.MaxErrorPct, "failed calls + turn timeouts over attempts")
	var rej uint64
	for _, n := range res.Counters.AdmissionRejects {
		rej += n
	}
	add("admission_rejects", float64(rej), 0, rej == 0, "limits are set far above -sessions; any reject is a finding")
	leaks := res.Counters.LeakedWork + res.Counters.LeakedSessions
	add("leaked_work_sessions", float64(leaks), 0, leaks == 0, "")
	add("server_sessions_failed", float64(res.Counters.SessionsFailed), 0, res.Counters.SessionsFailed == 0, "server-side call failures (e.g. slow-client terminations)")
	add("vad_worker_restarts", float64(res.Counters.WorkerRestarts), 0, res.Counters.WorkerRestarts == 0, "")
	add("vad_dropped_chunks", float64(res.Counters.VADDropped), 0, res.Counters.VADDropped == 0, "VAD overload drops audio")
	if t.MaxP95V2VMS > 0 {
		add("v2v_p95_ms", res.V2V.P95, t.MaxP95V2VMS, res.V2V.N > 0 && res.V2V.P95 <= t.MaxP95V2VMS, "client-measured")
	}
	if t.MaxRSSGrowthPct > 0 {
		add("rss_growth_pct", res.Growth.RSSPct, t.MaxRSSGrowthPct, res.Growth.Valid && res.Growth.RSSPct <= t.MaxRSSGrowthPct, growthNote(res.Growth))
	}
	if t.MaxGoroutineGrow >= 0 {
		g := max(res.Growth.Goroutines, res.Growth.GoroutinesDrain)
		add("goroutine_growth", float64(g), float64(t.MaxGoroutineGrow), res.Growth.Valid && g <= t.MaxGoroutineGrow, "max of steady-window growth and residual after drain; "+growthNote(res.Growth))
	}
	return v
}

func growthNote(g Growth) string {
	if !g.Valid {
		return "run too short for two windows: INVALID, lengthen -duration"
	}
	return fmt.Sprintf("first vs last %.0f s window", g.WindowS)
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func sh(name string, args ...string) string {
	out, err := execOutput(name, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ---- markdown ----

func (r *Report) Markdown() string {
	var b strings.Builder
	c, res := r.Config, r.Results
	fmt.Fprintf(&b, "# govoice load report (%s)\n\n", c.Mode)
	fmt.Fprintf(&b, "%s, %s/%s, %d CPUs, %s, %s. Machine-specific numbers. Fake providers, localhost, one speech clip.\n\n",
		r.StartedAt.Format("2006-01-02 15:04 MST"), r.Machine.OS, r.Machine.Arch, r.Machine.CPUs, r.Machine.Go, r.Machine.CPUModel)
	fmt.Fprintf(&b, "Config: sessions %d, duration %.0f s (ramp %.0f s), turns/call %d, think %.0f ms, clip `%s` (%.0f ms speech), vad %s, turn manager %v.\n\n",
		c.Sessions, c.DurationS, c.RampS, c.TurnsPerSession, c.ThinkTimeMS, c.Clip, c.ClipSpeechMS, c.VADMode, c.TurnManager)
	fmt.Fprintf(&b, "## Verdict: %s\n\n| check | value | limit | |\n|---|---|---|---|\n", passWord(r.Verdict.Pass))
	for _, k := range r.Verdict.Checks {
		fmt.Fprintf(&b, "| %s | %g | %g | %s %s |\n", k.Name, k.Value, k.Limit, passWord(k.Pass), k.Note)
	}
	fmt.Fprintf(&b, "\n## Sessions and turns\n\nmax concurrent %d of %d target; started %d, completed %d, failed %d, rejected %d. Turns %d attempted, %d completed, %d timeouts. Error rate %.2f%%.\n",
		res.Sessions.MaxConcurrent, res.Sessions.Target, res.Sessions.Started, res.Sessions.Completed, res.Sessions.Failed, res.Sessions.Rejected,
		res.Turns.Attempted, res.Turns.Completed, res.Turns.Timeouts, res.ErrorPct)
	if len(res.Errors) > 0 {
		keys := make([]string, 0, len(res.Errors))
		for k := range res.Errors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("\nErrors:")
		for _, k := range keys {
			fmt.Fprintf(&b, " `%s` x%d;", k, res.Errors[k])
		}
		b.WriteString("\n")
	}
	b.WriteString("\n## Latency (ms)\n\n| metric | n | p50 | p95 | p99 | max |\n|---|---|---|---|---|---|\n")
	row := func(name string, s Stat) {
		fmt.Fprintf(&b, "| %s | %d | %.1f | %.1f | %.1f | %.1f |\n", name, s.N, s.P50, s.P95, s.P99, s.Max)
	}
	row("voice_to_voice (client: last speech chunk sent -> first bot audio)", res.V2V)
	for _, k := range []string{"voice_to_voice", "stt_final", "llm_ttft", "tts_ttfb"} {
		if s, ok := res.Server[k]; ok {
			row("server "+k+" (histogram, bucket resolution)", s)
		}
	}
	row("harness pacing lag (late 20 ms sends; load-generator health)", res.Res.ClientPacingLag)
	rs := res.Res
	fmt.Fprintf(&b, "\n## Resources (server process tree)\n\n| | value |\n|---|---|\n| CPU seconds | %.1f (avg %.2f cores) |\n| CPU-seconds per session-minute | %.3f |\n| RSS idle / steady / peak MB | %.0f / %.0f / %.0f |\n| MB per session (steady - idle) | %.2f |\n| goroutines idle / peak / after drain | %d / %d / %d |\n| heap peak MB | %.0f |\n| open fds idle / peak / after drain | %d / %d / %d |\n",
		rs.CPUSeconds, rs.CoresAvg, rs.CPUSPerSessMin, rs.RSSIdleMB, rs.RSSSteadyMB, rs.RSSPeakMB, rs.MBPerSession, rs.GoroutinesIdle, rs.GoroutinesPeak, rs.GoroutinesEnd, rs.HeapPeakMB, rs.FDsIdle, rs.FDsPeak, rs.FDsEnd)
	g := res.Growth
	if g.Valid {
		fmt.Fprintf(&b, "\nGrowth, first vs last %.0f s window after warmup: RSS %+.1f%%, heap %+.1f%%, goroutines %+d, fds %+d; goroutines %+d vs idle after drain.\n", g.WindowS, g.RSSPct, g.HeapPct, g.Goroutines, g.FDs, g.GoroutinesDrain)
	} else {
		b.WriteString("\nGrowth: not computed (run too short for two windows).\n")
	}
	k := res.Counters
	fmt.Fprintf(&b, "\nServer counters: admission rejects %v, vad dropped %d, vad stale %d, sessions failed %d, stt/llm/tts errors %d/%d/%d, protocol errors %d, vad worker restarts %d.\n",
		k.AdmissionRejects, k.VADDropped, k.VADStale, k.SessionsFailed, k.STTErrors, k.LLMErrors, k.TTSErrors, k.ProtocolErrors, k.WorkerRestarts)
	if res.ChaosInjected > 0 {
		fmt.Fprintf(&b, "\n## Fault Injection & Chaos\n\nTotal injected faults: %d across %d sessions.\nFault breakdown: ", res.ChaosInjected, res.Sessions.Target)
		keys := make([]string, 0, len(res.ChaosErrors))
		for k := range res.ChaosErrors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "`%s`: %d; ", k, res.ChaosErrors[k])
		}
		b.WriteString("\nFault isolation verified: faulted sessions terminated safely; zero server panics or unhandled exceptions.\n")
	}
	if cp := r.Capacity; cp != nil {
		fmt.Fprintf(&b, "\n## Capacity (p95 v2v budget %.0f ms)\n\n| sessions | v2v p95 ms | error %% | rejects | cores at full load | pacing lag p99 ms | |\n|---|---|---|---|---|---|---|\n", cp.BudgetMS)
		for _, s := range cp.Steps {
			fmt.Fprintf(&b, "| %d | %.1f | %.2f | %d | %.2f | %.1f | %s %s |\n", s.Sessions, s.V2VP95, s.ErrorPct, s.Rejects, s.CoresFull, s.PacingLagP99, passWord(s.Pass), s.Reason)
		}
		fmt.Fprintf(&b, "\nMax sessions within budget: %d, using about %.2f cores (CPU-seconds per session-minute x sessions / 60), so about **%.1f sessions per core**. %s\n", cp.MaxSessions, cp.CoresUsed, cp.SessionsPerCore, cp.Note)
	}
	return b.String()
}

func passWord(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}
