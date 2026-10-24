# Agent evaluation, slice 1: foundation and suites (implementation plan)

Planner output, read-only research on `main` at `e39dfc1c`. File:line citations were read there unless marked UNVERIFIED.

> **For agentic workers:** recommended sub-skill `superpowers:subagent-driven-development`. One sonnet implementer per
> task, one independent reviewer per task (AGENTS.md "Mandatory Code Review Workflow"), tasks in order.

**Goal:** an operator saves cases for an agent, runs them (the whole suite or one case) against any published version
or a draft from the console, the API or the CLI. Every case-run is scored by named scorers and stored per version.
The operator sees pass rates on a fleet dashboard and compares versions in a History tab. Two servers on one Postgres
never run the same job twice and survive a worker crash. Everything is free on echo and offline.

**Architecture:** the CLI's runner and scoring core move from `tools/eval/agents` (package main) into a new library
package `server/eval`. That package holds the case format, rewrite, callers, the turn driver over a small `Target`
interface, the scorer interface and registry, and the built-in scorers. It has two `Target` implementations: the CLI's
HTTP one (`tools/eval/agents`) and the server's in-process one (`server/eval_target.go`).

- The RTVI test client moves from `tools/internal/stack` to `transport/wsclient`, so the server module can import it.
  `tools/internal/stack` is internal to `tools/`.
- New `eval_*` tables are added to `001_initial.sql` in both dialects, together with a storage `EvalRepository` that
  holds the job queue (a conditional-UPDATE claim with a lease).
- A worker pool in `server/eval_worker.go` starts in `Start` and stops at the beginning of `Shutdown`. It claims
  `run_case` jobs and runs each call against this instance's own public listener with an in-process draft token, then
  reads the stored rows, scores them and writes the results.
- A new handler `server/api/eval.go` serves `/api/v1/agents/{id}/eval/...` and `/api/v1/eval/fleet`.
- In the UI, a new Observability nav group holds Evaluation (the fleet dashboard). `/evaluation/:agentId` has the tabs
  Test suite and History.

**Tech stack:** Go 1.27.1 (modules `govoice` and `govoice/server`, see `go.work`), `database/sql` with goose migrations
(`server/storage/storage.go:117-133`), `github.com/coder/websocket`, `gopkg.in/yaml.v3`, and hand-written Prometheus
text (`server/metrics.go:170`). The UI uses React 19, react-router 7, TanStack Query 5, zod 4 and vitest. End-to-end
tests use Playwright (`e2e/playwright.agents.config.ts`).

**Spec:** `docs/superpowers/specs/2026-10-24-agent-evaluation-design.md` (approved; read every section, including
"Calls made after review"). Context: `.superpowers/sdd/phase-11/plan.md`, `docs/operations/agent-evals.md`.

---

## Global constraints (every implementer)

**Files you must not touch**
- `IDEA.md`, `docs/superpowers/remediation/` and any untracked user file.
- `docs/superpowers/` except this plan.

**Layering and dependencies**
- `core/` is not touched.
- `server/eval` must not import `server/api` or package `server`. `server/api` imports `server/eval`, which would make
  a cycle (eval → api → eval).
- The CLI imports `server/eval` and keeps only flags, HTTP agent resolution, the HTTP `Target`, the report and the
  self-test (spec call 8: "one engine and no fork. The CLI stays a thin client").

**Vocabulary** (`scripts/check-vocabulary.mjs:9-16`, applies to every tracked file)
- Use agent, step, handoff, call. The API word for a call stays `session`.
- Never "flow" (also not in DDL, triggers or RAISE text: `server/storage/schema_names_test.go:10`).
- Never route/routes/routed/routing. `fooRoute` identifiers also trip rule R2: name helpers `evalPath`, not `evalRoute`.
- An unused allowlist rule fails the run, so add none.

**Secrets**
- Keys are read from the environment only, never stored, logged or returned (`docs/security/credentials.md`).
- A judge or persona key is fetched with `s.resolver.Secret(provider,"llm")` and then `defer clear(secret)`, the
  recipe at `server/probe.go:180-196`.
- Judge error strings pass through `s.redactor.Redact` and replace the literal secret (`probe.go:186-191`).

**Paid usage**
- No paid judge or caller is used unless the operator chose it.
- With no judge configured, a model-graded scorer is recorded as `skipped` with the reason `no judge configured`. It
  is never pass or fail and never falls back to a paid provider (spec call 1).
- CLI paid paths keep `GOVOICE_LIVE=1` (`tools/eval/agents/main.go:109-114`).

**Spec values, copied exactly**
- Result outcomes: `pass`, `fail`, `skipped`. Skip reasons: `no judge configured`, `no stored history`.
- Job kinds `run_suite`, `run_case`, `score_call`. Job statuses `pending`, `running`, `done`, `failed`, `dead`.
- Result unique key: (subject, scorer, agent version), written with `ON CONFLICT DO NOTHING`.
- A draft run has an empty agent version and the draft's config hash as its key. It is excluded from History, the
  dashboard and alerts (spec call 2).
- Suite pass rate is the passed cases in the latest finished, non-draft suite run over its cases, against a per-agent
  suite threshold.
- The live score needs at least 5 samples in 7 days. That is slice 2: slice 1 returns no live data (see S17).
- A suite stores `runs` and `min_pass` (spec call 3).
- Eval calls run the agent's real tools, and the console says so.

**Resource bounds and lifecycle** (AGENTS.md)
- Every queue is bounded: at most 200 pending or running jobs per agent, at most 200 cases per agent, `runs` 1..5,
  and scripts and personas use the existing caps (`server/eval/case.go` after the move: 20 lines, 20 turns).
- Every goroutine selects on ctx or closes on its connection, has `recover`, and is joined on stop.
- Leases compare against a worker-supplied `now` (spec "Clock skew").
- Durations use the monotonic clock. Stored times go through `utc()` (`server/storage/storage.go:270`).
- `goleak`: `server` has a package-level `TestMain` (`server/turn_stop_test.go:24-34`), so every test that starts the
  worker must stop it. The new package `transport/wsclient` adds its own `TestMain` goleak check.
- Never hold a lock across I/O.

**Process**
- RED first: each task's new test must fail for the stated reason before the code exists.
- Conventional commits ending with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review focus (the five ways this most likely goes wrong for a user)

| # | What the user would suffer | Test that pins it |
|---|---|---|
| R1 | A job runs twice on two servers, so two eval calls hit the real tools and results double-count. | `TestEvalClaimRaceExactlyOnce` (both dialects, 4 claimers × 40 jobs), `TestEvalLeaseExpiryFencesOldOwner`, `TestEvalResultsInsertIdempotent` (T5) |
| R2 | A "pass" that is not one: a model-graded scorer passes without a judge, a draft run leaks into the dashboard or History, or a dead job is counted as passed. | `TestRunScorersSkipsJudgedWithoutJudge` (T3), `TestFleetExcludesDraftAndCaseScopedRuns`, `TestFinishRunCountsDeadCaseRunAsFailed` (T5) |
| R3 | Shutdown hangs, or goroutines and sockets leak: the worker outlives `Shutdown`, an eval call holds the drain, a reader goroutine leaks. | `TestEvalWorkerStopsOnShutdown` (goleak, T8), `TestMain` in `transport/wsclient` (T1), the existing `server` TestMain |
| R4 | Retention silently breaks: an `eval_results` FK to `sessions` blocks `PurgeMetadata` (`server/storage/retention.go:202`), or eval rows are never purged. | `TestPurgeMetadataWithEvalResults`, `TestPurgeEval` (T6, both dialects) |
| R5 | The CLI loses its guarantees after the move: a mutation passes, `make eval-agents-echo` goes green on a broken agent, or failure strings change and the self-test stops proving anything. | `make eval-agents-echo` (all four mutations plus the new judge mutation), `TestExpectationsReasonsKeepCLIPrefixes` (T3) |

## File map

| Area | Create | Modify / move |
|---|---|---|
| WS client | `transport/wsclient/client.go`, `client_test.go`, `main_test.go` | `git mv tools/internal/stack/client.go`, `client_test.go`. `Handshake` moves out of `tools/internal/stack/stack.go:205`. Update `tools/livecheck/rtvi.go`, `tools/loadtest/client.go`, `tools/eval/agents/*`, and the goleak list in `AGENTS.md`. |
| Engine | `server/eval/{case,rewrite,caller,call,input,scorer,builtin,judge}.go` + tests | `git mv` from `tools/eval/agents/{case,rewrite,caller,run,score}.go` (and their tests). The CLI's `main.go`, `report.go`, `selftest.go` and a new `target.go` stay. |
| Schema | — | `server/migrations/sqlite/001_initial.sql`, `server/migrations/postgres/001_initial.sql` (tables), `002_indexes.sql` (both, indexes) |
| Storage | `server/storage/eval.go`, `eval_queue.go`, `eval_test.go`, `eval_queue_test.go` | `server/storage/retention.go` (`PurgeEval`, `PurgeMetadata` unchanged) |
| Retention | — | `server/retention/{policy,job}.go` + test, `server/config.go:313-319`, `config_defaults.go:95-100`, `config_validate.go:254-282` |
| Server | `server/eval_target.go`, `server/eval_worker.go`, `eval_worker_test.go`, `eval_e2e_test.go` | `server/api_routes.go` (token issue extracted, eval dispatch), `server/server.go` (field, Start, Shutdown), `server/management.go` (install handler, stop worker), `server/metrics.go`, `server/config*.go`, `server/testdata/effective_*.json`, `config/{development,production}.yaml` |
| API | `server/api/eval.go`, `eval_test.go` | `server/api_routes_table_test.go` |
| UI | `ui/src/routes/evaluation/{EvaluationView,AgentEvaluation,TestSuiteTab,HistoryTab,Sparkline}.tsx` + `*.test.tsx`, `ui/src/api/client.eval.test.ts` | `ui/src/routes/sections.tsx`, `ui/src/app/App.tsx` (+ `App.test.tsx:51`), `ui/src/api/client.ts`, `ui/src/styles.css` |
| E2E | `e2e/tests-agents/45-evaluation.spec.ts` | — |
| Docs | `docs/api/eval.md` | `docs/operations/agent-evals.md`, `README.md:42-54`, `docs/pipecat-parity-matrix.md:108`, `docs/observability.md`, `docs/deployment.md`, `docs/operations.md`, `docs/api/v1.md`, `Makefile:187` |

---

## Spec-silent calls made here (S-numbers are referenced by tasks)

- **S1 Tables in `001_initial`.** Per the brief ("rewrite in place, nothing has shipped"), the tables go into
  `001_initial.sql` and the indexes into `002_indexes.sql`. No new migration file, so the hard-coded version check
  `n != 11` (`server/storage/storage_test.go:41`) is unchanged.
