# Call records (S10) and reply times (S9 tail) — Design

Status: draft for review. Two gaps in the Calls screens, built together because they touch the same rows and screens: **S10**, the server records who called and why a call ended; **S9 tail**, the reply-time views that need data the server does not keep yet (the per-turn waterfall, a median in the Calls list, reply times in the Test call panel and First run).

## Goal

An operator looking at Calls can see, for every call, **who** it was (the caller label the integration chose) and **how it ended** in plain words (the caller hung up, the caller went silent, the flow ended it, the call hit the time limit, the server restarted, it failed to start). In Call detail each reply shows a **waterfall** of where its time went. The Calls list shows a **median reply** per call. The Test call panel (and so First run) shows how fast the agent replies, measured by the server.

## Not in scope

Search, When filter, totals, CSV (S11); version number and path on the session row (S12); live call control (S13); a filter by end reason; IP address or browser of the caller (see Privacy); a new RTVI message type; the Calls list "Path" column.

## Today (from the code)

- `sessions` has `status` (`active` | `ended` only; `draining` is read in places but never written), `safe_summary` (empty except for calls a crash orphaned: an English sentence), `draft`. Nothing stores why a call ended: the reason goes to the client (RTVI error and WebSocket close), the in-memory timeline and metrics. The one exception is `ended_by_flow`, a `call.end` trace span.
- Nothing stores the caller: the voice token's `principal` (a label of 1 to 128 characters chosen by whoever requests the token; the console sends `flow-test-<flow id>`) is used for admission limits only.
- Turn rows carry `latencies` (`stt_final_ms`, `llm_ttft_ms`, `tts_ttfb_ms`, `voice_to_voice_ms`, only what was measured), written from a per-call collector (`server/turnlatency.go`) that keeps durations and drops the times at which stages started. Call detail shows "Replied in", a median and per-turn pills.
- The server sends no latency to the web client; the Test call panel measures none.

## Design

### 1. End reason

New column `sessions.end_reason` (TEXT, empty while active), a **closed vocabulary**:

