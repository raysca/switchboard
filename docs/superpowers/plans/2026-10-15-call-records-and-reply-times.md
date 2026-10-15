# Call Records (S10) and Reply Times (S9 tail) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (inline). Steps use checkbox (`- [ ]`) syntax.

**Goal:** Calls record who called and why they ended; Call detail draws a per-reply waterfall; the Calls list shows caller, end reason and a median reply; the Test call panel (and First run) shows server-measured reply times.

**Architecture:** Two additive session columns (`caller`, `end_reason`) written from a per-call end-reason holder; the latency collector keeps stage start times so stored `latencies` gain two offset keys; the sessions API adds `caller`, `end_reason`, `median_reply_ms` (one extra turns query per list page); the UI renders them and the panel polls the existing turns endpoint.

**Tech Stack:** Go (`server`, `server/storage`, `transport/ws`), SQLite + Postgres (goose migrations), React 19 + TS + vitest, Playwright echo suite.

**Spec:** `docs/superpowers/specs/2026-10-15-call-records-and-reply-times-design.md` (binding; read it first).

## Global Constraints

- End-reason vocabulary (exact strings): `caller_hung_up`, `silent_caller`, `agent_stuck`, `ended_by_flow`, `time_limit`, `connection_lost`, `server_shutdown`, `error`, `setup_failed`, `server_restart`. Empty while a session is active. Default for an unnamed end: `caller_hung_up`.
- `caller`: the token principal, trimmed, at most 128 characters, stored as given; no IP, no user agent. The console shows `flow-test-*` as **Console test**.
- New `latencies` keys `llm_start_ms`, `tts_start_ms`: integer ms offsets from the turn's origin (the earliest measured stage start); only when measured; last model request of a tool-loop turn.
- Both SQL dialects in step (new `007_*.sql` for each; `server/storage/schema_names_test.go` and `make test-postgres` stay green). Nothing deployed: no data migration beyond the defaults.
- Bounded and race-free (AGENTS.md): the holder is an atomic first-writer-wins value; the collector keeps its 32-turn bound and one short mutex; the median query takes at most the page's ids (limit 100).
- UI: tokens only, no shadows, every figure has words, a text alternative for the waterfall; labels use "Call".
- E2E on the echo providers only (`make e2e-echo`). Playwright locators use `exact: true` where Explain-style labels could collide.

## Review Focus

1. **End reason correctness per path:** each of the ten reasons is reachable and recorded once; the first cause wins (an idle hang-up followed by the socket closing stays `silent_caller`); an unnamed close is `caller_hung_up`; a call that failed after its row exists is `setup_failed`, not a normal short call. Tasks 3 and 8.
2. **Hot path and bounds:** the latency collector change adds no allocation per frame (it runs per measurement, a few per turn); the holder is lock-free. Task 4.
3. **Privacy:** nothing about the network or browser is stored; the principal limit is enforced. Tasks 1–2.
4. **Old rows:** sessions without `caller`/`end_reason`, turns without offsets, and live calls render sensibly (no `undefined`, no fabricated numbers). Tasks 6–8.
5. **Orphans:** the S14 sweep writes `server_restart` and keeps its note. Task 1.
6. **Polling cost:** the panel's 2 s poll stops when the call ends or the panel closes and never overlaps requests. Task 8.

## Rulings made while planning (overrule any)

1. **Migration 007** (a new file per dialect, like 006) rather than editing 001: the sessions DDL is already split across migrations.
2. **Holder lives next to the call's other per-call state in `server/`**, a tiny type with `Set(reason)` (first wins) and `Get()`; the transport only exposes `CloseCause()` (a string it sets once); the mapping from transport causes to the vocabulary is in `server/`, not in `ws` (the transport knows nothing about sessions).
3. **`median_reply_ms` is computed in Go from the page's turn rows** using the same rule as the UI helper (`voice_to_voice_ms`, else `llm_ttft_ms + tts_ttfb_ms`); the Go version is the source of truth for the list, the UI's own helper stays for Call detail.

---

### Task 1: Storage: columns, End with a reason, orphans

**Files:**
- Create: `server/migrations/sqlite/007_session_caller_end_reason.sql`, `server/migrations/postgres/007_session_caller_end_reason.sql`
- Modify: `server/storage/history.go` (`SessionRecord`, `Create`, `End`, `EndOrphans`, the two SELECT/Scan lists), `server/storage/repositories.go` if it lists columns, callers of `End`
- Test: `server/storage/history_query_test.go`, `server/storage/orphans_test.go`, `server/storage/storage_test.go`

