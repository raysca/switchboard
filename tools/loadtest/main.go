// loadtest drives N concurrent real calls over the actual WebSocket/RTVI
// protocol (binary protobuf frames, single-use voice tokens) against a locally
// started server built with -tags fakeproviders, so transport, resampling,
// Silero VAD (vadworker), the STT/turn/LLM/TTS stages (fakes), the pacer and
// metrics all run, with no paid calls. Fake providers isolate framework cost
// only; everything is localhost with one speech clip. See docs/benchmarks/capacity.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type flags struct {
	sessions, turns                  int
	duration, ramp, think, turnLimit time.Duration
	clip, vadMode, root, serverBin   string
	turnManager, keepWork            bool
	soak, findCap                    bool
	sampleEvery, stepDur             time.Duration
	capSteps                         string
	capBudgetMS                      float64
	jsonOut, mdOut, compare, current string
	tolScale                         float64
	th                               Thresholds

	// Target / Container server support
	targetURL, adminURL, metricsURL string
	apiKey, container               string

	// Churn and Chaos flags
	minSessDur, maxSessDur time.Duration
	minTurns, maxTurns     int
	chaosRate              float64
}

func parseFlags(args []string) (*flags, error) {
	f := &flags{}
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.IntVar(&f.sessions, "sessions", 8, "concurrent calls")
	fs.DurationVar(&f.duration, "duration", 30*time.Second, "load phase length (includes -ramp); soak: e.g. 1h")
	fs.DurationVar(&f.ramp, "ramp", 5*time.Second, "time over which calls are started")
	fs.IntVar(&f.turns, "turns-per-session", 5, "speech turns per call; the slot then starts a new call (0 = one call for the whole run)")
	fs.DurationVar(&f.think, "think-time", 1*time.Second, "silence between a bot reply ending and the next user turn")
	fs.DurationVar(&f.turnLimit, "turn-timeout", 10*time.Second, "max wait for first bot audio after end of speech")
	fs.StringVar(&f.clip, "speech-clip", "e2e/fixtures/voice-debug.wav", `16 kHz mono PCM16 WAV (silence trimmed), or "synthetic"`)
	fs.StringVar(&f.vadMode, "vad-mode", "process", "server VAD mode: process (vadworker child) | inprocess")
	fs.BoolVar(&f.turnManager, "turn-manager", false, "enable the server turn manager (turn.manager: true)")
	fs.StringVar(&f.root, "root", ".", "repo root")
	fs.StringVar(&f.serverBin, "server-bin", "", "prebuilt -tags fakeproviders server (vadworker next to it); default builds one")
	fs.BoolVar(&f.keepWork, "keep-work", false, "keep the temp dir (server log, config) and print its path")
	fs.BoolVar(&f.soak, "soak", false, "label the run a soak and require valid growth windows")
	fs.DurationVar(&f.sampleEvery, "sample-interval", 0, "server sampling interval (0 = duration/60 clamped to 2s..60s)")
	fs.BoolVar(&f.findCap, "find-capacity", false, "step concurrency until p95 v2v degrades beyond the budget")
	fs.StringVar(&f.capSteps, "cap-steps", "4,8,16,32,64,96", "session counts to try (at most 8 steps)")
	fs.DurationVar(&f.stepDur, "step-duration", 30*time.Second, "length of each capacity step")
	fs.Float64Var(&f.capBudgetMS, "cap-budget-ms", 0, "p95 v2v budget in ms (0 = first step's p95 + 150 ms)")
	fs.StringVar(&f.jsonOut, "json-out", "", "write the JSON report here")
	fs.StringVar(&f.mdOut, "md-out", "", "write the markdown report here (default: stdout)")
	fs.StringVar(&f.compare, "compare", "", "baseline JSON to compare against (exit 1 on regression)")
	fs.StringVar(&f.current, "current", "", "with -compare: compare this existing report instead of running")
	fs.Float64Var(&f.tolScale, "tolerance-scale", 1, "multiplier on the per-metric tolerance table in compare.go (>1 loosens, for noisy hosts)")
	fs.Float64Var(&f.th.MaxP95V2VMS, "max-p95-v2v", 0, "fail when client p95 voice-to-voice exceeds this many ms (0 = off)")
	fs.Float64Var(&f.th.MaxRSSGrowthPct, "max-rss-growth-pct", 0, "fail when RSS grows more than this between first and last window (0 = off)")
	fs.IntVar(&f.th.MaxGoroutineGrow, "max-goroutine-growth", -1, "fail when goroutines grow more than this (steady windows, or residual after drain; -1 = off)")
	fs.Float64Var(&f.th.MaxErrorPct, "max-error-pct", 1, "fail when failed calls + turn timeouts exceed this percent")

	fs.StringVar(&f.targetURL, "target-url", "", "connect to an existing server (e.g. 127.0.0.1:8080) instead of starting a local child")
	fs.StringVar(&f.adminURL, "admin-url", "", "management API URL for -target-url (e.g. http://127.0.0.1:9090)")
	fs.StringVar(&f.metricsURL, "metrics-url", "", "metrics/probe URL for -target-url (e.g. http://127.0.0.1:9092, default: admin-url)")
	fs.StringVar(&f.apiKey, "api-key", "perf-test-mgmt-key-32-bytes-long!", "management API key for -target-url")
	fs.StringVar(&f.container, "container", "", "docker container name (e.g. govoice-perf) to collect container metrics")
	fs.DurationVar(&f.minSessDur, "min-session-dur", 0, "minimum session duration for churn simulation (e.g. 30s)")
	fs.DurationVar(&f.maxSessDur, "max-session-dur", 0, "maximum session duration for churn simulation (e.g. 60s)")
	fs.IntVar(&f.minTurns, "min-turns", 0, "minimum turns per session for churn simulation")
	fs.IntVar(&f.maxTurns, "max-turns", 0, "maximum turns per session for churn simulation")
	fs.Float64Var(&f.chaosRate, "chaos-rate", 0, "probability of injected client faults per call (0.0 to 1.0, e.g. 0.15)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if f.sessions < 1 || f.duration <= 0 || f.ramp < 0 || f.ramp >= f.duration {
		return nil, fmt.Errorf("need sessions >= 1 and 0 <= ramp < duration")
	}
	if f.sampleEvery <= 0 {
		f.sampleEvery = min(max(f.duration/60, 2*time.Second), 60*time.Second)
	}
	if f.soak && !f.findCap {
		// analyze(): growth window = quarter of the post-warmup region, needs >= 3 sample
		// intervals; +1 interval because the first post-warmup sample can land up to one late.
		ramp := max(f.ramp, time.Duration(f.sessions)*minStartGap)
		if need := max(ramp, f.duration/10) + 13*f.sampleEvery; f.duration < need {
			return nil, fmt.Errorf("soak -duration %s is too short for a valid growth reading with -sample-interval %s and -ramp %s: need at least %s (use -duration >= %s, or a smaller -sample-interval/-ramp)",
				f.duration, f.sampleEvery, ramp, need.Round(time.Second), need.Round(time.Second))
		}
	}
	return f, nil
}

