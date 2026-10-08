package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// planItem is one scheduled scenario run.
type planItem struct {
	prof   Profile // resolved
	mode   mode
	sc     scenario
	repeat int
	na     string // static not-applicable reason
	calls  int    // upper-bound estimate of provider requests; 0 when na or fake
}

// buildPlan lists every (profile, mode, scenario, repeat) in execution order.
func buildPlan(profs []Profile, ms []mode, scs []scenario, repeat int, o *opts) []planItem {
	var plan []planItem
	for _, p := range profs {
		for _, m := range ms {
			for _, class := range []serverClass{classBase, classIdle, classPing} {
				for _, sc := range scs {
					if sc.class.name != class.name {
						continue
					}
					for rep := 1; rep <= repeat; rep++ {
						it := planItem{prof: p, mode: m, sc: sc, repeat: rep}
						if sc.na != nil {
							it.na = sc.na(p, m, o)
						}
						if it.na == "" && !p.Fake {
							it.calls = estCalls(sc.shape(o), p)
						}
						plan = append(plan, it)
					}
				}
			}
		}
	}
	return plan
}

// estimate is the total provider requests the plan will spend plus clip synthesis.
func estimate(plan []planItem, synthClips int) int {
	n := synthClips
	for _, it := range plan {
		n += it.calls
	}
	return n
}

type runCfg struct {
	root, work, bin string
	plan            []planItem
	clips           clips
	o               *opts
	env             func(string) string
	maxCalls        int
	act             *spend // actual provider requests (measured); nil in tests that build no server
	spentBase       int    // clip synthesis already spent
	scenarioTimeout time.Duration
	logLevel        string
}

// runPlan executes the plan: one fresh server (ephemeral ports, own credential store) per
// profile x mode x server class. Failures are recorded per result and never abort other profiles.
func runPlan(ctx context.Context, cfg runCfg, rep *Report) {
	spent := cfg.spentBase
	type key struct {
		prof, mode, class string
	}
	var order []key
	groups := map[key][]planItem{}
	for _, it := range cfg.plan {
		k := key{it.prof.Name, it.mode.name, it.sc.class.name}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], it)
	}
	record := func(it planItem, r *Result) {
		r.Estimate = it.calls
		r.Profile, r.Mode, r.Scenario, r.Repeat = it.prof.Name, it.mode.name, it.sc.id, it.repeat
		r.finalize()
		rep.Results = append(rep.Results, r)
		line := r.Reason
		if line == "" {
			line = formatMetrics(r)
		}
		fmt.Fprintf(stdout, "%-3s %-26s %-8s %-5s %s\n", r.Scenario, r.Profile, r.Mode, strings.ToUpper(r.Status), truncate(line, 220))
		for _, c := range r.Checks {
			if !c.Pass {
				fmt.Fprintf(stdout, "      FAILED check: %s %s\n", c.Name, c.Note)
			}
		}
	}
	for _, k := range order {
		items := groups[k]
		var runnable []planItem
		for _, it := range items {
			if it.na != "" {
				r := &Result{}
				r.na(it.na)
				record(it, r)
				continue
			}
			if ctx.Err() != nil {
				r := &Result{Status: stSkip, Reason: "run interrupted"}
				record(it, r)
				continue
			}
			if spent += it.calls; cfg.maxCalls > 0 && spent > cfg.maxCalls {
				r := &Result{Status: stSkip, Reason: fmt.Sprintf("-max-calls %d reached", cfg.maxCalls)}
				record(it, r)
				continue
			}
			runnable = append(runnable, it)
		}
		if len(runnable) == 0 {
			continue
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					for _, it := range runnable {
						if !hasResult(rep, it) {
							record(it, &Result{Status: stErr, Reason: theRedactor.String(fmt.Sprint("harness panic: ", p))})
						}
					}
				}
			}()
			runGroup(ctx, cfg, rep, k.prof+"-"+k.mode+"-"+k.class, runnable, record)
		}()
	}
}

func hasResult(rep *Report, it planItem) bool {
	for _, r := range rep.Results {
		if r.Profile == it.prof.Name && r.Mode == it.mode.name && r.Scenario == it.sc.id && r.Repeat == it.repeat {
			return true
		}
	}
	return false
}