**Interfaces:**
- Produces: `SessionRecord.Caller string`, `SessionRecord.EndReason string`; `SessionRepository.End(ctx, id, reason string) error`; constants in `storage`: `EndCallerHungUp = "caller_hung_up"` … one per vocabulary entry (`storage.EndReasons` set for validation).

- [ ] **Step 1: Write the failing tests.** Create a session with a caller; list/get return `Caller`; `End(ctx, id, "silent_caller")` stores the reason and `ended_at`; ending twice keeps the first reason; an unknown reason is refused (`ErrInvalid…`); an active session has `EndReason == ""`; `EndOrphans` sets `server_restart` and still sets the note; `Create` rejects a caller over 128 characters (after trimming) and accepts empty.
- [ ] **Step 2: Run to see them fail.** `cd server && go test ./storage/ -count=1 2>&1 | tail` — Expected: FAIL (compile).
- [ ] **Step 3: Implement.** Migrations: `ALTER TABLE sessions ADD COLUMN caller TEXT NOT NULL DEFAULT ''; ADD COLUMN end_reason TEXT NOT NULL DEFAULT ''` (SQLite: two `ALTER TABLE … ADD COLUMN`, with the Down as in 006). Update every INSERT/SELECT/Scan; `End` takes the reason and `UPDATE … SET status='ended', ended_at=?, end_reason=?`.
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 ./storage/ && make test-postgres` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server/storage): sessions record the caller and why they ended`.

---

### Task 2: Plumb the caller

**Files:**
- Modify: `server/flow/service.go` (`StartCapturedSession` and the runtime start helpers it goes through), `server/runtime/*.go` where `StartSession` is defined, `server/call.go` (`resolveCall`), `server/server.go` (`onSessionReady` passes `principal`)
- Test: `server/flow/service_test.go`, `server/call_behavior_e2e_test.go` style test added in Task 8

- [ ] **Step 1: Write the failing test.** `StartCapturedSession(..., caller)` stores it (trimmed); a 129-character principal is refused at token issue already (`api_routes.go`: confirm) so the store never sees it; a call through `onSessionReady` stores the token's principal (assert in the Go e2e of Task 8; here a unit test on the service).
- [ ] **Step 2: Run to see it fail.** Expected: FAIL (compile).
- [ ] **Step 3: Implement** by adding a `Caller string` to the start request struct rather than another positional argument; draft calls pass the principal too.
- [ ] **Step 4: Run.** `cd server && go test -race -count=1 ./flow ./runtime ./storage .` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): a session stores the caller's label`.

---

### Task 3: The end reason is learned and recorded

**Files:**
- Create: `server/endreason.go`
- Modify: `server/call.go` (`release` reads the holder; `fail` sets `setup_failed`), `server/server.go` (`SetOnEnd` callback, max-duration timer, drain, `finishCall`), `transport/ws/transport.go` (+ `CloseCause()`), `server/orphans.go` if it passes a reason
- Test: `server/endreason_test.go`, `transport/ws/transport_test.go`

**Interfaces:**
- Produces: `type endReason struct{ v atomic.Pointer[string] }` with `set(r string)` (first call wins) and `get() string` (default `caller_hung_up`); `func reasonFromEnd(idleReason string) string` mapping `core.IdleReasonUser`→`silent_caller`, `core.IdleReasonBot`→`agent_stuck`, `EndReasonFlow`→`ended_by_flow`, `"max_call_duration"`→`time_limit`, `"server_draining"`→`server_shutdown`; `(*ws.Connection).CloseCause() string` returning `""` | `"peer_unresponsive"` | `"connection_lifetime"`; `func reasonFromTransport(cause string) string` (`peer_unresponsive`→`connection_lost`, `connection_lifetime`→`time_limit`).

