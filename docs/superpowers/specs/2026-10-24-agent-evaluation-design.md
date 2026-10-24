# Agent evaluation and monitoring: design

Date: 2026-10-24. Status: draft for review. Nothing in this document is built yet.

## Intent

Operators change an agent's prompts, steps, handoffs and tools and publish them on faith. The harness we have (`tools/eval/agents`, Phase 11) can run a scripted or simulated caller against a stored agent and score the outcome, but a run prints a report and forgets it. An operator cannot see whether an agent still works a month after shipping, whether version 7 beats version 6, or whether live calls are quietly getting worse.

Build a quality loop in two stages, the way Vapi and Retell offer pre-ship testing and Mastra offers sampled scoring of live traffic:

1. **Suites:** saved, scripted cases per agent, run against agent versions on demand, scored and stored.
2. **Live monitoring:** sampled real calls scored in the background, stored and trended per agent and version.

Both write to one results model, use one scorer interface, and surface in a new **Evaluation** page under an **Observability** section of the console. A **Traces** page will join Observability later and is not part of this work, but every score must link to the call it came from.

## Vocabulary

Agent (what an operator builds and versions), step, handoff, call (API word: session). Suite = the saved cases of one agent. Case = one scenario. Run = one execution of a suite or case against one agent version. Scorer = a named unit that turns a call into a score. Judge = the LLM a model-graded scorer calls.

## Goals

- A suite per agent: saved cases (script or persona caller, expected path, tools, phrases, optional expected answer), editable in the console and the API, importable and exportable as the existing case YAML.
- On-demand suite runs from the console, the API and the CLI. A run executes against an immutable agent version (or a draft) and stores per-case, per-run, per-scorer results.
- Named scorers, day one: **correctness**, **tool-call accuracy**, **answer relevancy**, **toxicity**. Easy to author more (see Scorers).
- Live monitoring: per agent, attach scorers with a **sampling rate**; sampled calls are scored after they end, off the call path.
- A fleet dashboard (all agents: latest suite pass rate, live scores, trend, alert flag) with per-agent tabs: Test suite, Live scoring, History.
- Multi-server safe on Postgres; correct and simple on SQLite (one instance).
- Judge models are any already configured LLM provider; secrets stay environment-only.

## Non-goals

- Server-side scheduling of suites in the first slices (external cron or CI can call the API).
- A publish gate and alert delivery in slice 1 and 2 (slice 3).
- Capturing tool arguments or results (deferred, see Privacy).
- Audio-level evaluation (speech quality, turn-taking). Suites use text turns with the `silence` speech provider, as the CLI does today.
- A Traces viewer.
- Real-time scoring during a call.

## Anti-goals

- Live scoring must not widen what is stored about a call, add latency to a call, or let a judge outage affect calls.
- No job that runs twice and double-counts.
- No eval call that books, writes or sends on real systems by surprise: eval calls run the agent's real tools, so the console must say so plainly (the CLI docs already do).
- No paid judge or model usage without the operator having chosen the provider.

## Constraints

- `core/` stays server-agnostic; evaluation lives in `server/` (jobs, results, API) and `tools/` (the runner), consuming `core/` only through existing frames and APIs. Everything added must be consumed on a real path (AGENTS.md).
- Vocabulary guard (`scripts/check-vocabulary.mjs`) applies.
- Both SQL dialects, migrations rewritten in place (nothing has shipped; the database can be wiped).
- Gates: `make verify`, `make e2e-echo`, `make offline-test` when the stack is up, `make test-postgres`. Free by default; paid runs behind `GOVOICE_LIVE=1`.

## Decided with the user

- Two stages, suites first, then live monitoring.
- Named scorers: correctness, tool-call accuracy, answer relevancy, toxicity; architecture makes more easy to author.
- Console: Observability section with child links Traces (later) and Evaluation (now). Evaluation front door is a **fleet dashboard**, with **per-agent tabs** (Test suite, Live scoring, History) inside it.
- Judge: any pre-configured LLM provider.
- Correctness = consistent with the agent's instructions and with what its tools did (names and outcomes), judged from the call's turns, step path and tool rows; a suite case may add an optional expected answer as extra evidence.
- Names and outcomes only for tool data. Opt-in capture of tool arguments and results is deferred and recorded in `docs/roadmap-next-phases.md`; scorer inputs carry optional `Arguments` and `Result` fields, empty today.
- Job claiming is database-agnostic: a row in the database, claimed with a conditional update.
- No server-side scheduler in the first slices; if added later, a schedule is a row claimed the same way.

## Design

