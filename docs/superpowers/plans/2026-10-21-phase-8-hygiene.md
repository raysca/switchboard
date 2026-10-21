# Phase 8: Repo hygiene and documentation (Implementation Plan)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. One task = one reviewable deliverable (one or a few conventional commits). Order: 8.0 → 8.1 → … → 8.7, strictly sequential (8.2/8.3 both edit `Makefile` and the Dockerfiles; 8.4 moves this file; 8.5/8.6 edit the same docs).

**Goal:** Delete orphaned files, fix every stale reference and every doc claim that Phases 1–7b made false, reorganise `docs/superpowers/` so a newcomer can navigate it (current remediation records in one folder, older feature designs in `docs/history/`), tidy module files where it is safe, and give the parity matrix the planned/parked vocabulary. No product behaviour changes.

**Architecture:** Pure deletion, moves (`git mv`, history preserved), path rewrites and doc edits. Every deletion is checked against Make targets, CI workflows, Dockerfiles, `go.work`, tests and docs first (Verified facts). One local reference checker (`check-links.mjs`, ledger-only) runs in every gate and fails on a dangling relative link, a missing backticked repo path, or any remaining reference to a moved path.

**Tech stack:** Go 1.27.1 workspace (`go.work`: `.`, `core`, `offline`, `offline/contract`, `providers/{cartesia,deepgram,echo,elevenlabs,google,nvidia,openai,xai}`, `server`), React/TS `ui/`, Playwright `e2e/`, GitHub Actions.

**Spec:** master plan `docs/superpowers/plans/2026-10-15-review-remediation.md` Phase 8 (Tasks 8.1–8.4, D-0.1, D-0.7) and review `docs/superpowers/specs/2026-10-15-deep-review.md` §C "tools / repo". Carried items from the ledger `p5/p6/p7/p7b/pr-notes.md`. Mapping: master 8.1 → 8.1; 8.2 → 8.2 + 8.3 + 8.5; 8.3 → 8.4 (extended by the user to a full `docs/superpowers` cleanup); 8.4 → 8.6; gate → 8.7. (After Task 8.4 these paths are `docs/superpowers/remediation/{plans,specs}/…`.)

**Leave alone:** the untracked `IDEA.md` at the repo root is the user's file: never touch, move, stage or delete it (`git add -A` is forbidden in this phase; stage explicit paths).

---

## Decisions for the user (ANSWERED 2026-10-21: "use your recommendations" for every item; U3 added by the user)

| # | Question | Decision |
|---|---|---|
| U1 | **DB columns left unused by Phase 5.4a** (`turns.tools_json`, `tool_calls.arguments_summary`, `tool_calls.result_summary`; inserts write the literals `'[]'`, `''`, `''` at `server/storage/history.go:240,574`). Dropping them needs migration 008 in both dialects, edits to two migration-version tests (`server/storage/storage_test.go:576,581,671,677`) and the privacy pin (`server/tool_records_e2e_test.go:61`), and makes a rollback to an older binary require `goose down` first. | **Leave them** (no migration). They cost three constant values per row and no code reads them; the privacy pin keeps proving they stay empty. Recorded as KEEP in the PR notes. |
| U2 | **Settings audit R8 (LLM temperature / max output tokens per step)** is headed "Phase 8" in `2026-10-19-settings-coverage-audit.md:302`. It is a feature (five provider adapters, a catalogue flag, failover inheritance), not hygiene. | **Not in Phase 8.** Task 8.5 changes the heading to "own phase, not scheduled"; carried in the PR notes. |
| U3 | **Clean up `docs/superpowers/` beyond D-0.7** (user request): index, current vs history, one folder for the remediation records, no dangling references. | **Do it** as Task 8.4 (layout and rules below). No file is deleted: none is empty, duplicated or a placeholder (Verified fact 14). |

## Rulings (mine; recorded in the ledger; the reviewer may challenge them)

- **R1 Delete only what nothing runs.** A file referenced by a Make target, CI job, Dockerfile, test or current doc is KEEP, or is rewired in the same commit. KEEP list with reasons is in Task 8.1.
- **R2 No blanket `go mod tidy`.** It cannot run at the root (root packages import `govoice/providers/nvidia`, which only `go.work` resolves) nor in `core` (no `require` at all). `go work sync` rewrites 7 files (+188/−140) and changes the build list (prunes test-only modules, downgrades `stretchr/objx` 0.5.3 → 0.1.0). Instead: root `go.mod` hand edit (drop the unused `providers/echo` require + replace, move `golang.org/x/time` to the direct block) and `core/go.mod` gains its one real requirement (`google.golang.org/protobuf v1.36.12`, the version the workspace already selects) so `core` builds standalone, as AGENTS.md demands of a library. Both leave `go list -m all` byte-identical (Verified fact 9). `providers/google` / `offline/contract` tidy diffs are cosmetic (direct/indirect markers) and are skipped.
- **R3 `go.work.sum` is not pruned.** The toolchain appends to it; a prune must be re-derived for every tag set and `go test` resolution, for no runtime value.
- **R4 Vendored ONNX Runtime headers go.** `third_party/onnxruntime-osx-arm64-1.29.0/` (31 tracked files, 30,184 lines: headers, cmake, licence copies) is never compiled: `github.com/yalue/onnxruntime_go` loads the dylib with dlopen and carries its own header; no `#cgo`/`#include` exists in the repo; `scripts/fetch-ort.sh fetch` copies only `lib/` and recreates the directory. `.gitignore` gets `third_party/onnxruntime-*/` (the dylib is already ignored via `*.dylib`; `.dockerignore` already excludes the tree). The two models stay.
- **R5 `offline/contract` folds into the server module** as `server/offlinecontract/` (package `offlinecontract`, same build tags). `offline/adapter` is the wrong home: the `offline` module must stay free of `govoice/*` deps (it builds the adapter image alone). `server/go.mod` already requires `govoice/offline`, `providers/xai`, `core` and `coder/websocket`.
- **R6 Matrix vocabulary: two new statuses, no new column.** `planned (hook exists)` (Go hook present, not wired; Default `library only`) and `parked` (kept for a documented deliverable). **`superseded` is not added:** every review-§G2 remnant was deleted in Phase 5 and none is a matrix row; a status with no row is dead vocabulary. `TestParityMatrix`'s `valid` map gains exactly the two strings.
- **R7 docs/superpowers rule.** *Current* = the remediation records (master plan, phase plans 2–8, the deep review, the settings audit): they move to `docs/superpowers/remediation/{plans,specs}/` and their path strings are rewritten mechanically (exact old → new map, nothing else edited). *History* = every other plan/spec (35 files: all before 2026-10-15 plus the seven 2026-10-15 feature docs, 4 plans + 3 specs): moved to `docs/history/superpowers/{plans,specs}/`, content frozen except **one banner line inserted above the title**; old paths inside them stay as written (the history README explains the mapping). File names never change.
- **R8 Phase 9 owns AGENTS.md rules.** Phase 8 corrects only false facts in AGENTS.md (paths, "keyring", "agents"); D-0.8, the "consumed by server or parked" line and a `make deadcode` target stay in Phase 9.
- **R9 The checker stays in the ledger** (`.superpowers/.../p8/check-links.mjs`, git-ignored) and runs in every Phase 8 gate. Wiring it into `make verify` is a Phase 9 call (it needs the allowlist to stay small).
- **R10 `generate_fixtures.js` is wired into `make verify`** as the master asked: it is deterministic (regenerating in a scratch clone left `git status` clean), so verify regenerates and fails on `git diff --exit-code -- tools/compat/fixtures`.