- [ ] **Step 1: Write the failing tests.** `endReason`: first writer wins under 100 concurrent goroutines (`-race`), default value, `get` stable after set; the two mapping functions as tables (including unknown strings → `caller_hung_up`); `transport/ws`: a ping failure sets `CloseCause()` to `peer_unresponsive` and a lifetime expiry to `connection_lifetime`, set once (a second cause does not overwrite); no cause on a normal client close.
- [ ] **Step 2: Run to see them fail.** `cd server && go test -run EndReason -count=1 .; cd ../transport && go test ./ws -run CloseCause -count=1` — Expected: FAIL.
- [ ] **Step 3: Implement.** Create the holder in `onSessionReady` (before `resolveCall`'s `release` closure is built: pass it in or have `resolveCall` return it); writers: the idle `SetOnEnd` callback (`server.go` ~666), the max-duration timer (~733), the drain (`Shutdown`, ~1042), `fail()` (`setup_failed`), `finishCall` when `session.Err() != nil` (`error`, only if nothing was set); `release` calls `End(ctx, id, holder.get())` after consulting `reasonFromTransport(conn.CloseCause())` **only when the holder is still empty** (so an idle hang-up followed by a socket close stays `silent_caller`). `transport/ws`: set the cause in the ping-failure path (~603) and the lifetime path before closing.
- [ ] **Step 4: Run to see them pass.** `cd transport && go test -race ./... ; cd ../server && go test -race -count=1 . ./flow ./storage` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): record why each call ended`.

---

### Task 4: Stage start offsets in `latencies`

**Files:**
- Modify: `server/turnlatency.go`
- Test: `server/turnlatency_test.go`

**Interfaces:**
- Consumes: `observe.Measurement{Stage, Turn, Value, At}` (`At` = end time).
- Produces: `take(turn)` JSON additionally has `llm_start_ms` and `tts_start_ms`.

- [ ] **Step 1: Write the failing tests.** A table of measured stage sets with times: full spoken turn (`stt_final` ending at t0+410 with value 410 → start t0; `llm_ttft` ending t0+900 value 380 → start t0+520; `tts_ttfb` ending t0+1165 value 260 → start t0+905; `voice_to_voice` 1165) gives `{"stt_final_ms":410,"llm_start_ms":520,"llm_ttft_ms":380,"tts_start_ms":905,"tts_ttfb_ms":260,"voice_to_voice_ms":1165}`; a typed turn (no STT, no v2v) uses the model request start as origin (`llm_start_ms` 0, `tts_start_ms` relative); a turn with only `llm_ttft` gives `llm_start_ms: 0`; no stages gives `{}`; two `llm_ttft` measurements keep the last; a missing `At` (zero time) omits the offsets (durations only, no negative numbers); never a negative offset; no allocation growth beyond the existing maps (benchmark or `testing.AllocsPerRun` on `observe`).
- [ ] **Step 2: Run to see them fail.** `cd server && go test -run TurnLatenc -count=1 .` — Expected: FAIL.
- [ ] **Step 3: Implement.** Store per turn `{value time.Duration; end time.Time}` per stage; at `take`, start = end − value; origin = earliest start among measured stages; write the offsets for LLM and TTS stages. Keep `maxTurnLatencies`.
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 -run 'TurnLatenc|Recorder' .` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): turn latencies carry when the model and voice stages started`.

---

### Task 5: The sessions API

**Files:**
- Modify: `server/api/traces.go` (`SessionDTO`, `toSessionDTO`), `server/storage/history.go` (a `TurnLatenciesFor(ctx, ids []string) (map[string][]string, error)` returning each session's `latencies_json` strings), the list and get handlers
- Test: `server/api/traces_test.go` or the existing sessions API tests, `server/storage/history_query_test.go`

**Interfaces:**
- Produces: `SessionDTO.Caller string json:"caller"`, `EndReason string json:"end_reason"`, `MedianReplyMS *int json:"median_reply_ms,omitempty"` (absent when no turn was measured); `func medianReplyMS(latencies []string) *int` implementing: per turn `voice_to_voice_ms`, else `llm_ttft_ms+tts_ttfb_ms` when both present; median of those (even count: mean of the two middle values, rounded).

- [ ] **Step 1: Write the failing tests.** `medianReplyMS` table (odd, even, mixed typed/spoken turns, none measured → nil, malformed JSON skipped); the list returns `median_reply_ms` for a page with measured turns and omits it otherwise; the batch query runs once per page (count queries with a wrapped DB or assert via a single call to `TurnLatenciesFor` in a test double); `GET /sessions/{id}` includes it; `caller`/`end_reason` present, empty for an active call; ids list capped at the page limit.
- [ ] **Step 2: Run to see them fail.** Expected: FAIL.
- [ ] **Step 3: Implement.** `TurnLatenciesFor` uses `WHERE session_id IN (…)` with bound placeholders (no string concatenation of ids) through the dialect's `bind`.
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 ./api ./storage . && make test-postgres` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): sessions expose the caller, the end reason and a median reply`.

---

### Task 6: Calls list and Call detail show them

**Files:**
- Modify: `ui/src/api/client.ts` (`sessionSchema`: `caller`, `end_reason`, `median_reply_ms` optional), `ui/src/routes/sessions/callStats.ts` (`callerLabel`, `endReasonText`), `ui/src/routes/sessions/CallsView.tsx`, `ui/src/routes/sessions/CallView.tsx`
- Test: `callStats.test.ts`, `CallsView.test.tsx`, `CallView.test.tsx`

**Interfaces:**
- Produces: `callerLabel(caller: string): string` (`""` → `Not recorded`; `flow-test-*` → `Console test`; else as given); `endReasonText(reason: string, stepName?: string): string` with the words from the spec (unknown or empty reason on an ended call → `Ended`).

- [ ] **Step 1: Write the failing tests.** The two helpers (every vocabulary entry, unknown values, the step name for `ended_by_flow`); the list shows columns Caller and Median reply (`612 ms` / `—` when absent), the Outcome cell reads `Live`, or `Ended` followed by the reason words; Call detail shows a Caller line and `Ended: <words>` replacing the old flow-ended line (the step still read from the trace span as today); the old column set otherwise unchanged; a hint line under the table explains `Median reply` and `Caller` in one sentence each.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/routes/sessions` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): Calls shows who called, how it ended and the median reply`.

---

### Task 7: The waterfall

**Files:**
- Create: `ui/src/routes/sessions/ReplyWaterfall.tsx`
- Modify: `ui/src/routes/sessions/CallView.tsx` (use it per bot reply), `ui/src/styles.css` (tokens only), `ui/src/test/noHardcodedColors.test.ts` (`CONVERTED` list if that is the convention: check the file)
- Test: `ReplyWaterfall.test.tsx`, `CallView.test.tsx`

**Interfaces:**
- Produces: `parseStages(latencies: unknown): { label: "Heard" | "Model" | "Voice"; start: number; end: number }[] | null` and `<ReplyWaterfall latencies={…} />` (null when fewer than two stages have offsets, so older rows keep the pills).

- [ ] **Step 1: Write the failing tests.** `parseStages` for the spec's example (`Heard 0–410`, `Model 520–900`, `Voice 905–1165`), a typed turn (no Heard), missing offsets → `null`, garbage → `null`, no negative or reversed ranges; the component renders one bar per stage with widths proportional to the numbers on a shared axis (assert the inline `width` percentages), the accessible text `Heard 0 to 410 ms, Model 520 to 900 ms, Voice 905 to 1165 ms` (an element with `role="img"` and that `aria-label`, or visually hidden text), the total `Replied in 1165 ms`; Call detail uses it for a turn with offsets and the pills for one without.
- [ ] **Step 2: Run to see them fail.** Expected: FAIL.
- [ ] **Step 3: Implement.** A plain flex/grid of `div`s with percentage `margin-left`/`width` from the offsets; colours from existing tokens; no shadows; gaps are empty space with a one-line legend naming them ("waiting for the caller to finish", "tools or waiting").
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): a reply waterfall in Call detail`.