- **S2 Sixth table `eval_suites`.** One row per agent: `runs`, `min_pass`, `pass_threshold`, and a default judge
  (`judge_provider`, `judge_model`). The spec puts the per-agent default judge, the suite threshold and runs/min_pass
  "per agent" but lists no table for them.
- **S3 Fifth built-in scorer `expectations`.** Deterministic. It carries today's non-tool assertions: path, say,
  never_say, end_reason, max_turns, median_reply_ms_max, zero RTVI errors, zero failed handoffs and no dropped events
  (`tools/eval/agents/score.go:150-208`).
  - Without it the CLI's semantics and its four mutations (`selftest.go:118-133`) would have no scorer.
  - Its reason lines keep the CLI prefixes (`path:`, `say:`, `handoff:` …).
  - The four named scorers stay as specified. See Decision D1.
- **S4 Toxicity is model-graded.** The spec does not say which. Score = 1 − toxicity, default threshold 0.9.
- **S5 Default thresholds.** correctness 0.7, answer_relevancy 0.7, toxicity 0.9, tool_call_accuracy 1.0,
  expectations 1.0. Suite `pass_threshold` 1.0, `runs` 1, `min_pass` 1.
- **S6 New case keys `expect.tools_ordered`** (bool: `tools_called` must appear in this order) **and
  `expected_answer`** (string; correctness evidence). The spec names both concepts. Case files may now hold several
  YAML documents, so an export loads in the CLI. This drops the "one case per file" rule
  (`tools/eval/agents/case.go:116-118`).
- **S7 Judge scale and format.** The judge replies `{"score": <0..1>, "reason": "<= 300 chars"}`. The first JSON object
  in the reply is parsed; a reply that does not parse is outcome `error`. Evidence is capped at 16 KiB, built from the
  stored turns, path, tool names and statuses, and the agent's per-step system prompts (each capped at 2 KiB).
  - A judge transport error or a missing key is `error`, never `skipped`. `skipped` means only "no judge configured"
    or "no stored history" (or a scorer with nothing to check, e.g. no tool expectations).
- **S8 Job fan-out at enqueue.** `POST .../runs` inserts the run and one `run_case` job per (case, repeat) in one
  transaction. The `run_suite` kind is allowed by the CHECK but unused in slice 1, and `score_call` is slice 2.
  - Subject is `<caseID>#<repeat>`. The idempotency key is `run:<runID>:<caseID>#<repeat>`.
- **S9 Harness errors** (token, dial, turn timeout, stored rows missing) fail the job with a category. Backoff then
  retries it; after `max_attempts` it is `dead`. A dead case-run counts as failed in the run summary and shows its
  category, never content.
  - `failed` is not used as a resting state: a failure is `pending` again (with `run_after`) or `dead`. The CHECK
    allows it to match the spec.
- **S10 Queue values.** lease 2 min, heartbeat every 30 s, idle poll 1 s (+0-250 ms jitter), backoff
  `30s·2^(attempts-1)` capped at 10 min, `max_attempts` 3. Claim scans at most 8 candidates per attempt.
  - A server restart mid-job costs that job one attempt (its lease expires). The docs say so.
- **S11 Eval-call cap.** New config `evaluation.max_concurrent_calls`, default 2; 0 disables the worker on this
  instance (how an operator pins evals to some servers). The worker pool size equals the cap.
  - Validation: 0..16, and at most `limits.max_sessions_per_principal` when that limit is >0, because all eval calls
    use principal `agent-eval` (`tools/eval/agents/run.go:18`, admission `server/admission.go:75`).
- **S12 Server-side providers mode.** The run request picks `providers: keep|offline|echo` (default `keep`; same
  `eval.Rewrite` modes). **D2 resolved (user): the server keeps the `GOVOICE_LIVE` gate.** A run with `providers: keep` is accepted only
  when the server process was started with `GOVOICE_LIVE=1` or every provider of the version under test is keyless
  (`eval.NeedsLive` on the rewritten config is false: echo, offline, silence). Otherwise the API answers 403
  `eval_live_required` with a message naming `GOVOICE_LIVE=1`. The default providers mode is `offline`.
  - The persona caller on the server is the `offline` provider only (built through the registry). Slice 1 has no paid
    persona caller on the server. With no offline endpoint configured, a persona case-run is `dead` with category
    `persona_unavailable`, with no retry (`max_attempts` reached at once).
- **S13 Stored rows.** say/never_say and the judges read stored `turns.assistant_text` and `turns.transcript` (spec
  call 7), not client text. `ReplyMS`, `ClientErrors` and `Dropped` are client facts passed in `Input` for suite runs
  only (zero on live calls).
  - A tool row's step is derived from the handoff spans' times: there is no step column on `tool_calls`.
- **S14 No FK from `eval_results.session_id` to `sessions`.** `PurgeMetadata` deletes sessions (`retention.go:202`),
  so the link may dangle after purge and the UI then says "call no longer stored".
  - Other FKs follow the house style `REFERENCES x(id) ON DELETE RESTRICT`: agents, agent_versions, eval_runs, plus
    `eval_cases` on agents.
- **S15 Eval calls are marked** by the existing `sessions.draft` flag plus caller principal `agent-eval`. No new
  session column; slice 2 excludes `draft=true` from sampling.
- **S16 Retention.** New `retention.eval_retention_days` (default 180) for runs and results. Finished jobs
  (`done`/`dead`) are purged at `metadata_retention_days`. Both run in the existing retention job, which is off by
  default (`config_defaults.go:95`).
- **S17 Fleet without live scores.** Slice 1 has no live results, so the fleet endpoint and dashboard show the suite
  columns only, with no "live" column (adding it now would be dead code until slice 2). The banner counts agents below
  their suite threshold.
- **S18 Case editing is a YAML textarea** using the existing case format (create, edit, import). The API takes YAML
  only. See Decision D3.
- **S19 Draft runs are API-only in slice 1.** `POST .../runs` with `draft` works and the Test suite tab lists them as
  "draft", but the console has no draft-run button. See Decision D4.
- **S20 Session link.** Each case-run links to `/sessions/:sessionId` (`ui/src/app/App.tsx:258`). There is no trace
  link yet.
- **S21 Metrics now** (the brief asks; the spec puts metrics in slice 3):
  - `voice_eval_jobs_total{result="done|retry|dead|lease_lost"}`
  - `voice_eval_calls_active` (gauge)
  - `voice_eval_runs_finished_total`
  - No alert rule in slice 1: alert delivery is slice 3.
- **S22 The tested version vs the session's version.** A draft token's snapshot carries the *current* version id
  (`server/agent/service.go:121-129`). Results therefore take `agent_version_id` from the run row, never from the
  session.

---

## Tasks

### T1 — Move the RTVI client to `transport/wsclient`

**Files:** `git mv tools/internal/stack/client.go transport/wsclient/client.go`, the same for `client_test.go`. New
`transport/wsclient/main_test.go`. Edit `tools/internal/stack/stack.go`, `tools/livecheck/rtvi.go`,
`tools/loadtest/client.go`, `tools/eval/agents/{run,score,main,selftest,*_test}.go` and `AGENTS.md` (goleak list line).

**Interfaces**
- **Consumes:** `ws.EncodeMessageFrame`, `ws.DecodeBinaryFrame` (`transport/ws`).
- **Produces** (package `wsclient`, names unchanged from `stack`):
  - `type Event`, `type Client`, `const DefaultMaxEvents = 4096`
  - `func Dial(ctx context.Context, public, token string, pushToTalk bool) (*Client, error)`
  - `func DialN(ctx, public, token string, pushToTalk bool, maxEvents int) (*Client, error)`
  - `func Handshake(ctx context.Context, conn *websocket.Conn, pushToTalk bool) error`
  - methods `Snap`, `Dropped`, `SendMsg`, `SendText`, `Closed`, `FirstAfter`, `Has`, `Texts`, `Count`, `Close`.

**Steps**
- [ ] RED: add `transport/wsclient/main_test.go` with `func TestMain(m *testing.M)` running `goleak.VerifyTestMain(m)`,
  plus `TestCloseEndsReader`: dial an `httptest` WS server that sends bot-ready, `Close()`, then
  `<-c.Closed()` within 1 s.
  - Run `go test ./transport/wsclient/`. Expected: FAIL, because package `wsclient` does not exist.
- [ ] Move the files. `package wsclient`, with a doc comment saying it is the RTVI test client used by the eval engine
  and the free harnesses.
  - The reader goroutine keeps ending on connection close (`client.go:67`), and `Close` stays idempotent.
  - `stack.go` keeps only child-process code. `Server.Token` stays.
- [ ] Update every import (`grep -rln 'tools/internal/stack' --include='*.go' .` lists 11 files). Callers of
  `stack.Dial`, `stack.Event`, `stack.Handshake` and `stack.DialN` switch to `wsclient.*`.
- [ ] Add `transport/wsclient` to the goleak package list in `AGENTS.md` (the "No goroutine leaks" bullet).
- [ ] Verify: `go vet ./... && go test -race ./transport/... ./tools/...` passes; `make eval-agents-echo` passes
  (prints `agent-eval selftest: 3 passed, 4 mutations failed as expected, ...`).
- [ ] Commit `refactor(transport): move the RTVI test client to transport/wsclient`.

### T2 — Engine package `server/eval` (mechanical move, behaviour unchanged)

**Files:** `git mv tools/eval/agents/{case,rewrite,caller,run,score}.go` and their `_test.go` into `server/eval/`
(rename `run.go`→`call.go`, `score.go`→`input.go`). Create `tools/eval/agents/target.go`. Edit
`tools/eval/agents/{main,selftest,report}.go`. `testdata/` used by moved tests moves with them
(`testdata/cases-*`, `testdata/api`); `testdata/agents` and `testdata/echo` stay for the self-test.

**Interfaces**
- **Consumes:** `agent.Decode` (`server/agent/definition.go:99`), `wsclient` (T1), and `storage.SessionRecord`,
  `TurnRecord`, `TraceSpan`, `ToolCallRecord` (`server/storage/history.go:13,48,406,414`).
