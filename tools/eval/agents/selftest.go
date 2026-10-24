package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"govoice/tools/internal/stack"
)

// The self-test proves the harness on the free echo provider: the plumbing cases must pass, and
// each deliberately broken agent must FAIL its case, for the stated reason. A harness that
// passes a broken agent is the failure it exists to catch.

const (
	selftestDir   = "tools/eval/agents/testdata"
	selftestQuiet = 700 * time.Millisecond
)

// childEnvAllow is everything the server child inherits: no provider keys, tokens or store DSN.
var childEnvAllow = []string{"PATH", "HOME", "TMPDIR", "USER", "LANG", "TZ",
	"DYLD_LIBRARY_PATH", "LD_LIBRARY_PATH", "ORT_DISABLE_TELEMETRY", "GOVOICE_ORT_LIB", "GOVOICE_MODEL_PATH"}

func childEnv(environ []string) (env []string) {
	for _, e := range environ {
		if k, _, _ := strings.Cut(e, "="); slices.Contains(childEnvAllow, k) {
			env = append(env, e)
		}
	}
	return env
}

// selftestConfig is config/development.yaml on the chosen ports with the echo provider switched on
// (echo is extra options for its inline map, e.g. ", llm_extra_delay: 1500ms").
func selftestConfig(base string, pub, adm int, echo string) []byte {
	set := func(src, key, val string) string {
		return regexp.MustCompile(`(?m)^`+key+`:.*$`).ReplaceAllString(src, key+": "+fmt.Sprintf("%q", val))
	}
	y := set(base, "public_listen_addr", fmt.Sprintf("127.0.0.1:%d", pub))
	y = set(y, "admin_listen_addr", fmt.Sprintf("127.0.0.1:%d", adm))
	return []byte(regexp.MustCompile(`(?m)^providers:$`).ReplaceAllString(y, "providers:\n  echo: {mimic: instant, stt: audio"+echo+"}"))
}

// buildServer builds the server (the self-test runs the VAD in process: no worker binary).
func buildServer(root, dir string) (string, error) {
	bin := filepath.Join(dir, "server")
	c := exec.Command("go", "build", "-o", bin, "./cmd/server")
	c.Dir = root
	if out, err := c.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build ./cmd/server: %v\n%s", err, out)
	}
	return bin, nil
}