func main() {
	f, err := parseFlags(os.Args[1:])
	if err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(2)
	}
	code, err := realMain(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

// realMain returns 0 pass, 1 verdict/compare failure; err for harness failures.
func realMain(f *flags) (int, error) {
	if f.compare != "" && f.current != "" {
		cur, err := loadReport(f.current)
		if err != nil {
			return 0, err
		}
		return doCompare(f, cur)
	}
	rep, err := run(f)
	if err != nil {
		return 0, err
	}
	md := rep.Markdown()
	if f.jsonOut != "" {
		if err := rep.writeJSON(f.jsonOut); err != nil {
			return 0, err
		}
	}
	code := 0
	if !rep.Verdict.Pass {
		code = 1
	}
	if f.compare != "" {
		c, err := doCompareTo(f, rep, &md)
		if err != nil {
			return 0, err
		}
		code = max(code, c)
	}
	if f.mdOut != "" {
		if err := os.WriteFile(f.mdOut, []byte(md), 0o644); err != nil {
			return 0, err
		}
	} else {
		fmt.Print(md)
	}
	return code, nil
}

func doCompare(f *flags, cur *Report) (int, error) {
	var out string
	c, err := doCompareTo(f, cur, &out)
	fmt.Print(out)
	return c, err
}

func doCompareTo(f *flags, cur *Report, md *string) (int, error) {
	base, err := loadReport(f.compare)
	if err != nil {
		return 0, err
	}
	rows, warns, ok := Compare(base, cur, f.tolScale)
	*md += "\n" + renderCompare(rows, warns, f.tolScale, ok)
	if !ok {
		return 1, nil
	}
	return 0, nil
}

// run starts the server, drives load (or capacity steps) and assembles the report.
func run(f *flags) (*Report, error) {
	root, err := filepath.Abs(f.root)
	if err != nil {
		return nil, err
	}
	clipPath := f.clip
	if clipPath != "synthetic" && !filepath.IsAbs(clipPath) {
		clipPath = filepath.Join(root, clipPath)
	}
	clip, err := loadClip(clipPath)
	if err != nil {
		return nil, err
	}
	// Signals cancel ctx, which unwinds run() through the deferred cleanup
	// below (and the server process-group kill); installed before the temp
	// dir exists so no window leaks it. SIGKILL is covered by the next sweep.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sweepStaleWork(os.TempDir(), time.Hour)
	work, err := os.MkdirTemp("", workPrefix)
	if err != nil {
		return nil, err
	}
	defer func() {
		if f.keepWork {
			fmt.Fprintln(os.Stderr, "loadtest: work dir kept:", work)
		} else {
			os.RemoveAll(work)
		}
	}()
	// Without the pid file a concurrent harness's sweep would delete this dir after an hour.
	if err := os.WriteFile(filepath.Join(work, pidFile), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return nil, err
	}

	maxSess := f.sessions
	steps, err := parseSteps(f.capSteps)
	if f.findCap {
		if err != nil {
			return nil, err
		}
		for _, s := range steps {
			maxSess = max(maxSess, s)
		}
	}
	var srv *Server
	if f.targetURL != "" {
		fmt.Fprintf(os.Stderr, "loadtest: connecting to target server at %s...\n", f.targetURL)
		adminURL := f.adminURL
		if adminURL == "" {
			adminURL = "http://" + f.targetURL
		}
		metricsURL := f.metricsURL
		if metricsURL == "" {
			metricsURL = adminURL
		}
		srv, err = ConnectTarget(ctx, f.targetURL, adminURL, metricsURL, f.apiKey, f.container)
		if err != nil {
			return nil, err
		}
	} else {
		fmt.Fprintln(os.Stderr, "loadtest: starting server (fakeproviders)...")
		srv, err = StartServer(ctx, ServerOpts{Root: root, ServerBin: f.serverBin, WorkDir: work, VADMode: f.vadMode, TurnManager: f.turnManager, MaxSessions: maxSess * 2})
		if err != nil {
			return nil, err
		}
	}
	defer srv.Stop() // also runs on panic; signal handling cancels ctx, which unwinds here
	agent, err := srv.Provision()
	if err != nil {
		return nil, err
	}
	cp := CallParams{
		IdleRSSMB:   srv.Proc().RSSMB,
		Server:      srv,
		AgentID:     agent,
		Clip:        clip,
		Turns:       f.turns,
		MinTurns:    f.minTurns,
		MaxTurns:    f.maxTurns,
		MinDuration: f.minSessDur,
		MaxDuration: f.maxSessDur,
		Think:       f.think,
		TurnLimit:   f.turnLimit,
		ChaosRate:   f.chaosRate,
	}

	rep := &Report{SchemaVersion: SchemaVersion, Kind: "govoice-loadtest", StartedAt: time.Now(), Machine: thisMachine()}
	rep.Config = Config{Mode: "load", Sessions: f.sessions, DurationS: f.duration.Seconds(), RampS: f.ramp.Seconds(), TurnsPerSession: f.turns,
		ThinkTimeMS: float64(f.think / time.Millisecond), Clip: f.clip, ClipSpeechMS: float64(len(clip)) / 2 / sampleRate * 1000, VADMode: f.vadMode, TurnManager: f.turnManager}
	if f.soak {
		rep.Config.Mode = "soak"
	}
	if f.findCap {
		rep.Config.Mode = "capacity"
		if len(steps) > 8 {
			return nil, fmt.Errorf("at most 8 capacity steps")
		}
		res, samples, capc, err := findCapacity(ctx, f, cp, steps)
		if err != nil {
			return nil, err
		}
		rep.Results, rep.Samples, rep.Capacity = *res, samples, capc
		rep.Config.Sessions = res.Sessions.Target
		rep.Config.DurationS, rep.Config.RampS = f.stepDur.Seconds(), 0
	} else {
		res, samples, err := runLoad(ctx, f, cp, f.sessions, f.duration, f.ramp)
		if err != nil {
			return nil, err
		}
		rep.Results, rep.Samples = *res, samples
	}
	th := f.th
	if f.findCap {
		th.MaxP95V2VMS = 0 // the sweep's own pass/fail is the capacity table
	}
	rep.Verdict = evaluate(rep, th)
	return rep, nil
}

// minStartGap keeps call starts under the server's per-IP handshake rate limit.
const minStartGap = 100 * time.Millisecond

func parseSteps(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 {
			return nil, fmt.Errorf("bad -cap-steps %q", s)
		}
		out = append(out, n)
	}
	return out, nil
}