---

### Task 8: Reply times in the Test call panel and First run

**Files:**
- Create: `ui/src/routes/flows/useReplyTimes.ts`
- Modify: `ui/src/routes/flows/FlowVoiceTest.tsx` (a `ReplyStrip` under the header; First run embeds the same component)
- Test: `useReplyTimes.test.tsx`, `FlowVoiceTest.test.tsx`, `OnboardingView` test if it asserts the step

**Interfaces:**
- Produces: `useReplyTimes(client, sessionId: string | null, live: boolean)` → `{ last: number | null; median: number | null; perTurn: Map<number, number> }` using react-query on `client.getSessionTurns(id)` with `refetchInterval: live ? 2000 : false`, a final fetch when `live` turns false, never overlapping (react-query dedupes), `enabled` only with a session id.

- [ ] **Step 1: Write the failing tests.** The hook: no session id → nothing fetched; while live it refetches every 2 s (fake timers) and stops when `live` is false after one last fetch; `last` and `median` computed with the Go rule (voice_to_voice, else ttft+ttfb); no turns or no measured turns → nulls (no zeros); the panel shows `Last reply` and `Median reply` only when numbers exist, and `Replied in X ms` under the matching bot message; nothing is shown before the first turn lands; the First run step shows the same strip.
- [ ] **Step 2: Run to see them fail.** Expected: FAIL.
- [ ] **Step 3: Implement.** Session id from the voice client state (`state.sessionId`, the token's jti); share the median helper from `callStats.ts`.
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): reply times in the Test call panel and First run`.

---

### Task 9: Go end-to-end on the echo providers

**Files:**
- Create: `server/call_records_e2e_test.go`
- Modify: `Makefile` (`test-e2e-go -run` gains `CallRecords`)

**Interfaces:**
- Consumes: the flow e2e rig (`newFlowRigE2E`, `r.dial`, `behaviorFlow`, `eventually`, `e2eCall.done`), `r.s.store.Sessions()` for reads.

- [ ] **Step 1: Write the failing tests** (read `call_behavior_e2e_test.go` first):
  - `TestCallRecordsTheCallersLabelAndAHangUp`: the token's principal (`alice`) is stored as `caller`; closing the socket stores `caller_hung_up` with an `ended_at`; an active call shows an empty reason.
  - `…SilentCaller`: a flow with `idle` short wait → `silent_caller`.
  - `…EndedByTheFlow`: a two-step flow whose model calls `end_call` (the echo model calls it on the phrase the rig already uses; check `flow_e2e_test.go`) → `ended_by_flow`.
  - `…TimeLimit`: `limits.max_call_duration` set to about 1 s → `time_limit`.
  - `…ServerShutdown`: `Shutdown` with a live call → `server_shutdown`.
  - `…SetupFailed`: a flow whose TTS build fails (`probe.failTTS`) → the row ends `setup_failed` (not a normal short call).
  - `…OrphanSweep`: a row left active, then `EndOrphans` → `server_restart`.
  - `…FirstCauseWins`: an idle hang-up then the socket closing stays `silent_caller`.
  - `…LatencyOffsets`: after a typed turn, the stored turn's `latencies` has `llm_start_ms` ≤ `llm_start_ms + llm_ttft_ms` ≤ `voice_to_voice_ms` when present, and the sessions API's `median_reply_ms` is present.
- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -tags xaispeech -run CallRecords .` — Expected: FAIL until Tasks 1–5 are in (they are: failures here are real gaps).
- [ ] **Step 3: Implement** only what the failures show missing.
- [ ] **Step 4: Run to see them pass.** `make test-e2e-go` — Expected: PASS.
- [ ] **Step 5: Commit** `test(server): every way a call ends is recorded, on the echo providers`.