// startServer runs the built server on echo in its own data dir under work. On a failed start it
// prints the tail of the server log first, because the temp dir goes away with the run.
func startServer(ctx context.Context, stderr io.Writer, root, bin, work, name, echoOpts string) (*stack.Server, error) {
	base, err := os.ReadFile(filepath.Join(root, "config", "development.yaml"))
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(work, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	srv, err := stack.Start(ctx, stack.Spec{Root: root, Bin: bin, Dir: dir, Env: childEnv(os.Environ()),
		Args:   []string{"-vad-mode", "inprocess"},
		Config: func(pub, adm int) []byte { return selftestConfig(string(base), pub, adm, echoOpts) }})
	if err != nil {
		if log, rerr := os.ReadFile(filepath.Join(dir, "server.log")); rerr == nil {
			lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
			fmt.Fprintf(stderr, "agent-eval selftest: server log (last lines):\n%s\n", strings.Join(lines[max(0, len(lines)-30):], "\n"))
		}
		return nil, err
	}
	return srv, nil
}

// createFixture creates the fixture agent over the API, as the named variant of its stored config.
func createFixture(srv *stack.Server, file, name string, edit func(cfg map[string]any)) error {
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	req["name"] = name
	if edit != nil {
		edit(req["config"].(map[string]any))
	}
	if _, err := srv.Call("POST", "agents", req); err != nil {
		return fmt.Errorf("create agent %q: %w", name, err)
	}
	return nil
}

// mutation is a broken agent and the case it must fail, with text every failure line set must contain.
// edit changes the stored fixture (a new agent named agent); hand instead changes the rewritten draft
// of the existing agent, for faults the text-only rewrite would otherwise overwrite.
type mutation struct {
	agent, caseName, wantFailure string
	edit, hand                   func(cfg map[string]any)
	tweak                        func(c *Case)
}

func steps(cfg map[string]any) []any { return cfg["steps"].([]any) }

var mutations = []mutation{
	{agent: "Eval desk (no handoffs)", caseName: "echo-desk-handoff", wantFailure: "path:",
		edit: func(cfg map[string]any) { cfg["handoffs"] = []any{} }},
	{agent: "Eval desk (no end_call)", caseName: "echo-desk-end-call", wantFailure: "end_reason:",
		edit: func(cfg map[string]any) { delete(steps(cfg)[1].(map[string]any), "end_call") }},
	// The assistant does say "You said": a case that forbids it must fail, which shows never_say reads real replies.
	{agent: "Eval assistant", caseName: "echo-assistant-replies", wantFailure: "never_say:",
		tweak: func(c *Case) { c.Expect.NeverSay = []string{"(?i)you said"} }},
	// The rewrite turns every voice into silence, so a step that cannot be built is put in the draft by hand:
	// the sales step then fails to start, which the always-on handoff check must report.
	{agent: "Eval desk", caseName: "echo-desk-handoff", wantFailure: "handoff:",
		hand: func(cfg map[string]any) {
			steps(cfg)[1].(map[string]any)["config"].(map[string]any)["tts"] = map[string]any{"provider": "echo", "voice": "brokenvoice"}
		}},
}

// handEdit applies a mutation's draft change to the resolved agent.
func handEdit(ref agentRef, edit func(cfg map[string]any)) (agentRef, error) {
	var cfg map[string]any
	if err := json.Unmarshal(ref.Draft, &cfg); err != nil {
		return ref, err
	}
	edit(cfg)
	d, err := json.Marshal(cfg)
	ref.Draft = d
	return ref, err
}

// requireSilero skips (exit 0) without the VAD model, except in CI, where a skipped safety net is a failure.
func requireSilero(root string, stdout, stderr io.Writer) (ok bool, rc int) {
	if fileExists(filepath.Join(root, "third_party", "silero_vad.onnx")) {
		return true, 0
	}
	if os.Getenv("CI") != "" {
		fmt.Fprintln(stderr, "agent-eval selftest: FAILED: third_party/silero_vad.onnx missing in CI")
		return false, 1
	}
	fmt.Fprintln(stdout, "agent-eval selftest: SKIPPED: third_party/silero_vad.onnx missing, the harness was NOT checked")
	return false, 0
}

func selftest(ctx context.Context, stdout, stderr io.Writer) int {
	fail := func(f string, a ...any) int { fmt.Fprintf(stderr, "agent-eval selftest: "+f+"\n", a...); return 1 }
	root, err := os.Getwd()
	if err != nil || !fileExists(filepath.Join(root, "go.work")) {
		return fail("run it from the repository root (make eval-agents-echo)")
	}
	if ok, rc := requireSilero(root, stdout, stderr); !ok {
		return rc
	}
	work, err := os.MkdirTemp("", "agent-eval-selftest-")
	if err != nil {
		return fail("%v", err)
	}
	defer os.RemoveAll(work)
	bin, err := buildServer(root, work)
	if err != nil {
		return fail("%v", err)
	}
	srv, err := startServer(ctx, stderr, root, bin, work, "run", "")
	if err != nil {
		return fail("%v", err)
	}
	defer srv.Stop()

	fx := filepath.Join(root, selftestDir, "agents")
	assistant, desk := filepath.Join(fx, "assistant.json"), filepath.Join(fx, "desk.json")
	if err := createFixture(srv, assistant, "Eval assistant", nil); err != nil {
		return fail("%v", err)
	}
	if err := createFixture(srv, desk, "Eval desk", nil); err != nil {
		return fail("%v", err)
	}
	for _, m := range mutations {
		if m.edit != nil {
			if err := createFixture(srv, desk, m.agent, m.edit); err != nil {
				return fail("%v", err)
			}
		}
	}

	cases, err := LoadCases(filepath.Join(root, selftestDir, "echo"), nil)
	if err != nil {
		return fail("%v", err)
	}
	c := config{Mode: modeEcho, Runs: 1, MinPass: 1, Opts: opts{TurnTimeout: 30 * time.Second, Quiet: selftestQuiet}}
	run := func(srv *stack.Server, cs []Case, hand map[string]func(map[string]any), ask AskFunc) (Report, error) {
		refs, _, err := prepare(srv, cs, c)
		if err != nil {
			return Report{}, err
		}
		for agent, edit := range hand {
			if refs[agent], err = handEdit(refs[agent], edit); err != nil {
				return Report{}, err
			}
		}
		return execute(ctx, srv, cs, refs, c, ask), nil
	}

	good, err := run(srv, cases, nil, nil)
	if err != nil {
		return fail("%v", err)
	}
	passed := 0
	for _, r := range good.Results {
		if r.Passed {
			passed++
		}
	}
	if passed != len(cases) || len(good.Results) != len(cases) {
		WriteText(stderr, good)
		return fail("%d of %d plumbing cases passed", passed, len(cases))
	}

	var broken []Case
	hand := map[string]func(map[string]any){}
	for _, m := range mutations {
		i := slices.IndexFunc(cases, func(c Case) bool { return c.Name == m.caseName })
		if i < 0 {
			return fail("mutation %q names no case %q", m.agent, m.caseName)
		}
		mc := cases[i]
		mc.Agent = m.agent
		if m.tweak != nil {
			m.tweak(&mc)
		}
		if m.hand != nil {
			hand[m.agent] = m.hand
		}
		broken = append(broken, mc)
	}
	bad, err := run(srv, broken, hand, nil)
	if err != nil {
		return fail("%v", err)
	}
	if len(bad.Results) != len(mutations) {
		if ctx.Err() != nil {
			return fail("interrupted")
		}
		return fail("the mutation run stopped early")
	}
	failed := 0
	for i, r := range bad.Results {
		m := mutations[i]
		switch {
		case r.Passed:
			fmt.Fprintf(stderr, "agent-eval selftest: BROKEN AGENT PASSED: %q still passes case %s\n", m.agent, m.caseName)
		case len(r.Runs) == 0 || len(r.Runs[0].Errors) > 0:
			fmt.Fprintf(stderr, "agent-eval selftest: %q failed %s with a harness error, not an assertion\n", m.agent, m.caseName)
		case !slices.ContainsFunc(r.Runs[0].Failures, func(f string) bool { return strings.Contains(f, m.wantFailure) }):
			fmt.Fprintf(stderr, "agent-eval selftest: %q failed %s, but not on %q: %v\n", m.agent, m.caseName, m.wantFailure, r.Runs[0].Failures)
		default:
			failed++
		}
	}
	if failed != len(mutations) {
		WriteText(stderr, bad)
		return fail("%d of %d broken agents failed as expected", failed, len(mutations))
	}

	// The persona caller on echo, with a scripted persona model (no Ollama): one that asks for the
	// handoff and then says DONE must pass; one that never gets to the point must fail the same case.
	pc := Case{Name: "echo-persona-handoff", Agent: "Eval desk",
		Caller: Caller{Persona: &Persona{Who: "a caller", Goal: "reach sales", MaxTurns: 3}},
		Expect: Expect{Path: []string{"front", "sales"}, ToolsCalled: []string{"transfer_to_sales"}}}
	goal := scriptedAsk("please transfer to sales", "DONE")
	if rep, err := run(srv, []Case{pc}, nil, goal); err != nil || len(rep.Results) != 1 || !rep.Results[0].Passed {
		WriteText(stderr, rep)
		return fail("the persona caller did not reach its goal on echo: %v", err)
	}
	if rep, err := run(srv, []Case{pc}, nil, scriptedAsk("hello there")); err != nil || len(rep.Results) != 1 || rep.Results[0].Passed ||
		len(rep.Results[0].Runs[0].Errors) > 0 || rep.Results[0].Runs[0].TurnCount < 3 {
		WriteText(stderr, rep)
		return fail("a persona that never reaches its goal was not failed on an assertion within max_turns: %v", err)
	}

	// A slow model: the reply pieces come 1.5 s apart, longer than the quiet window, and the call must
	// still be read as one reply (a cut turn would be a harness error, never a verdict on the agent).
	slow, err := startServer(ctx, stderr, root, bin, work, "slow", ", llm_extra_delay: 1500ms")
	if err != nil {
		return fail("%v", err)
	}
	defer slow.Stop()
	if err := createFixture(slow, assistant, "Eval assistant", nil); err != nil {
		return fail("%v", err)
	}
	var replies []Case
	for _, cs := range cases {
		if cs.Name == "echo-assistant-replies" {
			replies = append(replies, cs)
		}
	}
	slowRep, err := run(slow, replies, nil, nil)
	if err != nil {
		return fail("%v", err)
	}
	if len(slowRep.Results) != 1 || !slowRep.Results[0].Passed {
		if ctx.Err() != nil {
			return fail("interrupted")
		}
		WriteText(stderr, slowRep)
		return fail("a slow model was not read as one reply per turn")
	}

	// The command-line path against this server, as an operator runs it: key from the environment,
	// an explicit published version, a JSON report.
	os.Setenv("GOVOICE_MANAGEMENT_API_KEY", srv.Key)
	defer os.Unsetenv("GOVOICE_MANAGEMENT_API_KEY")
	report := filepath.Join(work, "report.json")
	args := []string{"-cases", filepath.Join(root, selftestDir, "echo"), "-case", "echo-desk-handoff", "-providers", modeEcho, "-version", "1",
		"-quiet", selftestQuiet.String(), "-admin", srv.Admin, "-public", srv.Public, "-json", report}
	if rc := realMain(args, io.Discard, stderr); rc != 0 {
		return fail("the command-line path exited %d", rc)
	}
	if fi, err := os.Stat(report); err != nil || fi.Mode().Perm() != reportFileMode {
		return fail("the JSON report is missing or not mode 0600: %v", err)
	}
	fmt.Fprintf(stdout, "agent-eval selftest: %d passed, %d mutations failed as expected, persona caller driven, goal-less persona failed, slow model read as one reply\n", passed, failed)
	return 0
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// scriptedAsk is a persona model that replays replies, repeating the last one.
func scriptedAsk(replies ...string) AskFunc {
	n := 0
	return func(context.Context, string, string) (string, error) {
		n++
		return replies[min(n, len(replies))-1], nil
	}
}