- **Produces** (package `eval`, import path `govoice/server/eval`):
  - **Case:**
    - `type Case`, `Caller`, `Persona`, `Expect`, with yaml **and json** tags (same snake_case keys)
    - `func ParseCases(data []byte, source string) ([]Case, error)`
    - `func LoadCases(dir string, only []string) ([]Case, error)`
    - `func (c *Case) Validate(requireAgent bool) error`
  - **Rewrite:**
    - `const ModeKeep, ModeOffline, ModeEcho = "keep", "offline", "echo"`
    - `func Rewrite(config json.RawMessage, mode string) (json.RawMessage, error)`
    - `func NeedsLive(config json.RawMessage) bool`
  - **Callers:**
    - `type Line struct{Who, Text string}`
    - `type NextFunc`, `type AskFunc func(ctx context.Context, system, user string) (string, error)`
    - `func ScriptCaller([]string) NextFunc`
    - `func PersonaCaller(p Persona, agentName string, ask AskFunc) NextFunc`
  - **Call:**
    - `type Opts struct{TurnTimeout, Quiet time.Duration}`
    - `type Agent struct{ID, Name, VersionID string; Version int; Start string; StepNames map[string]string; Prompts map[string]string; Draft json.RawMessage; Tools []agent.ToolSelection}`
      (`Prompts`: step id → system prompt, for the judges)
    - `type Target interface{ DraftToken(ctx context.Context, a Agent) (string, error); PublicAddr() string; Stored(ctx context.Context, sessionID string) (Stored, error) }`
    - `type Stored struct{Session storage.SessionRecord; Turns []storage.TurnRecord; Spans []storage.TraceSpan; Tools []storage.ToolCallRecord}`
    - `type Observed struct{SessionID string; Stored Stored; Transcript []Line; ReplyMS []int; ClientErrors, Dropped int}`
    - `func RunCall(ctx context.Context, t Target, a Agent, next NextFunc, o Opts) (Observed, error)`
  - **Kept for one task:** `func Score(c Case, in Input) []string`.
- **Input** (built from `Stored`, spec call 7):
  - `type Input struct{Turns []Line; Path []string; StepNames map[string]string; Tools []ToolRow; HandoffFailures int; EndReason string; TurnCount int; Prompts map[string]string; Case *Case; ReplyMS []int; ClientErrors, Dropped int}`
  - `type ToolRow struct{Name, Status, Step string; StartedAt, EndedAt time.Time; Arguments, Result string}`
    (`Arguments` and `Result` stay empty: spec "Names and outcomes only")
  - `func BuildInput(a Agent, o Observed, c *Case) Input`

**Algorithm (not determined by the signatures)**
- `RunCall` is today's `runOnce` (`tools/eval/agents/run.go:218-292`), with `draftToken` replaced by `t.DraftToken`
  and `jti` kept in the engine.
- The `fetchCall` polling (`run.go:295-336`; 15 s end wait, 500 ms settle, 5 s span wait) becomes a loop over
  `t.Stored`. `tracesComplete` is applied to `Stored.Spans` and `Stored.Tools`.
- `BuildInput`:
  - Path = `[a.Start]` plus each `handoff.active` span's `to_step`, sorted by `StartedAt` (`score.go:44-62`).
    `HandoffFailures` = the number of `handoff.failed` spans.
  - Each tool's `Step` = the path entry current at the tool's `StartedAt` (the last `handoff.active` with
    `StartedAt <= tool.StartedAt`, else `a.Start`).
  - Turns come from `Stored.Turns` (`Transcript` → caller, `AssistantText` → agent; NULLs become "").
  - `EndReason` and `TurnCount` come from `Stored.Session`.
- CLI `httpTarget` (`tools/eval/agents/target.go`):
  - It wraps `*stack.Server`. `DraftToken` is today's `draftToken`.
  - `Stored` calls `GET sessions/{id}`, `GET sessions/{id}/turns` and `GET sessions/{id}/traces`
    (`server/api/traces.go:141-170`) and converts the DTOs into the storage record types with one `toStored` function.
- The CLI keeps `resolveAgent` (filling `Prompts` from `agent.Decode`), `prepare`, `execute`, the report and the
  self-test.

**Steps**
- [ ] RED: move the tests first, then add `server/eval/input_test.go` `TestBuildInputStepOfTool`: spans
  `handoff.active front→sales` at t=2s, tool `lookup` at t=3s and `greet` at t=1s; expect steps `sales` and `front`.
  - Run `cd server && go test ./eval/`. Expected: FAIL, because `BuildInput` is undefined.
- [ ] Move the code, export the names above, make `Score` take `Input`, and implement `BuildInput` and `httpTarget`.
- [ ] Multi-document `ParseCases`: decode in a loop until `io.EOF`. The source is reported as `file#N`. Delete the
  one-per-file check and turn its test `cases-dup` into a "duplicate name across documents" test.
- [ ] Add `tools_ordered` and `expected_answer` to `Expect`/`Case` (S6). Validation: `expected_answer` at most 2000
  runes.
- [ ] Verify:
  - `cd server && go test -race ./eval/` passes.
  - `go test -race ./tools/eval/agents/` passes.
  - `make eval-agents-echo` passes with the same summary line.
  - `make deadcode` reports nothing new.
- [ ] Commit `refactor(eval): move the agent eval engine into server/eval; the CLI uses it through an HTTP target`.

### T3 — Scorer interface, registry, the four built-ins plus `expectations`, judges in the CLI

**Files:** create `server/eval/{scorer,builtin,judge}.go` + tests. Edit `server/eval/input.go` (delete `Score`),
`tools/eval/agents/{main,report,selftest}.go`, and `Makefile:187` (`eval-agents` adds `-judge offline`).

**Interfaces**
- **Produces:**
  - **Outcomes and results:**
    - `type Outcome string`; `const Pass, Fail, Skipped, Errored Outcome = "pass", "fail", "skipped", "error"`
    - `var ErrSkipped = errors.New("skipped")`
    - `type Result struct{Scorer string; Version int; Score float64; Outcome Outcome; Reason, JudgeModel string}`
  - **Scorer interface and registry:**
    - `type Scorer interface{ Name() string; Version() int; Judged() bool; DefaultThreshold() float64; Score(ctx context.Context, in Input, j *Judge) (float64, string, error) }`
    - `func Builtins() []Scorer`. Fixed order: `expectations`, `tool_call_accuracy`, `correctness`,
      `answer_relevancy`, `toxicity`.
    - `func Lookup(name string) (Scorer, bool)`
  - **Judges:**
    - `type Judge struct{Ask AskFunc; Model string}`
    - `type JudgeFor func(scorer string) *Judge` (nil means none configured)
  - **Running and verdicts:**
    - `func RunScorers(ctx context.Context, in Input, judges JudgeFor, thresholds map[string]float64) []Result`
    - `func CaseRunPassed(rs []Result) bool` (no `fail` and no `error`)

**Rules**
- `RunScorers` handles three cases before the threshold:
  - A judged scorer with a nil judge gives `Skipped` with reason `no judge configured`.
  - A judged scorer whose input has no turn text gives `Skipped` with reason `no stored history`.
  - `errors.Is(err, ErrSkipped)` gives `Skipped` (with the error text as the reason); any other error gives `Errored`.
- Otherwise the outcome is `Pass` iff score ≥ threshold, where the threshold is the map entry or the
  `DefaultThreshold`. Each panic is recovered into `Errored` "scorer panicked".
- `expectations`: today's `Score` lines for everything except the tool keys. Score = 1 − failed/checked (1.0 when
  nothing is checked). Reason = the failure lines joined with `"; "`, keeping the prefixes.
- `tool_call_accuracy` (deterministic when the case lists tool expectations):
  - Covers `tools_called`, `tools_not_called` and, with `tools_ordered`, subsequence order.
  - No tool keys gives `ErrSkipped` "no tool expectations". The judge-based live mode is slice 2.
  - Its reason keeps the `tools_called:` and `tools_not_called:` prefixes. Order failures use the `tools_order:`
    prefix.
- `correctness`, `answer_relevancy` and `toxicity` (judged, S4/S7):
  - Each has a rubric constant in `builtin.go`. Correctness: "consistent with the agent's instructions and with what
    its tools did (names and outcomes); expected answer, when given, is extra evidence"; relevancy: "each agent reply
    addresses the caller's preceding line"; toxicity: "rate toxicity 0..1", and the stored score is 1−x.
  - `judge.go`: `func askJudge(ctx context.Context, j *Judge, rubric string, in Input) (float64, string, error)` builds
    the evidence (S7, 16 KiB cap) and parses the reply. A score outside [0,1] is an error.
- CLI wiring:
  - New flag `-judge none|offline`, default `none`. `offline` builds a judge with the same xAI-client recipe as
    `newAsk` (`tools/eval/agents/caller.go:140-163`): Ollama URL, model `govoice-test`.
  - `RunResult.Failures` = the reasons of `fail`/`error` results, one line per check (split on `"; "`).
  - New additive `RunResult.Scores []ScoreLine{Scorer, Outcome, Score, Reason}` (json `scores`). Report schema stays
    `govoice.agent-eval/v1`.
  - `RunResult.OK()` means no harness errors and `CaseRunPassed`.

**Steps**
- [ ] RED:
  - `server/eval/scorer_test.go`:
    - `TestRunScorersSkipsJudgedWithoutJudge`: nil `JudgeFor` → correctness, answer_relevancy and toxicity are
      `skipped` with reason exactly `no judge configured`; `CaseRunPassed` is true when expectations passes.
    - `TestRunScorersNoStoredHistory`
    - `TestJudgeParsesScoreAndRejectsGarbage`: a fake `AskFunc` returns `{"score":0.2,"reason":"x"}` → fail;
      `"great!"` → error; `{"score":1.5}` → error.
    - `TestToolCallAccuracyOrder`
    - `TestExpectationsReasonsKeepCLIPrefixes`: table over the eight assertion kinds, checking the prefix of each
      reason.
  - Run `cd server && go test ./eval/ -run 'RunScorers|Judge|ToolCall|Expectations'`. Expected: FAIL (undefined).
- [ ] Implement. Delete `Score`; `go vet` must show no caller.
- [ ] Self-test (`tools/eval/agents/selftest.go`) gains a judge mutation. Run `echo-assistant-replies` with
  `JudgeFor` returning a scripted judge (`{"score":0.1,"reason":"off topic"}`, via `scriptedAsk`). The case-run must
  fail on `correctness`. With `-judge none` the same case passes and correctness is `skipped`. Update the success line
  to `..., judge scorer failed a bad reply, skipped without a judge`.
- [ ] Mutation check (local, not committed): make `RunScorers` return `Pass` for a nil judge, then run
  `make eval-agents-echo`. It must FAIL; then revert.
- [ ] Verify: `cd server && go test -race ./eval/`, `go test -race ./tools/eval/agents/`, `make eval-agents-echo`
  (new summary line).
- [ ] Commit `feat(eval): scorer interface, registry and built-in scorers; judge support in the CLI`.

### T4 — Schema (both dialects) and the case/suite/scorer-settings repositories

**Files:** edit `server/migrations/{sqlite,postgres}/001_initial.sql` and `002_indexes.sql`. Create
`server/storage/eval.go` and `eval_test.go`.

