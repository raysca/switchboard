// Command agents scores a stored agent's behaviour with simulated callers (see
// docs/operations/agent-evals.md). It drives the public API like any integration: a draft voice
// token, the /ws RTVI text turns, and the stored session and traces.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"govoice/tools/internal/stack"
)

const defaultMaxCalls = 40 // caller turns per run; paid runs stay bounded

// config is the parsed command line.
type config struct {
	Dir, Only, Mode, JSON, Admin, Public string
	Version, Runs, MinPass, MaxCalls     int
	Opts                                 opts
	Selftest                             bool
}

func parseFlags(args []string) (config, error) {
	var c config
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.Dir, "cases", "tools/eval/agents/cases", "directory of case files (*.yaml, *.yml)")
	fs.StringVar(&c.Only, "case", "", "comma-separated case names to run (default all)")
	fs.StringVar(&c.Mode, "providers", modeKeep, "model under test: keep (the agent's own, paid), offline (Ollama) or echo (plumbing only)")
	fs.IntVar(&c.Version, "version", 0, "run this published version of each agent (default the current one)")
	fs.IntVar(&c.Runs, "runs", 1, "runs per case")
	fs.IntVar(&c.MinPass, "min-pass", 1, "runs that must pass for a case to pass")
	fs.StringVar(&c.JSON, "json", "", "also write the report as JSON to this file (mode 0600)")
	fs.DurationVar(&c.Opts.TurnTimeout, "turn-timeout", 45*time.Second, "most time one typed turn may take")
	fs.DurationVar(&c.Opts.Quiet, "quiet", 1500*time.Millisecond, "agent silence that ends a turn (tool rounds and handoffs restart it)")
	fs.IntVar(&c.MaxCalls, "max-calls", defaultMaxCalls, "most caller turns in the whole run")
	fs.StringVar(&c.Admin, "admin", "http://127.0.0.1:9090", "management API base URL")
	fs.StringVar(&c.Public, "public", "127.0.0.1:8080", "public listener host:port")
	fs.BoolVar(&c.Selftest, "selftest", false, "prove the harness on a server child and the echo provider, with deliberately broken agents (free)")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	switch {
	case c.Mode != modeKeep && c.Mode != modeOffline && c.Mode != modeEcho:
		return c, fmt.Errorf("-providers %q: want keep, offline or echo", c.Mode)
	case c.Runs < 1 || c.MinPass < 1 || c.MinPass > c.Runs:
		return c, errors.New("-runs must be at least 1 and -min-pass between 1 and -runs")
	case c.MaxCalls < 1 || c.Version < 0 || c.Opts.TurnTimeout <= 0 || c.Opts.Quiet <= 0:
		return c, errors.New("-max-calls must be at least 1, -version not negative, -turn-timeout and -quiet positive")
	}
	return c, nil
}

// plannedTurns is the caller turns a run will type.
func plannedTurns(cases []Case, runs int) (n int) {
	for _, c := range cases {
		if p := c.Caller.Persona; p != nil {
			n += p.MaxTurns * runs
		} else {
			n += len(c.Caller.Script) * runs
		}
	}
	return n
}

// checkPlan refuses a run before its first call: callers that do not exist yet, or more turns than the cap.
func checkPlan(cases []Case, runs, maxCalls int) error {
	for _, c := range cases {
		if c.Caller.Persona != nil {
			return fmt.Errorf("case %s: persona callers are not available yet, use a script", c.Name)
		}
	}
	if n := plannedTurns(cases, runs); n > maxCalls {
		return fmt.Errorf("the run plans %d caller turns, more than -max-calls %d", n, maxCalls)
	}
	return nil
}

// liveGate refuses a run that reaches a paid model unless GOVOICE_LIVE=1.
func liveGate(needsLive bool, env string) error {
	if needsLive && env != "1" {
		return errors.New("this run calls the agents' own models and spends API credit: set GOVOICE_LIVE=1 to allow it, or use -providers offline")
	}
	return nil
}