func sumRejects(m map[string]uint64) (n uint64) {
	for _, v := range m {
		n += v
	}
	return
}

func findCapacity(ctx context.Context, f *flags, cp CallParams, steps []int) (*Results, []Sample, *Capacity, error) {
	capc := &Capacity{BudgetMS: f.capBudgetMS, Note: "Fake providers, localhost, load generator on the same machine as the server, one real speech clip: framework cost only, an upper bound for real deployments."}
	var best *Results
	var bestSamples []Sample
	for i, n := range steps {
		fmt.Fprintf(os.Stderr, "loadtest: capacity step %d/%d: %d sessions for %s\n", i+1, len(steps), n, f.stepDur)
		ramp := time.Duration(n) * minStartGap
		res, samples, err := runLoad(ctx, f, cp, n, f.stepDur, ramp)
		if err != nil {
			return nil, nil, nil, err
		}
		if i == 0 && capc.BudgetMS == 0 {
			capc.BudgetMS = math.Round(res.V2V.P95 + 150)
		}
		st := CapStep{Sessions: n, V2VP95: round2(res.V2V.P95), ErrorPct: round2(res.ErrorPct), Rejects: sumRejects(res.Counters.AdmissionRejects),
			CoresFull: round2(res.Res.CPUSPerSessMin * float64(n) / 60), PacingLagP99: res.Res.ClientPacingLag.P99, Pass: true}
		switch {
		case res.V2V.N == 0:
			st.Pass, st.Reason = false, "no completed turns"
		case res.V2V.P95 > capc.BudgetMS:
			st.Pass, st.Reason = false, "p95 over budget"
		case res.ErrorPct > f.th.MaxErrorPct:
			st.Pass, st.Reason = false, "errors"
		case st.Rejects > 0:
			st.Pass, st.Reason = false, "admission rejects"
		case res.Counters.SessionsFailed > 0 || res.Counters.WorkerRestarts > 0:
			st.Pass, st.Reason = false, "server-side session failures or VAD worker restarts"
		case res.Res.ClientPacingLag.P99 > 100:
			st.Pass, st.Reason = false, "load generator saturated (pacing lag p99 > 100 ms): result invalid"
		}
		capc.Steps = append(capc.Steps, st)
		if !st.Pass {
			if best == nil { // nothing passed: report the failing step
				best, bestSamples = res, samples
			}
			break
		}
		best, bestSamples = res, samples
		capc.MaxSessions, capc.CoresUsed = n, st.CoresFull
		if st.CoresFull > 0 {
			capc.SessionsPerCore = round2(float64(n) / st.CoresFull)
		}
	}
	if best == nil {
		return nil, nil, nil, fmt.Errorf("no capacity steps ran")
	}
	return best, bestSamples, capc, nil
}