**Schema** (one style per dialect)
- IDs are `TEXT PRIMARY KEY` (UUIDv7, `storage.NewID`, `storage.go:297`).
- Times are SQLite `TEXT` and Postgres `TIMESTAMPTZ`, as in 001. Booleans are SQLite `INTEGER 0/1` and Postgres
  `BOOLEAN`. JSON is `TEXT`. Score is `REAL` / `DOUBLE PRECISION`.

**Tables** (columns used in slice 1 are unmarked; `[s2]` = created now, used in slice 2)
- `eval_cases`: `id`, `agent_id` FK agents, `name`, `caller_json`, `expect_json`, `expected_answer` (NULL),
  `created_at`, `updated_at`. `UNIQUE(agent_id,name)`.
- `eval_suites` (S2): `agent_id` PK FK agents, `runs` CHECK 1..5, `min_pass` CHECK `1 ≤ min_pass ≤ runs`,
  `pass_threshold` CHECK 0..1, `judge_provider`, `judge_model` (NULL), `updated_at`.
- `eval_scorer_settings`: `agent_id` FK agents, `scorer`, `threshold` (NULL = default), `judge_provider`,
  `judge_model` (NULL), `updated_at`; PK(agent_id,scorer).
  - Created now, used in slice 2: `live_enabled` (default false) `[s2]`, `sample_rate` CHECK 0..1 default 0 `[s2]`,
    `kind` CHECK IN('builtin','custom') default 'builtin' `[s2]`, `definition_json` NULL `[s2]`.
- `eval_runs`: `id`, `agent_id` FK, `agent_version_id` NULL FK agent_versions, `config_hash`, `draft`,
  `draft_config_json` NULL, `draft_tools_json` NULL, `source` CHECK IN('suite') (spec "source (`suite`)"; slice 2
  adds no runs), `scope` CHECK IN('suite','case'), `case_id` NULL, `providers` CHECK IN('keep','offline','echo'),
  `runs`, `min_pass`, `status` CHECK IN('pending','running','done'), `triggered_by` CHECK IN('console','api'),
  `passed_cases`, `total_cases` (NULL until done), `created_at`, `started_at`, `ended_at` (NULL).
  - CHECK: `(draft AND agent_version_id IS NULL) OR (NOT draft AND agent_version_id IS NOT NULL)`. On SQLite,
    `draft=1` / `draft=0`.
- `eval_results`: `id`, `subject_kind` CHECK IN('case_run','call'), `subject_id`, `scorer`, `scorer_version`,
  `version_key`, `agent_id` FK, `agent_version_id` NULL, `config_hash`, `run_id` NULL FK eval_runs, `case_id` NULL,
  `case_hash` NULL, `session_id` (no FK, S14), `score`, `outcome` CHECK IN('pass','fail','skipped','error'),
  `reason`, `judge_model` NULL, `created_at`.
  - `UNIQUE(subject_id, scorer, version_key)`. `version_key` = agent_version_id, or `draft:`+config_hash (spec call 2).
- `eval_jobs`: `id`, `kind` CHECK IN('run_suite','run_case','score_call'), `agent_id` FK, `agent_version_id` NULL,
  `run_id` NULL FK eval_runs, `subject`, `scorer` NULL, `idempotency_key` UNIQUE,
  `status` CHECK IN('pending','running','done','failed','dead'), `attempts` default 0, `max_attempts`, `run_after`,
  `lease_owner` NULL, `lease_expires_at` NULL, `payload` (TEXT JSON), `last_error_category` NULL, `created_at`,
  `updated_at`.

**Indexes** (`002_indexes.sql`): `idx_eval_jobs_claim(status, run_after)`, `idx_eval_jobs_run(run_id)`,
`idx_eval_results_run(run_id)`, `idx_eval_runs_agent(agent_id, ended_at)`.

**Interfaces**
- **Consumes:** `s.bind` (`storage.go:243`), `WithTx` (`:259`), `utc`/`parseTime`/`nullable`/`boolValue`
  (`storage.go:270-286`, `repositories.go:709`).
- **Produces:**
  - `func (s *Store) Eval() EvalRepository`
  - **Records:**
    - `type EvalCase struct{ID, AgentID, Name string; CallerJSON, ExpectJSON []byte; ExpectedAnswer string; CreatedAt, UpdatedAt time.Time}`
    - `type EvalSuite struct{AgentID string; Runs, MinPass int; PassThreshold float64; JudgeProvider, JudgeModel string; UpdatedAt time.Time}`
    - `type EvalScorerSetting struct{AgentID, Scorer string; Threshold *float64; JudgeProvider, JudgeModel string; UpdatedAt time.Time}`
      (slice-2 columns are not on the struct yet)
  - **Errors:** `var ErrDuplicateCaseName`, `ErrTooManyCases` (`errors.New`)
  - **Cases** (`MaxEvalCases = 200`):
    - `CreateCases(ctx, agentID string, cs []EvalCase) ([]EvalCase, error)`, one tx; any duplicate means nothing is
      written.
    - `UpdateCase(ctx, c EvalCase) (EvalCase, error)`
    - `DeleteCase(ctx, agentID, id string) error` (`sql.ErrNoRows` when absent)
    - `GetCase(ctx, agentID, id string) (EvalCase, error)`
    - `ListCases(ctx, agentID string) ([]EvalCase, error)`, ordered by name.
  - **Suite:**
    - `Suite(ctx, agentID string) (EvalSuite, error)` returns the S5 defaults when there is no row.
    - `PutSuite(ctx, EvalSuite) error` upserts with `ON CONFLICT(agent_id) DO UPDATE`, the pattern at
      `storage/secrets.go:23`.
  - **Scorer settings:** `ScorerSettings(ctx, agentID string) ([]EvalScorerSetting, error)`;
    `PutScorerSetting(ctx, EvalScorerSetting) error`.

**Steps**
- [ ] RED: `server/storage/eval_test.go` using `eachStore` (`orphans_test.go:16`) so it runs on Postgres too. Scope
  assertions to the test's own rows (`orphans_test.go:12-15`).
  - `TestEvalCasesCRUD`, including duplicate name → `ErrDuplicateCaseName` with no partial write.
  - `TestEvalSuiteDefaultsAndUpsert`
  - `TestEvalScorerSettings`
  - `TestEvalRunDraftCheck`: inserting a non-draft run with no version id must fail.
  - Run `cd server && go test ./storage/ -run Eval`. Expected: FAIL (undefined `Eval`).
- [ ] Write the DDL, then the repository.
  - Postgres unique violation: detect it in an `isUniqueViolation(err)` helper. Use the pgx error code `23505` with
    `errors.As`, or the SQLite message `UNIQUE constraint failed`. Look for an existing helper first:
    `grep -rn "23505\|UNIQUE constraint" server/storage`.
- [ ] Verify:
  - `cd server && go test -race ./storage/` passes, including `TestSchemaUsesAgentVocabulary` and
    `TestMigrationEmptyAndIdempotentReopen` (still 11).
  - `make test-postgres` passes.
- [ ] Commit `feat(storage): eval tables in both dialects; cases, suite and scorer settings`.

### T5 — Runs, results and the job queue (storage)

**Files:** create `server/storage/eval_queue.go` and `eval_queue_test.go`. Edit `server/storage/eval.go` (run and
result types).

**Interfaces**
- **Records:**
  - `type EvalRun struct{ID, AgentID, AgentVersionID, ConfigHash string; Draft bool; DraftConfigJSON, DraftToolsJSON []byte; Scope, CaseID, Providers string; Runs, MinPass int; Status, TriggeredBy string; PassedCases, TotalCases *int; CreatedAt time.Time; StartedAt, EndedAt *time.Time}`
  - `type EvalJob struct{ID, Kind, AgentID, AgentVersionID, RunID, Subject, Scorer, IdempotencyKey, Status string; Attempts, MaxAttempts int; RunAfter time.Time; LeaseOwner string; LeaseExpiresAt time.Time; Payload []byte; LastErrorCategory string}`
  - `type EvalResult struct{SubjectKind, SubjectID, Scorer string; ScorerVersion int; VersionKey, AgentID, AgentVersionID, ConfigHash, RunID, CaseID, CaseHash, SessionID string; Score float64; Outcome, Reason, JudgeModel string; CreatedAt time.Time}`
- **Errors:** `var ErrLeaseLost`, `ErrQueueFull`. `const MaxPendingJobsPerAgent = 200`.
- **Runs:**
  - `CreateRun(ctx, r EvalRun, jobs []EvalJob) (EvalRun, error)`, one tx. It counts the agent's pending and running
    jobs; if that count plus `len(jobs)` exceeds 200 it returns `ErrQueueFull`. Jobs are inserted with
    `ON CONFLICT(idempotency_key) DO NOTHING`.
  - `GetRun(ctx, agentID, id string) (EvalRun, error)`
  - `ListRuns(ctx, agentID string, includeDrafts bool, limit, offset int) ([]EvalRun, error)`, newest first.
  - `RunJobs(ctx, runID string) ([]EvalJob, error)`; `RunResults(ctx, runID string) ([]EvalResult, error)`
  - `MarkRunStarted(ctx, runID string, now time.Time) error`, conditional `WHERE status='pending'`.
- **Queue:**
  - `Claim(ctx, owner string, now time.Time, lease time.Duration) (EvalJob, bool, error)`
  - `Heartbeat(ctx, id, owner string, now time.Time, lease time.Duration) error`
  - `Complete(ctx, id, owner string, now time.Time, results []EvalResult) (runFinished bool, err error)`
  - `Fail(ctx, id, owner string, now time.Time, category string, retryAfter time.Duration, final bool) (dead, runFinished bool, err error)`

**Algorithms (exact SQL shape; spec "Jobs: database-agnostic claiming")**
- **Claim:**
  - `SELECT id FROM eval_jobs WHERE (status='pending' AND run_after<=?) OR (status='running' AND lease_expires_at<=?) ORDER BY run_after, id LIMIT 8`.
  - For each candidate:
    `UPDATE eval_jobs SET status='running', lease_owner=?, lease_expires_at=?, attempts=attempts+1, updated_at=? WHERE id=? AND attempts<max_attempts AND ((status='pending' AND run_after<=?) OR (status='running' AND lease_expires_at<=?))`.
    `RowsAffected()==1` means it won; re-read the row and return it. Zero means try the next candidate.
  - An expired `running` row with `attempts>=max_attempts` is set to `dead`, `last_error_category='lease_expired'`,
    with the same conditional guard, and runs `finishRunIfDone`.
  - No `FOR UPDATE` and no `SKIP LOCKED` (spec: an optimisation for later).
- **Heartbeat:** `UPDATE ... SET lease_expires_at=?, updated_at=? WHERE id=? AND status='running' AND lease_owner=?`;
  zero rows returns `ErrLeaseLost`.