// prepare resolves and rewrites the agent of every case (read-only API calls) and reports whether any
// rewritten config reaches a paid model.
func prepare(srv *stack.Server, cases []Case, c config) (refs map[string]agentRef, needsLive bool, err error) {
	refs = map[string]agentRef{}
	for _, cs := range cases {
		if _, done := refs[cs.Agent]; done {
			continue
		}
		ref, err := resolveAgent(srv, cs.Agent, c.Version, c.Mode)
		if err != nil {
			return nil, false, fmt.Errorf("case %s: %w", cs.Name, err)
		}
		refs[cs.Agent] = ref
		needsLive = needsLive || NeedsLive(ref.Draft)
	}
	return refs, needsLive, nil
}

// execute runs every case c.Runs times and builds the report.
func execute(ctx context.Context, srv *stack.Server, cases []Case, refs map[string]agentRef, c config) Report {
	rep := Report{Schema: reportSchema, Mode: c.Mode}
	for _, cs := range cases {
		res := CaseResult{Case: cs.Name}
		ok := 0
		for range c.Runs {
			run := runCase(ctx, srv, refs[cs.Agent], cs, c.Opts)
			if run.OK() {
				ok++
			}
			res.Runs = append(res.Runs, run)
			if ctx.Err() != nil {
				break
			}
		}
		res.Passed = ok >= c.MinPass
		rep.Results = append(rep.Results, res)
		if ctx.Err() != nil {
			break
		}
	}
	return rep
}

func runCase(ctx context.Context, srv *stack.Server, ref agentRef, cs Case, o opts) RunResult {
	obs, err := runOnce(ctx, srv, ref, ScriptCaller(cs.Caller.Script), o)
	run := RunResult{SessionID: obs.SessionID, AgentVersionID: obs.AgentVersionID, ConfigHash: obs.ConfigHash,
		Path: obs.Path, Tools: obs.Tools, EndReason: obs.EndReason, TurnCount: obs.TurnCount,
		MedianReplyMS: medianMS(obs.ReplyMS), Transcript: obs.Transcript}
	if err != nil {
		run.Errors = strings.Split(err.Error(), "\n")
	}
	run.Failures = Score(cs, obs, ref.StepNames)
	return run
}

func main() { os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr)) }

func realMain(args []string, stdout, stderr io.Writer) int {
	c, err := parseFlags(args)
	if err != nil {
		fmt.Fprintln(stderr, "agent-eval:", err)
		return 2
	}
	if c.Selftest {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return selftest(ctx, stdout, stderr)
	}
	setup := func(err error) int { fmt.Fprintln(stderr, "agent-eval:", err); return 2 }
	var only []string
	if c.Only != "" {
		only = strings.Split(c.Only, ",")
	}
	cases, err := LoadCases(c.Dir, only)
	if err != nil {
		return setup(err)
	}
	if err := checkPlan(cases, c.Runs, c.MaxCalls); err != nil {
		return setup(err)
	}
	key := os.Getenv("GOVOICE_MANAGEMENT_API_KEY") // never printed
	if key == "" {
		return setup(errors.New("GOVOICE_MANAGEMENT_API_KEY is not set (make dev writes it to .govoice-dev/dev.env)"))
	}
	srv := &stack.Server{Admin: c.Admin, Public: c.Public, Key: key}
	refs, needsLive, err := prepare(srv, cases, c)
	if err != nil {
		return setup(err)
	}
	if err := liveGate(needsLive, os.Getenv("GOVOICE_LIVE")); err != nil {
		return setup(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep := execute(ctx, srv, cases, refs, c)
	WriteText(stdout, rep)
	if c.JSON != "" {
		if err := WriteJSON(c.JSON, rep); err != nil {
			return setup(err)
		}
	}
	for _, r := range rep.Results {
		if !r.Passed {
			return 1
		}
	}
	if ctx.Err() != nil {
		return 1
	}
	return 0
}