func runGroup(ctx context.Context, cfg runCfg, rep *Report, name string, items []planItem, record func(planItem, *Result)) {
	first := items[0]
	failAll := func(err error) {
		for _, it := range items {
			record(it, &Result{Status: stErr, Reason: redact(err)})
		}
	}
	srv, err := startServer(ctx, serverSpec{root: cfg.root, bin: cfg.bin, work: cfg.work, name: name, mode: first.mode,
		class: first.sc.class, infra: first.prof.Infra, fake: first.prof.Fake, logLevel: cfg.logLevel})
	if err != nil {
		failAll(fmt.Errorf("start server: %w", err))
		return
	}
	defer srv.stop()
	defer func() { // runs before stop: the child must still answer /metrics
		n, ok := srv.requestTotal()
		if !ok {
			rep.Warnings = append(rep.Warnings, name+": provider request counter unreadable at stop; actual total uses the last good scrape")
		}
		cfg.act.finish(n)
	}()
	mcp := ""
	for _, it := range items {
		if it.sc.id == "S6" {
			mcp = cfg.o.mcpURL
		}
	}
	agent, err := srv.provision(first.prof, cfg.env, mcp)
	if err != nil {
		failAll(fmt.Errorf("provision: %w", err))
		return
	}
	for _, it := range items {
		if ctx.Err() != nil {
			record(it, &Result{Status: stSkip, Reason: "run interrupted"})
			continue
		}
		if n, ok := srv.requestTotal(); cfg.act != nil && !ok {
			record(it, &Result{Status: stSkip, Reason: "provider request counter unreadable (server /metrics): not started, failing closed"})
			continue
		} else if cfg.act.exhausted(n) {
			record(it, &Result{Status: stSkip, Reason: fmt.Sprintf("-max-calls %d reached (actual provider requests)", cfg.maxCalls)})
			continue
		}
		record(it, runScenario(ctx, cfg, srv, agent, it, name))
	}
	for _, l := range srv.logProblems() {
		fmt.Fprintf(stdout, "  [%s log] %s\n", name, l)
		rep.Warnings = append(rep.Warnings, name+": "+l)
	}
}

func runScenario(ctx context.Context, cfg runCfg, srv *server, agent string, it planItem, name string) *Result {
	r := &Result{}
	c, err := dial(ctx, srv, agent)
	if err != nil {
		r.abort(redact(err))
		return r
	}
	x := &sctx{c: c, srv: srv, clips: cfg.clips, p: it.prof, mode: it.mode, o: cfg.o, fake: it.prof.Fake}
	t0 := time.Now()
	reqBefore := srv.requests()
	var overBudget atomic.Bool
	stopWatch := watchSpend(cfg.act, srv.requestTotal, time.Second, func() { overBudget.Store(true); _ = c.conn.CloseNow() })
	var timedOut atomic.Bool
	wd := time.AfterFunc(cfg.scenarioTimeout, func() { timedOut.Store(true); _ = c.conn.CloseNow() })
	stopOnCancel := context.AfterFunc(ctx, func() { _ = c.conn.CloseNow() }) // Ctrl-C ends the call now
	func() {
		defer func() {
			if p := recover(); p != nil {
				r.abort(theRedactor.String(fmt.Sprint("scenario panic: ", p)))
			}
		}()
		it.sc.run(x, r)
	}()
	wd.Stop()
	stopOnCancel()
	stopWatch()
	r.Seconds = time.Since(t0).Seconds()
	by := reqDelta(reqBefore, srv.requests())
	if n, ok := srv.requestTotal(); overBudget.Load() || cfg.act.over(n) || (cfg.act != nil && !ok) {
		r.abort(fmt.Sprintf("actual provider requests exceeded -max-calls %d or could not be read: call ended", cfg.maxCalls))
	}
	if timedOut.Load() {
		r.abort(fmt.Sprintf("scenario exceeded %s", cfg.scenarioTimeout))
	}
	if ctx.Err() != nil {
		*r = Result{Status: stSkip, Reason: "run interrupted"}
	}
	r.ReqBy = by
	for _, n := range by {
		r.Requests += n
	}
	if cfg.o.trace {
		fmt.Fprintln(stdout, "  timeline", it.sc.id, name)
		c.timeline()
		srv.timeline(c.n)
	}
	if it.sc.id == "S5" || it.sc.id == "S8" {
		_ = c.conn.CloseNow()
	} else {
		c.close()
	}
	return r
}

// makeWork creates the private temp dir for binaries, configs and logs.
func makeWork() (string, error) {
	d, err := os.MkdirTemp("", "govoice-agentmatrix-")
	if err != nil {
		return "", err
	}
	return d, os.MkdirAll(filepath.Join(d, "clips"), 0o700)
}