// LoadParams-free core: one load phase against an already running server.
func runLoad(ctx context.Context, f *flags, cp CallParams, sessions int, duration, ramp time.Duration) (*Results, []Sample, error) {
	srv := cp.Server
	// The server rate-limits upgrades per source IP (limits.handshake_rate_per_ip, default
	// 20/s, burst 40) and every simulated caller is 127.0.0.1, so starts must be spread.
	if min := time.Duration(sessions) * minStartGap; ramp < min {
		fmt.Fprintf(os.Stderr, "loadtest: ramp raised to %s (server allows ~20 upgrades/s per source IP)\n", min)
		ramp = min
	}
	if ramp >= duration {
		return nil, nil, fmt.Errorf("ramp %s leaves no steady state in %s; lengthen -duration", ramp, duration)
	}
	idle, err := srv.Scrape()
	if err != nil {
		return nil, nil, err
	}
	idleProc := srv.Proc()
	if cp.IdleRSSMB > 0 {
		idleProc.RSSMB = cp.IdleRSSMB // fresh-server baseline: later capacity steps start from a grown heap
	}
	st := newLoadState()
	t0 := time.Now()
	var samples []Sample
	take := func() Sample {
		sn, err := srv.Scrape()
		pr := srv.Proc()
		s := Sample{T: time.Since(t0).Seconds(), RSSMB: pr.RSSMB, CPUSec: pr.CPUSec, FDs: pr.FDs}
		if err == nil {
			s.Active, s.Goroutines, s.HeapMB = int(sn.Gauges["voice_active_sessions"]), int(sn.Gauges["voice_process_goroutines"]), sn.Gauges["voice_process_heap_bytes"]/1048576
		}
		return s
	}
	loadCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if ramp > 0 {
				select {
				case <-time.After(ramp * time.Duration(i) / time.Duration(sessions)):
				case <-loadCtx.Done():
					return
				}
			}
			for loadCtx.Err() == nil {
				if err := runCall(loadCtx, st, cp, fmt.Sprintf("loadtest-%d", i)); err != nil && loadCtx.Err() == nil {
					select { // back off so a refusing server is not hot-looped
					case <-time.After(time.Second):
					case <-loadCtx.Done():
					}
				}
			}
		}(i)
	}
	tick := time.NewTicker(f.sampleEvery)
	loop := true
	for loop {
		select {
		case <-tick.C:
			samples = append(samples, take())
		case <-loadCtx.Done():
			loop = false
		}
	}
	tick.Stop()
	end := take() // load-phase end, before teardown
	samples = append(samples, end)
	wallLoad := time.Since(t0)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		fmt.Fprintln(os.Stderr, "loadtest: WARNING: calls still running 20 s after the load phase")
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	// Drain: wait for the server to see all sessions end, then settle.
	for i := 0; i < 30; i++ {
		if sn, err := srv.Scrape(); err == nil && sn.Gauges["voice_active_sessions"] == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	final, err := srv.Scrape()
	if err != nil {
		return nil, nil, err
	}
	finalProc := srv.Proc()
	return analyze(st, idle, final, idleProc, finalProc, samples, end, wallLoad, ramp, f.sampleEvery, sessions, f.soak), samples, nil
}

