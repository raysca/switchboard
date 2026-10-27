# Developer commands (`./dev`)

Switchboard's developer front door is `./dev`, a thin wrapper around the pinned
[`just`](https://github.com/casey/just) task runner. The Makefile remains the implementation and
the CI path; `./dev` gives the same tasks memorable names, argument defaults and a help layer.

## One-time setup

```sh
make init
```

`make init` downloads **just 1.59.0** for macOS (Intel/Apple silicon) or Linux
(x86_64/aarch64) into `.tools/bin/just`, verifies the release's published SHA-256 before
installing, and is idempotent. `.tools/` is git-ignored and excluded from Docker build contexts.
There is no `curl | sh`, no sudo and no global install.

Then:

```sh
./dev                 # common commands + the full recipe index
./dev --list          # every task with a one-line description
./dev doctor          # check Go, Node, Docker, ONNX, keys and ports
```

`./dev` works from any directory in the checkout. If you have `.tools/bin` on `PATH`, plain
`just` works too; shell completion is available with `just --completions bash|zsh`.

## Everyday commands

| Command | What it does |
|---|---|
| `./dev up` | Server, test MCP servers and admin UI; seeds the Dev Assistant and Dealership desk agents. Foreground: Ctrl-C stops everything |
| `./dev up-fresh` | Same stack on an empty `.govoice-dev-fresh/` store, for `/onboarding` |
| `./dev status` / `./dev down` | Inspect or stop this checkout's stack |
| `./dev doctor` | Prerequisites, ONNX model, provider keys and ports |
| `./dev admin-link` | Magic sign-in link for the admin UI |
| `./dev example-web` / `example-cli` / `example-go NAME=...` | Browser, CLI and standalone examples |
| `./dev mcp-test` | The test MCP servers on `127.0.0.1:18765` |

## Tests and checks

```sh
./dev test          # build, vet, race and Go e2e (about 3 min)
./dev test-go       # Go end-to-end tests only
./dev test-e2e      # Go e2e + agent-eval self-test + browser suites on echo (free)
./dev test-browser  # browser suites only
./dev test-postgres # Postgres tests (Docker or TEST_POSTGRES_DSN)
./dev verify        # the PR gate
./dev fmt           # gofmt + UI typecheck
```

Paid/live checks are opt-in in the underlying targets: they refuse without `GOVOICE_LIVE=1`.
Use `./dev live-list` and `./dev matrix-plan` to preview a live run.

## Demos

```sh
./dev demo-list                # dental and atlas, with titles and next commands
./dev up                       # terminal 1
./dev demo-start atlas         # terminal 2: the demo MCP fixture (foreground)
./dev demo-seed atlas          # terminal 3: wipe and create the demo
./dev demo-reset dental        # same as demo-seed
./dev demo-wipe dental         # remove it from the stack
```

`demo-seed`/`demo-reset` use the dev stack's management key and write the demo token into the
server's encrypted secret store. `demo-start` uses the dev token defaults
(`dev-dental-token` / `dev-atlas-token`). Full demo docs: [demos/README.md](../demos/README.md).

## Deployments

Two independent pipelines: the server ([deploy.yml](../.github/workflows/deploy.yml)) and the
optional demo pipeline ([deploy-demo.yml](../.github/workflows/deploy-demo.yml)). `./dev` dispatches
them with GitHub CLI; it never reads a secret itself.

```sh
./dev deploy-server cloudrun                  # dispatch deploy.yml on main
./dev deploy-server cloudrun --min-instances 1
./dev deploy-server cloudflare                # local wrangler deploy

./dev deploy-demo atlas cloudrun \
  --server https://switchboard.example --environment demo
./dev deploy-demo dental cloudflare \
  --server https://switchboard.example --environment demo \
  --mcp-base-url https://dental-mcp.example
./dev deploy-status
```

`deploy-demo` needs `gh auth login` and a merged `main`. The GitHub environment named by
`--environment` must hold the demo token secret (`DENTAL_MCP_TOKEN` / `ATLAS_MCP_TOKEN`) and
the management key; the workflow masks both. See [deployment.md](deployment.md) for the full
Cloud Run/Cloudflare setup.

## Help and discovery

```sh
./dev help              # the common journeys
./dev help dev          # local stack, ports, state, stop
./dev help test         # every test layer and what it costs
./dev help demo         # start / seed / reset / wipe, with examples
./dev help deploy       # server vs demo, Cloud Run vs Cloudflare, required flags
./dev help eval         # agent evals
./dev help live         # paid/live/perf checks and their cost gates
./dev <recipe> --help   # sent to the matching runbook
```

Every public recipe has a doc comment, so `./dev --list` is a complete menu. The runbooks live
in [`scripts/help.sh`](../scripts/help.sh); when you add a recipe, add a `#` description above it
and, if it belongs to a journey, an example to the matching runbook. `./dev check-tasks` enforces
the description coverage and the required recipes/help topics (CI runs it after `make init`).

## Migration from Make

There is no flag day. Every old target still works, and the `./dev` recipes call the same
targets/scripts, so task behaviour is unchanged. CI still runs `make verify`; it now also
installs just and runs `make check-tasks` on Linux and macOS.

| Make | `./dev` |
|---|---|
| `make dev` | `./dev up` |
| `make dev-fresh` | `./dev up-fresh` |
| `make test` | `./dev test` |
| `make verify` | `./dev verify` |
| `make e2e-echo` | `./dev test-e2e` |
| `make e2e-fake` | `./dev test-browser` |
| `make seed DEMO=atlas` | `./dev demo-seed atlas` |
| `make dental-mcp` / `atlas-mcp` | `./dev demo-start dental` / `atlas` |
| `make deploy-cloudflare` | `./dev deploy-server cloudflare` |
| `make eval-agents CASES=...` | `./dev eval-agents CASES=...` |

Recipes forward extra arguments to Make, so existing variables keep working:
`./dev eval-agents CASES=demos/dental/eval`, `./dev live-check LIVE_ARGS="-scenarios S1"`.
Positional arguments to `demo-*`/`help` are passed as data, never re-evaluated. A `VAR=value`
argument is Make syntax: as with calling `make` directly, values are trusted Make input, so do not
pass untrusted text through `VAR=value`.

`make` is still the right command for CI, the `MODULES` test loop and rare targets that do not
have a curated alias; `make help` continues to list them. New developer-facing commands should be
added to the justfile with a description, and can later gain implementation in
`scripts/` without changing the public command.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `dev: just is not installed yet` | `make init` |
| `init: checksum mismatch` or a failed download | Delete `.tools/` and retry; a proxy/firewall can block `github.com/casey/just/releases`. The pinned URL and SHA-256 are in `scripts/init.sh` |
| Unsupported platform | The bootstrap supports macOS x86_64/arm64 and Linux x86_64/aarch64. Install `just` 1.59.0 with `cargo install just --version 1.59.0` and place the binary at `.tools/bin/just`, or open an issue with your platform |
| `./dev up` says a named port is held | It names the process. Pick another port, e.g. `PUBLIC_PORT=18080 ./dev up`; default ports move to the next free one automatically |
| `dev: not running` from `./dev status` | `./dev up`; state lives in `.govoice-dev/` and survives a restart |
| `deploy-demo` says `gh is not authenticated` | `gh auth login`; deployments run from `main`, so merge first |
| Paid target refuses | Set `GOVOICE_LIVE=1` explicitly; preview with `./dev live-list` / `./dev matrix-plan` |
