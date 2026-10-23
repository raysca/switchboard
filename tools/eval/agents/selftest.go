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

// selftestConfig is config/development.yaml on the chosen ports with the echo provider switched on.
func selftestConfig(base string, pub, adm int) []byte {
	set := func(src, key, val string) string {
		return regexp.MustCompile(`(?m)^`+key+`:.*$`).ReplaceAllString(src, key+": "+fmt.Sprintf("%q", val))
	}
	y := set(base, "public_listen_addr", fmt.Sprintf("127.0.0.1:%d", pub))
	y = set(y, "admin_listen_addr", fmt.Sprintf("127.0.0.1:%d", adm))
	return []byte(regexp.MustCompile(`(?m)^providers:$`).ReplaceAllString(y, "providers:\n  echo: {mimic: instant, stt: audio}"))
}

func buildBinaries(root, dir string) (string, error) {
	for _, b := range [][2]string{{"server", "./cmd/server"}, {"vadworker", "./cmd/vadworker"}} {
		c := exec.Command("go", "build", "-o", filepath.Join(dir, b[0]), b[1])
		c.Dir = root
		if out, err := c.CombinedOutput(); err != nil {
			return "", fmt.Errorf("go build %s: %v\n%s", b[1], err, out)
		}
	}
	return filepath.Join(dir, "server"), nil
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
type mutation struct {
	agent, caseName, wantFailure string
	edit                         func(cfg map[string]any)
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
}

func selftest(ctx context.Context, stdout, stderr io.Writer) int {
	fail := func(f string, a ...any) int { fmt.Fprintf(stderr, "agent-eval selftest: "+f+"\n", a...); return 1 }
	root, err := os.Getwd()
	if err != nil || !fileExists(filepath.Join(root, "go.work")) {
		return fail("run it from the repository root (make eval-agents-echo)")
	}
	if !fileExists(filepath.Join(root, "third_party", "silero_vad.onnx")) {
		fmt.Fprintln(stdout, "agent-eval selftest: SKIPPED: third_party/silero_vad.onnx missing")
		return 0
	}
	work, err := os.MkdirTemp("", "agent-eval-selftest-")
	if err != nil {
		return fail("%v", err)
	}
	defer os.RemoveAll(work)
	bin, err := buildBinaries(root, work)
	if err != nil {
		return fail("%v", err)
	}
	base, err := os.ReadFile(filepath.Join(root, "config", "development.yaml"))
	if err != nil {
		return fail("%v", err)
	}
	srv, err := stack.Start(ctx, stack.Spec{Root: root, Bin: bin, Dir: filepath.Join(work, "run"), Env: childEnv(os.Environ()),
		Args:   []string{"-vad-mode", "inprocess"},
		Config: func(pub, adm int) []byte { return selftestConfig(string(base), pub, adm) }})
	if err != nil {
		return fail("%v", err)
	}
	defer srv.Stop()

	fx := filepath.Join(root, selftestDir, "agents")
	for _, f := range []struct {
		file, name string
		edit       func(map[string]any)
	}{{"assistant.json", "Eval assistant", nil}, {"desk.json", "Eval desk", nil}} {
		if err := createFixture(srv, filepath.Join(fx, f.file), f.name, f.edit); err != nil {
			return fail("%v", err)
		}
	}
	for _, m := range mutations {
		if m.edit != nil {
			if err := createFixture(srv, filepath.Join(fx, "desk.json"), m.agent, m.edit); err != nil {
				return fail("%v", err)
			}
		}
	}

	cases, err := LoadCases(filepath.Join(root, selftestDir, "echo"), nil)
	if err != nil {
		return fail("%v", err)
	}
	c := config{Mode: modeEcho, Runs: 1, MinPass: 1, Opts: opts{TurnTimeout: 30 * time.Second, Quiet: selftestQuiet}}
	run := func(cs []Case) (Report, error) {
		refs, _, err := prepare(srv, cs, c)
		if err != nil {
			return Report{}, err
		}
		return execute(ctx, srv, cs, refs, c), nil
	}

	good, err := run(cases)
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
		broken = append(broken, mc)
	}
	bad, err := run(broken)
	if err != nil {
		return fail("%v", err)
	}
	if len(bad.Results) != len(mutations) {
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
	fmt.Fprintf(stdout, "agent-eval selftest: %d passed, %d mutations failed as expected\n", passed, failed)
	return 0
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }
