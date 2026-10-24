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

const defaultMaxTurns = 40 // caller turns per run (not calls: a call has up to one per typed line); paid runs stay bounded

// config is the parsed command line.
type config struct {
	Dir, Only, Mode, JSON, Admin, Public string
	CallerLLM, CallerURL, CallerModel    string
	Version, Runs, MinPass, MaxTurns     int
	Opts                                 opts
	Selftest                             bool
}

// parseFlags parses the command line; -h prints the flag defaults to usage and returns flag.ErrHelp.
func parseFlags(args []string, usage io.Writer) (config, error) {
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
	fs.IntVar(&c.MaxTurns, "max-turns", defaultMaxTurns, "most caller turns in the whole run")
	fs.StringVar(&c.Admin, "admin", "http://127.0.0.1:9090", "management API base URL")
	fs.StringVar(&c.Public, "public", "127.0.0.1:8080", "public listener host:port")
	fs.StringVar(&c.CallerLLM, "caller-llm", callerOffline, "model that plays persona callers: offline (Ollama, free) or xai (paid, XAI_API_KEY, needs GOVOICE_LIVE=1)")
	fs.StringVar(&c.CallerURL, "caller-url", "", "persona model endpoint for -caller-llm offline (default Ollama on 127.0.0.1:11434; a non-loopback host counts as paid)")
	fs.StringVar(&c.CallerModel, "caller-model", "", "persona model name (default govoice-test, or the xAI default)")
	fs.BoolVar(&c.Selftest, "selftest", false, "prove the harness on a server child and the echo provider, with deliberately broken agents (free)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(usage)
			fmt.Fprintln(usage, "usage: go run ./tools/eval/agents [flags]")
			fs.PrintDefaults()
		}
		return c, err
	}
	switch {
	case c.Mode != modeKeep && c.Mode != modeOffline && c.Mode != modeEcho:
		return c, fmt.Errorf("-providers %q: want keep, offline or echo", c.Mode)
	case c.Runs < 1 || c.MinPass < 1 || c.MinPass > c.Runs:
		return c, errors.New("-runs must be at least 1 and -min-pass between 1 and -runs")
	case c.CallerLLM != callerOffline && c.CallerLLM != callerXAI:
		return c, fmt.Errorf("-caller-llm %q: want offline or xai", c.CallerLLM)
	case c.CallerLLM == callerXAI && c.CallerURL != "":
		return c, errors.New("-caller-url is for -caller-llm offline: the xAI key is sent only to the xAI endpoint")
	case c.MaxTurns < 1 || c.Version < 0 || c.Opts.TurnTimeout <= 0 || c.Opts.Quiet <= 0:
		return c, errors.New("-max-turns must be at least 1, -version not negative, -turn-timeout and -quiet positive")
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

// checkPlan refuses a run before its first call: more caller turns than the cap.
func checkPlan(cases []Case, runs, maxTurns int) error {
	if n := plannedTurns(cases, runs); n > maxTurns {
		return fmt.Errorf("the run plans %d caller turns, more than -max-turns %d", n, maxTurns)
	}
	return nil
}

// hasPersona reports whether any case needs the persona model.
func hasPersona(cases []Case) bool {
	for _, c := range cases {
		if c.Caller.Persona != nil {
			return true
		}
	}
	return false
}

// liveGate refuses a run that reaches a paid model unless GOVOICE_LIVE=1.
func liveGate(needsLive bool, env string) error {
	if needsLive && env != "1" {
		return errors.New("this run reaches a paid model (an agent's own, or the persona caller) and spends API credit: set GOVOICE_LIVE=1 to allow it, or use -providers offline with the offline caller")
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
func execute(ctx context.Context, srv *stack.Server, cases []Case, refs map[string]agentRef, c config, ask AskFunc) Report {
	rep := Report{Schema: reportSchema, Mode: c.Mode}
	for _, cs := range cases {
		res := CaseResult{Case: cs.Name}
		ok := 0
		for range c.Runs {
			run := runCase(ctx, srv, refs[cs.Agent], cs, c.Opts, ask)
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

func runCase(ctx context.Context, srv *stack.Server, ref agentRef, cs Case, o opts, ask AskFunc) RunResult {
	next := ScriptCaller(cs.Caller.Script)
	if p := cs.Caller.Persona; p != nil {
		next = PersonaCaller(*p, ref.Name, ask)
	}
	obs, err := runOnce(ctx, srv, ref, next, o)
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
	c, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
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
	if err := checkPlan(cases, c.Runs, c.MaxTurns); err != nil {
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
	var ask AskFunc
	if hasPersona(cases) {
		needsLive = needsLive || callerNeedsLive(c.CallerLLM, c.CallerURL)
	}
	if err := liveGate(needsLive, os.Getenv("GOVOICE_LIVE")); err != nil {
		return setup(err)
	}
	if hasPersona(cases) {
		var closeAsk func()
		if ask, closeAsk, err = newAsk(c); err != nil {
			return setup(err)
		}
		defer closeAsk()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rep := execute(ctx, srv, cases, refs, c, ask)
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
