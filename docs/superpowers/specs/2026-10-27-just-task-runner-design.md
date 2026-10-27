# `just` developer task runner — design

Status: draft for review. Date: 2026-10-27. Branch: `just-task-runner`.

## Goal

Give developers one memorable command surface (`./dev …`) backed by `just`, with every
task discoverable through `just --list` and topic runbooks, without breaking the existing
Makefile or CI. `make init` downloads a pinned `just` binary; the rest of the Makefile
remains the implementation until tasks migrate into scripts.

Non-goal for this change: deleting the Makefile or rewriting every recipe. This is the
discoverable front door; migration of individual recipes can follow.

## Decisions

### D1 — `./dev` wrapper + pinned `just`

- `make init` runs `scripts/init.sh`, which downloads **just 1.59.0** from the pinned
  GitHub release into `.tools/bin/just`, verifies the published SHA-256 for the platform,
  and refuses anything else. Supported: macOS x86_64/arm64, Linux x86_64/aarch64 (musl).
  It is idempotent and never `curl | sh`.
- `./dev` is a committed Bash wrapper that execs
  `.tools/bin/just --justfile <root>/justfile --working-directory <root> "$@"`. It works
  from any subdirectory; if the binary is missing it prints `run: make init`.
- `.tools/` is git-ignored.
- `just` itself (with `.tools/bin` on PATH) is an alternative, but `./dev` is the documented
  command.

### D2 — Recipe surface

Recipes are thin wrappers over the existing Makefile targets and scripts, so behaviour does
not move in this change. Every recipe has a `#` doc comment with an example; `just --list`
is the index.

```
./dev                       # help: five common journeys + recipe list
./dev help [topic]          # runbooks: dev | test | demo | deploy | eval | live
./dev up                    # server + UI (foreground; Ctrl-C stops)
./dev up-fresh              # empty store, onboarding
./dev down                  # stop this checkout's dev stack
./dev status                # is the dev stack up? ports/URLs
./dev doctor                # prerequisites, ONNX runtime, ports, keys

./dev test                  # build + vet + race modules and the Go e2e (make test)
./dev test-go               # Go e2e
./dev test-e2e              # Go e2e + eval self-test + browser suites (make e2e-echo)
./dev test-postgres         # Postgres tests
./dev verify                # PR gate (make verify)
./dev fmt                   # gofmt + UI typecheck (new make fmt)

./dev demo-list             # the demos and their commands
./dev demo-start <name>     # run the demo MCP fixture (foreground)
./dev demo-seed <name>      # wipe + create the demo in the dev stack
./dev demo-reset <name>     # alias of seed, named for intent
./dev demo-wipe <name>      # archive the demo agent and delete its MCP server/secret (call history stays)

./dev deploy-server <target>        # cloudrun | cloudflare
./dev deploy-demo <name> [target]   # dispatch the demo pipeline; needs --server and --environment
./dev deploy-status                 # recent workflow runs (gh)

./dev eval-agents [args…]   # make eval-agents with forwarded variables
./dev live-list|live-check|matrix-plan|live-selftest
./dev bench-latency|loadtest-smoke|perf-gate
./dev offline-up|offline-down|offline-test
./dev admin-link|example-web|example-cli|example-go
```

The recipe list also maps every existing public Make target that is not in the table above
(`test-browser`, `test-oneport`, `check-tasks`, `deadcode`, `agent-matrix`, `provider-*`,
`offline-*`, and so on) so "everything" has a `./dev` spelling, not only the curated journeys.

Dependency/prerequisite failures and paid gates stay in the scripts/Make targets
(`GOVOICE_LIVE=1`, `DEMO=`, etc.); recipes forward arguments unchanged where needed. Named
arguments are passed as shell positional parameters (`"$1"`, `"$@"`), never interpolated into
the recipe body, so a demo name or help topic is data and cannot become code.

### D3 — Help is a first-class layer

- `just --list` shows every public recipe with its doc comment (the Makefile itself remains
  documented by `make help`, for CI/legacy users). 1.59.0 shows only the last line of a
  multi-line doc comment, so public recipes use one-line comments; examples live in the
  runbooks.
- `./dev help` prints a curated quick start; `./dev help <topic>` prints the runbook for
  that area. The runbooks live in `scripts/help.sh`, so they are copy-pasteable and cover
  prerequisites and the server-vs-demo distinction.
