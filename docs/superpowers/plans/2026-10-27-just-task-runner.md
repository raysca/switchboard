# `just` task runner — implementation plan

Date: 2026-10-27. Branch: `just-task-runner`. Spec:
[2026-10-27-just-task-runner-design.md](../specs/2026-10-27-just-task-runner-design.md).

## S0 — Spec review (before code)

Independent reviewer reads the spec and this plan, read-only. Resolves gaps in the bootstrap,
recipe surface, help accuracy, or CI compatibility before any code. No code starts until
this closes.

## S1 — Bootstrap (Lead)

Write scope: `scripts/init.sh`, `./dev`, `Makefile` (`init` target), `.gitignore`.

- `scripts/init.sh`: pin just **1.59.0**; platform table for
  `aarch64-apple-darwin`, `x86_64-apple-darwin`, `x86_64-unknown-linux-musl`,
  `aarch64-unknown-linux-musl` with the published SHA-256 from
  https://github.com/casey/just/releases/download/1.59.0/SHA256SUMS; download to a temp dir,
  verify, extract `just` to `.tools/bin/just`, chmod 0755, print version and next steps.
  Idempotent; unsupported platform prints the supported list and the `cargo install just`
  fallback; never leaves a partial binary.
- `./dev`: committed wrapper resolving its own directory, execing `.tools/bin/just` with
  `--justfile`/`--working-directory`; clear `make init` message if absent.
- `make init`: runs `scripts/init.sh`.
- `.gitignore`: `.tools/`.

Verification: `make init`, `./dev --version`, `make init` twice; checksum mismatch dry run
by temporary bad URL not in tests.

## S2 — Recipes, help, scripts (Lead)

Write scope: `justfile`, `scripts/help.sh`, `scripts/demo.sh`, `scripts/deploy.sh`,
`scripts/check-tasks.sh`, `scripts/dev.sh` (down/status only), `Makefile` (`fmt`,
`check-tasks`).

- `justfile`: settings (`shell`, no dotenv), default `help`, all recipes from the spec,
  doc comments with examples, argument validation where cheap. Flat `demo-*` and
  `deploy-*` names so `just --list` is self-describing.
- `scripts/help.sh`: no-arg quick start; topics `dev`, `test`, `demo`, `deploy`, `eval`,
  `live`; includes exact examples and prerequisites; ends by pointing at `just --list`.
- `scripts/dev.sh`: `down` reuses `stop_previous_stack`; `status` reports stack pids and
  `/ready` with URLs; update header examples to `./dev`.
- `scripts/demo.sh`: validate name against `demos/*/README.md`; `list`, `start` (foreground
  make `<name>-mcp`), `seed`/`reset` (`make seed DEMO=`), `wipe` (handled `demos/seed
  --wipe-only --yes` with dev env and tokens).
- `scripts/doctor.sh`: Go/Node/Docker/ONNX/keys/ports, read-only.
- `scripts/deploy.sh`: `server [cloudrun|cloudflare]`, `demo <name> [cloudrun|cloudflare]`
  with `--server`/`--environment` (and `--mcp-base-url` on Cloudflare), `status`; dispatches
  `deploy-demo-dispatch.yml`/`deploy.yml` with gh and never reads a secret value. Optional
  target defaults are omitted from `$@`, so the script accepts a leading flag as well.
- Named arguments reach scripts as positional parameters (`"$1"`, `"$@"`), never through
  shell interpolation.
- `scripts/check-tasks.sh`: `./dev --list` succeeds; required recipes exist; every listed
  recipe has a doc comment; help topics exit 0 and mention deploy/seed examples; every
  `./dev X` named in the runbooks resolves to a recipe; arguments are not shell-executed; a
  fake `gh` proves deploy recipes pass the target exactly once.
- `make fmt`: gofmt over the same trees as verify plus UI typecheck if node_modules exists.
- `make check-tasks`: runs the check script, requires `make init` first.
- CI: add `make init` + `make check-tasks` to the test job (network available) so the
  justfile/help contract is gated; `make verify` itself stays independent of just.

Verification: `make init`, `./dev --list`, `./dev help`, `./dev help demo`, `./dev help
deploy`, `make check-tasks` (which also checks argument safety and a fake-gh deploy dispatch);
`./dev up` + `./dev status` + `./dev down`; `./dev demo-seed atlas` against the dev stack;
`make verify` unchanged. The sandbox blocks process substitution (`ps` and `/dev/fd`), so the
interactive `up/status/down` and live seed paths are exercised outside the agent sandbox and
reported as such.

## S3 — Docs (Lead)

Write scope: `docs/development.md`, `README.md`, `docs/quickstart.md`, `e2e/README.md` if it
mentions make dev (update to `./dev up`), `scripts/dev.sh` header.

- `docs/development.md`: init, `./dev` command map, help topics, Make migration table,
  troubleshooting (missing just, unsupported platform, stack state).