func median[T int | float64](xs []T) T {
	if len(xs) == 0 {
		var z T
		return z
	}
	s := append([]T(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func analyze(st *LoadState, idle, final *Snapshot, idleProc, finalProc ProcStats, samples []Sample, end Sample, wall, ramp, every time.Duration, target int, soak bool) *Results {
	r := &Results{Errors: st.errs, Server: map[string]Stat{}}
	st.mu.Lock()
	r.Sessions, r.Turns, r.V2V = st.sessions, st.turns, statOf(st.v2v)
	st.mu.Unlock()
	r.Sessions.Target, r.Sessions.MaxConcurrent = target, int(st.max.Load())
	if att := r.Turns.Attempted + r.Sessions.Failed + r.Sessions.Rejected; att > 0 {
		r.ErrorPct = round2(float64(r.Turns.Timeouts+r.Sessions.Failed+r.Sessions.Rejected) * 100 / float64(att))
	}
	if st.chaosInjected.Load() > 0 {
		r.ChaosInjected = int(st.chaosInjected.Load())
		st.mu.Lock()
		r.ChaosErrors = make(map[string]int, len(st.chaosErrs))
		for k, v := range st.chaosErrs {
			r.ChaosErrors[k] = v
		}
		st.mu.Unlock()
	}
	// server stage latency from histogram deltas
	for stage, fb := range final.Buckets {
		var bounds, cum []float64
		for le := range fb {
			if !math.IsInf(le, 1) {
				bounds = append(bounds, le)
			}
		}
		sort.Float64s(bounds)
		for _, le := range bounds {
			cum = append(cum, fb[le]-idle.Buckets[stage][le])
		}
		n := final.Count[stage] - idle.Count[stage]
		if n <= 0 {
			continue
		}
		q := func(p float64) float64 { return round2(bucketQuantile(p, bounds, cum, n) * 1000) }
		r.Server[stage] = Stat{N: int(n), P50: q(.5), P95: q(.95), P99: q(.99), Max: q(1), Mean: round2((final.Sum[stage] - idle.Sum[stage]) / n * 1000)}
	}
	d := func(name string) uint64 { return uint64(max(final.Gauges[name]-idle.Gauges[name], 0)) }
	r.Counters = ServerCounters{AdmissionRejects: map[string]uint64{}, VADDropped: d("voice_vad_dropped_chunks_total"), VADStale: d("voice_vad_stale_chunks_total"),
		SessionsFailed: d("voice_sessions_failed_total"), LeakedWork: d("voice_leaked_work_total"), LeakedSessions: d("voice_leaked_sessions_total"),
		ProtocolErrors: d("voice_transport_protocol_errors_total"), STTErrors: d("voice_stt_errors_total"), LLMErrors: d("voice_llm_errors_total"),
		TTSErrors: d("voice_tts_errors_total"), WorkerRestarts: d("voice_vad_worker_restarts_total")}
	for k, v := range final.Rejects {
		if n := uint64(max(v-idle.Rejects[k], 0)); n > 0 {
			r.Counters.AdmissionRejects[k] = n
		}
	}
	// resources
	rs := &r.Res
	rs.ClientPacingLag = st.lagStat()
	rs.RSSIdleMB, rs.GoroutinesIdle, rs.FDsIdle = round2(idleProc.RSSMB), int(idle.Gauges["voice_process_goroutines"]), idleProc.FDs
	rs.GoroutinesEnd, rs.FDsEnd = int(final.Gauges["voice_process_goroutines"]), finalProc.FDs
	warm := max(ramp, wall/10).Seconds()
	var region []Sample
	for _, s := range samples {
		rs.RSSPeakMB = max(rs.RSSPeakMB, s.RSSMB)
		rs.HeapPeakMB = max(rs.HeapPeakMB, s.HeapMB)
		rs.GoroutinesPeak, rs.FDsPeak = max(rs.GoroutinesPeak, s.Goroutines), max(rs.FDsPeak, s.FDs)
		if s.T >= warm {
			region = append(region, s)
		}
	}
	rs.RSSPeakMB, rs.HeapPeakMB = round2(rs.RSSPeakMB), round2(rs.HeapPeakMB)
	col := func(ss []Sample, g func(Sample) float64) []float64 {
		o := make([]float64, len(ss))
		for i, s := range ss {
			o[i] = g(s)
		}
		return o
	}
	rs.RSSSteadyMB = round2(median(col(region, func(s Sample) float64 { return s.RSSMB })))
	act := max(median(col(region, func(s Sample) float64 { return float64(s.Active) })), 1)
	rs.MBPerSession = round2(max(rs.RSSSteadyMB-rs.RSSIdleMB, 0) / act)
	rs.CPUSeconds = round2(end.CPUSec - idleProc.CPUSec)
	rs.CoresAvg = round2(rs.CPUSeconds / wall.Seconds())
	rs.SessionMinutes = round2(float64(st.sessNS.Load()) / 6e10)
	if rs.SessionMinutes > 0 {
		rs.CPUSPerSessMin = round2(rs.CPUSeconds / rs.SessionMinutes)
	}
	// growth: first vs last window of the post-warmup region
	if len(region) > 0 {
		regionLen := region[len(region)-1].T - region[0].T
		w := math.Min(regionLen/4, 300)
		if w >= 3*every.Seconds() || (!soak && w >= 2*every.Seconds()) {
			var a, b []Sample
			for _, s := range region {
				if s.T <= region[0].T+w {
					a = append(a, s)
				}
				if s.T >= region[len(region)-1].T-w {
					b = append(b, s)
				}
			}
			ra, rb := median(col(a, func(s Sample) float64 { return s.RSSMB })), median(col(b, func(s Sample) float64 { return s.RSSMB }))
			ha, hb := median(col(a, func(s Sample) float64 { return s.HeapMB })), median(col(b, func(s Sample) float64 { return s.HeapMB }))
			ga, gb := median(col(a, func(s Sample) float64 { return float64(s.Goroutines) })), median(col(b, func(s Sample) float64 { return float64(s.Goroutines) }))
			fa, fb := median(col(a, func(s Sample) float64 { return float64(s.FDs) })), median(col(b, func(s Sample) float64 { return float64(s.FDs) }))
			pct := func(x, y float64) float64 {
				if x <= 0 {
					return 0
				}
				return round2((y - x) / x * 100)
			}
			r.Growth = Growth{WindowS: math.Round(w), RSSPct: pct(ra, rb), HeapPct: pct(ha, hb), Goroutines: int(gb - ga), FDs: int(fb - fa),
				GoroutinesDrain: rs.GoroutinesEnd - rs.GoroutinesIdle, Valid: true}
		}
	}
	if !r.Growth.Valid {
		r.Growth.GoroutinesDrain = rs.GoroutinesEnd - rs.GoroutinesIdle
	}
	return r
}