---

## Verified facts (2026-10-21, main 0c0778b, commands actually run)

`S` = the planner's scratchpad; `$S/tidy` was a `git clone --local` used for every mutating experiment.

1. **Candidate files** (`git ls-files <p> | wc -l`; lines via `xargs cat | wc -l`; `git check-ignore`): `tools/eval` 20 files/2,278 lines; `deploy/sbom.json` 1/1,879; `baseline/python` 3/384; `scripts/plot.py` 1/191; `docs/linux-ci-report.md` 1/117; `tools/vadcompare/pipecat_probs.py` 1/31; `tools/compat/generate_fixtures.js` 1/133; `offline/contract` 7/511; `reports/` 7/1,571; `assets/` 18 (WAV); `third_party` 33; `bin/` ignored (0 tracked). The five deletions of Task 8.1 total 2,912 lines.
2. **References** (`git grep` excluding `pipecat-impl`, `docs/superpowers`): `deploy/sbom.json` only `Makefile:111` (written by `release-security`; nothing reads it; it embeds `/Users/raymondottun/go/pkg/mod/...` paths). `baseline/python`, `scripts/plot.py` (reads a nonexistent `results/`), `docs/linux-ci-report.md`: only themselves (and `.dockerignore:9 baseline`). `pipecat_probs.py`: `vad/compare_test.go:72` (optional input) → KEEP. `generate_fixtures.js`: `tools/compat/fixtures/provenance.json:2` → KEEP + wire (R10). `reports/*`: `docs/benchmarks/container-soak-churn-chaos.md:15-17` → KEEP. `assets/speech`: `server/offline_e2e_test.go:32`, `examples/*`, `vad/compare_test.go` → KEEP. `govoice-eval`: `docs/pipecat-compatibility.md:89`, runner tests `tools/eval/runner_test.go` → KEEP (D-0.1 Whisker runner). `offline/contract`: `Makefile:48,335`, `go.work:7`, `deploy/Dockerfile:28`, `deploy/Dockerfile.dental-mcp:15`, `offline/.dockerignore:12`.
3. **deadcode, wide roots** (`go run golang.org/x/tools/cmd/deadcode@latest -tags xaispeech -test=false ./cmd/... ./examples/... ./tools/... ./offline/cmd/...`) → **28 lines**: 16 `core/coretest` doubles, `server/config_load.go:14 LoadConfig`, `server/mcp/mcptest/mcptest.go:310 BlackHole.Accepted`, `server/retention/job.go:33 WithClock`, `server/server.go:1035 Server.PublicAddr`, `tools/eval/eval.go:460 checkCancellation`, `tools/eval/scenarios/scenarios.go:22,180,241,246` (`SevenScenarios`, `ExtendedParityScenarios`, `AllScenarios`, `GetScenario`), `tools/livecheck/estimate.go:144 spend.String`, `tools/provtest/redact.go:84 HasSecret`, `transport/ws/transport.go:63 DefaultServerConfig`. The `tools/eval/scenarios` package is imported only by its own test (`scenarios_test.go:6`): the whole package (261 + 80 lines) is dead. `HasSecret` is a test oracle (`tools/provtest/provtest_test.go:275,299`, same package). `LoadConfig` is live in `server/deploy_contract_test.go` (rehearsal config) → KEEP; the remaining server/transport lines are Phase 9's deadcode pass. The p6 note `provtest/math_test.go: sinf` is already gone (file holds only `log10`, used at `provtest_test.go:221`).
4. **Makefile:** `verify` = `test` + gofmt + `validate_manifest.js` + `verify_fixtures.js` + `probe_playback_clear.js`; `MODULES` (line 48) lists 13 modules incl. `offline/contract`; `offline-test` (334-336) runs `cd offline/contract && … ./...` then the server `TestOfflineStack` run; `release-security` writes `deploy/sbom.json`; line 311 cites `docs/superpowers/specs/2026-10-15-offline-speech-service-design.md`.
5. **Pinning tests that guard this phase:** `server/deploy_contract_test.go` `TestEveryWorkspaceModuleIsInTheImagesAndTheGate` (every `go.work` module is in `MODULES` and COPYed in both Dockerfiles; one-directional, so removing a module from all three stays green), `TestCIWorkflowCoversTheSafetyNet` (workflows parse; `perf-gate` stays `continue-on-error`), compose/nginx/Dockerfile substring tests (`read_only: true`, `cap_drop`, tini, `-tags xaispeech`, probe port). `tools/compat/parity_matrix_test.go` `TestParityMatrix`: 7 cells per row, status ∈ {`parity`, `partial`, `intentionally different`, `not planned`}, Default prefix ∈ {`on`, `off: `, `library only`, `n/a`}, every backticked path and `Test…` name exists, intro sentence "goldens are in `tools/compat/testdata/parity/`, 61 scenarios in 10 areas, 15 with a declared" matches the goldens, ≥ 30 rows.
6. **perf-gate.yml:27-33** hand-curls the macOS ORT tarball with no checksum and builds `go build ./...` without `-tags xaispeech`. `ci.yml` already runs `make ort` in linux, graph-e2e and macos jobs. `scripts/fetch-ort.sh` has no pinned macOS sha (prints the computed one with a WARNING): `make ort` in perf-gate gives the same (unchecked) bytes plus the warning line that lets us pin it later (carried).
7. **deploy headers:** `deploy/Dockerfile:1`, `deploy/docker-compose.yaml:1-2,9` (`image: govoice:v1.0.0`), `deploy/govoice.service:2,24`, `deploy/nginx.conf:1-2,19`, `deploy/security_scans.md:3,7,25-29,38` (states a release digest `govoice/server@sha256:d8c5…` that nothing produces, and `govoice/server:v1.0.0`). `release-security` accepts a compose image line containing `govoice:` (`Makefile:102`). Compose has a `build:` section, so a local tag is enough.
8. **Stale references** (checker, below, run on main): `AGENTS.md:51 server/credential` (no such dir; secrets are env-only, `docs/security/credentials.md:3`), `docs/pipecat-parity-matrix.md:148 docs/benchmarks/streaming-tts-live.md` and `:149 docs/benchmarks/flow-matrix.md` (`streaming-tts-live.md` deleted in de26c62; `flow-matrix.md` was never tracked: the generated file is `docs/benchmarks/agent-matrix.md`), `e2e/README.md:76 scripts/make-fixtures.sh` (real path `e2e/scripts/make-fixtures.sh`). Code comments: `tools/livecheck/main.go:15,19` and `tools/livecheck/report.go:192,440,450,451` (`flow-matrix` → `agent-matrix`; the code already writes `agent-matrix.{md,json}`), `ui/src/routes/flows/editor/model.ts:2,424` (`server/flow/flow.go` → `server/flow/definition.go`, where `maxCondition` lives). No dangling `make <target>` in tracked docs/scripts.
9. **Modules:** `GOPROXY=off go mod tidy` fails at `.` and `core` (fact R2); `go work sync` diff +188/−140 in 7 files and `go list -m all` drops about 25 test-only modules. Hand edit (root: −echo require, −echo replace, x/time to direct) and `core/go.mod` `require google.golang.org/protobuf v1.36.12` + 4-line `core/go.sum`: `go build ./...` OK, `cd core && GOWORK=off go build ./... && go test ./transport/...` OK, `go list -m all` **identical** to main. Today `cd core && GOWORK=off go build ./...` fails ("no required module provides package google.golang.org/protobuf/…").
10. **ORT headers** (R4): `git grep -n '#cgo\|#include' -- '*.go'` empty; `scripts/fetch-ort.sh:53-55` copies `lib/.` only.
11. **Docs made false by Phases 1–7b** (grep of the p5/p6/p7/p7b "deleted" symbol lists over `*.md`, `*.yaml` outside history: no hits besides those below). Matrix: row 15 says both "on (default since 2026-10-11)" and "so the default stays off"; Known gaps 140 "`turn.manager` defaults off" (false: `server/config_defaults.go:60` sets it true); Known gaps "Deepgram and xAI STT export no counters" (Deepgram has `STTCounts` via `core/sttunit.Runner`, Phase 2 B7; xAI still none); "Config defaults", "Start strategies", "Parallel tools are wired", "TTS text filters are enabled" bullets describe done work, not gaps; Whisker row status; DTMF row lacks the Phase 5.1c frame path (`OutputDTMFFrame`) and 7b "every step"; per-flow keys from 7b missing in the matrix (`speech_timeout_ms`, `max_mute_ms`, `smart_hold_ms`, `audio.tool_cue`, `max_call_duration_seconds`, one-step `end_call`: 0 matrix hits each; `docs/api/v1.md` has them); RTVI row does not say `bot-output`/word-level text/`vad-*` are not sent (the compat doc does, line 38). `docs/pipecat-compatibility.md`: line 32 "five `go.mod` files" (13 tracked), line 103 "59 paired scenarios" (61), line 187 `make verify` "runs root, core, providers/google, providers/xai, and server" (it runs `MODULES`), line 181 Whisker "P0 gate" (parked, D-0.1). `docs/operations_runbook.md:290` ("reloading still clears it") predates the Sign-out button (5.6b); `docs/quickstart.md:254` predates the call page's "Guide this call" panel (7.1).
12. **No doc or test cites a removed Make target, metric, config key or REST field** (`critical_output`, `arguments_summary`, `mcp-tools`, `OnAsyncResult`, … : 0 hits outside history).
13. **Offline-stack long-speech A/B** (ledger line 120, "Phase 8 backlog"): a measurement needing ≥ 30 load-controlled offline runs; not hygiene → carried to Phase 9.3.
14. **docs/superpowers inventory:** `git ls-files docs/superpowers | wc -l` → **45** (+1 = this plan → 46): plans 30 (18 before 2026-10-15, 12 from 2026-10-15 on), specs 15 (10 before, 5 from). Only `.md`; no subfolders besides `plans/`, `specs/`; no zero-byte file (smallest 73 lines); `git ls-files -s … | uniq -d` on blob ids → no duplicate content; the two 2026-10-12 UI specs are distinct (the console spec builds on the polish spec). No relative markdown links inside `docs/superpowers` (`git grep '\]\((\.\./|\./|[a-z])…\.md'` empty), so moves cannot break links inside them; only path strings.
15. **References to docs/superpowers paths** (tracked, outside the folder): `Makefile:311`, `README.md:140` (link to `docs/superpowers/specs`), `offline/README.md:3` (link `../docs/superpowers/specs/2026-10-15-offline-speech-service-design.md`). Inside the folder: 63 path mentions (full list: `git grep -o -E '(docs/superpowers/)?(plans|specs)/2026-10-[0-9]{2}-[A-Za-z0-9._-]+\.md' -- docs/superpowers`), of which 15 lines in the 10 remediation docs name full `docs/superpowers/...` paths (rewritten by Task 8.4) and the rest sit in history docs (frozen) or are bare sibling file names (still valid: siblings move together). Git-ignored ledger: 19 files under `.superpowers/` cite plan paths (incl. `sdd/2026-10-15-review-remediation/plan-path` and the phase briefs/reports); not tracked, not checked; only `plan-path` is updated (Task 8.4). No code, test, workflow, `ui/`, `e2e/` or `tools/` reference.
16. **Dry run of Task 8.4** in `$S/tidy`: 46 `git mv` from `moves.tsv` + `sed -f moves.sed` on `git grep -l -F docs/superpowers/ -- . ':!docs/history'` (11 files: Makefile, README.md, offline/README.md, 8 remediation docs) → `check-links.mjs --paths --moves moves.tsv` reports only the 4 pre-existing problems of fact 8 (fixed in 8.5); `git diff --stat -M` = 48 files, 17 lines changed, all moves detected as renames. `git mv` leaves empty `docs/superpowers/{plans,specs}` dirs on disk: remove them (`rmdir`) or the checker's `existsSync` would accept a link to them.
17. **UI** gate commands (Phase 7): `cd ui && npx tsc --noEmit -p . && npx vitest run && npx --yes knip@5 --no-progress` (knip output empty on main).

