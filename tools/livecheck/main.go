// livecheck is the agent-level provider-matrix harness. It runs ONE standard agent
// (standard system prompt, fixed scenario set S1-S8) against a real server child for each
// provider PROFILE in profiles.yaml, over the real WebSocket/RTVI protocol, and reports
// pass/fail assertions plus latency per profile x scenario x turn-manager mode.
//
// Real providers spend API credit, so a run needs GOVOICE_LIVE=1 (or -yes) after the
// provider-request estimate is printed. `-selftest` runs the same engine offline against a
// `-tags fakeproviders` server (no network, no cost). Secrets come from examples/.env into
// the process environment only, are stored through the management API like `make dev` does,
// and every byte of output and every report file is redacted.
//
// Examples:
//
//	GOVOICE_LIVE=1 make agent-matrix PROFILES=google,nvidia SCENARIOS=S1 MODES=off,on
//	go run ./tools/livecheck -list
//	make live-check-selftest
//
// See docs/benchmarks/README.md (how to add a profile) and docs/benchmarks/agent-matrix.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"govoice/examples/envload"
)

func main() {
	code := realMain(os.Args[1:])
	stdout.Flush()
	stderr.Flush()
	os.Exit(code)
}

func realMain(args []string) int {
	fs := flag.NewFlagSet("livecheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		root      = fs.String("root", ".", "repo root")
		clipDir   = fs.String("clips", "", "clip directory: our cached <name>.pcm and/or the shared provtest set (clips.json); missing clips are synthesized into it (default: temp dir)")
		profNames = fs.String("profiles", "google", "comma list of profile names, or all (every real profile)")
		profFile  = fs.String("profiles-file", "", "profiles YAML to use instead of the embedded profiles.yaml")
		scList    = fs.String("scenarios", "", "comma list of S1..S8 (default all)")
		modeList  = fs.String("modes", "off,on,on+smart", "comma list of turn modes: off, on (manager + min_words 3), on+smart (adds smart-turn), plain, fast")
		repeat    = fs.Int("repeat", 1, "run every scenario this many times")
		maxCalls  = fs.Int("max-calls", 60, "hard cap on provider requests: refuses a plan whose upper-bound estimate exceeds it, and aborts the run mid-call when the MEASURED requests exceed it")
		yes       = fs.Bool("yes", false, "proceed after printing the estimate (GOVOICE_LIVE=1 does the same)")
		list      = fs.Bool("list", false, "list profiles and whether this environment can run them; spends nothing")
		dryRun    = fs.Bool("dry-run", false, "print the plan and provider-request estimate; spends nothing")
		selftest  = fs.Bool("selftest", false, "offline engine self-test: fake profile, -tags fakeproviders, no network")
		outDir    = fs.String("out", ".agent-matrix-out", "directory for results.json and agent-matrix.md")
		writeDocs = fs.Bool("write-docs", false, "also merge the results into docs/benchmarks/agent-matrix.{json,md} (latest run per profile x mode x scenario)")
		jsonOut   = fs.String("json-out", "", "also write results JSON to this file")
		keep      = fs.Bool("keep-work", false, "keep the temp dir (server logs, configs)")
		probe     = fs.Bool("stt-probe", false, "only stream the question and pause clips to Google STT and print result timing (2 streams)")
		sttModel  = fs.String("stt-model", "", "override the STT model of every selected profile (and -stt-probe)")
		logLevel  = fs.String("log-level", "info", "server log level")
		timeout   = fs.Duration("scenario-timeout", 4*time.Minute, "per-scenario watchdog")
		mcpURL    = fs.String("mcp-url", "", "public HTTPS MCP server exposing a get_time tool; enables S6")
		mcpExpect = fs.String("mcp-expect", `\d`, "regexp the S6 answer must match (the tool result)")
		// compatibility with the first livecheck
		only     = fs.String("only", "", "alias of -scenarios")
		variants = fs.String("variants", "", "alias of -modes")
		provs    = fs.String("providers", "", "alias of -profiles")
	)
	o := &opts{}
	fs.IntVar(&o.s1Turns, "s1-turns", 2, "S1 turns per call")
	fs.DurationVar(&o.pause, "pause", time.Second, "S4: silence inside the utterance")
	fs.Float64Var(&o.werMax, "wer-max", 0.4, "max word error rate of what the STT heard")
	fs.DurationVar(&o.v2vMax, "v2v-max", 8*time.Second, "S1: voice-to-voice ceiling")
	fs.DurationVar(&o.stopMax, "stop-max", 1500*time.Millisecond, "S2: bot audio must stop this fast after the user starts")
	fs.BoolVar(&o.trace, "trace", false, "print the event timeline of every scenario")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	for _, a := range [][2]*string{{only, scList}, {variants, modeList}, {provs, profNames}} {
		if *a[0] != "" {
			*a[1] = *a[0]
		}
	}
	re, err := regexp.Compile(*mcpExpect)
	if err != nil {
		fmt.Fprintln(stderr, "-mcp-expect:", err)
		return 2
	}
	o.mcpURL, o.toolExpect = *mcpURL, re
	if *selftest {
		if *writeDocs {
			fmt.Fprintln(stderr, "-write-docs with -selftest would put fake results in the docs")
			return 2
		}
		*profNames = "fake"
		if *modeList == "off,on,on+smart" {
			*modeList = "off,on"
		}
	}

	rootAbs, _ := filepath.Abs(*root)
	if err := os.Chdir(rootAbs); err != nil {
		fmt.Fprintln(stderr, redact(err))
		return 1
	}
	_ = envload.Load()
	theRedactor.addEnv(os.Environ())
	env := os.Getenv

	all, err := loadProfiles(*profFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *list {
		for _, p := range all {
			state := "runnable"
			if why := p.gate(env); why != "" {
				state = "SKIPPED: " + why
			}
			p = p.resolve(env)
			fmt.Fprintf(stdout, "%-30s %-45s %s\n", p.Name, stageText(p.Stages.STT)+" / "+stageText(p.Stages.LLM)+" / "+stageText(p.Stages.TTS), state)
		}
		return 0
	}
	sel, err := selectProfiles(all, *profNames)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var runnable []Profile
	rep := newReport(rootAbs)
	for _, p := range sel {
		if *sttModel != "" {
			p.Stages.STT.Model = *sttModel
		}
		why := p.gate(env)
		p = p.resolve(env)
		rep.Profiles = append(rep.Profiles, profileReport(p, why))
		if why != "" {
			fmt.Fprintf(stdout, "profile %s: SKIPPED (%s)\n", p.Name, why)
			continue
		}
		runnable = append(runnable, p)
	}
	if len(runnable) == 0 {
		fmt.Fprintln(stderr, "no selected profile can run in this environment (see SKIPPED reasons; -list shows all)")
		return 1
	}
	var ms []mode
	for _, n := range splitList(*modeList) {
		if a := modeAlias[n]; a != "" {
			n = a
		}
		m, ok := modes[n]
		if !ok {
			fmt.Fprintf(stderr, "unknown mode %q\n", n)
			return 2
		}
		ms = append(ms, m)
		rep.Modes = append(rep.Modes, n)
	}
	var scs []scenario
	for _, id := range splitList(*scList) {
		sc, ok := scenarioByID(strings.ToUpper(id))
		if !ok {
			fmt.Fprintf(stderr, "unknown scenario %q\n", id)
			return 2
		}
		scs = append(scs, sc)
	}
	if len(scs) == 0 {
		scs = scenarios
	}
	for _, s := range scs {
		rep.Scenarios = append(rep.Scenarios, s.id)
	}
	if *repeat < 1 {
		*repeat = 1
	}
	rep.Repeat, rep.Selftest, rep.Tags = *repeat, *selftest, tagUnion(runnable)

	// Clips: the committed fixture, the shared set, our cache; the rest is synthesized (counted).
	work, err := makeWork()
	if err != nil {
		fmt.Fprintln(stderr, redact(err))
		return 1
	}
	if *keep {
		fmt.Fprintln(stdout, "keeping", work, "(server logs and configs; mode 0700; each server's credential store is deleted when it stops)")
	} else {
		defer os.RemoveAll(work)
	}
	dir := *clipDir
	if dir == "" {
		dir = filepath.Join(work, "clips")
	}
	_ = os.MkdirAll(dir, 0o700)
	cl, missing := loadClips(dir, rootAbs)
	missing = neededMissing(missing, scs)
	fake := runnable[0].Fake
	if fake {
		if cl["question"] == nil {
			fmt.Fprintln(stderr, "fake profile needs", fixtureQuestion)
			return 1
		}
		cl, missing = fakeClips(cl["question"]), nil
	}
	plan := buildPlan(runnable, ms, scs, *repeat, o)
	rep.Estimate = estimate(plan, len(missing))
	printPlan(plan, len(missing), rep.Estimate, *maxCalls)
	if rep.Estimate > *maxCalls && !*dryRun { // a plan preview reports the verdict and exits 0
		fmt.Fprintf(stderr, "refused before any provider call: upper-bound estimate %d exceeds -max-calls %d.\n"+
			"  narrow the plan: -profiles/-scenarios/-modes (make: PROFILES=google SCENARIOS=S1 MODES=off)\n"+
			"  or raise the cap on purpose: -max-calls %d (make: MAX_CALLS=%d)\n", rep.Estimate, *maxCalls, rep.Estimate, rep.Estimate)
		return 2
	}
	if *dryRun {
		return 0
	}
	if !*selftest && os.Getenv("GOVOICE_LIVE") != "1" && !*yes {
		fmt.Fprintln(stderr, "this run spends real provider credit: set GOVOICE_LIVE=1 (or pass -yes) to proceed")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	synthN, err := synthesize(ctx, cl, missing, dir, env)
	if err != nil {
		fmt.Fprintln(stderr, redact(err))
		return 1
	}
	act := newSpend(*maxCalls, synthN)
	if *probe {
		sa, err := credSpecs["google"].secret("stt", env)
		if err != nil {
			fmt.Fprintln(stderr, "stt probe:", err)
			return 1
		}
		model := *sttModel
		if model == "" {
			model = "chirp_2"
		}
		sttProbe(ctx, "question", model, cl["question"], []byte(sa))
		sttProbe(ctx, "pause", model, pauseClip(cl, o.pause), []byte(sa))
		return 0
	}
	for _, s := range clipSpecs {
		if b := cl[s.name]; b != nil {
			fmt.Fprintf(stdout, "clip %-12s %4.1fs %q\n", s.name, pcmDur(b).Seconds(), s.text)
		}
	}

	fmt.Fprintf(stdout, "building server (tags: %s)...\n", strings.Join(append([]string{"none"}, rep.Tags...)[min(len(rep.Tags), 1):], ","))
	bin, err := buildServer(rootAbs, work, rep.Tags)
	if err != nil {
		fmt.Fprintln(stderr, redact(err))
		return 1
	}
	runPlan(ctx, runCfg{root: rootAbs, work: work, bin: bin, plan: plan, clips: cl, o: o, env: env, maxCalls: *maxCalls,
		spentBase: len(missing), act: act, scenarioTimeout: *timeout, logLevel: *logLevel}, rep)

	rep.Actual = act.total(0)
	fmt.Fprintf(stdout, "actual provider requests: %d (upper-bound estimate %d, -max-calls %d)\n", rep.Actual, rep.Estimate, *maxCalls)
	md, err := writeReport(rep, *outDir)
	if err != nil {
		fmt.Fprintln(stderr, redact(err))
		return 1
	}
	fmt.Fprintln(stdout, "report:", md, "and", filepath.Join(*outDir, "results.json"))
	if *writeDocs {
		if err := mergeDocs(rep, filepath.Join(rootAbs, "docs/benchmarks")); err != nil {
			fmt.Fprintln(stderr, redact(err))
			return 1
		}
		fmt.Fprintln(stdout, "merged into docs/benchmarks/agent-matrix.md (+ agent-matrix.json)")
	}
	if *jsonOut != "" {
		if j, err := os.ReadFile(filepath.Join(*outDir, "results.json")); err == nil {
			_ = os.WriteFile(*jsonOut, j, 0o600)
		}
	}
	for _, r := range rep.Results {
		if r.Status == stFail || r.Status == stErr {
			return 1
		}
	}
	return 0
}

// printPlan prints the per-profile provider-request estimate before anything is spent.
func printPlan(plan []planItem, synth, total, max int) {
	per := map[string]int{}
	var order []string
	for _, it := range plan {
		if _, ok := per[it.prof.Name]; !ok {
			order = append(order, it.prof.Name)
		}
		per[it.prof.Name] += it.calls
	}
	fmt.Fprintln(stdout, "upper-bound estimate of provider requests (STT stream opens + LLM + TTS requests; pessimistic, measured after the run):")
	for _, n := range order {
		fmt.Fprintf(stdout, "  %-30s %4d\n", n, per[n])
	}
	if synth > 0 {
		fmt.Fprintf(stdout, "  %-30s %4d (missing clips)\n", "clip synthesis", synth)
	}
	fmt.Fprintf(stdout, "  %-30s %4d\n", "total (upper bound)", total)
	verdict := "within the cap"
	if total > max {
		verdict = "OVER the cap: the run will be refused"
	}
	fmt.Fprintf(stdout, "hard cap -max-calls %d (checked against measured requests during the run): %s\n", max, verdict)
}