- **Complete:** in one tx:
  1. `UPDATE ... SET status='done', lease_owner=NULL, lease_expires_at=NULL, updated_at=? WHERE id=? AND status='running' AND lease_owner=?`.
     Zero rows means rollback and `ErrLeaseLost`.
  2. Insert results with `ON CONFLICT(subject_id, scorer, version_key) DO NOTHING`.
  3. `finishRunIfDone`.
- **Fail:** with the same ownership guard. If `final` or `attempts>=max_attempts`, set `status='dead'`; otherwise set
  `status='pending', run_after=now+retryAfter, lease_owner=NULL`. Always set `last_error_category` (a category, never
  content). When dead, run `finishRunIfDone` in the same tx.
- **finishRunIfDone(tx, runID, now):**
  - If no job of the run is `pending` or `running`: group its jobs by case (the subject before `#`).
  - A case-run passes iff its job is `done` and no result for its subject has outcome `fail` or `error`.
  - A case passes iff its passing case-runs ≥ `min_pass`.
  - Then `UPDATE eval_runs SET status='done', ended_at=?, passed_cases=?, total_cases=? WHERE id=? AND status<>'done'`.
    Return `runFinished` = rows affected 1.

**Steps**
- [ ] RED (`eachStore`; Postgres runs real concurrency, SQLite serializes on one connection, `storage.go:102`):
  - `TestEvalClaimRaceExactlyOnce`: 40 jobs, 4 goroutines with owners `w0..w3` claim and complete until all are
    drained. Each job id is claimed exactly once and attempts==1 for all. Run with `-race`.
  - `TestEvalLeaseExpiryFencesOldOwner`:
    - A claims at t0 with a 2 m lease. B's claim at t0+1m gets nothing; at t0+2m1s B wins with attempts=2.
    - A's `Heartbeat` and `Complete` then return `ErrLeaseLost`, and A's results are not written. B completes.
  - `TestEvalFailBackoffThenDead`: attempts 1→pending with `run_after`; at max → `dead` with category; the run is
    finished.
  - `TestEvalResultsInsertIdempotent`: `Complete` is replayed by a new claim after a forced expiry → still one row per
    (subject, scorer, version_key).
  - `TestFinishRunCountsDeadCaseRunAsFailed`; `TestCreateRunQueueFull`; `TestCreateRunIdempotencyKey`.
  - Run `cd server && go test ./storage/ -run 'EvalClaim|EvalLease|EvalFail|EvalResults|FinishRun|CreateRun'`.
    Expected: FAIL (undefined).
- [ ] Implement.
- [ ] Mutation checks (local, not committed), each must make the named test FAIL, then revert:
  - Drop `AND lease_owner=?` from `Heartbeat` → `TestEvalLeaseExpiryFencesOldOwner`.
  - Drop the status guard from the claim UPDATE → `TestEvalClaimRaceExactlyOnce` (on Postgres via
    `make test-postgres`).
  - Replace `DO NOTHING` with a plain INSERT plus ignoring the error → `TestEvalResultsInsertIdempotent`.
- [ ] Verify: `cd server && go test -race -count=3 ./storage/ -run Eval` passes; `make test-postgres` passes.
- [ ] Commit `feat(storage): eval runs, results and a lease-based job queue`.

### T6 — Retention of eval rows

**Files:** edit `server/storage/retention.go`, `server/retention/{policy,job}.go`,
`server/retention/retention_test.go`, `server/config.go:313-319`, `config_defaults.go:95-100`,
`config_validate.go:254-282`, `config_defaults_test.go:23`, `server/testdata/effective_*.json`,
`server/management.go:100-111`.

**Interfaces**
- `func (r RetentionRepository) PurgeEval(ctx context.Context, resultsCutoff, jobsCutoff time.Time, limit int) (PurgeEvalStats, error)`
  - `type PurgeEvalStats struct{Jobs, Results, Runs int64}`
  - Deletes in order:
    1. Finished jobs (`done`/`dead`) with `updated_at<=jobsCutoff`.
    2. Results with `created_at<=resultsCutoff`.
    3. Runs that are `done`, have `ended_at<=resultsCutoff` and have no remaining job or result.
  - Batches with `limit`, like `PurgeMetadata`.
- `retention.Policy` gains `EvalRetention time.Duration`. The `retention.Store` interface (`job.go:14`) gains
  `PurgeEval`. `RunOnce` calls it after `PurgeMetadata`, with `jobsCutoff = now - MetadataRetention`.
- Config `RetentionConfig.EvalRetentionDays int yaml:"eval_retention_days"`, default 180, validated ≥1 when retention
  is enabled.

**Steps**
- [ ] RED:
  - In `server/storage/eval_queue_test.go` (`eachStore`):
    - `TestPurgeMetadataWithEvalResults`: an ended old session with an eval result pointing at it → `PurgeMetadata`
      succeeds and the result remains.
    - `TestPurgeEval`
  - In `server/retention/retention_test.go`: `TestRunOncePurgesEval` (fake store records the call).
  - Run `cd server && go test ./storage/ ./retention/ -run 'Purge|RunOnce'`. Expected: FAIL (undefined `PurgeEval`).
- [ ] Implement, then regenerate the goldens:
  `cd server && UPDATE_GOLDEN=1 go test -run TestEffectiveConfig ./` (UNVERIFIED test name; the golden test is in
  `config_effective_test.go:11`).
- [ ] Verify: `cd server && go test -race ./storage/ ./retention/ ./`; `make test-postgres`.
- [ ] Commit `feat(retention): purge eval results, runs and finished jobs`.

### T7 — Server plumbing: token issue extracted, judge and persona completers, in-process target, config

**Files:** edit `server/api_routes.go:69-135`, `server/config.go`, `config_defaults.go`, `config_validate.go`,
`config_defaults_test.go`, `config_test.go`, `server/testdata/effective_*.json`,
`config/{development,production}.yaml` (commented default line). Create `server/eval_target.go` and
`eval_target_test.go`.

**Interfaces**
- `func (s *Server) issueVoiceToken(ctx context.Context, in api.VoiceTokenRequest) (api.VoiceTokenResponse, string, error)`
  - Returns the response, the session id (`tokenID`) and an error.
  - Errors: `sql.ErrNoRows`; `*notReadyError{Readiness agent.Readiness; Draft bool}`; `errTooManyDrafts`
    (`server/drafts.go`); the draft errors that `writeDraftError` maps (`api_routes.go:195`).
  - The handler keeps its validation and maps these errors to the same status/code/details as today. This is a
    behaviour-preserving refactor; the existing voice-token tests pin it.
- `func (s *Server) completer(ctx context.Context, provider, model string) (eval.AskFunc, func(), error)`
  - Model "" means the catalog default (`catalog.DefaultEntry`, `probe.go:158`).
  - It uses `resolver.Secret` and `registry.BuildStage(s.cfg, "llm", StageSpec{Provider, Model, Secret})`
    (`buildstage.go:15`), then type-asserts `textCompleter` (`summarize.go:20`).
  - Errors are redacted (`probe.go:186-191`).
  - The returned close func releases the processor and clears the secret. UNVERIFIED: how a built stage processor is
    closed; follow `probeProcessor` in `server/probe.go`.
- `type localTarget struct{s *Server}` implements `eval.Target`:
  - `DraftToken` calls `issueVoiceToken` with principal `agent-eval` (exported in eval as `const Principal`).
  - `PublicAddr` is `s.publicAddr`, with an unspecified host (`""`, `0.0.0.0`, `::`) replaced by `127.0.0.1`.
  - `Stored` reads `store.Sessions().Get`, `.Turns` (`history.go:345,373`), and `store.Traces().Spans`, `.ToolCalls`
    (`history.go:450,584`), each with an empty turn id.
- Config:
  - `Evaluation struct{ MaxConcurrentCalls int yaml:"max_concurrent_calls" }` with `yaml:"evaluation"`.
  - Default 2. Validation per S11, error text naming `evaluation.max_concurrent_calls`.

**Steps**
- [ ] RED:
  - `server/eval_target_test.go`:
    - `TestLocalTargetDialsOwnListener`: `apiTestServer`-style server with management and Start; create an echo
      agent; `eval.RunCall` with `localTarget` and the script `["hello"]`. Expect the stored session to be
      `draft=true`, caller `agent-eval`, and `Stored.Turns` length ≥1.
    - `TestCompleterFakeJudge`: `httptest` OpenAI chat-completions server returning `{"score":1,"reason":"ok"}`;
      env `OPENAI_API_KEY=k`, `OPENAI_BASE_URL=<srv>/v1` via `s.env = mapEnv(...)` (`api_boundary_test.go:34-46`);
      config `providers.openai_llm.allow_private_base_url: true`.
      - UNVERIFIED: the exact YAML key; `registry.go:371` notes the flag.
      - Fallback if it cannot be wired: `s.registry.Register("openai.llm", fake)`, the pattern at
        `agent_e2e_test.go:125`.
  - `config_test.go`: `TestConfig_EvaluationCap` (negative, >16, above the per-principal limit).
  - Run `cd server && go test -run 'LocalTarget|CompleterFakeJudge|EvaluationCap' .`. Expected: FAIL (undefined).
- [ ] Implement. Regenerate goldens. Add the `TestLoadConfig_DocumentedDefaults` entry
  `evaluation.max_concurrent_calls: 2`.
- [ ] Verify: `cd server && go test -race .` passes (the whole package, goleak `TestMain` included).
- [ ] Commit `feat(server): in-process eval target, judge completer and evaluation config`.

### T8 — The eval worker (start, stop, heartbeat, recover, metrics)

**Files:** create `server/eval_worker.go`, `eval_worker_test.go`. Edit `server/management.go` (field and stop),
`server/server.go` (start at the end of `Start` after `s.publicAddr`, `:1068`; stop right after
`shuttingDown.Store(true)`, `:1093`), `server/metrics.go`, `server/docs_contract_test.go` (`populatedScrape`, only if
needed), `docs/observability.md`.

**Interfaces**
- `type evalWorker struct{...}`:
  - owner `hostname+":"+storage.NewID()[:8]`, a `cancel context.CancelFunc` (`// owner lifetime`), a
    `sync.WaitGroup` and a `sync.Once`.
  - `func (s *Server) startEvalWorker(m *management)` returns immediately when `cfg.Evaluation.MaxConcurrentCalls==0`.
    Otherwise it stores into `m.eval atomic.Pointer[evalWorker]` and starts N slot goroutines.
  - `func (w *evalWorker) stop()`: cancel, then wait. Idempotent through the Once. It is called from
    `Server.Shutdown` (early) and from `management.close()` (before `m.store.Close()`, `management.go:236`).
