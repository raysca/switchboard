# Retention holds and erasure: spec and plan

Status: proposed. Slice 2 of Phase 8 in [roadmap-next-phases.md](../roadmap-next-phases.md). Slice 1 (status, dry run, metrics, alert) is built; see [privacy.md](../privacy.md#server-data-retention-and-privacy-controls).

## Goal

An operator can protect a call from the retention job (a hold), and can erase one call, or everything one caller label produced, on request: transcript, trace, tool-call rows, recording and the call row itself. Every hold, release and erasure is recorded.

## Non-goals (later slices)

Per-agent or per-class lifetimes and the policy model; an export before erasure; a console form for erasing by caller (the API exists, the console covers one call); production profiles requiring an explicit retention choice; erasing data we cannot reach (backups, log files, an external trace backend, provider-side retention), which the documentation states.

## What exists today (checked in the code)

- `server/retention` purges by `ended_at` in two passes (content, metadata); `PurgeContent` and `PurgeMetadata` select ended sessions; `Preview` counts the same selection.
- A call's recording is named `<agent id>-<core session number>-<unix time>` (`server/recording.go`); the number is the in-process core session id, **not** the stored session id, so nothing ties a recording file to a stored call. Its sidecar carries the same number. The janitor (`core/record` `Sweep`) deletes files by modification time only.
- A stored call is the `sessions` row plus `turns`, `tool_calls`, `trace_spans`, `trace_events`, `session_mcp_tools`; `PurgeMetadata` already deletes them in foreign-key order inside a transaction per 25 sessions.
- `sessions.caller` is the label an integration chose (at most 128 characters).
- The sessions API has `GET` for a list and an item, turns and traces; no write.

## Decisions

1. **A hold is a flag on the stored call**: `sessions.held` (boolean, default false) and `sessions.hold_reason` (text, at most 200 characters, operator-written, default empty). A hold may be placed on any call, including one in progress; it takes effect for the purge when the call ends.
2. **A held call is skipped by every purge**: content and metadata passes, the dry run (so the preview still equals a run), and its recording is skipped by the recording janitor.
3. **Recordings become linkable**: a new recording is named by the stored session id (`<stored session id>.wav` and `.json`), and its sidecar `SessionID` is the stored id. Recordings made before this change keep their old names; they cannot be tied to a call, are not erased by an erasure, and age out under `recording.retention_days` as today. This limit is documented.
4. **The janitor learns about holds without `core/` knowing the server**: `Sweep` and `Janitor` take a `keep` function over recording ids. The server supplies the set of held session ids, read once per sweep; if it cannot be read the sweep deletes nothing that run (fail safe).
5. **Erasing one call** (`DELETE /api/v1/sessions/{id}`) deletes, in this order: the call's recording files; then, in one transaction, its trace events, spans, tool calls, turns, MCP tool pins and the session row; then writes the audit entry in the same transaction. Recording files first: if a file cannot be removed the call is untouched and the request fails, so an erasure never leaves a recording whose call is gone. If the database step then fails, the recording is gone and the call remains: the request fails and can be repeated. Only an **ended** call that is **not held** can be erased: an active or draining call is 409 `session_active`, a held call is 409 `session_held` (release it first). An unknown id is 404. The response reports what was removed by class.
6. **Erasing by caller** (`POST /api/v1/sessions/erase` with `{"caller": "<label>", "confirm": true}`) erases up to 500 matching ended, non-held calls per request, each as in decision 5, and reports `erased`, `held` and `active` (matching calls it skipped) and `remaining` (matching calls still to erase: held and active ones are not counted, so a client repeats until `remaining` is 0). An empty label, a missing `confirm` or a label over 128 characters is 400. Matching is exact.
7. **Audit**: a table `retention_audit` (time, action, subject, detail, request id) with actions `hold`, `release`, `erase_call`, `erase_caller`. The subject is the session id, or for a caller erasure the label the operator typed; the detail is the counts removed (and the hold reason). The rows of an erased call are gone but the audit entry names its id; nothing else identifies the caller. The request id is server-generated, as for secrets. The table is bounded at read (the last 100) and covered by the metadata retention policy in a later slice; it is documented as not yet purged.
8. **API additions** (all behind the management key): `PUT /api/v1/sessions/{id}/hold` (body `{"reason": "..."}` optional; idempotent; replaces the reason), `DELETE /api/v1/sessions/{id}/hold` (idempotent), `DELETE /api/v1/sessions/{id}`, `POST /api/v1/sessions/erase`. `SessionDTO` gains `held` and `hold_reason`; the list takes `held=true|false`. `GET /api/v1/retention` gains `held_sessions` (count) and `recent_actions` (the last 20 audit entries). Metric `voice_retention_erasures_total{kind="call"|"caller"}` counts calls erased.
9. **Console**: the call page gets Hold and Release (with an optional reason) and "Erase this call…" with a confirmation that says what goes and that it cannot be undone; the calls list shows an "On hold" chip and filters by it; the retention card on the Server page shows how many calls are on hold and the recent actions.

## Edge cases and how each is handled

| Case | Behaviour |
|---|---|
| Hold a call that does not exist | 404 |
| Hold twice, release when not held | 200, no change; the second hold replaces the reason; each real change is audited, a no-op is not |
| Hold a call in progress | allowed; the purge and janitor skip it once it ends |
| Erase a held call | 409 `session_held` |
| Erase an active or draining call | 409 `session_active` |
| Erase while the purge job is running on the same call | both delete by id; the one that finds nothing reports 0 rows; no error is surfaced to either |
| Erase a call whose recording is still being finalised just after the call ended | the recording may be missed (it appears after the lookup); the response reports `recordings` removed; the file ages out by retention. Documented |
| A recording file cannot be removed (permissions) | 500, call untouched, nothing audited |
| Database failure after the recording is removed | 500; repeat the request (the missing recording is not an error) |
| Erase by caller with 2000 matches | 500 per request; `remaining` tells the client to repeat |
| Erase by caller while some matches are held or active | those are skipped and counted; never deleted |
| Caller label with unusual characters | exact string match, bound as a parameter; a label that is only whitespace is refused |
| Janitor cannot read the held set | no deletion that sweep |
| Legacy recording names | cannot be tied to a call: never kept by a hold, never erased by an erasure (documented) |
| Dry run | excludes held calls, so it still equals what a run removes |
| SQLite and Postgres | new columns and table in both migration trees; booleans via the existing helpers; counts scanned as 64-bit integers |
| Old API clients | `SessionDTO` fields are additive; the list filter is optional |
| Retention disabled | holds and erasure still work; there is just no automatic purge |
| Draft test calls | are stored calls and can be held and erased like any other |
| Audit growth | one row per human action; bounded at read; purge coverage is a later slice and documented |

## Plan

Each slice is committed with its tests, and the whole set goes through the code review and the gate before merge.

1. **Storage.** Migration 013 (SQLite and Postgres): `sessions.held`, `sessions.hold_reason`, `retention_audit`. Repository: `SetHold`, `Erase` (shared per-session delete extracted from `PurgeMetadata`), `EraseCaller` (batch), `HeldSessionIDs`, `Audit`. `PurgeContent`, `PurgeMetadata` and `Preview` skip held calls. Tests: the purge and the preview skip held calls and still match; erase removes every child row and refuses active and held calls; caller erase batches and counts; audit rows are written in the same transaction and absent for refused requests; both dialects (`eachStore`).
2. **Recordings.** New name and sidecar id from the stored session id; `Sweep` and `Janitor` take `keep`; the server passes the held set. Tests: a held call's recording survives the sweep and a released one is swept; a failed held-set read deletes nothing; a legacy-named file is swept by age as before; the name is a valid id; an erasure removes the `.wav`, `.json` and their `.part` files and not a neighbour's.
3. **Service and API.** Erase orchestration (recording files first, then the transaction), the four endpoints, `held` and `hold_reason` in the DTO, the `held` filter, `held_sessions` and `recent_actions` in the retention status, the erasure metric. Tests: every status code in the table, idempotency, the unknown-parameter rule for the list, secrets and content never in responses, request id equals the response's.
4. **Console and end to end.** Hold, Release and Erase on the call page, the chip and filter on the list, the retention card additions; a browser test that holds, fails to erase, releases and erases; documentation (api/v1.md, privacy.md, roadmap rows); screenshots from the worktree's dev stack for the report.

## Acceptance criteria

- A held call, its content and its recording survive a purge and a janitor sweep that would otherwise remove them; releasing it lets the next run remove it. Both actions are in the audit trail.
- Erasing a call removes its row, turns, tool calls, spans, events, tool pins and its recording, and leaves nothing that identifies the caller except the audit entry; an active or held call is refused with the codes above.
- Erasing by caller removes every matching ended, non-held call over as many requests as `remaining` asks for, and never a held or active one.
- The dry run equals what a run removes with holds present.
- No response, log or audit row carries transcript text, a recording path or a secret.
- Existing recordings, the existing purge and the existing sessions API behave as before when no hold or erasure is used.