---

## Global Constraints

Inherited from `AGENTS.md`, the master plan and user memory.

- No product behaviour change; no new goroutines, queues or exported API.
- `go build`/`go vet` with and without `-tags xaispeech` in every module; `gofmt -l` empty.
- Conventional commits (`chore(repo): …`, `docs: …`, `build: …`, `ci: …`, `test(compat): …`). Trailer: `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Stage explicit paths only; never `git add -A` (IDEA.md).
- Moves with `git mv` only. Never edit content under `docs/history/` except the one banner line (R7).
- Provider secrets: environment only. Never run paid/live targets. Never kill processes by pattern; leave a developer's own `make dev`/`bin/server` alone. Never run `offline-up`/`offline-models` or download models. Private temp paths only (`$P/tmp`).
- Do not push or merge.

### Gate block (after EVERY task; substitute the task id for `8.X`)

```bash
P=.superpowers/sdd/2026-10-15-review-remediation/p8; L=$P/logs; mkdir -p "$L"; set -o pipefail
gofmt -l $(git ls-files '*.go' | grep -v '^pipecat-impl/') | tee "$L/8.X-gofmt.log"; test ! -s "$L/8.X-gofmt.log"
node $P/check-links.mjs --paths $( [ -f $P/moved ] && echo --moves $P/moves.tsv ) | tee "$L/8.X-links.log"   # see Expected
bash $P/tripwire.sh | tee "$L/8.X-tripwire.log"
make verify 2>&1 | tee "$L/8.X-verify.log"
make e2e-echo 2>&1 | tee "$L/8.X-e2e-echo.log"
make provider-matrix-dry 2>&1 | tee "$L/8.X-provider-matrix-dry.log"
# when tools/ changed (8.1, 8.5, 8.6):
make loadtest-smoke 2>&1 | tee "$L/8.X-loadtest-smoke.log"
# when tools/livecheck changed (8.1, 8.5):
make live-check-selftest 2>&1 | tee "$L/8.X-live-check-selftest.log"
# when ui/ changed (8.5):
(cd ui && npx tsc --noEmit -p . && npx vitest run && npx --yes knip@5 --no-progress) 2>&1 | tee "$L/8.X-ui.log"
# offline stack: never start it. Both must answer, else STOP and report BLOCKED (user requirement: this gate must pass).
curl -fsS http://127.0.0.1:11434/api/tags >/dev/null && curl -fsS http://127.0.0.1:8099/healthz >/dev/null \
  && make offline-test 2>&1 | tee "$L/8.X-offline-test.log" || echo "BLOCKED: offline stack down (Ollama :11434 or adapter :8099)"