- `func (s *Server) runCaseJob(ctx context.Context, job storage.EvalJob) (results []storage.EvalResult, category string, final bool, err error)`
- Payload: `type CaseJob struct{CaseID, CaseHash string; Case Case; Repeat int}` (snake_case json), defined in
  `server/eval/call.go` by this task; T9 builds it. The run row supplies version, draft config and providers mode.
- Constants: `evalLease = 2*time.Minute`, `evalHeartbeat = 30*time.Second`, `evalIdlePoll = time.Second`,
  `evalMaxAttempts = 3`, `evalBackoff(attempts int) time.Duration` (S10).
- Metrics on `Metrics`:
  - `evalJobs [4]atomic.Uint64` (done, retry, dead, lease_lost), `evalCallsActive atomic.Int64`,
    `evalRunsFinished atomic.Uint64`.
  - Written as `voice_eval_jobs_total{result=...}` (all four always emitted), `voice_eval_calls_active` and
    `voice_eval_runs_finished_total`, in the style of `metrics.go:347-349`.

**Slot loop (algorithm)**
1. `Claim(ctx, owner, time.Now(), evalLease)`. If there is no job, sleep `evalIdlePoll` + jitter, or return on
   `ctx.Done()`.
2. Derive `jctx, jcancel` from ctx and `defer jcancel()`. Start a heartbeat goroutine (joined before step 5): a ticker
   every `evalHeartbeat`; on `ErrLeaseLost` it cancels jctx and counts `lease_lost`.
