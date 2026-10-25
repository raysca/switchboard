# Eval API

Saved test cases for an agent, runs of those cases against a published version or a draft, and the score of every case-run. The server makes each call itself, on the instance that claims the job, and scores it from the stored rows. A run is queued and answers `202`; poll it until `status` is `done`.

**Eval calls run the agent's real tools.** A case that makes the agent book, write or send does so on the real system, once per case-run. Point such an agent's tools at a test system first.

All endpoints need the management key (`Authorization: Bearer <key>`) and use the common envelope of [v1.md](v1.md). Unknown JSON fields and unknown query parameters are refused with `400 invalid_request`. A case or run of another agent is `404 not_found`, as is an unknown agent.

| Method and path | Purpose |
|---|---|
| `GET`, `POST /api/v1/agents/{id}/eval/cases` | List the cases; create one or more from YAML |
| `GET /api/v1/agents/{id}/eval/cases/export` | Every case as one YAML file the CLI loads |
| `GET`, `PUT`, `DELETE /api/v1/agents/{id}/eval/cases/{caseId}` | Read, replace or delete a case |
| `GET`, `PUT /api/v1/agents/{id}/eval/suite` | Repeats, required passes, pass threshold and default judge |
| `GET /api/v1/agents/{id}/eval/scorers` | The built-in scorers merged with this agent's settings |
| `PUT /api/v1/agents/{id}/eval/scorers/{name}` | Set one scorer's threshold and judge |
| `POST /api/v1/agents/{id}/eval/runs` | Run the suite or one case |
| `GET /api/v1/agents/{id}/eval/runs` | List runs, newest first (`limit`, `offset`, `include_drafts=true`) |
| `GET /api/v1/agents/{id}/eval/runs/{runId}` | One run with every case-run and its scores |

## Cases

A case is edited as YAML, in the format the CLI reads ([agent evals](../operations/agent-evals.md)): `name`, `caller` (a `script` of lines, or a `persona` with `who`, `goal`, `max_turns`), `expect` and an optional `expected_answer`. The `agent` key is accepted and ignored: a stored case belongs to the agent whose path it was saved under.

```
POST /api/v1/agents/{id}/eval/cases
{ "yaml": "name: books-a-visit\ncaller:\n  script: [\"I need a visit\", \"Tuesday\"]\nexpect:\n  say: [\"(?i)tuesday\"]\n---\nname: refuses-refund\ncaller: {script: [\"refund me\"]}\n" }
```

`201`, `data`: an array of cases, each `{ "id", "name", "caller", "expect", "expected_answer?", "yaml", "created_at", "updated_at" }`. `yaml` is that one case in the file format. The batch is saved in one transaction: if any case is refused, none is saved.

| Status and code | Meaning |
|---|---|
| 400 `invalid_case` | The YAML does not parse or a case fails validation. The message names the case: `cases.yaml#2: caller: set exactly one of script or persona`. |
| 409 `duplicate_case_name` | A name is already used by a saved case of this agent. Names inside one batch must differ too (`400 invalid_case`). |
| 409 `too_many_cases` | An agent keeps at most 200 cases. |
| 413 `payload_too_large` | The request body is over 1 MiB. |

Limits per case: the name is at most 200 characters, a script has at most 20 lines of at most 500 characters, a persona at most 20 turns, `expected_answer` at most 2000 characters, and the whole case at most 16 KiB.

`PUT /cases/{caseId}` takes `{ "yaml" }` holding exactly one document and replaces the case; the id stays. `DELETE` answers `204`. `GET /cases/export` answers `{ "yaml": "..." }`: all cases as YAML documents separated by `---`, each naming the agent, so a file saved in a directory loads with the CLI's `-cases <dir>`.

## Suite and scorers

```
PUT /api/v1/agents/{id}/eval/suite
{ "runs": 3, "min_pass": 2, "pass_threshold": 0.9, "judge": { "provider": "offline", "model": "govoice-test" } }
```

`runs` is 1 to 5 (each case is called that many times), `min_pass` is 1 to `runs` (the case passes when that many of its calls pass), `pass_threshold` is 0 to 1 (the suite pass rate the dashboard compares against). `judge` is the default judge of the model-graded scorers; `null` removes it. All of `runs`, `min_pass` and `pass_threshold` are required. With nothing saved, `GET` answers `runs: 1, min_pass: 1, pass_threshold: 1, judge: null`. Without a judge a model-graded scorer is recorded as `skipped` (`no judge configured`), never as a pass.