### Scorers

A scorer is a registered, named, versioned unit with a fixed shape:

- **Input:** the call's turns (caller text, agent reply, per turn), the step path, tool rows (name, status, step, timing; optional `Arguments` and `Result` fields for later), the agent's instructions and settings, and, for suite cases, the case (expectations, optional expected answer).
- **Scope:** one turn or the whole call.
- **Output:** score in [0,1], pass/fail against a threshold, and a short reason that is stored with the score.
- **Method:** deterministic code, or a model-graded judge (provider and model resolved from configuration).

Built-ins are Go types implementing the interface, registered by name. Operators add **custom judge scorers** in configuration: a name, a rubric prompt, a scale, and a judge model, no code. The same scorer runs on suite runs and on live calls. Deterministic scorers cost nothing; judge scorers use the operator's chosen provider, and sampling controls cost.

Tool-call accuracy is deterministic where a case lists expectations (tools called, not called, order) and judge-based on live calls (was a reasonable tool used for this request, by name and outcome).

### Data model (both dialects)

- `eval_cases`: per agent (not per version), id, name, caller (script or persona), expectations, optional expected answer, created/updated. A change is versioned by the results: every result records the case's content hash.
- `eval_scorer_settings`: per agent, per scorer: enabled for live, sampling rate, threshold, judge model override, kind (built-in name or custom definition).
- `eval_runs`: one suite or case execution against an agent version (or a draft): source (`suite`), status, provider configuration used, started/ended, who triggered.
- `eval_results`: one row per (subject, scorer, agent version): subject is a suite case-run or a live call (session id); score, pass, reason, judge model, case hash, links to the session. Unique key (subject, scorer, agent version); writes use `ON CONFLICT DO NOTHING` so retries never double-count.
- `eval_jobs`: the job queue (below).

Suite and live results share `eval_results`, so the dashboard, trends and alerts read one table.

### Jobs: database-agnostic claiming

One `eval_jobs` table: id, kind (`run_suite`, `run_case`, `score_call`), agent, agent version, subject, scorer, idempotency key (unique), status (`pending`, `running`, `done`, `failed`, `dead`), attempts, max attempts, run_after, lease_owner, lease_expires_at, payload, last error category.