# 8.3 and 8.7 only:
make test-postgres 2>&1 | tee "$L/8.X-test-postgres.log"
```

Expected: gofmt log empty; links log: before 8.5 exactly the 4 problems of fact 8 (`diff` against `$P/links-before.txt` shows no new line), from 8.5 on `check-links: 0 problem(s)`; `tripwire: OK`; `=== All verify checks passed ===` (the recorded `PLAYBACK_CLEAR_REQUIREMENT_FAILED … relax-unmodified-client-playback-clear` line is expected); `make e2e-echo` exits 0 with every Playwright test passed; `provider-matrix-dry`, `loadtest-smoke`, `live-check-selftest` exit 0; ui: 0 tsc errors, all vitest pass, knip prints nothing; `make offline-test` prints `ok` for both packages (8.3 changes the first package path). On failure: name the failing test (`grep -nE -- '--- FAIL|^FAIL|✘' <log>`), keep the log, never overwrite it with a rerun. Known intermittent `TestOfflineStackLongCallerSpeechIsOneTurn`: rerun it alone 5× (`cd server && OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 go test -count=1 -v -tags "xaispeech offlinestack" -run TestOfflineStackLongCallerSpeechIsOneTurn .`) and report all results. Run gates on an otherwise idle machine.

Per-commit compile check (before every commit; after 8.3 drop `offline/contract`):

```bash
for t in "" xaispeech; do for m in . core server providers/cartesia providers/deepgram providers/echo providers/elevenlabs providers/google providers/nvidia providers/openai providers/xai offline offline/contract; do (cd $m && go build -tags "$t" ./... && go vet -tags "$t" ./...) || echo "FAIL $m tags=$t"; done; done
```

---

## Task 8.0 — Safety net (ledger only, no repo change)

**Files (git-ignored):** `$P/{check-links.mjs,tripwire.sh,deleted-paths.txt,moves.tsv,moves.sed,links-before.txt,deadcode-wide-before.txt,deadcode-master-before.txt,numstat-base.txt}`.

- [ ] **Step 1: checker.** Write `$P/check-links.mjs` exactly:

```js
// Phase 8 reference checker (tracked files only; run from the repo root).
//   node check-links.mjs [--paths] [--moves moves.tsv] [pathspec...]
// 1. Every relative markdown link in a tracked *.md resolves (default set: all *.md except
//    docs/history/** and docs/superpowers/** records, whose code blocks look like links).
// 2. --paths: every backticked repo path in those files exists (allowlist below).
// 3. --moves: for each "old<TAB>new" line, new exists, old does not, and no tracked file outside
//    docs/history/** (frozen content) and the Phase 8 plan (the move record) still names old.
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join, normalize } from 'node:path';
const argv = process.argv.slice(2);
const paths = argv.includes('--paths');
const mi = argv.indexOf('--moves'), movesFile = mi >= 0 ? argv[mi + 1] : null;
const specs = argv.filter((a, i) => a !== '--paths' && (mi < 0 || (i !== mi && i !== mi + 1)));
const git = (...a) => { try { return execFileSync('git', a, { encoding: 'utf8' }); } catch (e) { return e.stdout || ''; } };
// Explicit pathspecs (give exact file paths) are checked as given, so the two READMEs can be checked; the default set excludes records.
const files = git('ls-files', '-z', '--', ...(specs.length ? specs : ['*.md', ':!docs/history', ':!docs/superpowers']),
  ':!pipecat-impl', ':!third_party', ':!**/node_modules/**').split('\0').filter(f => f.endsWith('.md'));
const top = /^(AGENTS\.md|README\.md|Makefile|go\.work|\.github|assets|cmd|config|core|deploy|docs|e2e|examples|offline|proto|providers|reports|scripts|server|smartturn|third_party|tools|transport|ui|vad)(\/|$)/;
// Not repo files: runtime outputs (deploy/sbom.json: written by make release-security), operator-created files, Go identifiers, MCP method names, globs.
const allow = new Set(['e2e/.stack.json', 'deploy/govoice.env', 'deploy/sbom.json', 'server/mcp.Handler', 'tools/list', 'examples/...']);
let bad = 0;
const fail = m => { bad++; console.log(m); };
for (const f of files) {
  for (const [i, line] of readFileSync(f, 'utf8').split('\n').entries()) {
    for (const m of line.matchAll(/\]\(([^)\s#]+)(#[^)]*)?\)/g)) {
      const t = m[1];
      if (/^[a-z]+:/i.test(t) || t.startsWith('/')) continue;
      if (!existsSync(normalize(join(dirname(f), decodeURIComponent(t))))) fail(`${f}:${i + 1}: link ${t}`);
    }
    if (!paths) continue;
    for (const m of line.matchAll(/`([A-Za-z0-9_.\/-]+\/[A-Za-z0-9_.\/-]*)`/g)) {
      const t = m[1].replace(/:\d+(-\d+)?$/, '').replace(/\/$/, '');
      if (top.test(t) && !allow.has(t) && !/(^|\/)(\.env|node_modules)$/.test(t) && !existsSync(t)) fail(`${f}:${i + 1}: path ${t}`);
    }
  }
}
if (movesFile) {
  for (const row of readFileSync(movesFile, 'utf8').split('\n').filter(Boolean)) {
    const [from, to] = row.split('\t');
    if (!existsSync(to)) fail(`moves: ${to} missing`);
    if (existsSync(from)) fail(`moves: ${from} still exists`);
    for (const hit of git('grep', '-n', '-F', from, '--', '.', ':!docs/history', ':!pipecat-impl',
      ':!**/*phase-8-hygiene.md').split('\n').filter(Boolean)) fail(`old path: ${hit.slice(0, 160)}`);
  }
}
console.log(`check-links: ${bad} problem(s), ${files.length} markdown files${movesFile ? ', moves checked' : ''}`);
process.exit(bad ? 1 : 0);
```

Run `node $P/check-links.mjs --paths > $P/links-before.txt`; expected exactly the 4 lines of fact 8 plus `check-links: 4 problem(s), 46 markdown files`.

- [ ] **Step 2: move map** (used by 8.4; generated now so the reviewer sees it before anything moves):

```bash
: > $P/moves.tsv
for f in $(git ls-files docs/superpowers); do b=$(basename $f); t=$(echo $f | cut -d/ -f3)
  case "$b" in
    2026-10-15-review-remediation.md|2026-10-1[6-9]-phase-*|2026-10-2[0-9]-phase-*|2026-10-15-deep-review.md|2026-10-19-settings-coverage-audit.md)
      printf '%s\tdocs/superpowers/remediation/%s/%s\n' "$f" "$t" "$b" >> $P/moves.tsv ;;
    *) printf '%s\tdocs/history/superpowers/%s/%s\n' "$f" "$t" "$b" >> $P/moves.tsv ;;
  esac; done