`GET /scorers` answers `[{ "name", "judged", "default_threshold", "threshold?", "judge?" }]`: `judged` says whether the scorer is model-graded; `threshold` and `judge` appear when this agent overrides them. `PUT /scorers/{name}` takes `{ "threshold?": n, "judge?": {...} | null }` and replaces the setting, so a field left out goes back to the default. An unknown name is `404`; a `judge` on a deterministic scorer is `400 invalid_request`; a judge provider that cannot complete text is `400 invalid_judge`.

**Paid judges need `GOVOICE_LIVE=1`.** Saving a suite or scorer judge on any provider other than `offline` or `echo` answers `403 eval_live_required` unless the server process was started with `GOVOICE_LIVE=1`. The worker checks again when it scores, so a judge saved on another instance cannot spend credit either.

## Runs

```
POST /api/v1/agents/{id}/eval/runs
{ "version": 4, "case_id": "…", "providers": "offline" }
```

| Field | Meaning |
|---|---|
| `version` | A published version number. Absent or `0`: the agent's current version. |
| `draft` | `{ "config", "mcp_tools?" }`: test an unpublished config, validated as a publish is. Not together with `version`. A draft run has no version, is stored with the draft's `config_hash`, and is left out of History, the dashboard and alerts. |
| `case_id` | Run one case (`scope: "case"`). Absent: every case (`scope: "suite"`). |
| `providers` | `offline` (default), `echo` or `keep`. `offline` and `echo` swap the agent's language model for the free one (the offline model is free only while `providers.offline.llm_endpoint` is on this host; a remote endpoint counts as paid); `keep` calls the agent's own models. Speech is always replaced by the text-only provider. |

The run uses the suite's `runs` and `min_pass`. One job is queued per case and repeat; the cases are copied into the jobs, so editing a case later does not change a queued run. The response is `202`, `data`: the run `{ "id", "agent_id", "version?", "agent_version_id?", "draft", "config_hash", "scope", "case_id?", "providers", "runs", "min_pass", "status", "triggered_by", "passed_cases?", "total_cases?", "created_at", "started_at?", "ended_at?" }`. `status` is `pending`, `running` or `done`; `passed_cases` and `total_cases` are set when it is done. `triggered_by` is `console` when the request carries `X-Switchboard-UI: 1`, otherwise `api`.

| Status and code | Meaning |
|---|---|
| 400 `invalid_request` | `version` and `draft` both set, an unknown `providers`, or a draft the agent service refuses (`agent_not_attached` for a tool the agent does not have). |
| 403 `eval_live_required` | The run would reach a paid model and the server was not started with `GOVOICE_LIVE=1`: `providers: keep` on a paid agent, or `offline` when `providers.offline.llm_endpoint` is not on this host. A case with a persona caller asks the offline model too and follows the same rule. A run whose models are all keyless (echo, loopback offline) never needs it. |
| 404 `not_found` | No such agent, version or case. |
| 409 `no_cases` | The agent has no cases to run. |
| 409 `suite_too_large` | Cases x runs exceeds 200, the most one agent can have queued; it can never fit, so remove cases or lower the suite's `runs`. |
| 409 `agent_not_ready` | The agent is disabled, archived, has no published version, or (for `providers: keep`) is not ready to take calls; `error.details` is its readiness. |
| 429 `eval_queue_full` | The agent's pending or running eval jobs plus this run would exceed 200. Wait for the queue to drain. |

**No idempotency.** `POST /runs` takes no idempotency key: a retried request creates a second run and repeats the real side effects of the agent's tools.

A run is worked by the servers sharing the database, at most `evaluation.max_concurrent_calls` calls at a time on each. A job that loses its server is taken by another after its lease expires, and runs again, so a case-run may call the agent's tools twice.

### One run

`GET /runs/{runId}` answers `data`: `{ "run": {…}, "case_runs": [ … ] }`, one entry per case and repeat:

```json
{ "case_id": "…", "case_name": "books-a-visit", "repeat": 0, "status": "done",
  "session_id": "…", "passed": true,
  "scores": [ { "scorer": "expectations", "score": 1, "outcome": "pass", "reason": "" },
              { "scorer": "correctness", "score": 0, "outcome": "skipped", "reason": "no judge configured" } ] }
```

`status` is the job's: `pending`, `running`, `done` or `dead`. A `dead` case-run could not be made or read back; `error_category` names why (`dial`, `token`, `turn_timeout`, `stored_rows`, `persona_unavailable`, …), never any call content, and it counts as failed. `passed` is true only for a finished case-run in which no scorer failed or errored (a skipped scorer does not count against it). `outcome` is `pass`, `fail`, `skipped` or `error`; `session_id` is the call (`/api/v1/sessions/{id}`) and is empty for a dead case-run; `judge_model` names the judge of a model-graded score. A `reason` is cut at 1000 characters.