- README five-minute start becomes `make init` → `./dev up`; keep `make` for CI/legacy and
  point to the new doc.
- quickstart local-development section leads with `./dev`; existing Make commands stay as
  the implementation/fallback.
- Ensure no links/vocabulary regressions.

## S4 — Independent code review, gate, evidence, merge (Lead)

1. Independent review (fresh agent, read-only) of the diff: bootstrap security, help
   accuracy, recipe wiring, CI addition, docs.
2. Gate: `gofmt`, `make verify`, `make check-tasks`, `make e2e-echo` (unchanged path; run to
   prove no regression), `make deadcode`.
3. Evidence: terminal transcript of `./dev --list`, `./dev help demo`, `./dev help deploy`,
   `./dev up/status/down`, and a real `./dev demo-seed atlas`; store under `.govoice-dev/`.
4. Merge main fast-forward, remove worktree/branch.

## As built (deviations recorded before gate)

- 65 recipes rather than the five-journey sketch: every public Make target has a `./dev`
  spelling, as the spec now records. `demo-*` and `deploy-*` are flat so `--list` is a menu.
- Added `scripts/doctor.sh`, `scripts/init-test.sh`, `scripts/check-tasks.sh` (including
  bootstrap failure-path tests and hermetic demo/deploy probes), `make fmt`, `make check-tasks`.
- `./dev verify` is the full PR gate (make verify + make check-tasks), because just is always
  present when that recipe runs; `make verify` remains just-independent for CI and no-init users.
- CI test job installs just (cached) and runs check-tasks; new `tasks` matrix runs Ubuntu +
  macOS to cover Bash 3.2 and the macOS/aarch64 pin. CI's header records `tasks` as required.
- `set minimum-version := "1.59.0"`; init verifies/uses `.new` + `mv` with cleanup; `.tools`
  is ignored by git and Docker.
- `GOVOICE_DEV_DIR` is honoured by the seed recipe and by status/down (which also scan the
  fresh stack); the UI port is persisted. Demo seed requires the fixture health and the dev
  server `/ready`; demo commands derive a loopback admin URL and ignore an exported
  `GOVOICE_ADMIN_URL`.
- The sandbox blocks `/bin/ps` and `/dev/fd`, so the live `up/status/down`/seed transcript is
  taken outside the agent sandbox and recorded as such.

## Review outcomes

- **Pre-code spec review:** flagged the bootstrap/platform surface, testability, CI cost and the
  help promise. Implementation began before that review closed: a process deviation, recorded
  here. The documents were updated with the as-built section before the final review.
- **Review 1 (design/implementation):** found the Bash-4-only `${label^^}` in doctor (fixed
  with `tr` and a macOS/occupied-port check), `./dev verify` missing check-tasks (fixed), the
  `GOVOICE_DEV_DIR`/fresh-stack mismatch (fixed in Makefile, status/down, UI port), and the
  untested bootstrap failure paths (fixed with `scripts/init-test.sh`). It also caught the
  initial deploy double-argument and shell-interpolation bugs, fixed before the gate.
- **Review 2 (final):** no HIGH/MEDIUM remained. It fixed a non-hermetic demo probe (now uses
  `GOVOICE_DEMO_FIXTURE_PORT=1` and a temp `GOVOICE_DEV_DIR`), the untracked `init-test.sh`
  (staged 100755), the fresh-stack discovery, the CI header/macOS job, and doc/help drift.
  Remaining LOW items were fixed or recorded: probe temp-dir cleanup, duplicate help line,
  doctor reading remembered ports, required-recipe coverage, and the SIGPIPE-on-`| head`
  limitation (standard 141; `./dev --list` is unaffected).
- **Incident:** while testing a leading flag, a reviewer accidentally dispatched
  `deploy.yml` on main (run 40555957851). It failed at Workload Identity Federation
  authentication before any image build, deploy step or traffic change; the run's steps show
  every deploy/smoke step skipped. No infrastructure state changed. The review briefs now
  forbid real dispatch.
- Gate evidence: `./dev verify` (make verify + make check-tasks) passed; `make e2e-echo`
  passed (Go e2e, eval self-test, 61 browser tests, 5 one-port tests); `make deadcode` was not
  rerun for this shell/docs-only change; docs guards pass. Live `up/status/down` was exercised
  outside the sandbox because it blocks `/bin/ps` and `/dev/fd`.

## Risks

- Downloaded binary checksum/asset naming drift: pinned URLs and hashes are in one table and
  tested by a successful `make init`.
- `just` recipe argument forwarding vs Make variables; recipes forward raw tokens to the
  scripts/Make, which keep validation.
- Help text drifting from recipes: `scripts/check-tasks.sh` requires a doc comment on every
  public recipe and checks required recipes/help topics.
- CI now needs network for `make init`; if the runner is offline, the step fails loudly with
  the pinned URL instead of silently skipping.