awk -F'\t' '{printf "s|%s|%s|g\n",$1,$2}' $P/moves.tsv > $P/moves.sed
printf 's|docs/superpowers/plans/|docs/superpowers/remediation/plans/|g\ns|docs/superpowers/specs/|docs/superpowers/remediation/specs/|g\n' >> $P/moves.sed
cut -f2 $P/moves.tsv | cut -d/ -f2-3 | sort | uniq -c
```
Expected: `35 history/superpowers`, `11 superpowers/remediation` (10 + this plan). The two generic lines (applied after the exact ones) only rewrite template strings such as the master plan's `docs/superpowers/plans/2026-10-15-phase-N-<name>.md`.

- [ ] **Step 3: tripwire.** `deleted-paths.txt` lists each path Task 8.1/8.3 deletes (one per line; appended by those tasks). `tripwire.sh`:
```bash
#!/bin/bash
# Fails if a path deleted in Phase 8 is still named by a tracked file outside history and the plan.
cd "$(git rev-parse --show-toplevel)"; P=.superpowers/sdd/2026-10-15-review-remediation/p8; rc=0
while read -r p; do [ -z "$p" ] && continue
  [ -e "$p" ] && { echo "still exists: $p"; rc=1; }
  git grep -n -F "$p" -- . ':!docs/history' ':!docs/superpowers' ':!pipecat-impl' && rc=1
done < $P/deleted-paths.txt
[ $rc = 0 ] && echo "tripwire: OK"; exit $rc
```
(`docs/superpowers` is excluded because the remediation records quote deleted paths by design.)

- [ ] **Step 4: baselines.** deadcode wide and master (commands of fact 3 and Phase 5 R7) → `deadcode-{wide,master}-before.txt` (28 and 7 lines expected); `git rev-parse HEAD > $P/base`.

---

## Task 8.1 — Delete orphaned files and dead tool code (≈ −2,925 lines)

**Delete** (append each path to `deleted-paths.txt` **except `deploy/sbom.json`**, which `.gitignore`, `Makefile` and `security_scans.md` keep naming as a release output; the checker allowlists it): `deploy/sbom.json` (1,879), `baseline/python/` (384), `scripts/plot.py` (191), `docs/linux-ci-report.md` (117), `tools/eval/scenarios/` (341). Go: `tools/eval/eval.go:460-469 checkCancellation` (unreferenced), `tools/livecheck/estimate.go:144 spend.String` (then drop the now-unused `"fmt"` import, `estimate.go:4`). **Move** `HasSecret` (`tools/provtest/redact.go:83-94`, doc comment included) into `tools/provtest/provtest_test.go` (same package; its only callers are there).

**Rewire:** `.gitignore` gains `deploy/sbom.json` (still written by `make release-security`); `.dockerignore` loses the `baseline` line.

**KEEP (reason):** `tools/eval/eval.go`, `tools/eval/cmd/govoice-eval` (D-0.1 Whisker runner; `runner_test.go`; cited by `docs/pipecat-compatibility.md:89`); `tools/eval/cmd/{govoice-bench,govoice-turn-eval}`, `tools/eval/pipecat`, `tools/eval/turnquality` (Make targets `bench-latency`, `eval-turn`, `eval-pipecat`; `perf-gate.yml` path filter); `tools/vadcompare/pipecat_probs.py` (input of `vad/compare_test.go:72`); `tools/compat/generate_fixtures.js` (fixture generator, wired in 8.2); `reports/` (linked by `docs/benchmarks/container-soak-churn-chaos.md`); `assets/speech` (tests and examples); `third_party/*.onnx` (models); deadcode server/transport lines (Phase 9).

- [ ] Step 1: `git rm -r` the five paths; delete the two functions; move `HasSecret`; edit `.gitignore`, `.dockerignore`.
- [ ] Step 2: pins: `git check-ignore -q deploy/sbom.json && echo ignored`; `go run golang.org/x/tools/cmd/deadcode@latest -tags xaispeech -test=false ./cmd/... ./examples/... ./tools/... ./offline/cmd/... | sort > $P/deadcode-wide-8.1.txt` → 21 lines, `comm -13 $P/deadcode-wide-before.txt $P/deadcode-wide-8.1.txt` empty (no new symbol); `go test ./tools/eval/... ./tools/provtest/... ./tools/livecheck/...` ok.
- [ ] Step 3: commits `chore(repo): delete orphaned sbom, python baseline, plot script and CI report` and `chore(tools): delete dead eval scenarios and unused helpers`.
- [ ] Gate (+ `loadtest-smoke`, `live-check-selftest`).
- **Rollback:** `git revert` the two commits.

## Task 8.2 — Build, CI and deploy references (≈ −15 lines)

**Files:** `.github/workflows/perf-gate.yml`, `Makefile` (`verify`), `deploy/{Dockerfile,docker-compose.yaml,govoice.service,nginx.conf,security_scans.md}`.

- [ ] Step 1: `perf-gate.yml`: replace the "onnxruntime 1.29.0" curl step with `- run: make ort` and `go build ./...` with `go build -tags xaispeech ./...`; keep `continue-on-error: true` (pinned by `TestCIWorkflowCoversTheSafetyNet`).
- [ ] Step 2: `Makefile` `verify`, after "Verifying Protobuf Fixtures":
```make
	@echo "=== Regenerating protobuf fixtures (must be byte-identical) ==="
	node tools/compat/generate_fixtures.js >/dev/null
	git diff --exit-code -- tools/compat/fixtures
```
Pin (negative, then restore): change `10000 *` to `10001 *` in `tools/compat/generate_fixtures.js` (client tone amplitude), run the three lines above (`node tools/compat/generate_fixtures.js >/dev/null; git diff --exit-code -- tools/compat/fixtures; echo rc=$?`) → `rc=1`; then `git checkout -- tools/compat/generate_fixtures.js tools/compat/fixtures` and rerun → `rc=0`.
- [ ] Step 3: deploy headers: replace "Voice Framework V1" with "Switchboard" and delete every "spec §NN" reference (`Dockerfile:1`, `docker-compose.yaml:1-2`, `govoice.service:2,24`, `nginx.conf:1-2,19`); compose `image: govoice:v1.0.0` → `image: govoice:local` (built by its `build:` section; passes `Makefile:102`). `security_scans.md`: drop the "V1 — September 2026" line and §-references, delete section 2 (the release digest nothing produces), renumber sections 3–4 to 2–3, replace `govoice/server:v1.0.0` by `govoice:release-security`, and state that `make release-security` writes the SBOM to `deploy/sbom.json` (git-ignored).
- [ ] Step 4: pins: `go test -run 'TestCIWorkflowCoversTheSafetyNet|TestDeploy|TestProbeListenerWiring|TestProductionImage' ./server/` (from `server/`); `git grep -n -E 'Voice Framework|spec §|v1\.0\.0' -- deploy .github` → no hit outside `go.sum`.
- [ ] Commits: `ci: perf-gate fetches ORT with make ort and builds with xaispeech`, `build: make verify regenerates the protobuf fixtures`, `chore(deploy): current names in deploy headers, no invented release digest`.
- [ ] Gate.
- **Rollback:** revert per commit.

## Task 8.3 — Modules and vendored headers (≈ −30,210 lines)

**Files:** `go.mod`, `core/go.mod`, `core/go.sum` (new), `go.work`, `Makefile` (`MODULES`, `offline-test`), `deploy/Dockerfile`, `deploy/Dockerfile.dental-mcp`, `offline/.dockerignore`, `offline/contract/**` → `server/offlinecontract/**`, `third_party/onnxruntime-osx-arm64-1.29.0/**`, `.gitignore`.

- [ ] Step 1 (R5): `git mv offline/contract/{contract_test,stack_test,stack_long_test,tts_unary_test}.go offline/contract/doc.go server/offlinecontract/`; `package contract` → `package offlinecontract` in all five; `git rm offline/contract/go.mod offline/contract/go.sum` (append to `deleted-paths.txt`: `offline/contract`); remove `./offline/contract` from `go.work`, `offline/contract` from `MODULES`, the two `COPY offline/contract/...` lines, and `contract` from `offline/.dockerignore`. `offline-test` first line becomes `cd server && OFFLINE_ADAPTER_URL=http://127.0.0.1:8099 go test -count=1 -v -tags "xaispeech offlinestack" ./offlinecontract/`. Update the run comment inside `stack_test.go` (`go test … ./...` → `./offlinecontract/` from `server/`).
  Pins: `cd server && go test -tags xaispeech -list '.*' ./offlinecontract/` lists `TestRealXAISTTClientAgainstAdapter`, `TestRealXAIUnaryTTSAgainstAdapter`, `TestRealXAIStreamingTTSAgainstAdapter`; `go vet -tags "xaispeech offlinestack" ./offlinecontract/` clean; `make -n offline-test` shows the new path; `go test -run TestEveryWorkspaceModuleIsInTheImagesAndTheGate .` ok.
- [ ] Step 2 (R2): root `go.mod`: delete `govoice/providers/echo …` require and `replace govoice/providers/echo => ./providers/echo`; move `golang.org/x/time v0.15.0` into the first (direct) block. `cd core && GOWORK=off GOFLAGS=-mod=mod go get google.golang.org/protobuf@v1.36.12 && GOWORK=off go mod tidy` (module cache only: `GOPROXY=off`), producing `require google.golang.org/protobuf v1.36.12` and a 4-line `core/go.sum`. Both Dockerfiles: `COPY core/go.mod ./core/` → `COPY core/go.mod core/go.sum* ./core/`.
  Pins: `go list -m all > $P/tmp/mods-before.txt` **after Step 1 and before Step 2**, then again after Step 2 into `mods-after.txt`: identical (comparing with `$(cat $P/base)` differs by exactly one line, `govoice/offline/contract`, removed in Step 1); `cd core && GOWORK=off go build ./... && GOWORK=off go vet ./...`.
- [ ] Step 3 (R4): `git rm -r third_party/onnxruntime-osx-arm64-1.29.0` (tracked files only; the ignored dylib stays on disk); `.gitignore` gains `third_party/onnxruntime-*/`. Do **not** append it to `deleted-paths.txt` (the ignored dylib keeps the directory and `scripts/fetch-ort.sh:6` names the install path). Pins: `git ls-files third_party/onnxruntime-osx-arm64-1.29.0 | wc -l` = 0; `make ort-check` exits 0 on this machine; `git status --porcelain third_party` empty; in a scratch clone (`git clone --local . $P/tmp/clone`) `scripts/fetch-ort.sh check` fails with the same "run 'make ort'" message as on main (fresh clones never had the dylib).
- [ ] Step 4 (optional, only if the pinned base images are already local: `docker image inspect` of the `FROM …@sha256` digests succeeds): `bash deploy/smoke.sh`; else record "image build left to CI image-smoke".
- [ ] Commits: `refactor(offline): fold the offline contract tests into the server module`, `build: root go.mod drops the unused echo require; core requires protobuf`, `chore(repo): stop tracking the ONNX Runtime headers`.
- [ ] Gate (+ `test-postgres`).
- **Rollback:** revert per commit (the header revert restores tracked files; no local state needed).

## Task 8.4 — docs/superpowers cleanup and history move (U3, D-0.7; ≈ +120 lines, 0 deleted)

**Layout after (counts from fact 14/16):**

```
docs/superpowers/                         before: 46 files in plans/ (31) and specs/ (15)
  README.md                               new: index (what each folder is; current vs history; how to add a phase)
  remediation/plans/   (9)                2026-10-15-review-remediation, phase plans 2, 3, 4, 5, 6, 7, 7b, 8
  remediation/specs/   (2)                2026-10-15-deep-review, 2026-10-19-settings-coverage-audit