| `end_reason` | When | Words in the console |
|---|---|---|
| `caller_hung_up` | the client disconnected or sent `disconnect-bot` (the default for a normal end) | Caller hung up |
| `silent_caller` | the quiet-caller handling ended it (`user_idle`) | Caller went silent |
| `agent_stuck` | the stuck-bot watchdog ended it (`bot_idle`) | Agent stopped responding |
| `ended_by_flow` | the `end_call` tool (the step is still read from the `call.end` span) | Ended by the flow |
| `time_limit` | `limits.max_call_duration` (or the transport's connection lifetime) | Reached the time limit |
| `connection_lost` | the peer stopped answering pings | Connection lost |
| `server_shutdown` | the server drained | Server shutting down |
| `error` | the session ended with an error (`session.Err()`) | Ended with an error |
| `setup_failed` | the row exists but the pipeline could not be built | Could not start |
| `server_restart` | found active at start-up (the S14 sweep) | Ended when the server restarted |

How it is learned (the reason is not known where the row is closed today): each call gets one small **end-reason holder** (first writer wins, atomic, no lock) created in `onSessionReady` and read when the row is closed. Writers: the idle controller's end callback (`user_idle` → `silent_caller`, `bot_idle` → `agent_stuck`, `ended_by_flow`), the max-duration timer, the drain code, `finishCall` (`session.Err()` → `error`), `fail()` (→ `setup_failed`). The transport records **why it closed a connection** (ping failure, connection lifetime) on `ws.Connection` (`CloseCause()`, set once before the close) so the server can map it; anything not set by then is `caller_hung_up`. `SessionRepository.End(ctx, id, reason)` writes it with `ended_at`; `EndOrphans` writes `server_restart` (keeping its note in `safe_summary`). The orphan marker is therefore no longer an English string match.

A call refused before a row exists (admission rejection, a flow that cannot be resolved) has no session and is not listed; that is stated in the docs, not faked.

`end_reason` is exposed on the session DTO (`end_reason`, empty while live). The Outcome chips (Live/Ended) and filters stay as they are; the **row shows the reason** next to "Ended", and Call detail replaces "Ended: By the flow (step)" with "Ended: <words>" (the step name for `ended_by_flow`).

### 2. Caller

New column `sessions.caller` (TEXT): the token's principal, trimmed, at most 128 characters, stored as given. Plumbed from `onSessionReady` through `resolveCall` to `StartCapturedSession`. The console renders `flow-test-*` as **Console test**; any other value is shown as written. The Calls list gets a **Caller** column and a Caller line in Call detail.

**Privacy.** Only the label an integration chose is stored; no IP address, no user agent, no token id beyond the existing session id. `docs/privacy.md` and the API docs say the principal must not carry personal data (a phone number or e-mail), because it is stored and shown in the console. Retention follows sessions (metadata retention).

### 3. Waterfall (Call detail)

The collector keeps, per turn and stage, when the stage **started** (end time minus its duration) in addition to its duration. When the turn is written, offsets from the turn's origin are added to `latencies` (integer milliseconds, only what was measured):

```json
{ "stt_final_ms": 410, "llm_start_ms": 520, "llm_ttft_ms": 380, "tts_start_ms": 905, "tts_ttfb_ms": 260, "voice_to_voice_ms": 1165 }
```

`origin` is the earliest measured stage start (the moment the caller stopped speaking for a spoken turn, the model request for a typed one); `llm_start_ms` and `tts_start_ms` are the offsets of the model request and the first voice request from it; `voice_to_voice_ms` is unchanged. Bounds: the collector already holds at most 32 turns; the new fields are two ints per turn. A turn with a tool loop reports the **last** model request (documented).

Call detail's per-turn reply gets a small waterfall: one bar per stage on a shared time axis (Heard, Model, Voice, then the reply start), widths in proportion, labelled with the milliseconds, and a text alternative that reads the same numbers (`Heard 0 to 410 ms, Model 520 to 900 ms, Voice 905 to 1165 ms`). The gaps between bars are left empty and named in the note ("waiting for the caller to finish", "tools"): not invented. Tokens only, no shadows. A turn with no offsets (an older row) keeps the existing pills.

### 4. Median reply in the Calls list

`SessionDTO` gains `median_reply_ms` on the list and get responses: one extra query per page (`turns` for the page's session ids, using the existing index on `turns(session_id)`), median computed in Go with the same rule as Call detail (`voice_to_voice_ms`, else `llm_ttft_ms + tts_ttfb_ms`), absent when no turn was measured. No schema change, no backfill, live calls included. The list gets a **Median reply** column.

### 5. Reply times in the Test call panel and First run

The panel already knows the session id. While a call is connected it asks `GET /sessions/{id}/turns` (the existing endpoint) every 2 seconds and, once the call ends, once more; it shows **Last reply** and **Median reply** in the header strip, and a "Replied in X" note under each bot message as the turn rows land. The figures are the server's measurement (`voice_to_voice_ms`), not the browser's, so they do not include the network legs and they match Call detail. First run's test-call step uses the same component and so shows the same figures ("first reply 612 ms" is the first turn's). Typed turns show the model plus voice time (no "heard" stage). No new message on the call's WebSocket: the web client stays unmodified.

### 6. Defaults, hints

An operator configures nothing: both are always on. Each new column has a header tooltip-free explanation in the page's own hint line (what "Median reply" counts; that "Caller" is the label the integration chose), and the end-reason words are the only vocabulary. (Explain buttons are for editor settings; there are none here.)

## Testing

- **Storage (both dialects):** migration `007`; `End` writes the reason and time; `EndOrphans` writes `server_restart`; list and get return `caller` and `end_reason`; `make test-postgres`; `schema_names_test` still passes.
- **Server unit:** the holder is first-writer-wins and race-free (`-race`, concurrent writers); mapping from each source (idle callback strings, max-duration, drain, `session.Err()`, setup failure, transport close cause) to the vocabulary; `latencies` offsets (a table of measured stages incl. a typed turn, a tool-loop turn, missing stages; origin choice; ints only); median over a page.
- **Go e2e on the echo providers (real WebSocket calls):** one test per path with a stored session: caller closes the socket → `caller_hung_up`; a flow with a short quiet-caller wait → `silent_caller`; an `end_call` flow → `ended_by_flow`; a shortened `limits.max_call_duration` → `time_limit`; server `Shutdown` with a live call → `server_shutdown`; a pipeline that cannot be built → `setup_failed`; a token with a given principal → `caller` stored, and a stored turn's `latencies` include offsets in order (`llm_start_ms` ≥ `stt_final_ms`).
- **UI (vitest):** the Calls list columns (Caller with `Console test` mapping, reason words, Median reply, empty when absent), Call detail (waterfall widths from the numbers, the text alternative, the fallback to pills, the reason line with the step for `ended_by_flow`), the panel's reply strip (polling stops when the call ends; no fabricated numbers before a turn lands), First run's chip.
- **Browser (Playwright, echo):** place a call from the console and hang up → the list shows "Console test" and "Caller hung up"; open it → a waterfall; a flow with a short silent-caller wait → "Caller went silent"; the Test call panel shows a reply time after a typed turn.
- **Gate:** `go test -race ./...`, `make test-postgres`, `npx vitest run && tsc`, `make e2e-echo`, independent Opus review (bugs, bloat, dead code).

## Documentation

`docs/api/v1.md` (`caller`, `end_reason` and its vocabulary, `median_reply_ms`, the new `latencies` keys), `docs/privacy.md` (what is stored about the caller and what is not), `docs/operations_runbook.md` (the vocabulary, "no row for refused calls", nothing stored from the network), the console spec (rows S9 and S10 done, deviations removed).

## Rulings made while writing this (overrule any)

1. **Principal only**, not IP or user agent: they are personal data, free text, and need plumbing in five places; the label an integration chooses is what operators actually search by. Cost if wrong: a later column and a privacy note.
2. **Closed vocabulary in one column**, not a second status: `draining` is never written and a status that nothing sets is a lie; the Outcome chips stay Live/Ended.
3. **Default is `caller_hung_up`** for anything the transport did not name: most calls end that way, and an unknown cause is more often a closed tab than a fault. Cost if wrong: a rare abnormal close reads as a hang-up (the metrics still count it).
4. **Server-measured reply times in the panel** (a 2 s poll of the turns endpoint) instead of a browser-side stopwatch or a new RTVI message: the number then equals Call detail's, the pinned web client stays unmodified, and the cost is one cheap request per 2 s while a test call is open.
5. **The median is computed per page on read**, not stored: bounded by the page size, no backfill, correct for live calls. Cost if wrong: add a column set at `End()` later.
6. **`ended_by_flow`'s step stays in the trace span** (it is already there); `end_reason` says that it was the flow.
7. **Refused calls are not listed**: no row exists; the metrics already count them.

## Open points

- A filter by end reason in the Calls list (cheap once the column exists; left out because nothing asked for it).
- Whether a flow's `end_call` should record the step on the row (S12 territory).
- The transport's own 60-minute connection lifetime is mapped to `time_limit` here even though it is a separate setting from `limits.max_call_duration`.
