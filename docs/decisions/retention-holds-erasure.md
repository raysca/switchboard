# Retention holds and erasure: spec and plan

Status: reviewed, ready to build. Slice 2 of Phase 8 in [roadmap-next-phases.md](../roadmap-next-phases.md). Slice 1 (status, dry run, metrics, alert) is built; see [privacy.md](../privacy.md#server-data-retention-and-privacy-controls). An independent review of the first draft changed the erase order, the caller-erase confirmation, the audit subject and the call teardown order; the findings and what was done are at the end.

## Goal

An operator can protect a call from the retention job (a hold), and can erase one call, or everything one caller label produced, on request: transcript, trace, tool-call rows, recording and the call row itself. Every hold, release and erasure is recorded.

## Non-goals (later slices)

Per-agent or per-class lifetimes and the policy model; an export before erasure; a console form for erasing by caller (the API exists, the console covers one call); production profiles requiring an explicit retention choice; erasing data we cannot reach: backups, log files, an external trace backend, provider-side retention, metrics (their labels are agent and provider names, never a call or a caller), and recordings on another instance (below).

## What exists today (checked in the code)

- `server/retention` purges by `ended_at` in two passes (content, metadata); `PurgeContent` and `PurgeMetadata` select ended sessions; `Preview` counts the same selection.
- A call's recording is named `<agent id>-<core session number>-<unix time>` (`server/recording.go`); the number is the in-process core session id, not the stored session id, so nothing ties a recording file to a stored call. `CallSpec.StoredSessionID` is set at the call site, so the stored id is available. The janitor (`core/record` `Sweep`) deletes files by modification time only.
- A stored call is the `sessions` row plus `turns`, `tool_calls`, `trace_spans`, `trace_events`, `session_mcp_tools`. Every foreign key is `ON DELETE RESTRICT`, including `trace_events.span_id`, `trace_events.turn_id` and `trace_spans.turn_id`; the current purge deletes events and spans by `session_id` only.
- At call teardown (`finishCall`) the recording is finalised in a goroutine while `release()` ends the stored session, so a session can read as ended before its recording file and sidecar are complete.
- `sessions.caller` is the label an integration chose (at most 128 characters) and has no index. The sessions API is read-only; the table-based router sends every `/api/v1/sessions/...` path to one handler after the live-call paths (`/context`, `/run`).

## Decisions

1. **A hold is a flag on the stored call**: `sessions.held` (boolean, default false) and `sessions.hold_reason` (text, at most 200 characters, operator-written, default empty). It may be placed on any call, including one in progress; it takes effect for the purge when the call ends. The reason lives only on the call row (it goes when the call is erased) and is never logged or put in a metric or an audit row.
2. **A held call is skipped by every purge**: the content and metadata passes and the dry run use one shared SQL predicate (`held = false`), so the preview still equals a run. Its recording is skipped by the janitor. The standalone span and event cleanup is not session-bound and is unchanged.
3. **Recordings become linkable**: when the call has a stored session id, a new recording is named by it (`<stored session id>.wav` and `.json`) and its sidecar `SessionID` is that id; with no stored id the old name is kept. One recording per call (the file is created exclusive; a token is single use, so a call connects once). Recordings made before this change keep their old names; they cannot be tied to a call, are not erased by an erasure and not protected by a hold, and age out under `recording.retention_days` as today. The documentation says so. The recording tests and docs that show the old format are updated.
4. **The call is ended only after its recording is finalised.** `finishCall` waits for the recorder before `release()` marks the session ended. Erasure and holds rely on this: an ended call has a complete recording, so an erasure cannot miss a late sidecar.
5. **The janitor learns about holds without `core/` knowing the server**: `Sweep` and `Janitor` take a `keep(id) bool` over recording ids. A file name maps to an id by stripping its known suffixes, and only a stored session id (UUIDv7) is ever asked about. The server's `keep` asks the database for that one session (a held session, or any error, means keep: fail safe). A name that is not a stored id (legacy) is swept by age as today.
6. **Erasing one call** (`DELETE /api/v1/sessions/{id}`, 200 with counts):
   1. In one transaction: a guarded delete of the session row (`status = 'ended' AND held = false`) and, first, its children (below), then the audit entry. If the guarded delete removes no row, the request is classified by a read: absent is 404 `not_found`, held is 409 `session_held`, active or draining is 409 `session_active`; the transaction rolls back and nothing is audited. This is atomic against a hold placed or a purge running meanwhile.
   2. After commit: remove the recording files of that id (`Remove` builds the four fixed names `<id>.wav`, `.json` and their `.part` forms, requires the id to be a stored session id, and removes only regular files). A held or active call never reaches this step, so a hold can never cost a recording.
   3. A file that cannot be removed gives 500 with `recordings_failed` in the audit detail; the call row is already gone. **Retry**: `DELETE` of an id with no row looks for recording files of that id; found and removed is 200 with `recordings_removed` and an audit entry, nothing found is 404.
   The response reports the rows removed by class and `recordings_removed`.
7. **The per-session delete is shared with the purge** and made safe for the restrict constraints: events are removed where `session_id` matches **or** `span_id` is a span of the session **or** `turn_id` is a turn of it; spans where `session_id` matches or `turn_id` is a turn of the session; then tool calls, turns, MCP tool pins, the session. This also fixes a latent case in the purge where a linked row with no `session_id` would make a delete fail.
8. **Erasing by caller** (`POST /api/v1/sessions/erase`): body `{"caller": "<label>", "confirm_caller": "<the same label>", "dry_run": false}`. The label typed twice is the confirmation, so a stray boolean cannot trigger it; `dry_run: true` returns the counts and deletes nothing. A missing, mismatching, whitespace-only or over-128-character label is 400. Matching is exact. Each request erases at most 500 matching ended, non-held calls, each as in decision 6, committed in batches of 25 sessions; one audit entry per request. The response has `erased`, `held` and `active` (matching calls skipped), `remaining` (matching ended, non-held calls still to erase) and `recordings_removed`. A client repeats until `erased` is 0 (a release can raise `remaining` between requests). An index on `sessions(caller)` keeps the lookup from scanning.
9. **Audit**: table `retention_audit` (id, time, action, subject, detail, request id) with actions `hold`, `release`, `erase_call`, `erase_caller`. The subject is the session id; for a caller erasure it is the first 16 hex characters of the SHA-256 of the label, so the table never holds a caller's label (the response to the operator does). The detail is a short JSON of counts (and `recordings_failed`). The request id is server-generated; a client-supplied `X-Request-ID` is ignored. A hold or release is audited only when it changes the flag. The table is bounded at read (the last 100) and is not yet purged by the retention policy; documented.
10. **API**: all behind the management key. `PUT /api/v1/sessions/{id}/hold` (optional body `{"reason": "..."}`; the stored reason becomes the body's, empty if none; idempotent), `DELETE /api/v1/sessions/{id}/hold` (idempotent), `DELETE /api/v1/sessions/{id}`, `POST /api/v1/sessions/erase`. `erase` is handled before the item lookup, and `GET /sessions/erase` is 404. `SessionDTO` gains `held` and `hold_reason`; the list accepts `held=true|false`. `GET /api/v1/retention` gains `held_sessions` and `recent_actions` (the last 20 audit entries). Metric `voice_retention_erasures_total{kind="call"|"caller"}` counts calls erased. The handler gets a `SessionLifecycle` interface injected (hold, release, erase, erase by caller), implemented in `server/retention` over the store and the recording sink, so `server/api` does not import the sink.
11. **Console**: the call page gets Hold and Release (optional reason) and "Erase this call…" with a confirmation that says what goes, that the recording goes if it is on this server, and that it cannot be undone; the calls list shows an "On hold" chip and filters by it; the retention card on the Server page shows the number of calls on hold and the recent actions.
12. **Multiple instances**: `recording.dir` is local to an instance. An erasure removes recordings only on the instance that handles the request; the response's `recordings_removed` says how many it found. Cross-instance erasure is not supported and the documentation states it next to the existing statement that cross-instance state is not promised.

## What an erasure removes and what it leaves

Removes (database): the session row (including `caller`, `safe_summary`, `runtime_snapshot_json`, `provider_stack_json`, the hold reason), its turns (transcript, assistant text, latencies), tool-call rows (including summaries), spans and events (including attributes), MCP tool pins; and, on this instance, the recording `.wav` and sidecar `.json`. Leaves: the audit entry (an id, or a hash for a caller erasure, and counts); agent versions and tool definitions (not call data); backups; log files (the caller label and transcript text are never logged: a log-capture test pins it); an external OpenTelemetry backend and provider-side retention; recordings that predate the new naming; recordings on other instances.

## Edge cases and how each is handled

| Case | Behaviour |
|---|---|
| Hold or erase an unknown id, or an id that is not a UUIDv7 | 404 / 404 (the existing session lookup behaviour) |
| Hold twice, release when not held | 200, no change, not audited; a second hold replaces the reason |
| Hold a call in progress | allowed; the purge and janitor skip it once it ends |
| Erase a held call | 409 `session_held`; its recording is untouched |
| Erase an active or draining call | 409 `session_active` |
| Hold placed between the check and the delete, or the purge deleting the same call | the guarded delete removes nothing and is classified by a read; no partial effect |
| Recording file cannot be removed (permissions) | 500 after the commit, `recordings_failed` audited, retry finishes it |
| Crash between the commit and the file removal | the files stay; `DELETE` of the same id finishes it; otherwise they age out |
| Late sidecar for an ended call | cannot happen: the call ends after its recording is finalised (decision 4) |
| Erase by caller with 2000 matches | 500 per request; the client repeats until `erased` is 0 |
| Caller erase when some matches are held or active | skipped and counted, never deleted |
| Mistyped caller label | the confirmation must repeat it exactly; `dry_run` shows the count first; a label matching nothing erases nothing |
| Caller label with unusual characters | exact string match bound as a parameter |
| `Remove` with a crafted id | refused unless it is a stored session id; only the four fixed names are touched; non-regular files are skipped |
| Janitor cannot read a session's hold state | that file is kept |
| Legacy recording names | not protected by a hold, not erased by an erasure; age out as before |
| Dry run | excludes held calls and still equals what a run removes (a hold placed between the two can differ by that call) |
| Rows linked to a session by span or turn but with no `session_id` | removed by the shared delete; no restrict failure |
| Draft test calls, calls ended by orphan cleanup at start-up | stored calls like any other; can be held and erased; an orphan that was held becomes ended and stays protected |
| SQLite and Postgres | columns, index and table in both migration trees (the down migration drops them); counts scanned as 64-bit integers; the guarded delete works in both |
| Old API clients | `SessionDTO` fields are additive; the list filter is optional |
| Retention disabled | holds and erasure still work |
| Audit growth | one row per human action; bounded at read; purge coverage is a later slice |
| MCP server and agent delete guards that join sessions | an erased session no longer counts; the pending-delete finalisation that follows a session ending runs after an erasure too |

## Plan

Each slice is committed with its tests; the whole set goes through the code review and the gate before merge.

1. **Storage.** Migration 013 (SQLite and Postgres): `sessions.held`, `sessions.hold_reason`, `sessions(caller)` index, `retention_audit`. Repository: `SetHold`, a shared per-session delete (decision 7) used by `PurgeMetadata` and the erasure, `EraseSession` (guarded, classified, audited), `EraseCaller` (counts, batches), `IsHeld`, audit write and read. `PurgeContent`, `PurgeMetadata` and `Preview` skip held calls through one shared predicate. Tests (both dialects): purge and preview skip held calls and still match, including a hold placed between preview and run; erase removes every child row, including events and spans linked only by span or turn; refusals for active and held; the guarded delete against a concurrent hold; caller erase batches, counts and `dry_run`; audit rows in the same transaction and absent for refused requests; migration up and down.
2. **Recordings.** Name by the stored session id with the old-name fallback; `Remove`; `Sweep` and `Janitor` take `keep`; the server's `keep`; `finishCall` waits for the recorder before `release()`. Tests: a held call's recording survives the sweep and a released one is swept; an error from `keep` keeps the file; a legacy name is swept by age; `Remove` removes the four names and nothing else, refuses a bad id and a symlink; after a call ends its sidecar exists (the order fix); an existing recording test updated for the new name.
3. **Service and API.** The `SessionLifecycle` implementation, the four endpoints, the DTO fields and filter, the retention status additions, the erasure metric, the post-erase pending-delete finalisation. Tests: every status code in the tables, idempotency, `GET /sessions/erase` is 404, the unknown-parameter rule with `held`, no secret, transcript or path in any response, a log-capture test that no erasure logs a caller label or transcript text, the audited request id equals the response's and a client header is ignored, the failure injections (file removal fails after commit; retry finishes it), a held call surviving orphan cleanup plus a purge, a table-wide scan that no table holds the erased id or caller label.
4. **Console and end to end.** Hold, Release and Erase on the call page, the chip and filter, the retention card additions; a browser test that holds, fails to erase, releases and erases a call with a recording; documentation (api/v1.md, privacy.md including what an erasure removes and leaves and the instance limit, roadmap rows); screenshots from the worktree's dev stack for the report.

## Acceptance criteria

- A held call, its content and its recording survive a purge and a janitor sweep that would otherwise remove them; releasing it lets the next run remove it. Both actions are in the audit trail.
- Erasing a call removes its row, turns, tool calls, spans, events, tool pins and its recording on this instance, and no table holds its id or its caller label afterwards; an active or held call is refused with the codes above, with its recording untouched.
- Erasing by caller removes every matching ended, non-held call over as many requests as needed, never a held or active one, only after the label is typed twice; `dry_run` reports the same counts without deleting.
- The dry run equals what a run removes when holds are present.
- No response, log line, metric or audit row carries transcript text, a recording path, a secret or a caller label (the caller-erase response echoes the label to the operator who sent it).
- A call is never ended before its recording is finalised.
- Existing recordings, the existing purge and the existing sessions API behave as before when no hold or erasure is used.

## Review log

The independent review of the first draft found the following; each is resolved above.

| Finding | Resolution |
|---|---|
| Erase removed files before the transaction, so a hold placed in between lost its recording, and a DB failure left a call with no recording | The transaction is first and guarded; files are removed after commit; retry finishes the files (6) |
| A late sidecar could appear after an erase, because the session ended before the recorder finished | `finishCall` waits for the recorder before ending the session (4) |
| Restrict foreign keys make events and spans linked by span or turn a hard failure | Shared per-session delete covers them, with a test (7) |
| `confirm: true` protects nothing against a mistyped label | The label is typed twice; `dry_run` added; one audit entry per request (8) |
| The audit stored the caller label | A hash of the label is stored instead (9) |
| The held set is unbounded and read on every sweep | The janitor asks per file and keeps on any error (5) |
| `caller` has no index and one 500-row transaction blocks SQLite's writer | Index added; batches of 25 (8) |
| Multiple instances cannot erase each other's recordings | Documented limit (12) |
| Wiring: the handler cannot import the sink | A `SessionLifecycle` interface is injected (10) |
| Guard must be in the DELETE itself, and a zero-row result must map to 404 or 409 | Specified (6) |
| Routing: `erase` versus an id, new methods on existing paths, the `held` query key | Specified and tested (10) |
| The recording name change: `StoredSessionID` may be empty, exclusive create, `keep` mapping, `Remove` safety | Fallback, one recording per call, suffix stripping and UUIDv7 check, fixed names and regular files only (3, 5, 6) |
| Data classes left over, and an untestable "nothing identifies the caller" | A removes-and-leaves section, a table-wide scan test and a log-capture test (plan 3) |
| `remaining` pagination | The client stops when `erased` is 0 (8) |
| Other deleters of sessions (orphan cleanup, pending MCP deletes) | Covered in the edge cases and the plan |
| Slice order: `Remove` belongs with the recording change; the metric needs wiring; add an end-to-end erasure test | Slices reordered and extended (plan 2 to 4) |