docs/history/
  README.md                               new: "features as designed at the time; may name removed packages"; old→new path rule
  superpowers/plans/   (22)               18 plans before 2026-10-15 + 4 feature plans of 2026-10-15
  superpowers/specs/   (13)               10 specs before 2026-10-15 + 3 feature specs of 2026-10-15
after: docs/superpowers 12 files (11 + README), docs/history 36 files (35 + README); nothing deleted
```

History set (35, names unchanged): plans `2026-10-11-context-injection`, `2026-10-12-{calls,flow-screens,flows-list,mcp-test-servers,mcp-tools,providers}`, `2026-10-13-{cloudrun-deploy,demo-features,echo-providers,first-run,one-port-server}`, `2026-10-14-{draft-test-calls,greeting,mcp-tool-picker,orphaned-sessions,rename-agent-node-handoff,turn-latency-and-provider-test}`, `2026-10-15-{call-records-and-reply-times,flow-settings-ui-gaps,offline-speech-service,per-flow-call-behavior}`; specs `2026-10-11-{agent-graph,context-injection,credentials-catalog,streaming-tts,turn-latency}-design`, `2026-10-12-{mcp-test-servers,switchboard-console,ui-switchboard}-design`, `2026-10-13-cloudrun-dentistry-demo-design`, `2026-10-14-greeting-design`, `2026-10-15-{call-records-and-reply-times,offline-speech-service,per-flow-call-behavior}-design`. **Deletions recommended: none** (fact 14).

**Reference rewrite list (exact; from fact 15/16):** `Makefile:311` → `docs/history/superpowers/specs/2026-10-15-offline-speech-service-design.md`; `offline/README.md:3` link → `../docs/history/superpowers/specs/2026-10-15-offline-speech-service-design.md`; `README.md:140` → two bullets: `[Design history](docs/history/README.md)` (the agent-graph, credentials, streaming TTS, turn latency and console designs) and `[Review remediation](docs/superpowers/README.md)`; the 15 full-path lines in the remediation docs (master plan 3, 11; phase-2 11, 4772; phase-3 11, 3467; phase-4 11, 3363; phase-5 11; phase-6 11; phase-7 11, 255; phase-7b 11, 14; deep review 160) via `moves.sed`; ledger `.superpowers/.../plan-path` → the new master-plan path (git-ignored; older ledger files stay as written).

- [ ] Step 1: `while IFS=$'\t' read -r a b; do mkdir -p "$(dirname "$b")"; git mv "$a" "$b"; done < $P/moves.tsv`; `rmdir docs/superpowers/plans docs/superpowers/specs` (fact 16); `touch $P/moved` (enables `--moves` in the gate).
- [ ] Step 2: `git grep -z -l -F 'docs/superpowers/' -- . ':!docs/history' ':!pipecat-impl' ':!**/*phase-8-hygiene.md' | xargs -0 sed -i '' -f $P/moves.sed` (BSD sed; on Linux `sed -i`), then fix `README.md:140` by hand (it links a directory) and check `git diff -M --stat`: only the 11 files of the rewrite list change content; every move is a rename.
- [ ] Step 3: banner (R7) on each of the 35 history files, inserted as line 1 (above the `#` title), nothing else:
  `> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.`
  `for f in $(cut -f2 $P/moves.tsv | grep '^docs/history/'); do head -1 "$f" | grep -q '^> \*\*Historical' || { printf '%s\n\n' "<banner>" | cat - "$f" > $P/tmp/b && mv $P/tmp/b "$f"; }; done` (idempotent: a rerun adds nothing). Pin: `git diff -M --numstat -- docs/history | awk '$1!=2||$2!=0'` empty (each file +2 lines, −0).