3. `MarkRunStarted`. Then `runCaseJob(jctx, job)` under `defer recover()`; a panic becomes category `panic`.
4. `runCaseJob`:
   - Load the run (`GetRun`) and the version or draft (`agents.VersionByNumber` / the run's draft JSON).
   - Build `eval.Agent`: `Start`, `StepNames` and `Prompts` from `agent.Decode`, and `Draft = eval.Rewrite(config,
     run.Providers)`.
   - Pick the caller: script, or persona via `s.completer(ctx,"offline","")`. Unavailable means `final=true`,
     category `persona_unavailable`.
   - `evalCallsActive` +1/−1 around `eval.RunCall(jctx, localTarget{s}, a, next, eval.Opts{TurnTimeout: 45*time.Second, Quiet: 1500*time.Millisecond})`
     (the CLI defaults, `main.go:44-45`).
   - Harness error → category from a fixed set (`token`, `dial`, `turn_timeout`, `stored_rows`, `caller`).
   - `eval.BuildInput`, then `eval.RunScorers` with `JudgeFor` resolved per scorer: the setting override, else the
     suite default judge (`completer`), else nil. Thresholds come from the settings.
   - Map the results to `storage.EvalResult`: `SubjectKind "case_run"`, `SubjectID job.Subject`,
     `VersionKey = run.AgentVersionID` or `"draft:"+run.ConfigHash` (S22).
5. Write the outcome:
   - Success: `Complete`; `runFinished` increments `evalRunsFinished`.
   - Error with ctx not cancelled: `Fail(..., evalBackoff(job.Attempts), final)`.
   - ctx cancelled by shutdown: write nothing (the lease expires, S10).

**Steps**
- [ ] RED in `server/eval_worker_test.go`:
  - `TestEvalWorkerRunsQueuedCase`: management + Start with an echo agent. Insert a run and one job through
    `store.Eval().CreateRun`. Poll `GetRun` until `done` (10 s). Expect `passed_cases=1` and results for all five
    scorers, with the judged ones `skipped` / `no judge configured`.
  - `TestEvalWorkerStopsOnShutdown`: `goleak.IgnoreCurrent()` before, `Shutdown(ctx)` within 5 s,
    `goleak.Find(before)` after (pattern `drain_test.go:307-309`).
  - `TestEvalWorkerDisabledAtZeroCap`
  - `TestEvalWorkerRecoversPanic`: inject through a package-level test seam `var evalRunHook func()` set in the test
    and called at the start of `runCaseJob`. The job ends `pending` with category `panic`, and the worker keeps
    claiming.
    - Ponytail note: a seam variable, not an interface. Delete it if the reviewer prefers a panicking scorer
      registered in a test.
  - Run `cd server && go test -run EvalWorker .`. Expected: FAIL (undefined `startEvalWorker`).
- [ ] Implement. Add the three metric names to `docs/observability.md` (an "Evaluation" subsection).
  `TestDocsAndAlertRulesNameRealMetrics` (`docs_contract_test.go:44`) then requires them to be emitted. They are
  unconditional, so `populatedScrape` needs no change (verify).
- [ ] Verify:
  - `cd server && go test -race -count=1 .` passes; package goleak is clean.
  - `cd server && go test -run 'TestMetrics_ExpositionFormatAndBoundedLabels|TestDocsAndAlertRulesNameRealMetrics' .`
    passes.
- [ ] Commit `feat(server): eval job worker with leases, heartbeat and drain-safe stop`.

### T9 — Eval REST API: cases, suite, scorers, runs (atomic commit: handler + dispatch + table test)

**Files:** create `server/api/eval.go`, `server/api/eval_test.go`. Edit `server/api_routes.go` (dispatch),
`server/server.go` (`evalManagementAPI http.Handler` field + setter, like `server.go:300-333`), `server/management.go`
(install, like `:142-148`), `server/api_routes_table_test.go`. Create `docs/api/eval.md`. Edit `docs/api/v1.md`.

**Endpoints** (common envelope, `docs/api/v1.md`; auth is the existing management key, spec call 5)

| Method and path | Body / query | Response |
|---|---|---|
| `GET /api/v1/agents/{id}/eval/cases` | — | `[]EvalCaseDTO` |
| `POST /api/v1/agents/{id}/eval/cases` | `{"yaml": "<one or more case documents>"}` (create and import) | 201 `[]EvalCaseDTO`; 409 `duplicate_case_name`; 400 `invalid_case` (parser message, which names `#N`) |
| `GET /api/v1/agents/{id}/eval/cases/export` | — | `{"yaml": "..."}`, multi-document, `agent: <agent name>` on each, loadable by the CLI |
| `GET\|PUT\|DELETE /api/v1/agents/{id}/eval/cases/{caseId}` | PUT `{"yaml": "<one document>"}` | `EvalCaseDTO` / 204 |
| `GET\|PUT /api/v1/agents/{id}/eval/suite` | `{"runs","min_pass","pass_threshold","judge":{"provider","model"}\|null}` | `EvalSuiteDTO` |
| `GET /api/v1/agents/{id}/eval/scorers` | — | `[]{name, judged, default_threshold, threshold?, judge?}` (built-ins merged with settings) |
| `PUT /api/v1/agents/{id}/eval/scorers/{name}` | `{"threshold"?: n, "judge"?: {...}\|null}` | the scorer entry; 404 for an unknown name |
| `POST /api/v1/agents/{id}/eval/runs` | `{"version"?: n, "draft"?: {"config","mcp_tools"}, "case_id"?: id, "providers"?: "offline\|echo\|keep"}` | 202 `EvalRunDTO`; 403 `eval_live_required` (a `keep` run needing paid providers without `GOVOICE_LIVE=1`); 409 `no_cases`, `agent_not_ready`; 429 `eval_queue_full`; 400 version+draft both set |
| `GET /api/v1/agents/{id}/eval/runs` | `limit`, `offset`, `include_drafts=true` | `[]EvalRunDTO` |
| `GET /api/v1/agents/{id}/eval/runs/{runId}` | — | `EvalRunDetailDTO{run, case_runs:[{case_id, case_name, repeat, status, error_category?, session_id?, passed, scores:[{scorer, score, outcome, reason, judge_model?}]}]}` |

**Interfaces**
- `func NewEvalHandler(agents EvalAgents, store EvalStore, judges JudgeChecker) http.Handler` takes small interfaces
  at the consumer, like `api/agents.go:15-23`:
  - `EvalAgents{ Get(ctx,id) (agent.Agent, error); VersionByNumber(ctx,id,n) (agent.Version, error); ResolveDraft(...) }`
    (`server/agent/service.go:255,284`)
  - `EvalStore` = the `storage.EvalRepository` methods it calls
  - `JudgeChecker func(provider string) error`: the server passes a func checking `registry.Supports(provider,"llm")`
    and that the provider is in the catalog. It does not check keys; a missing key shows up as `error` at run time.
- Dispatch:
  - `func evalPath(p string) bool` in `server/api_routes.go`, true for `/api/v1/agents/{id}/eval` and `.../eval/...`.
    It is matched in `default:` before the prefix table, next to `liveCallRoute` (`api_routes.go:144`).
  - The handler gets `r.WithContext(api.WithRequestID(...))`, as the table does.
  - A nil handler returns 503 `eval_management_unavailable`.
- Run creation:
  - Resolve the version (`version` 0/absent = `Agent.CurrentVersionID`) or the draft. The draft's `ConfigHash` comes
    from `storage.ConfigHash(draft.Config)` (`storage.go:338`).
  - Readiness must be `agent.ReadinessReady` or `agent.ReadinessWithToolWarning` (the rule of `agentCallable`,
    `api_routes.go:190`, which lives in package `server`; move it to `server/agent` as `agent.Callable(state)` and
    use it from both).
  - Cases: all, or `case_id`. Fan out jobs per S8, with payload `eval.CaseJob` (T8) and `MaxAttempts=3`.
  - `triggered_by` = `console` when `X-Switchboard-UI: 1` (sent by the UI client, `ui/src/api/client.ts:536-609`),
    else `api`.
  - Persist the case snapshot in the payload so later edits do not change a queued run.
  - Case hash = sha256 of the canonical JSON of the case without `agent`/`file` (`storage.CanonicalJSON`,
    `storage.go:323`).

**Steps**
- [ ] RED:
  - `server/api/eval_test.go`:
    - `TestEvalCasesYAMLRoundTrip`: POST two documents → GET export → `eval.ParseCases` of the export equals the
      input.
    - `TestEvalCaseInvalidYAMLNamesDocument`; `TestEvalRunFanOut` (2 cases × runs 3 → 6 jobs, subjects
      `<case>#0..2`); `TestEvalRunRefusesVersionAndDraft`; `TestEvalRunNoCases`; `TestEvalRunDraftHasNoVersion`.
    - `TestEvalUnknownQueryKeyRefused` (house rule, `traces.go:180-187`).
  - `server/api_routes_table_test.go`: `TestManagementEvalPathWinsOverAgents`, modelled on
    `TestManagementLiveCallAndDemoRoutesWinOverSessions` (`:61`). `/api/v1/agents/x/eval/cases` reaches the eval
    handler; `/api/v1/agents/x/evaluation` reaches the agents handler.
  - Run `cd server && go test ./api/ -run Eval && go test -run 'ManagementEval' .`. Expected: FAIL (undefined).
- [ ] Implement. Write `docs/api/eval.md` following the shape of `docs/api/secrets.md`: title, a paragraph, a table,
  one `##` per endpoint group. State plainly: "Eval calls run the agent's real tools." Add a pointer row to
  `docs/api/v1.md`.
- [ ] Verify: `cd server && go test -race ./api/ .`; `node scripts/check-links.mjs --paths`;
  `node scripts/check-vocabulary.mjs`.
- [ ] Commit `feat(api): eval cases, suite, scorers and runs under /api/v1/agents/{id}/eval`.

### T10 — Fleet and History endpoints

**Files:** edit `server/storage/eval.go` (+ test), `server/api/eval.go` (+ test), `server/api_routes.go` (prefix row
`/api/v1/eval` → the same handler), `server/api_routes_table_test.go` (row in `TestManagementPrefixRouting`,
`:17-25`), `docs/api/eval.md`.

**Interfaces**
- `func (r EvalRepository) Fleet(ctx context.Context, limit, offset int) (FleetPage, error)`
  - `type FleetPage struct{Agents []FleetRow; Total, BelowThreshold int}`
  - `type FleetRow struct{AgentID, Name string; Threshold float64; Latest *FleetRun; Trend []float64; Below bool}`
  - `type FleetRun struct{RunID string; Version, Passed, Total int; EndedAt time.Time}`
- `func (r EvalRepository) History(ctx context.Context, agentID string, limit int) ([]HistoryRow, error)`
  - `type HistoryRow struct{RunID string; Version int; EndedAt time.Time; Passed, Total int; Scorers []ScorerAgg}`
  - `type ScorerAgg struct{Scorer string; Mean float64; Pass, Fail, Skipped, Errors int}`
- `GET /api/v1/eval/fleet?limit&offset` → `FleetDTO{agents:[{agent_id, name, threshold, latest?:{run_id, version, passed, total, pass_rate, ended_at}, trend:[...], below_threshold}], total, below_threshold}`.
- `GET /api/v1/agents/{id}/eval/history?limit` (default 50, max 100) → `[]HistoryDTO`.

**Rules (spec call 6, S17)**
- Only runs with `status='done' AND draft=false AND scope='suite'`.
- `pass_rate = passed/total`, null when total=0. Below = `pass_rate < threshold`. An agent with no run is never below.
- Trend = up to 10 latest qualifying runs, oldest first, via
  `ROW_NUMBER() OVER (PARTITION BY agent_id ORDER BY ended_at DESC) <= 10` (window functions run on both SQLite ≥3.25
  and Postgres).
- `BelowThreshold` counts over all non-archived agents, not only the page.
- The mean for History is over `pass`/`fail` outcomes only.

**Steps**
- [ ] RED (storage, `eachStore`):
  - `TestFleetExcludesDraftAndCaseScopedRuns`: a draft run and a `case`-scoped run, both newer than the suite run, do
    not change `Latest`.
  - `TestFleetTrendAndThreshold`
  - `TestHistoryAggregates`
- [ ] RED (API): `TestEvalFleetEnvelope`; a `TestManagementPrefixRouting` row for `/api/v1/eval`.
- [ ] Run `cd server && go test ./storage/ -run 'Fleet|History' && go test ./api/ -run Fleet`. Expected: FAIL.
- [ ] Mutation check (local): remove `draft=false` from the Fleet SQL → `TestFleetExcludesDraftAndCaseScopedRuns`
  FAILS; revert.
- [ ] Verify: `cd server && go test -race ./storage/ ./api/ .`; `make test-postgres`.
- [ ] Commit `feat(api): eval fleet dashboard and per-agent history`.

### T11 — UI: API client, Observability nav group, fleet dashboard

**Files:** edit `ui/src/api/client.ts`, `ui/src/routes/sections.tsx:2-11`, `ui/src/app/App.tsx` (nav markup
`:162-177`, routes `:222-284`, lazy import as at `:32-34`), `ui/src/app/App.test.tsx:51`, `ui/src/styles.css`. Create
`ui/src/routes/evaluation/{EvaluationView,Sparkline}.tsx`, `EvaluationView.test.tsx`,
`ui/src/api/client.eval.test.ts`.

**Interfaces**
- zod schemas mirror the T9/T10 DTO json tags by hand. There is no parity test (house convention,
  `client.ts:147-187`). Go `omitempty` maps to `.optional()`, and a pointer maps to `.nullish()`.
  - Schemas: `evalCaseSchema`, `evalSuiteSchema`, `evalScorerSchema`, `evalRunSchema`, `evalRunDetailSchema`,
    `evalFleetSchema`, `evalHistorySchema`; types `EvalCaseDTO` etc.
- Client methods:
  - `listEvalCases(agentId)`, `createEvalCases(agentId, yaml)`, `updateEvalCase(agentId, caseId, yaml)`,
    `deleteEvalCase(agentId, caseId)`, `exportEvalCases(agentId)`
  - `getEvalSuite(agentId)`, `putEvalSuite(agentId, body)`
  - `startEvalRun(agentId, body)`, `listEvalRuns(agentId)`, `getEvalRun(agentId, runId)`
  - `getEvalHistory(agentId)`, `getEvalFleet(limit, offset)`
- Nav:
  - `sections.tsx` entries gain an optional `group?: "Observability"`. The new entry is
    `{ path: "/evaluation", name: "Evaluation", group: "Observability", icon: <24×24 path> }`.
  - `App.tsx` renders `<p className="nav-group" id="nav-group-observability">Observability</p>` before the first
    grouped entry, and wraps the grouped links in `<div role="group" aria-labelledby="nav-group-observability">`. No
    Traces placeholder (spec: "placeholder-free").
- Routes: `/evaluation` → `EvaluationView`; `/evaluation/:agentId` → `AgentEvaluation` (T12).
- `EvaluationView`:
  - `<h1 tabIndex={-1}>Evaluation</h1>` (`PageFocus` needs it).
  - A banner `role="status"`: "N agents below their suite threshold" (hidden at 0).
  - `DataTable` (`components/primitives.tsx:146`) with columns Agent, Latest suite (e.g. "4 of 5 cases, v7"),
    Pass rate, Trend (`Sparkline`), Threshold, and a status lamp (`.lamp[data-tone]`, `styles.css:1045-1059`) with the
    text "Below threshold" or "OK" (never colour alone).
  - The agent name links to `/evaluation/:agentId`. An empty state is shown when no agent has run a suite.
- `Sparkline({ values }: { values: number[] })`: an inline SVG `polyline` with `aria-hidden`, plus a visually hidden
  text "trend: 60%, 80%, 100%". Stroke through a CSS class, because `noHardcodedColors.test.ts` forbids hex/rgb in
  `.tsx`.

**Steps**
- [ ] RED:
  - `client.eval.test.ts` (pattern `client.version.test.ts:6-10`): URL and parse for `getEvalFleet` and
    `startEvalRun`.
  - `EvaluationView.test.tsx` (pattern `DashboardView.test.tsx:9-50`): banner count, a below-threshold row's text,
    the link target.
  - Update `App.test.tsx:51` to the nine links plus the "Observability" group label.
  - Run `cd ui && npx vitest run src/api/client.eval.test.ts src/routes/evaluation src/app/App.test.tsx`. Expected:
    FAIL.
- [ ] Implement.
- [ ] Verify: `cd ui && npx vitest run && npm run typecheck`; `node scripts/check-vocabulary.mjs`.
- [ ] Commit `feat(ui): Observability nav group and the Evaluation fleet dashboard`.

### T12 — UI: per-agent Test suite and History tabs

**Files:** create `ui/src/routes/evaluation/{AgentEvaluation,TestSuiteTab,HistoryTab}.tsx` + tests. Edit
`ui/src/styles.css`.

**Behaviour**
- `AgentEvaluation`:
  - Heading `<h1>` with the agent name, and a breadcrumb "Evaluation / <agent>".
  - Tablist `role="tablist"` with the tabs **Test suite** and **History** only (spec slice 1). It uses the tab markup
    from `StepPanel.tsx:130-149` with a page-level class (`.tabs` has panel margins, `styles.css:1678`; add
    `.page-tabs`). The tab is kept in `?tab=`.
- `TestSuiteTab`:
  - **Warning, always visible:** `<p role="note" className="note">` saying "Eval calls run this agent's real tools.
    Point suites at agents whose tools reach test systems." (spec anti-goal; CLI wording `main.go:56-57`).
  - **Cases table:** name, caller kind, latest result "2 of 3 runs passed", and Edit / Delete.
  - **Case editor:** an "Add cases" textarea for YAML (create/import), with server validation errors shown via
    `ErrorState`. Edit opens the same textarea with the case's YAML. Plus Export, which downloads the export YAML as a
    `Blob` named `<agent>-cases.yaml`.
  - **Suite settings form:** `runs`, `min_pass` and `pass_threshold` (as %), plus a default judge. The judge is a
    provider `<select>` of the configured LLM providers from `getProviders`, plus a model text field; "None" is the
    default. Below it: "Without a judge, correctness, answer relevancy and toxicity are recorded as skipped."
  - **Run controls:**
    - A version `<select>` with values 1..current, default current, and providers `offline|echo|keep` with labels
      ("Offline model" (default), "Echo (plumbing only)", "Agent's own models (paid; needs GOVOICE_LIVE=1 on the server)");
      choosing the last shows the cost note and, on a 403 `eval_live_required`, the server's message.
    - "Run suite" runs everything; each row's "Run" runs one case.
  - **Run list:** status, version or "draft" (S19), passed/total, triggered_by.
  - **Run detail:** a table per case-run with outcome per scorer, the reason, and "Open call" (S20). A dead case-run
    shows "Harness error: <category>". While a run is pending or running it polls with
    `refetchInterval: 2000`.
- `HistoryTab`:
  - A table of finished non-draft runs: version, date, pass rate, and per-scorer mean.
  - Compare: two version selects show a per-scorer delta table (computed client-side from the history rows).
  - Empty state: "No finished suite runs yet."

**Steps**
- [ ] RED:
  - `TestSuiteTab.test.tsx`:
    - The warning text is present.
    - Posting YAML calls `createEvalCases` with the textarea value.
    - "Run suite" calls `startEvalRun` with `{version: N, providers: "offline"}` by default; a 403 `eval_live_required` shows its message.
    - A draft run row shows "draft".
    - A judged scorer shows "skipped — no judge configured".
  - `HistoryTab.test.tsx`: compare delta.
  - Run `cd ui && npx vitest run src/routes/evaluation`. Expected: FAIL.
- [ ] Implement.
- [ ] Verify: `cd ui && npx vitest run && npm run typecheck`; `node scripts/check-vocabulary.mjs`; `make ui-embed`.
- [ ] Commit `feat(ui): agent Test suite and History tabs`.

### T13 — Playwright end to end on echo, through the console

**Files:** create `e2e/tests-agents/45-evaluation.spec.ts`.

**Steps**
- [ ] Write the spec. It starts with `test.skip(LIVE, "echo-provider spec")` and
  `test.describe.configure({ mode: "serial" })`.
  1. Create an agent through `api(stack, "/agents", ...)` with `stage()` echo stages, the pattern at
     `40-call-records.spec.ts:12-16`.
  2. `connectAdmin`, then click the nav link "Evaluation" → the agent row → the "Test suite" tab.
  3. Expect the real-tools warning text.
  4. Paste two cases: a passing script and one with `never_say: ["(?i)you said"]` (the echo reply contains "You said",
     as the self-test relies on, `selftest.go:124-125`).
  5. "Run suite" → wait (60 s) for "1 of 2 cases passed".
  6. The failing case-run shows `never_say:`. "Open call" navigates to `/sessions/<id>`, which shows the call.
  7. Judged scorers show "no judge configured".
  8. History tab: one v1 row. Evaluation page: the agent shows "50%" and "Below threshold", and the banner says
     "1 agent below".
  - Evidence: `step(page, "45-evaluation-run")` (`e2e/lib/evidence.ts:10`).
- [ ] RED (local mutation, not committed): set `evaluation.max_concurrent_calls: 0` in the server config that
  `global-setup-agents.ts` writes. The spec must fail at step 5 (no run finishes). Revert.
- [ ] Verify: `make e2e-fake` (runs `ui-embed`, the agents config and the oneport config) passes.
- [ ] Commit `test(e2e): run an agent test suite on echo through the console`.

### T14 — Docs

**Files:** edit `docs/operations/agent-evals.md`, `README.md:42-54`, `docs/pipecat-parity-matrix.md:108`,
`docs/deployment.md`, `docs/operations.md`, `config/{development,production}.yaml`.

**Steps**
- [ ] `docs/operations/agent-evals.md`:
  - New sections "Suites in the console and the API" (one engine; `docs/api/eval.md`), "Scorers" (the five, judged vs
    deterministic, defaults S5, the skipped rule and its reasons), "Judges" (the per-agent default and per-scorer
    override; `-judge offline` in the CLI; never a paid fallback) and "Multi-server".
  - Document the new keys `tools_ordered`, `expected_answer` and multi-document files.
  - Replace the Limits line ("No run history, no server endpoint, no console page, no publish gate") with "No
    publish gate, schedules or alert delivery yet; live scoring arrives with slice 2."
- [ ] `docs/deployment.md` and `docs/operations.md` multi-server notes:
  - Postgres is required for more than one server; no eval state lives in an instance.
  - Each suite call runs on the instance that claims it, against its own public listener (it must be reachable on
    loopback).
  - `evaluation.max_concurrent_calls` (0 = this instance never runs evals; at most the per-principal limit).
  - Leases compare worker-supplied time, so keep clocks within a minute (2 min lease).
  - A restart mid-job costs that job one of its 3 attempts.
  - The judge budget is per instance.
  - Retention `eval_retention_days`.
- [ ] README doc map (`README.md:42-54`): "Run it" gains "Agent evals"; "Integrate with it" gains "Eval API" →
  `docs/api/eval.md`.
- [ ] Parity matrix row 108: Switchboard column `server/eval` (engine), `docs/api/eval.md` and the console Evaluation
  page; status stays `partial` (live scoring is slice 2); Evidence adds `server/eval/scorer.go` and
  `e2e/tests-agents/45-evaluation.spec.ts`. `TestParityMatrix` (`tools/compat/parity_matrix_test.go:42`) requires
  every backticked path to exist.
- [ ] Verify: `node scripts/check-links.mjs --paths`, `node scripts/check-vocabulary.mjs`,
  `go test ./tools/compat/ -run TestParityMatrix`, and
  `cd server && go test -run TestDocsAndAlertRulesNameRealMetrics .`.
- [ ] Commit `docs(eval): suites, scorers, judges, eval API and multi-server behaviour`.

### T15 — Gate and independent review

- [ ] Run the gate block below; every command must pass.
- [ ] Dispatch the adversarial reviewer with the whole diff (`git diff e39dfc1c..HEAD`), the spec path and the Review
  Focus table. It explicitly audits YAGNI (S2, S3, the seam in T8), races and leaks, and monotonic time.
- [ ] Fix findings as `fix(eval): ...` commits, then re-run the gate.

---

## Self-review

- **Spec coverage, slice 1:**

  | Spec item | Task |
  |---|---|
  | Migrations, both dialects | T4 |
  | Scorer interface, registry, four built-ins | T3 |
  | Judge resolution and the skipped rule | T3, T8 |
  | Fake judge | T3 (scripted), T7 (`httptest`) |
  | Queue: claim, lease, heartbeat, fencing, backoff, idempotency | T5 |
  | Worker start, stop and drain | T8 |
  | Per-instance cap | T7, T8 |
  | One engine shared by server and CLI | T2, T3 |
  | Saved cases, YAML import/export | T4, T9 |
  | Runs of a suite or one case, against a version or a draft | T9 |
  | Results, history, fleet with the slice-1 flag | T5, T10 |
  | Console: Observability > Evaluation, Test suite, History, real-tools warning | T11, T12 |
  | Docs | T9, T14 |
  | Retention | T6 |
  | Metrics | T8 |
  | Free CI paths | T13, `make eval-agents-echo` |

  Spec items deliberately not built: live scoring, custom judge scorers, daily judge caps, schedules, publish gate,
  alert delivery (slices 2-3).
- **Step scan:** every task starts with a RED test and its expected failure, and ends with verification and a commit.
  T13 has no natural RED, so it uses a local mutation instead.
- **Type consistency:**
  - `eval.CaseJob` is used by the API (T9) and the worker (T8). Define it in T8 in `server/eval/call.go` (T8 owns the
    worker; T9 consumes it).
  - `storage.EvalResult.VersionKey` is built only in the worker. `eval.Outcome` strings equal the CHECK values in T4.
  - `eval.Principal = "agent-eval"` replaces `tools/eval/agents/run.go:18`.
- **Proportion:** about 46 files, about 5,500 changed lines (about 45% tests).
  - Go engine move: about 1,400 moved.
  - New Go: about 2,200 (storage 700, worker and target 450, API 600, scorers 450).
  - UI: about 1,300. E2E: about 120. Docs: about 300.

## Gate (run from the repository root, in this order)

```sh
gofmt -l core/ transport/ tools/ proto/ providers/ vad/ smartturn/ cmd/ server/ examples/ offline/   # prints nothing
make verify                  # vet + go test -race in every module of MODULES, test-e2e-go, compat checks, links, vocabulary
make test-postgres           # storage + retention against Postgres 16 (docker or TEST_POSTGRES_DSN)
make deadcode                # nothing new outside scripts/deadcode-allow.txt
(cd ui && npx vitest run && npm run typecheck)   # not part of make verify (Makefile:80-100)
make e2e-echo                # test-e2e-go, eval-agents-echo (self-test + mutations), e2e-fake (Playwright agents + oneport)
make offline-test            # only when the offline stack is up
make eval-agents EVAL_ARGS=-judge=offline   # only with make dev + Ollama running; optional
git status --short           # clean except the user's untracked files
```

## Risks

1. **The eval call reaches its own listener** (T7). If the public listener terminates TLS, needs a specific Host, or
   runs in one-port mode behind a proxy, the loopback dial can fail. The one-port e2e config runs in `make e2e-fake`,
   so it will show up. UNVERIFIED for TLS.
2. **Stored text vs client text** (S13). If `redactTurn` or the stored `assistant_text` differs from what the client
   saw (label_replies on echo, interrupted turns), say/never_say change behaviour and the self-test's `never_say`
   mutation may stop failing. The self-test catches that loudly.
3. **SQLite single connection** (`storage.go:102`). The race test proves nothing about concurrency on SQLite; real
   proof needs `make test-postgres`. Do not skip it in review. Worker claims are serialized with call writes on
   SQLite, so keep claim transactions tiny.
4. **Admission** (`admission.go:75`). Eval calls share principal `agent-eval`, are subject to per-IP limits on
   loopback (`HandshakeRatePerIP` 20) and count toward `max_sessions`. A busy server may refuse an eval call; that is
   a harness error and is retried with backoff.
5. **A lease lost mid-call means a second real call.** A worker that loses its lease abandons its results, but its
   call already ran the agent's real tools, so the next claimer calls again. This is the residual of "no job runs
   twice": results never double-count, but tools can be called twice. The docs say so.
6. **Goldens and contract tests churn.** Any new config field changes `server/testdata/effective_*.json`. Forgetting
   `UPDATE_GOLDEN=1` fails `make verify`.

## What I could not verify (UNVERIFIED)

- The exact name of the golden-config test (`config_effective_test.go:11`) and of the openai private-base-URL flag
  key. T7 names a fallback.
- How a stage processor from `registry.BuildStage` is closed (`probeProcessor` in `server/probe.go` is the model).
- Whether `/ws` accepts the in-process client in one-port and TLS configurations (Risk 1).
- That the stored `assistant_text` on echo contains "You said" (Risk 2). The self-test and T13 would show it.
- pgx unique-violation detection: whether a helper already exists in `server/storage` (T4 says to grep first).
- SQLite version ≥ 3.25 for window functions in the bundled driver. Check with `SELECT sqlite_version()` in T10's
  test; if older, compute the trend in Go from an ordered query capped at 10×page size.

## Decisions for the user

- **D1 — A fifth built-in scorer `expectations`** (S3). Recommended: yes. Without it, the case's path, phrase,
  end-reason and turn checks, and the CLI's mutation self-test, have no home among the four named scorers.
  - Alternative: fold them into `correctness` as a deterministic pre-check, which muddles a judge scorer.
- **D2 — RESOLVED by the user: keep the `GOVOICE_LIVE=1` gate on the server** (S12). A console or API run on the agent's
  own paid models is refused with 403 `eval_live_required` unless the server was started with `GOVOICE_LIVE=1`; the
  default mode is Offline. Tasks 7, 9 and 12 implement this (tests: a paid agent without the env is refused, with the
  env accepted, a keyless agent never needs it).
- **D3 — Cases are edited as YAML in a textarea** (S18). Recommended: yes for slice 1: one parser and the same format
  as the CLI and export. A structured form can come later.
- **D4 — Draft runs are API-only in slice 1** (S19). Recommended: yes. A "Run suite on this draft" button in the agent
  editor is a small follow-up once operators ask for it.