- `./dev help deploy` includes Cloud Run/Cloudflare and auto-seed requirements; `help demo`
  includes start/seed/reset/wipe examples; `help dev` explains ports, state and stop.
- Shell completion: document `just --completions bash|zsh`; `./dev` is a transparent
  wrapper, so completion works once `.tools/bin/just` is used directly.

### D4 — Additions to existing scripts

- `scripts/dev.sh` gains `down` (reuse `stop_previous_stack`) and `status` (stack pids +
  `/ready`), so `./dev down/status` do not shell out to ad-hoc ps/curl.
- `scripts/demo.sh` provides `list|start|seed|reset|wipe <name>` using the existing Make
  targets and `demos/seed`; it validates names against `demos/*/README.md`.
- `scripts/doctor.sh` checks the toolchain, ONNX model, provider config and ports.
- `scripts/deploy.sh` dispatches: `server cloudrun` (`gh workflow run deploy.yml`),
  `server cloudflare` (`make deploy-cloudflare`), `demo <name> <target>` (`gh workflow run
  deploy-demo-dispatch.yml -f demo=<name> -f target=<target> …`), `status`. It prints the
  exact GitHub command when `gh` is unavailable and never invents credentials.
- `Makefile` gains `init` and `fmt` targets; a new `check-tasks` target runs
  `scripts/check-tasks.sh` (justfile parses, required recipes and help topics exist, every
  public recipe is documented, wrapper bodies reach real make targets/scripts, the pinned
  version is exact, the no-runner path fails, demo arguments are not executed, and the demo
  seed refuses an absent fixture) and `scripts/init-test.sh` (bootstrap failure paths).
  `make verify` does not require `just`; `./dev verify` runs both gates; CI runs
  `make init` + `make check-tasks` in the test job and a Linux/macOS `tasks` matrix.

### D5 — Docs

- New `docs/development.md`: init, command map, help topics, migration table from Make,
  troubleshooting, and the rule that CI/legacy still uses Make.
- README quick start and `docs/quickstart.md` lead with `make init` + `./dev up`; the
  existing Make instructions remain for CI and rare targets.
- `scripts/dev.sh` header comments switch examples from `make dev` to `./dev up`.

## Failure paths

- `./dev` before `make init`: clear message, exit 2.
- Download/checksum failure: no binary installed, actionable error; existing binary left
  untouched.
- Unsupported OS/arch: name the supported platforms and the manual `just` install fallback.
- Unknown demo/test topic/target: list valid values, exit 2.
- Dev stack absent for seed/status: `status` exits 1 and `scripts/demo.sh` fails with the
  command to start it. `demo-seed` also refuses when the demo MCP fixture is unreachable, so a
  failed seed cannot wipe a working demo first. `demo-wipe`/`demo-seed` always target the
  loopback dev server, never an exported production admin URL.
- Paid/live commands are still gated by the existing Make targets/scripts.

## Security

- Pinned version and SHA-256 per platform; HTTPS only; no execution of downloaded scripts
  before verification.
- `.tools/` is local, git-ignored, not in Docker contexts.
- Help/deploy scripts never print management keys or demo tokens; they reference env var
  names only.

## Non-goals

- Removing Make targets, rewriting CI to `./dev`, or moving all recipe bodies into scripts.
- Replacing the Go `dev` CLI idea; the wrapper/recipe names are chosen so a later CLI can
  keep the same public commands.

## Acceptance

- `make init` installs just 1.59.0 and `./dev --list` works on this machine.
- `./dev help`, `help demo`, `help deploy` print useful runbooks; every recipe listed has a
  description.
- `./dev up` starts the stack; `./dev down` stops it; `./dev status` reports it.
- `./dev demo-seed atlas` seeds the demo against a running dev stack.
- `./dev test`/`verify` map to the existing gates; no behaviour change.
- `make check-tasks` passes; `make verify` (without just) still passes; docs links and
  vocabulary guards pass.
- Screenshots/terminal transcript of `./dev --list`, `./dev help demo` and a real
  `./dev demo-seed atlas` run; full AGENTS.md gate.

## Self-review

- Bootstrap works on the four supported platform/arch combinations; checksums correct; idempotent.
- Help is accurate against the recipes and scripts; no stale `make dev` instructions.
- Wrapper quoting/paths; works from subdirectories; `just --list` includes all recipes.
- Existing Makefile/CI unaffected until intentionally migrated.
