// provtest runs the stage-level provider conformance suite (tools/provtest).
//
//	GOVOICE_LIVE=1 make provider-matrix PROVIDERS=google,nvidia STAGES=stt,tts,llm
//
// Live runs spend real provider credit (about 17 requests per provider with
// all three stages) and need examples/.env. Secrets are read into the process
// environment only and never printed; all output is redacted.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"govoice/examples/envload"
	"govoice/tools/provtest"
)

func main() { os.Exit(run()) }

func run() int {
	var (
		providers = flag.String("providers", "", "comma-separated provider ids (default: all)")
		stages    = flag.String("stages", "", "comma-separated stages stt,tts,llm (default: all)")
		maxCalls  = flag.Int("max-calls", 40, "hard cap: stop issuing units once the planned units or the MEASURED provider requests reach this")
		repeat    = flag.Int("repeat", 1, "run each suite N times (failures stick, numbers are medians)")
		dry       = flag.Bool("dry", false, "run against in-process fakes (no network, no credentials)")
		out       = flag.String("out", "provtest-results.json", "results.json path ('' = none)")
		writeDocs = flag.Bool("write-docs", false, "write docs/benchmarks/provider-matrix.md")
		merge     = flag.String("merge", "", "render saved results.json files (comma-separated, later wins) instead of running: no provider calls")
		genClips  = flag.Bool("gen-clips", false, "synthesize the STT fixtures with Google TTS (3 requests) into tools/provtest/testdata/clips")
		listEnv   = flag.Bool("list-env", false, "print each provider row's env names and whether it would run (no calls, no values)")
	)
	flag.Parse()
	if *listEnv {
		if root := os.Getenv("GOVOICE_ROOT"); root != "" {
			_ = os.Chdir(root)
		}
		_ = envload.Load()
		fmt.Print(provtest.EnvMarkdown(provtest.Table, os.Getenv))
		return 0
	}
	if !*dry && *merge == "" && os.Getenv("GOVOICE_LIVE") != "1" {
		fmt.Fprintln(os.Stderr, "provtest spends real provider credit: run GOVOICE_LIVE=1 make provider-matrix (or -dry for the fakes)")
		return 2
	}
	if root := os.Getenv("GOVOICE_ROOT"); root != "" {
		_ = os.Chdir(root)
	}
	_ = envload.Load()
	provtest.RegisterEnvSecrets(os.Environ())

	var ids []string
	for _, s := range strings.Split(*providers, ",") {
		if s = strings.TrimSpace(s); s != "" {
			ids = append(ids, s)
		}
	}
	var sts []provtest.Stage
	for _, s := range strings.Split(*stages, ",") {
		if s = strings.TrimSpace(s); s != "" {
			sts = append(sts, provtest.Stage(s))
		}
	}
	if *dry {
		*maxCalls = 0 // fakes spend nothing: the paid-call cap must not skip dry rows
	}
	budget := provtest.NewBudget(*maxCalls)

	if *genClips {
		google := provtest.Provider{}
		for _, p := range provtest.Table {
			if p.ID == "google" && p.Stage == provtest.TTS {
				google = p
			}
		}
		t, why := provtest.Live(google, os.Getenv)
		if why != "" {
			fmt.Fprintln(os.Stderr, "gen-clips: google tts", why)
			return 1
		}
		if err := provtest.GenClips(t, "tools/provtest/testdata/clips", budget); err != nil {
			fmt.Fprintln(os.Stderr, "gen-clips:", provtest.Redact(err.Error()))
			return 1
		}
		fmt.Println("wrote tools/provtest/testdata/clips (", budget.Used(), "requests )")
		return 0
	}

	if *merge != "" {
		rep, err := provtest.MergeFiles(strings.Split(*merge, ","), provtest.Table, os.Getenv)
		if err != nil {
			fmt.Fprintln(os.Stderr, "merge:", err)
			return 1
		}
		return emit(rep, *out, *writeDocs)
	}
	clips, err := provtest.LoadClips()
	if err != nil {
		fmt.Fprintln(os.Stderr, "clips:", err)
		return 1
	}
	o := provtest.Options{Clips: clips, Budget: budget}
	selected := provtest.Filter(provtest.Table, ids, sts)
	inSel := map[string]bool{}
	for _, p := range selected {
		inSel[p.ID+"."+string(p.Stage)] = true
	}
	resolve := func(p provtest.Provider) (provtest.Target, string) {
		if !inSel[p.ID+"."+string(p.Stage)] {
			return provtest.Target{}, "not selected"
		}
		if *dry {
			return provtest.FakeTarget(p.Stage, p.ID, clips, provtest.FaultNone), ""
		}
		return provtest.Live(p, os.Getenv)
	}
	mode := "live"
	if *dry {
		mode, o.Fast = "dry", true
	}
	rep := provtest.Run(provtest.Table, resolve, o, *repeat, mode, os.Stderr)
	return emit(rep, *out, *writeDocs)
}

// emit prints the matrix, writes results.json and (optionally) the docs page; exit 1 if any check failed.
func emit(rep provtest.Report, out string, writeDocs bool) int {
	md := rep.Markdown()
	fmt.Println(md)
	if out != "" {
		if err := rep.WriteJSON(out); err != nil {
			fmt.Fprintln(os.Stderr, "results:", err)
			return 1
		}
	}
	if writeDocs {
		if err := os.MkdirAll("docs/benchmarks", 0o755); err != nil {
			return 1
		}
		doc := "# Provider conformance matrix\n\nStage-level conformance suite (`tools/provtest`): the same checks run against every STT, TTS and LLM provider, built through the server's own registry. Add a provider by adding one row to `Table` in `tools/provtest/providers.go`. Providers without credentials are skipped, not failed.\n\nReproduce: `GOVOICE_LIVE=1 make provider-matrix PROVIDERS=google,nvidia STAGES=stt,tts,llm` (needs `examples/.env`; spends about 17 provider requests per provider; `-write-docs` rewrites this file). Without credentials the logic is unit-tested against fakes: `go test ./tools/provtest/`. `-merge a.json,b.json` re-renders saved results without calls. xAI STT/TTS compile only with `make provider-matrix TAGS=xaispeech`.\n\n" + provtest.Legend + "## Environment\n\nValues come from `examples/.env` and are never printed; `go run ./tools/provtest/cmd/provtest -list-env` shows which rows would run. Planned units are an upper-bound estimate; the actual column is measured at the provider boundary (retries included). xai.llm uses the server default endpoint (`providers.xai_llm.endpoint`, the Responses API that tools need); `XAI_ENDPOINT` is an examples/ override production never reads and is ignored.\n\n" + provtest.EnvMarkdown(provtest.Table, nil) + "\n" + md
		if err := os.WriteFile(filepath.Join("docs/benchmarks", "provider-matrix.md"), []byte(doc), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	for _, r := range rep.Results {
		if r.Status == provtest.Fail {
			return 1
		}
	}
	return 0
}