- [ ] Step 4: `docs/history/README.md` (≈ 20 lines): purpose, the two folders, the rule "a path `docs/superpowers/{plans,specs}/X` inside these files now lives at `docs/history/superpowers/{plans,specs}/X` unless X is a remediation record (`docs/superpowers/remediation/…`)", and "do not edit; write a new doc instead". `docs/superpowers/README.md` (≈ 25 lines): table of the 11 remediation records with one line each (master plan → phases 2–8 in order, deep review, settings audit), current status pointer (`docs/pipecat-parity-matrix.md`), naming rule for new phase plans (`remediation/plans/YYYY-MM-DD-phase-N-<name>.md`), link to `docs/history/README.md`. Links in both READMEs are relative and checked.
- [ ] Step 5: pins: `node $P/check-links.mjs --paths --moves $P/moves.tsv` → only the 4 fact-8 problems; `node $P/check-links.mjs docs/superpowers/README.md docs/history/README.md` → `0 problem(s), 2 markdown files` (README links and paths; the explicit pathspec lifts the default exclusions); `git grep -n -E 'docs/superpowers/(plans|specs)/' -- . ':!docs/history' ':!**/*phase-8-hygiene.md'` empty; `make -n offline-up` unaffected (comment only).
- [ ] Commits: `docs: move remediation records to docs/superpowers/remediation` (moves + path rewrites + superpowers README), `docs: move earlier feature designs and plans to docs/history` (moves + banners + history README + Makefile/README/offline README refs).
- [ ] Gate.
- **Rollback:** revert both commits (renames revert cleanly).

## Task 8.5 — Doc claims and stale references (≈ ±70, net ≈ −10)

- [ ] `AGENTS.md` (facts only, R8): line 23 "Agent DB Storage & Credential Keyring (SQLite / Postgres)" → "Flow Storage (SQLite / Postgres) & Env-Only Secrets" padded with spaces to the box width (the line keeps its 61-character inner width; check `awk 'NR>=18&&NR<=35{print length}' AGENTS.md` shows one value for the box lines); line 47 "persistent agents, provider credentials" → "stored flows, provider configuration"; line 50 "pinned agent version" → "pinned flow version"; line 51 `server/credential` → provider catalog `server/catalog` (secrets read from the environment only, `docs/security/credentials.md`).
- [ ] `e2e/README.md:76` → `e2e/scripts/make-fixtures.sh`.
- [ ] `docs/pipecat-compatibility.md`: line 32 "the five `go.mod` files" → "the `go.mod` files of the modules in `go.work`"; line 103 drop "59" (refer to the matrix intro, which `TestParityMatrix` pins); line 187 module list → "every module in the Makefile's `MODULES`"; line 181 Whisker wire protocol status → `parked (D-0.1)`.
- [ ] `docs/operations_runbook.md:290`: add "or press Sign out in the top bar"; `docs/quickstart.md:254`: add that the call page's "Guide this call" panel sends the same note.
- [ ] Settings audit `docs/superpowers/remediation/specs/2026-10-19-settings-coverage-audit.md:302` heading → "R8. LLM sampling parameters (needs core/provider work: own phase, not scheduled)" (U2). Also line 182: the bare `2026-10-15-per-flow-call-behavior-design.md` (no longer a sibling after 8.4) → `docs/history/superpowers/specs/2026-10-15-per-flow-call-behavior-design.md`. No other change to the audit.
- [ ] Comments: `tools/livecheck/main.go:15,19`, `tools/livecheck/report.go:192,440,450,451` (`flow-matrix` → `agent-matrix`, `make flow-matrix` → `make agent-matrix`); `ui/src/routes/flows/editor/model.ts:2,424` → `server/flow/definition.go`.
- [ ] Pins: `node $P/check-links.mjs --paths --moves $P/moves.tsv` → `0 problem(s)`; `git grep -n -E 'flow-matrix|server/credential|server/flow/flow\.go' -- . ':!docs/history' ':!docs/superpowers'` empty.
- [ ] Commits: `docs: correct stale facts in AGENTS.md, compatibility record and runbook`, `chore: stale paths in livecheck and editor comments`.
- [ ] Gate (+ `loadtest-smoke`, `live-check-selftest`, ui).

## Task 8.6 — Parity matrix vocabulary, rows and Known gaps (≈ +10 lines)

**Files:** `docs/pipecat-parity-matrix.md`, `tools/compat/parity_matrix_test.go`.