---

### Task 10: The browser proves it, docs, gate, review

**Files:**
- Create: `e2e/tests-flows/40-call-records.spec.ts`
- Modify: `docs/api/v1.md`, `docs/privacy.md`, `docs/operations_runbook.md`, `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (S9 and S10 rows done; deviations removed)

- [ ] **Step 1: Write the spec** (echo, serial, `test.skip(LIVE, …)`): from the console place a Test call on a flow, send a typed turn, expect a `Replied in` note and a `Last reply` figure in the panel, hang up; open Calls: the first row shows `Console test`, `Ended` with `Caller hung up` and a median; open the call: a waterfall (`role=img` with the stage text) and `Ended: Caller hung up`; a flow with a short quiet-caller wait (`idle` through the API) called from the console and left silent → `Caller went silent` in the list.
- [ ] **Step 2: Run** `make ui-embed` then `cd e2e && npx playwright test -c playwright.flows.config.ts tests-flows/40-call-records.spec.ts` — Expected: PASS. Fix real failures in the code.
- [ ] **Step 3: Docs.** `docs/api/v1.md`: `caller`, `end_reason` and the vocabulary, `median_reply_ms`, the `latencies` offsets; `docs/privacy.md`: what is stored about the caller (a label the integration chooses, never IP or browser) and that it must not hold personal data; runbook: the vocabulary, refused calls leave no row, the 2 s turns poll of an open test panel; console spec rows.
- [ ] **Step 4: Full gate.** `gofmt -l .` empty; `go test -race ./core/... ./transport/... ./server/...`; `make test-postgres`; `cd ui && npx vitest run && npx tsc --noEmit`; `make e2e-echo` — Expected: all green.
- [ ] **Step 5: Independent review** (AGENTS.md): a fresh Opus reviewer on `main..HEAD` with Review Focus, bloat and dead code, concurrency (the holder, the transport cause), SQL placeholder safety, UI old-row rendering. Fix Critical/Important with a failing test first; ledger the rest.
- [ ] **Step 6: Commit** `docs: S9 and S10 done — who called, how it ended and reply times`.