- **Enqueue:** a suite run enqueues jobs; a sampled call enqueues `score_call` at call end (written with the call's other end-of-call rows).
- **Claim:** a worker selects candidate rows that are pending and due, or running with an expired lease, then runs `UPDATE … SET status='running', lease_owner=?, lease_expires_at=?, attempts=attempts+1 WHERE id=? AND <still claimable>`. Exactly one row affected means it won. Portable SQL; Postgres may later use `SKIP LOCKED` as an optimisation without changing the contract.
- **Heartbeat and fencing:** the worker extends its lease with a conditional update on `lease_owner`; zero rows affected means the lease was lost and the worker stops. Completion and failure writes carry the same ownership check.
- **Retries:** backoff via `run_after`; `dead` after max attempts; error categories only, never content.
- **Clock skew:** leases compare against a time the worker supplies, so they are generous against skew and the docs say so.
- **Suite runs execute on the instance that claims them**, against that instance's own local public listener, because the draft token store and admission limits are in memory per process (the API already requires token issuance and connection on the same instance).
- Eval and draft calls are marked and excluded from live sampling; eval calls count against a configurable concurrent-eval-call cap per instance.

### Live scoring

At call end the instance decides sampling deterministically from the call id and scorer (a stable hash against the rate), so every instance agrees. Only calls on non-draft, non-eval sessions are eligible. A background worker, off the call path, claims `score_call` jobs and reads what is already stored for the call (turn rows, step path from handoff spans, tool rows, after the existing redaction rules), runs the scorers and writes results. Live scoring reads only stored history: it does not widen what is stored. If the privacy settings leave no transcript stored, there is nothing to score, and the Live scoring tab says so rather than silently scoring nothing.

### Console

New **Observability** nav section with **Evaluation** (and a placeholder-free design for Traces to join later).

- **Evaluation (fleet dashboard):** one row per agent: latest suite pass rate, live scores (last 7 days), trend sparkline, alert flag against the agent's thresholds; a banner counts agents below threshold; click through to the agent.
- **Agent page, three tabs:** **Test suite** (cases, add/edit, run all, per-case pass counts over repeated runs, link to the call behind each failure); **Live scoring** (attached scorers, sampling rates, thresholds, judge model, recent scored calls each linking to the call); **History** (scores by agent version over time, version comparison).
- Every score links to its call detail (and later its trace). The console states that eval calls run the agent's real tools.

### Multi-server behaviour

Postgres is the shared store for multiple servers; SQLite means one server. No eval state lives in an instance: everything is rows, instances only claim work. Dashboards read the database only. Retention purges old results and finished jobs. The judge budget is per instance unless a shared counter is added later; the docs say so.

### Privacy and cost

- Live scoring sends a sampled call's already-stored, already-redacted text to the operator's chosen judge provider. The operator opts in per agent by attaching a model-graded scorer; sampling and a per-agent daily judge-call cap bound the spend.
- Provider keys come from the environment only; nothing about evaluation stores keys.
- Tool arguments and results are not stored or scored (deferred, see roadmap Phase 4).

## Slices

1. **Foundation and suites:** migrations (`eval_*` tables), the scorer interface and registry with the four built-ins, the job queue and workers, per-agent saved cases, API for cases, runs and results, on-demand runs from the console and the CLI (the CLI becomes a client of the same engine where practical), the fleet dashboard and the agent Test suite and History tabs, docs. Free: runs on echo and offline in CI and `make e2e-echo`.
2. **Live monitoring:** sampling, `score_call` jobs, the Live scoring tab, judge scorers on configured providers, custom judge scorers, daily judge caps, retention.
3. **Operations:** schedules (rows claimed like jobs), alert delivery, publish gate with an audited override, metrics for Prometheus.

## Calls made after review (builder check)

These settle questions a fresh reader raised; each is my call, open to change.

1. **Where the judge comes from.** Each agent has an optional **default judge** (a provider and model from the configured LLM providers); a scorer setting may override it. If no judge resolves for a model-graded scorer, the result is recorded as **skipped** with the reason "no judge configured". It is never a pass or a fail, and it never falls back to a paid provider. Free paths: `make eval-agents` uses the offline Ollama model as judge; unit tests and `make e2e-echo` use a fake judge server (an httptest endpoint speaking the provider protocol), the same way existing tests fake providers. Slice 1 therefore has a testable definition for correctness and answer relevancy without any paid key.
2. **Draft runs.** A run against a draft is stored in `eval_runs` and `eval_results` with the agent version empty and the draft's config hash as its key. The Test suite tab shows it as "draft". Draft results are excluded from the History tab, version comparison, the dashboard and alerts.
3. **Persona caller and repeats.** The persona caller defaults to the offline Ollama model; a paid caller needs `GOVOICE_LIVE=1` and counts toward a per-run call cap, not the judge cap. A suite stores `runs` and `min_pass` (the CLI's `-runs` and `-min-pass`), so repeated runs are a suite setting, and the operator can also just run again.
4. **Reaching the local listener.** Every suite run uses a draft token, because speech must be rewritten to `silence` (as the CLI does). The claiming instance issues the draft token itself and connects to its own local listener in the same process, so no cross-instance affinity field is needed. Only that instance ever holds the token.
5. **Authorization.** The new API and console reuse the existing management authentication. There is no new role. The console states that eval calls run the agent's real tools.
6. **Dashboard flag in slice 1.** Suite pass rate = passed cases in the latest finished, non-draft suite run over its cases, compared with a per-agent suite threshold. Live score = the mean score over a window (default 7 days) compared with each scorer's threshold, computed only when at least 5 samples exist. The flag is a computed badge; alert delivery stays in slice 3.
7. **Step path and tool data.** Scorer input is built from the **stored session rows** for both suite runs and live calls, so scorers are identical in both. Suite runs wait for the rows (the runner already polls for handoff spans). If retention or redaction leaves no stored history, the result is **skipped** with "no stored history".
8. **The CLI.** In slice 1 the runner and scoring core move into a package the server imports (under `server/`), and the CLI imports it too, so there is one engine and no fork. The CLI stays a thin client.

## Left to the builder

Table and column names and indexes, API paths and payloads, how the engine is shared between the server worker and the CLI (the runner currently lives in `tools/eval/agents` and talks to the server over HTTP and `/ws`; moving its core into a package the server can call is expected, but how is the builder's choice), the judge prompt templates and score scales for the four built-ins, the concurrent-eval-call cap defaults, lease and backoff values, UI details within the structure above.

## Success

An operator can save cases for an agent, run them against any version from the console, see pass rates per case and scorer stored per version, and compare versions. With scoring attached, sampled real calls produce scores that show up on the fleet dashboard and link to the call. Two servers on one Postgres never process the same job twice and survive a worker crash. The default CI gates remain free, and every paid path needs `GOVOICE_LIVE=1`.