- [ ] Step 1 (R6): `valid` gains `"planned (hook exists)": true, "parked": true`; the Status legend (line 5) defines them: `planned (hook exists)` = the Go hook is in the tree and tested, nothing in the server reaches it yet; `parked` = kept for a documented deliverable, not on a call path. Run `go test -run TestParityMatrix ./tools/compat/` before the doc edit (still green).
- [ ] Step 2: rows. Whisker → `parked` ("D-0.1: kept with its runner `tools/eval/cmd/govoice-eval`; owns its frames and envelope since Phase 5"). New Context-and-tools rows: **Mid-call context updates** — Pipecat `frames/frames.py` (LLMMessagesAppendFrame); Switchboard `server/live_calls_api.go`, `core/async_trigger.go`, `ui/src/routes/sessions/LiveCallControls.tsx`; `partial`; `on (management API and the call page)`; append a note and optionally run, operator notes labelled as notes; no client-side RTVI message, no cross-instance routing; Evidence `server/live_calls_api_test.go` (TestLiveCallContextAcceptsNotes), `server/context_injection_e2e_test.go` (TestContextInjectionFlowE2E), `ui/src/routes/sessions/LiveCallControls.test.tsx`. **Mid-call prompt, tools and settings replacement** — Pipecat `frames/frames.py` (LLMMessagesUpdateFrame, LLMSetToolsFrame, LLMUpdateSettingsFrame); Switchboard `core/context.go` (SetSystemInstruction, SetSystemInstructionChecked); `planned (hook exists)`; `library only`; hooks kept by Phase 5 U2, no API or frame reaches them; Evidence `core/context_test.go`. DTMF row: `send_dtmf` posts `OutputDTMFFrame` (frame path: observers and redaction see output digits) and is offered on every step when `dtmf.send_tool` is on. Per-flow 7b keys in the rows that own them: Start min words (flow `turn.min_words`), Stop speech timeout (`turn.speech_timeout_ms`), Stop turn analyzer (`turn.end_of_turn: smart`, `smart_hold_ms`), Stop eager (`turn.end_of_turn: eager`), User mute strategies (flow `turn.mute`, `max_mute_ms`), Mixer (per-step `audio.background`, hold sound `audio.tool_cue`), Flows (one-step `end_call`, `max_call_duration_seconds`). RTVI wire row: "`bot-output`, word-level TTS text and `vad-*` are not sent". Row 15: delete "so the default stays off" (contradicts its Default cell).
- [ ] Step 3: Known gaps: rewrite the turn-manager bullet (on by default since 2026-10-11; `server/config_defaults.go`); "Deepgram and xAI STT export no counters" → "xAI STT exports no counters"; line 148 → "(the live write-up was removed in the docs cleanup, de26c62; see git history)"; line 149 `docs/benchmarks/flow-matrix.md` → `docs/benchmarks/agent-matrix.md`; delete the four done-work bullets (Config defaults, Start strategies, Parallel tools, TTS text filters); "Dead or library-only code" adds the system-instruction hooks (planned). Remaining work #4: context updates append/run now also have the call-page panel (Phase 7).
- [ ] Pins: `go test -count=1 -run TestParityMatrix ./tools/compat/`; `grep -c '| parked |' docs/pipecat-parity-matrix.md` = 1, `grep -c '| planned (hook exists) |'` = 1; checker 0.
- [ ] Commits: `test(compat): parity matrix accepts planned and parked`, `docs: parity matrix rows and known gaps after Phases 3-7b`.
- [ ] Gate (+ `loadtest-smoke`: `tools/compat` changed; it is a test-only package the load test does not import, but the rule is per `tools/` change).

## Task 8.7 — Phase gate, adversarial review, PR notes

- [ ] Step 1: full gate block once more on the branch head, plus `make test-postgres` and `make loadtest-smoke`; deadcode wide/master after ⊂ before (`comm -13` empty), wide 28 → 21.
- [ ] Step 2: line delta: `git diff --numstat $(cat $P/base)...HEAD` grouped into non-test Go, test Go, vendored/generated (third_party, sbom), docs, other; record in `$P/pr-notes.md`.
- [ ] Step 3: dispatch the reviewer with this brief:

> Review Phase 8 (repo hygiene and docs) on `feat/phase-8-hygiene` against `docs/superpowers/remediation/plans/2026-10-21-phase-8-hygiene.md`. Hunt for: (1) references to deleted or moved files anywhere tracked: Makefile targets and comments, `.github/workflows/*`, both Dockerfiles, `offline/.dockerignore`, `.dockerignore`, `go.work`, `MODULES`, scripts, tests reading files by path (`server/deploy_*_test.go`, `tools/compat/*_test.go`), `ui/`, `e2e/`, README/AGENTS/docs; run `node .superpowers/sdd/2026-10-15-review-remediation/p8/check-links.mjs --paths --moves …/moves.tsv` and `tripwire.sh` yourself; (2) dangling relative links, and links that resolve only to an empty directory left by `git mv`; (3) accidental deletion of golden fixtures, testdata, models or anything a test or Make target reads (`tools/compat/fixtures`, `testdata/parity`, `assets/`, `third_party/*.onnx`, `reports/`); (4) go.mod side effects: `go list -m all` identical to base, `core` builds with `GOWORK=off`, Docker `go work sync` still works with the new `core/go.sum` COPY; (5) `offline-test` really runs the three contract tests and the stack tests from `server/offlinecontract` (read the log), `make test` still runs the xaispeech contract tests; (6) docs still claiming removed features or wrong defaults (matrix rows true against the code: open each cited path; turn manager default, counters, per-flow keys, Whisker/planned statuses), and `TestParityMatrix` vocabulary not wider than needed; (7) history docs: content unchanged except the banner (`git diff -M --numstat` +2/−0 each), remediation docs: only path strings changed; (8) YAGNI: the `generate_fixtures` verify step (does it catch drift?), the checker kept out of the repo, nothing added that nothing uses; (9) IDEA.md untouched and unstaged. Report each finding with file:line and how to reproduce.

- [ ] Step 4: fix round for Critical/Important findings (each with its gate), then the PR notes:
  - **Deleted:** files of 8.1 and the ORT headers with line counts; `tools/eval/scenarios` API (`SevenScenarios`, `ExtendedParityScenarios`, `AllScenarios`, `GetScenario`, `Scenario`); `checkCancellation`, `spend.String`; `offline/contract` module (tests now `server/offlinecontract`).
  - **Moved:** 46 docs (35 to history with banners, 11 to `docs/superpowers/remediation`), with the old→new table.
  - **Build/CI:** perf-gate uses `make ort` and `-tags xaispeech`; verify regenerates protobuf fixtures; root `go.mod` and `core/go.mod` changes (build list identical); `deploy/sbom.json` and the ORT tree are git-ignored outputs now.
  - **Docs fixed:** list of Task 8.5/8.6 items; matrix vocabulary `planned (hook exists)` and `parked`.
  - **Net line delta** (measured in Step 2; estimate ≈ −33,000: ORT headers −30,184, sbom −1,879, other deletions −1,045, docs ≈ +120).
  - **KEEP with reason:** U1 columns; Task 8.1 KEEP list; deadcode lines left for Phase 9; `go.work.sum` (R3); cosmetic tidy of google/offline-contract (R2).
  - **Carried:** U2 (R8 sampling, own phase); macOS ORT sha pin (take the hash from perf-gate's `fetch-ort` WARNING line); offline long-speech A/B (Phase 9.3); checker into `make verify` and deadcode target (Phase 9.1); flakes `TestFlushStaleInterruptionCannotCancelCurrentTurn`, `TestAnalyzerTimeoutFallsBackToSpeechTimeout` (unreproduced).
- [ ] Step 5: ledger: `progress.md` line "PHASE 8 gate …", do not push or merge.

---

## Estimated line delta per task

| Task | Delta | Note |
|---|---|---|
| 8.0 | 0 | ledger only |
| 8.1 | ≈ −2,925 | 2,912 file lines + ~13 Go lines |
| 8.2 | ≈ −15 | security_scans section 2, headers; +3 Makefile |
| 8.3 | ≈ −30,210 | ORT headers −30,184; contract go.mod/go.sum −19; core +6 |
| 8.4 | ≈ +120 | 46 renames (0 content), 35 banners (+70), two READMEs (~+45), 17 path lines rewritten |
| 8.5 | ≈ −10 | |
| 8.6 | ≈ +10 | two rows, four bullets out |
| total | ≈ −33,030 | ≈ −1,000 without vendored headers and the SBOM (reviewer's scratch apply: +167/−1,149 outside those two) |
