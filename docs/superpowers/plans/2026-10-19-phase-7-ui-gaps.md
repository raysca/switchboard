# Phase 7: Server to UI completeness (Implementation Plan)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. One task = one reviewable deliverable (one or a few conventional commits). Order: 7.0 → 7.1 → 7.2 → 7.3 → 7.4 → 7.5 → 7.6 → 7.7 → 7.8. Tasks touch disjoint files except `e2e/lib/flows-helpers.ts` (7.1 adds `openCallPage`, 7.7 reuses it) and `ui/src/api/client.ts` (7.1, 7.2, 7.3: edit sequentially, never in parallel).

**Goal:** Close the server→UI gaps of review §B that are still open on main dbcb21f, plus the console items the ledger parked for Phase 7, each with a RED test first and an end-to-end proof (Playwright on the echo rig, or the unmodified Pipecat client for wire changes).

**Architecture:** Almost all of it is UI (`ui/`) on top of endpoints that already exist and are tested (`POST /api/v1/sessions/{id}/context|run`, `readiness.missing_env`, `GET /flows?limit&offset`). The only server-side change is RTVI `bot-tts-text` (7.4): `core.TTSStoppedFrame` gains the unit's spoken `Text` (set in the one place that builds it, `core/ttsunit`), and `transport/ws` turns it into the Pipecat message. No storage, schema or REST change in this phase.

**Tech stack:** React 19 / TS / Vite / TanStack Query / zod (`ui/`), Vitest + Testing Library, Playwright (`e2e/tests-flows`, `e2e/tests-oneport`), Go 1.27.1 workspace for 7.4 (`core`, root module `transport/*`, `tools/compat`), pinned `@pipecat-ai/client-js` 1.13.1 + `websocket-transport` 1.7.2 in `tools/compat`, Pipecat reference `pipecat-impl` @ 2967e1c (goldens via `make parity-regen`).

**Spec:** `docs/superpowers/specs/2026-10-15-deep-review.md` §B (B1–B10). Master: `docs/superpowers/plans/2026-10-15-review-remediation.md` Phase 7 (Tasks 7.1–7.5). Ledger: `.superpowers/sdd/2026-10-15-review-remediation/progress.md` (Phase 7 backlog lines: 1.7 "row menu a11y" and "offline offered when unreachable", 4.6b "token before mic", 5.6 "key-bar copy"), `p5/pr-notes.md`, `p6/pr-notes.md`. Mapping: master 7.1 → 7.1; 7.2 → 7.2 (B4) + 7.3 (B5); 7.3 (B6) → skipped (done); 7.4 → 7.4; 7.5 (B9) → skipped (covered); ledger items → 7.5, 7.6; Phase 6 visible effect → 7.7; gate → 7.8.

---

## Decisions for the user

| # | Question | Recommendation |
|---|---|---|
| U1 | **`offline` offered when its stack is down (ledger 1.7 (a)).** The development profile defaults `providers.offline.*` to `127.0.0.1:8099` / `:11434` (`server/config_defaults.go:85-86`), so `GET /providers` reports offline LLM/STT/TTS `available` (`server/api/providers.go:64-74`: available = in build + "detected"; offline is keyless) whether or not anything listens. A dev without the stack can pick it in the editor and get a failed call. Options: (a) leave it; (b) probe reachability in `GET /providers` (a TCP dial per offline endpoint, cached, ~40 lines + tests). | **(a) Skip.** Development-only (production ships no offline endpoints, `server/registry_offline.go:22-33` prunes empty ones); the Providers page already has **Test** (`POST /providers/offline/test`, `server/probe.go`), which reports the refused connection; (b) puts network I/O on a list endpoint for one dev convenience. Say "b" and it becomes Task 7.9 (server, `make test-postgres` not needed). |
| U2 | **RTVI `bot-tts-text` (B8).** Pipecat's `RTVIObserver` sends it by default (`bot_tts_enabled=True`, `pipecat-impl/src/pipecat/processors/frameworks/rtvi/observer.py:180,792-794`) for each `TTSTextFrame`; client-js 1.13.1 surfaces it as `onBotTtsText` / `RTVIEvent.BotTtsText` (`tools/compat/node_modules/@pipecat-ai/client-js/dist/index.d.ts:562,629,1080`). Nothing in this repo consumes it (the console's own `VoiceClient` ignores unknown types, `ui/src/voice/VoiceClient.ts:692-694`). | **Do it (Task 7.4).** Mission is wire compatibility with stock Pipecat; cost is one frame field set in one place (`core/ttsunit/ttsunit.go:455`), one transport case, one golden, docs. The text is the spoken (cleaned) unit, already sent to the same client as `bot-llm-text`. Say "no" to keep it a declared gap in `docs/pipecat-compatibility.md:38`; then 7.4 is dropped and nothing else changes. |

## Rulings (mine; recorded in the ledger; the reviewer may challenge them)

- **R1 UI only where the server is done.** No REST or storage change in this phase. A task that finds the server contract wrong stops and reports (it is a bug for its own task, not a UI workaround).
- **R2 zod mirrors the server, never stricter.** New schemas use `.optional()` for fields the server omits (`omitempty`) and never `z.enum` on values the server may extend (e.g. readiness reasons stay `z.string()`); the live-call response is parsed as `{ data: { accepted: number } }` exactly as `server/live_calls_api.go:27-29,82` sends it.
- **R3 Live-call controls (B1) are minimal and safe.** One note per request (the API takes up to 8; nobody needs batching from a text box), a "Reply now" checkbox mapped to `run` (default **off**, as the API), and an "Ask for a reply" button (`POST .../run`). Shown only while `session.status === "active"`. Notes live in component state only: cleared on success, never in the URL, `localStorage`, the query cache key, or `console`. No auto-retry on 429/409 (the operator retries; `Retry-After: 1`). Error copy per status: 400 server message, 404 "The call has ended, or it runs on another server instance.", 409 "The call is still starting. Try again in a moment.", 429 "Too many requests for this call. Wait a second and try again." A 404 also refetches the session (status flips to ended).
- **R4 `getFlows` follows `offset` in the one client function** (root cause for all seven callers: `ui/src/routes/{dashboard/DashboardView.tsx:54, flows/FlowsView.tsx:25, mcp/MCPView.tsx:25, onboarding/OnboardingView.tsx:60, providers/ProvidersView.tsx:146, sessions/CallView.tsx:42, sessions/CallsView.tsx:41}`), pages of 100 (the server max, `server/api/api.go:111-118`), stops at a short page, bounded by `FLOW_PAGES_MAX = 10` (1000 flows). The server orders by `id` (`server/storage/repositories.go:136`), so offset paging is stable. No `useFlows` hook (YAGNI: callers keep their queries).
- **R5 `bot-tts-text` timing.** Emitted from `transport/ws` when the unit's `TTSStoppedFrame` arrives (synthesis complete), immediately before `bot-tts-stopped`, with `{"text": <unit text>}`; a unit cut off before `TTSStopped` sends none. This is Pipecat's `push_text_frames` path (`observer.py:767-768`: "TTSTextFrame arrives after synthesis completes"). Declared difference: Pipecat queues the text until `bot-started-speaking` (`observer.py:675-682`); here `bot-started-speaking` follows playback (pacer) and is not reordered against it. Word-level `bot-tts-text` (word-timestamp TTS) and `bot-output` (RTVI 2.x aggregate, `index.d.ts:548`) stay out of scope (Phase 8.4 matrix).
- **R6 Playwright determinism.** No `waitForTimeout` in new specs (existing `pace()` in `e2e/lib/mcp-helpers.ts:46` is not reused); every wait is an `expect(...)` on a visible state or `expect.poll` on the API. A second page in the same browser context holds the operator view while the first holds the call (`page.context().newPage()`; the key is per tab, so it signs in again).
- **R7 Route-mocked Playwright is acceptable only for states the echo stack cannot reach** without a server restart (7.2 `missing_env`), with the precedent `e2e/tests-flows/10-providers.spec.ts:26-43`; the request still goes through the real server and only the field is altered.
- **R8 Out of scope, carried:** `bot-output` and word-level TTS text (Phase 8.4 matrix); `turn_count`/"Turns" counting idle and greeting lines (Phase 6 behaviour change, documented in `docs/api/v1.md`; no UI relabel); flow version history UI beyond the editor's existing compare (B2, D-0.6).

---

## Verified facts (2026-10-19, worktree `feat/phase-7-ui-gaps` = main dbcb21f; commands actually run)

1. **B1 still open, server complete.** `server/live_calls_api.go` (83 lines): `POST /api/v1/sessions/{id}/context|run` (route matched `:14-25`, dispatched `server/api_routes.go:138-139` behind management auth); statuses: 405 non-POST (`:34-37`), 400 `invalid_request` (decode/validate, `:40-47`), 404 `not_found` "no live call with this id on this instance" (`:50-52`) or "the call has ended" (`:59-61`, `:78-79`), 429 `rate_limited` + `Retry-After: 1` (`:53-56`), 409 `call_not_ready` (`:62-64`), 429 `too_many_pending` (`:73-76`), 202 `{data:{accepted:n}}` (`:82`). Validation `server/live_calls.go:194-209`: 1–8 notes, each trimmed 1–4096 **bytes**, no NUL; limits `:15-22` (burst 4, 1/s). Notes are added server-side with the prefix `[note from the operator] ` (`:19,65`) into the in-memory history only; the stored turn for a note-triggered reply has transcript `[async tool result:context_note]` (`core/async_trigger.go:369`), which the UI already hides from the caller's words (`ui/src/routes/sessions/callStats.ts:50,113`). Logs carry counts only (`live_calls_api.go:81`). Go tests: `server/live_calls_api_test.go` (10 tests incl. auth, statuses, rate limit, ended call), `server/live_calls_test.go`, e2e `server/context_injection_e2e_test.go:14` (echo repeats the note: reply contains "the caller is a VIP"; run-only reply contains `run_llm`). Docs `docs/api/v1.md:21-22,38`. UI: `git grep -n 'context\|/run' ui/src/api/client.ts` → no live-call method; `CallView.tsx` (225 lines) is read-only; `refetchInterval` 5 s while `status === "active"` (`:14,20-26`).
2. **B4 still open.** Server: `Readiness.MissingEnv` `json:"missing_env,omitempty"` (`server/flow/service.go:91`, set `:471`; may repeat a name across stages, `server/flow/config.go:326`). UI: `readinessSchema` (`ui/src/api/client.ts:166-170`) has `state`, `reasons`, `tool_dependencies` only → zod strips `missing_env`; `FlowsView.tsx:159-167` renders `reasons` only. The providers schema already has `missing_env` (`client.ts:98-99`) and `StageFields.tsx:28` de-duplicates with `new Set` (pattern to copy).
3. **B5 still open (softened).** `getFlows` = `request("/flows?limit=100")` (`client.ts:552-553`); `FlowsView.tsx:12 LIST_LIMIT = 100`, `:142` shows "Showing the first 100 flows." when hit; the other six callers (R4) silently miss flow 101. Server: `ParsePagination` default 50, max 100, `offset ≥ 0` (`server/api/api.go:111-125`); `List` `ORDER BY id LIMIT ? OFFSET ?` (`server/storage/repositories.go:136`).
4. **B8 still open.** `git grep -n 'bot-tts-text' -- transport core server` → nothing; `docs/pipecat-compatibility.md:38` declares "`bot-tts-text` and `vad-*` messages are not emitted". `TTSStoppedFrame{Turn,Gen,UnitID}` (`core/frame.go:314-319`) is built only in `core/ttsunit/ttsunit.go:455` (product) and `tools/provtest/fake.go:184` (fake), `git grep -n 'TTSStoppedFrame{'`; every product TTS (google unary/streaming, xai unary/streaming, cartesia, elevenlabs, nvidia, echo = google unary with an echo client `server/registry_echo.go:31-40`) runs on ttsunit (Phase 6). `coretest` TTS emits no TTSStarted/Stopped (`core/coretest/doubles.go` grep empty) → `tools/compat/client_compat_test.go` (coretest pipeline) cannot see it; the real-server client-js driver `server/tool_events_client_e2e_test.go:17` + `tools/compat/test_rtvi_function_calls.js` (echo TTS through `stageProbe`, `server/flow_e2e_test.go:122-149`) can. Transport output switch: `transport/ws/transport.go:1033-1041` (`FrameTTSStarted`/`FrameTTSStopped` → `FormatBotTTSStarted/Stopped`, `transport/rtvi/rtvi.go:371-377`); trace naming of the frame carries no text (`core/session.go:970-971`). Golden infra: `tools/compat/parity/gen_goldens.py:500-552` (function-call RTVIObserver capture), `tools/compat/parity_fixtures_test.go:700-723` (`TestParityFunctionCallMessages`, `rtviTypeAndData`), `make parity-regen` (`Makefile:76-78`, venv present: `pipecat-impl/.venv/bin/python`). Wire size: `MaxMessageDataBytes = 16 KB` (`transport/ws/wire.go:17`). `go test -count=1 ./transport/rtvi/` → `ok govoice/transport/rtvi 1.380s`. Console event log records every incoming type (`VoiceClient.ts:526 recordEvent`), filterable in Playwright (`42-tool-calls.spec.ts:21-23`).
5. **Mic-before-token (ledger 4.6b) still open.** `VoiceClient.connect` (`ui/src/voice/VoiceClient.ts:233-275`): Step 1 `issueVoiceToken` (`:235-256`), Step 2 `startCapture` (`:258-272`), Step 3 socket (`:277+`). Token lifetime is capped at 1 min (`effectiveVoiceTokenLifetime`, Phase 4.6b), so a mic prompt left > 60 s yields a 401 at dial. Tests pinning today's order: `ui/src/voice/VoiceClient.test.ts:30-52` (cancel during token / during mic) and `:95-106` ("asks for a draft token only when given a draft": with a real `BrowserAudioController` and no `navigator.mediaDevices` it would fail at the mic once the order flips → must stub the audio controller).
6. **Row menu a11y (ledger 1.7 (b)) partly done.** `FlowsView.tsx:197-219`: native `<details>`/`<summary aria-label="More actions for …">`, Escape closes and refocuses (`:199-203`), items close the menu on click (`:211`). Missing: close on outside click / focus leaving (`ui/src/styles.css:1975` ponytail note), and any Vitest for Escape/close (`FlowsView.test.tsx:137,145` only click the items).
7. **Key-bar copy (ledger 5.6).** `ui/src/app/App.tsx:203` "The key stays in this tab only (reloading clears it)"; Sign out is `:192-194` (topbar, only with a typed key); pinned by `App.test.tsx:31,73`.
8. **Phase 6 visible effect.** Idle prompt/goodbye/fallback are stored turns (empty transcript, line as `assistant_text`, `p6/pr-notes.md` Behaviour 1). The call page renders them through `turnRows` (`callStats.ts:132-151`, text = `assistant_text`) and `transcript` (`:90-127`, labelled by step); no Vitest pins a say-turn fixture; Playwright `38-call-behavior.spec.ts:56-66` asserts them only in the live console transcript, not on the call page; Go `TestCallBehaviorIdleLinesAreInTheCallsTurns` (`server/call_behavior_e2e_test.go:195`) pins storage.
9. **Echo rig facts for e2e.** Flows stack idle is off (`e2e/global-setup-flows.ts:88` `idle: user_timeout: 0s`), offline endpoints emptied (`e2e/lib/echo-config.ts:10`), echo `mimic: instant`, `label_replies: true`. Holding a call: `openFlowTest` + `startCall` (mutes the fake mic) + `sendText` (`e2e/lib/flows-helpers.ts:81-106`); session id shown in `data-testid="session-summary"` `<code>` and `view-trace-link` (`FlowVoiceTest.tsx:308-321`). `connectAdmin` (`flows-helpers.ts:59-64`). No spec uses a second page yet (`git grep -n 'newPage()' e2e` → none).
10. **UI conventions.** Primitives `Status, EmptyState, LoadingState, ErrorState, FormField, ActionButton, DataTable` (`ui/src/components/primitives.tsx`); textarea pattern `ToolTestConsole.tsx:48-60` (`<label htmlFor>` + `<textarea>`, error `<p role="alert">`); colours only via tokens (`ui/src/test/noHardcodedColors.test.ts`, globbed in Phase 5); `settingCopy.ts` is the flow editor's help table only (not used by the call page). Mutations elsewhere use TanStack `useMutation`; `queryClient.clear()` on sign-out also clears the mutation cache (Phase 5 M4 pin).
11. **Skipped items verified** (see "Skipped"): B6 `not_in_build` (`server/api/providers.go:66-67`, `server/providers_test.go:121 TestProvidersNotInBuild`, since cc8e77f 2026-10-11; UI offers only `available` providers, `StageFields.tsx:24`); B7 deepgram on the STT runner with `Runner.STTCounts` (`core/sttunit/runner.go:183`); B9 `TestCallBehaviorAFlowsQuietCallerSettingsPromptThenEndTheCall` (`server/call_behavior_e2e_test.go:63`, in `test-e2e-go`'s `CallBehavior` filter, `Makefile:63`) + Playwright `38-call-behavior.spec.ts:62-65` (`user_idle`, prompt, goodbye, hang-up); B10 Sign out (Phase 5.6b); traces `arguments_summary/result_summary` gone from UI and API (`git grep` → only the unused DB columns); MCP enable/disable store errors → 500 (`docs/api/v1.md:23-24`, `server/mcp/http.go:26,269`).
12. **Tooling in this worktree.** No `node_modules` in `ui/`, `e2e/`, `tools/compat/` of the plan worktree (`ls -d` failed): Task 7.0 installs them with `npm ci` in each (private worktree; never in the main checkout). Vitest files: `git ls-files 'ui/src/**/*.test.ts*' | wc -l` → 61; Phase 5 left knip empty and vitest at 701 tests (ledger); 7.0 re-measures.

---

## Global Constraints

Inherited from `AGENTS.md` and the master plan.

- `core/` never imports `server/`; the one `core` addition (`TTSStoppedFrame.Text`) is consumed on the server call path by `transport/ws`.
- Bounded everything: paging loop capped (R4); no new channel/queue; RTVI text ≤ unit size (≤ 16 KB wire limit); UI state holds one draft note.
- Go: errors `%w`, `errors.Is/As`; transport writers keep their `recover()`; `go test -race`; `gofmt -l` empty; `go vet` in all three tag sets.
- UI: no new dependency; tokens only (no hard-coded colours); every control labelled, keyboard reachable, `aria-live`/`role="status"` for async outcomes, `role="alert"` for errors; React escaping only (no `dangerouslySetInnerHTML`); knip must stay empty (`npx --yes knip@5 --no-progress`); `npx tsc --noEmit -p .` 0 errors.
- Privacy: operator notes and TTS text are never logged, stored client-side, or put in URLs; no secret or management key in UI state beyond the existing in-memory key.
- Conventional commits; trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Provider secrets env-only. Never run paid/live targets; never `offline-up/models/down`; never kill processes by pattern; scratch files only under the session scratchpad or `.superpowers/` (git-ignored), never `/tmp/<generic>`; run gates on an otherwise idle machine.

### Gate block (after EVERY task; substitute the task id for `7.X`)

```bash
L=.superpowers/sdd/2026-10-15-review-remediation/flake; mkdir -p "$L"; set -o pipefail
P=.superpowers/sdd/2026-10-15-review-remediation/p7; B=$(cat $P/base-7.X)   # recorded at task start: git rev-parse HEAD
gofmt -l $(git ls-files '*.go' | grep -v '^pipecat-impl/') | tee "$L/7.X-gofmt.log"; test ! -s "$L/7.X-gofmt.log"
(cd ui && npx tsc --noEmit -p . && npx vitest run) 2>&1 | tee "$L/7.X-ui.log"
(cd ui && npx --yes knip@5 --no-progress) 2>&1 | tee "$L/7.X-knip.log"; diff $P/knip-before.txt "$L/7.X-knip.log"   # no new finding
make verify 2>&1 | tee "$L/7.X-verify.log"
make e2e-echo 2>&1 | tee "$L/7.X-e2e-echo.log"          # Go e2e + Playwright e2e-fake (flows + oneport)
make provider-matrix-dry 2>&1 | tee "$L/7.X-provider-matrix-dry.log"
# 7.4 only (core/transport touched): also
make loadtest-smoke 2>&1 | tee "$L/7.X-loadtest-smoke.log"
# last, so a down stack skips only this target (report it; never start the stack):
if curl -fsS http://127.0.0.1:8099/healthz >/dev/null && curl -fsS http://127.0.0.1:11434/api/tags >/dev/null; then make offline-test 2>&1 | tee "$L/7.X-offline-test.log"; else echo "offline stack is down: offline-test NOT run, report it"; fi
git diff --stat "$B"..HEAD | tail -1 | tee "$L/7.X-diffstat.log"
```

Expected: `ui.log` 0 tsc errors and every Vitest file/test passed (count ≥ the 7.0 count plus the task's new tests; the colour test `noHardcodedColors` is in it); knip output identical to 7.0's (Phase 5 left it clean); `=== All verify checks passed ===` (the recorded `PLAYBACK_CLEAR_REQUIREMENT_FAILED … relax-unmodified-client-playback-clear` line is expected); `make e2e-echo` exits 0 and every Playwright test passed (list the new ones by name); `provider-matrix-dry` exits 0; `loadtest-smoke` `Verdict: PASS`; `offline-test` prints `ok` for both packages. `make test-postgres` is **not** in the per-task gate (no `server/storage` change); 7.8 runs it once as a guard. On failure: name the failing test (`grep -nE -- '--- FAIL|^FAIL|✘|failed' <log>`), keep the log, never overwrite it with a rerun. Known intermittent `TestOfflineStackLongCallerSpeechIsOneTurn`: rerun alone 5× (Phase 4 command) and report every result.

**Per-commit compile check (7.4 only, Go touched):** the Phase 6 loop over 13 modules × three tag sets (`go build` + `go vet`), expected no `FAIL` line.

**Rollback (every task):** `git revert` the task's commits newest first. No schema, REST or stored-data change; 7.4 adds one RTVI message (a client that does not know it ignores it).

---

## Task 7.0 — Baseline (no product change)

**Files (git-ignored):** `.superpowers/sdd/2026-10-15-review-remediation/p7/{base-7.0,knip-before.txt,ui-before.log}`, `flake/7.0-*.log`.

- [ ] **Step 1:** `P=.superpowers/sdd/2026-10-15-review-remediation/p7; mkdir -p $P; git rev-parse HEAD > $P/base-7.0`. Install deps in this worktree only: `(cd ui && npm ci) && (cd e2e && npm ci) && (cd tools/compat && npm ci)`.
- [ ] **Step 2:** `(cd ui && npx tsc --noEmit -p . && npx vitest run) | tee $P/ui-before.log` (record files/tests counts); `(cd ui && npx --yes knip@5 --no-progress) > $P/knip-before.txt` → expected empty.
- [ ] **Step 3:** full gate block on base (no diffstat) so later failures are attributable. Ledger line: base sha, vitest counts, knip result, gate results, offline stack up/down.
- [ ] No commit.

---

## Task 7.1 — Live-call operator notes on the call page (B1)

**Files:** `ui/src/api/client.ts` (+ `client.test.ts`), `ui/src/routes/sessions/LiveCallControls.tsx` (new) + `LiveCallControls.test.tsx` (new), `ui/src/routes/sessions/CallView.tsx` (+ `CallView.test.tsx`), `ui/src/styles.css`, `e2e/lib/flows-helpers.ts` (`openCallPage`), `e2e/tests-flows/44-live-call-notes.spec.ts` (new), `docs/api/v1.md` (one sentence: the console's call page sends these).

Interface:
```ts
// client.ts (inside createApiClient's returned object; schema module-private)
const acceptedEnvelope = z.object({ data: z.object({ accepted: z.number() }) });
addSessionNote: async (id: string, content: string, run: boolean) =>
  (await request(`/sessions/${encodeURIComponent(id)}/context`, acceptedEnvelope,
    { method: "POST", body: { messages: [{ content }], run } })).data,
runSession: async (id: string) =>
  (await request(`/sessions/${encodeURIComponent(id)}/run`, acceptedEnvelope, { method: "POST" })).data,

// LiveCallControls.tsx
export function LiveCallControls(props: { client: ApiClient; sessionId: string; onEnded: () => void }): JSX.Element
```
Markup: `<section aria-labelledby="call-control-title" className="call-section call-control">`, `<h2 id="call-control-title">Guide this call</h2>`, help `<p className="subtext">` ("A note reaches the agent as an operator note, never as the caller's words, once the call is quiet. It is not saved with the call."), `<label htmlFor="call-note">Note for the agent</label><textarea id="call-note" rows={3} maxLength={4096} …>`, `<label className="check-row"><input type="checkbox"> Reply now</label>`, `<ActionButton type="submit" disabled={pending || !note.trim()}>Send note</ActionButton>`, `<button type="button" className="action-button-small">Ask for a reply</button>`, outcome `<p role="status" aria-live="polite">` ("Note sent." / "Reply requested."), error `<p role="alert" className="call-control__error">` (R3 copy). Two `useMutation`s (no cache keys). On success: clear the textarea, keep focus in it. On 404: show the copy and call `onEnded` (CallView passes `() => void sessionQuery.refetch()`). CallView renders it right after `.call-figures`/route notes (before `.call-columns`) only when `s.status === "active"`.

- [ ] **Step 1 (RED, Vitest):** `client.test.ts`: `addSessionNote` posts `{"messages":[{"content":"vip"}],"run":true}` to `/api/v1/sessions/s%2F1/context` (id encoded) and returns `{accepted:1}`; `runSession` posts no body to `…/run`; a 429 `too_many_pending` becomes an `ApiError` with `status 429` and the code. `LiveCallControls.test.tsx`: (a) labelled textarea, checkbox and both buttons reachable by role/name; Send disabled for whitespace; (b) success → `client.addSessionNote("s1","the caller is a VIP",false)`, textarea cleared, status "Note sent."; Reply now checked → `run: true`; (c) Ask for a reply → `runSession("s1")`, status "Reply requested."; (d) 404 → alert with the ended copy and `onEnded` called once; (e) 409, 429, 400 → their copy (400 shows the server message); (f) the note text never appears in `document.location`, `localStorage` (spy) or `console` (spy) during a send; (g) keyboard: Tab order textarea → checkbox → Send → Ask. `CallView.test.tsx`: controls shown for an `active` session, absent for `ended` (and absent with no key). All fail (no module/method).
- [ ] **Step 2:** implement client methods, component, CallView wiring, CSS (`.call-control`, `.call-control__error` with existing tokens only, e.g. `var(--bad)` as `.tool-console__error` uses — copy its rule). `cd ui && npx tsc --noEmit -p . && npx vitest run` → green; knip empty (the component is imported by CallView; methods are object members).
- [ ] **Step 3 (Playwright, echo):** `e2e/lib/flows-helpers.ts`:
```ts
// The call page in a second tab of the same browser. The key lives in each tab's memory (a full load
// drops it, 40-layout.spec.ts:25), so this tab loads the call URL first and signs in there.
export async function openCallPage(page: Page, stack: Stack, sessionId: string): Promise<Page> {
  const ops = await page.context().newPage();
  await ops.goto(`${stack.uiUrl}/sessions/${encodeURIComponent(sessionId)}`);
  await ops.getByLabel("Management key").fill(stack.managementKey);
  await ops.getByRole("button", { name: "Connect", exact: true }).click();
  await expect(ops.getByRole("heading", { level: 1, name: `call_${sessionId.slice(0, 8)}` })).toBeVisible();
  return ops;
}
```
Spec `44-live-call-notes.spec.ts` (flow = the suite's `FLOW`, echo): `openFlowTest` → `startCall` → `sendText("hello")` → `expect(botReply(page, /hello/))`; read the id from `page.getByTestId("session-summary").locator("code")`; `ops = openCallPage(...)`; on `ops`: `getByRole("region", {name: "Guide this call"})` visible, fill "the caller is a VIP", check "Reply now", Send → status "Note sent."; on `page`: `expect(botReply(page, /the caller is a VIP/)).toBeVisible()`; on `ops`: "Ask for a reply" → status "Reply requested."; on `page`: `expect(botReply(page, /run_llm/)).toBeVisible()` (echo's reply to a run-only request, `server/context_injection_e2e_test.go:39`); `page.getByTestId("disconnect-btn").click()`; on `ops`: `expect(region).toBeHidden({timeout: 15_000})` (5 s poll) **or**, if it is still shown, Send → alert with the ended copy and then hidden; finally the note text appears nowhere on the call page (`expect(ops.getByText("the caller is a VIP")).toHaveCount(0)` — it is not stored; the echo reply is, so assert on the transcript that the caller line is "hello" and no caller line starts with "[note"). `step(...)` evidence after each phase.
- [ ] **Commit(s):** `feat(ui): operator notes and reply requests on a live call's page (B1)`; `test(e2e): live-call notes from the call page reach the running call`. **Gate.**

---

## Task 7.2 — Readiness names the missing variables (B4)

**Files:** `ui/src/api/client.ts` (`readinessSchema`), `ui/src/routes/flows/FlowsView.tsx` (+ test), `e2e/tests-flows/10-providers.spec.ts` (one test).

- [ ] **Step 1 (RED):** `client.test.ts`: a flow with `readiness: {state:"not_ready", reasons:["missing_credential_llm"], missing_env:["XAI_API_KEY"]}` parses with `missing_env` kept; without the key it parses (optional). `FlowsView.test.tsx`: such a flow's row shows "Set XAI_API_KEY" once even when `missing_env` repeats the name (`["XAI_API_KEY","XAI_API_KEY"]`), inside the row's reasons list; a disabled flow shows no reasons (today's rule, `:159`). Fail today (zod strips the field).
- [ ] **Step 2:** `readinessSchema` gains `missing_env: z.array(z.string()).optional()` (R2). In the row, after the reason chips: `{[...new Set(f.readiness.missing_env ?? [])].map((v) => <span key={v} className="reason">Set <code>{v}</code></span>)}`. Names only, never values (the server sends names only, `docs/api/v1.md:71`).
- [ ] **Step 3 (Playwright, R7):** in `10-providers.spec.ts`, `test("flows list names the variables a not-ready flow is missing")`: `page.route("**/api/v1/flows?*", …)` fetches the real list and sets, on the suite's flow, `readiness = {state:"not_ready", reasons:["missing_credential_llm"], missing_env:["E2E_MISSING_LLM_KEY"]}`; `connectAdmin` → `openFlowsList` → the row contains "missing_credential_llm" and "Set E2E_MISSING_LLM_KEY". (The route glob must still match after 7.3 adds `&offset=`.)
- [ ] **Commit:** `feat(ui): a not-ready flow names the environment variables to set (B4)`. **Gate.**

---

## Task 7.3 — Every flow, not the first 100 (B5)

**Files:** `ui/src/api/client.ts` (`getFlows`, `FLOW_PAGES_MAX`), `ui/src/routes/flows/FlowsView.tsx`, tests, `e2e/tests-flows/70-many-flows.spec.ts` (new; `70-` so it runs last in the shared flows stack and cannot change earlier specs' lists).

```ts
const FLOW_PAGE = 100; // the server's max page (server/api/api.go ParsePagination)
export const FLOW_PAGES_MAX = 10; // ponytail: 1000 flows; a server-side search replaces this if anyone gets near it
getFlows: async () => {
  const all: FlowDTO[] = [];
  for (let page = 0; page < FLOW_PAGES_MAX; page++) {
    const batch = (await request(`/flows?limit=${FLOW_PAGE}&offset=${page * FLOW_PAGE}`, flowsEnvelope)).data;
    all.push(...batch);
    if (batch.length < FLOW_PAGE) break;
  }
  return all;
},
```
FlowsView: `LIST_LIMIT` becomes `FLOW_PAGES_MAX * 100` (import the constant; the note reads "Showing the first 1000 flows.").

- [ ] **Step 1 (RED, Vitest):** fetcher returns 100 flows for `offset=0` and 1 for `offset=100` → `getFlows()` resolves 101 flows and made exactly 2 requests with those query strings; exactly-100 total → 2 requests (second empty); a full page every time → stops after `FLOW_PAGES_MAX` requests; a 500 on page 2 rejects (no partial list). FlowsView: the "Showing the first …" note appears only at the cap.
- [ ] **Step 2:** implement; vitest green; `git grep -n 'limit=100' ui/src` → none.
- [ ] **Step 3 (Playwright, echo):** `70-many-flows.spec.ts`: create 101 flows `E2E Many 001`…`E2E Many 101` through `api(stack, "/flows", {method:"POST", …})` with the same echo stage config `mcpFlow` uses (`stage()` helper), in batches of 10 (`Promise.all`); `expect.poll(...)` that `GET /flows?limit=100&offset=100` is non-empty (the real server pages); `connectAdmin` → `openFlowsList`; filter "E2E Many 1" and expect the links `E2E Many 100` and `E2E Many 101` visible; no "Showing the first" note; Calls page flow filter (`CallsView`) lists `E2E Many 101` (second consumer through the same function).
- [ ] **Commit:** `fix(ui): flow lists follow the server's pages instead of stopping at 100 (B5)`. **Gate.**

---

## Task 7.4 — RTVI `bot-tts-text` (B8; U2)

**Files:** `core/frame.go` (`TTSStoppedFrame.Text`), `core/ttsunit/ttsunit.go:455`, `core/ttsunit/ttsunit_test.go`, `transport/rtvi/rtvi.go` (+ test), `transport/ws/transport.go:1038-1041` (+ test), `tools/compat/parity/gen_goldens.py`, `tools/compat/testdata/parity/tts_text.json` (generated), `tools/compat/parity_fixtures_test.go`, `tools/compat/test_rtvi_function_calls.js`, `server/tool_events_client_e2e_test.go`, `e2e/tests-flows/42-tool-calls.spec.ts` (one assertion), `docs/pipecat-compatibility.md:38`, `docs/pipecat-parity-matrix.md:98`.

```go
// core/frame.go
type TTSStoppedFrame struct {
	Turn   string
	Gen    uint64
	UnitID uint64
	Text   string // the unit's text as it was synthesized (after cleaning); the transport sends it as bot-tts-text
}
// transport/rtvi/rtvi.go
TypeBotTTSText = "bot-tts-text"
func (h *Handler) FormatBotTTSText(text string) (string, error) // {"text": text}, like FormatBotLLMText
```
ttsunit: `&core.TTSStoppedFrame{…, Text: u.Frame.Text}`. Transport `FrameTTSStopped`: if `Text != ""`, write `bot-tts-text` then `bot-tts-stopped` (R5). Nothing else reads the field; traces stay text-free (`core/session.go:970`).

- [ ] **Step 1 (golden first):** `gen_goldens.py`: `_tts_text(steps)` feeding `TTSStartedFrame`, `TTSTextFrame(text, aggregated_by=AggregationType.SENTENCE)`, `TTSStoppedFrame` to an `RTVIObserver(params=RTVIObserverParams(bot_output_enabled=False))` (record the param in the scenario; `bot-output` is out of scope, R5) after a `BotStartedSpeakingFrame` (so the observer does not queue) and capture only `bot-tts-*` types. The observer handles a text frame only when the `FramePushed` source is a `BaseOutputTransport` (`observer.py:515-519`), so the harness passes an instance as `source` for the text frame (e.g. `BaseOutputTransport(TransportParams())`; if it cannot be built offline in the venv, stop and report rather than hand-write the golden); scenarios `one_unit` ("Hello there."), `two_units`. `make parity-regen` → `tools/compat/testdata/parity/tts_text.json`; expected Pipecat lines: `{"data":{},"type":"bot-tts-started"}`, `{"data":{"text":"Hello there."},"type":"bot-tts-text"}`, `{"data":{},"type":"bot-tts-stopped"}`. Confirm by reading the JSON (and `git diff --stat tools/compat/testdata` shows only the new file; if other goldens change, stop: the venv drifted).
- [ ] **Step 2 (RED):** `TestParityTTSTextMessages` (`runParity(t, "tts_text", …)` mapping `start`→`FormatBotTTSStarted`, `text`→`FormatBotTTSText`, `stop`→`FormatBotTTSStopped`) fails (undefined). `transport/ws`: `TestTTSStoppedSendsTTSTextFirst` (a `TTSStoppedFrame{Text:"Hi."}` through the output switch → wire messages `bot-tts-text {"text":"Hi."}` then `bot-tts-stopped`; empty `Text` → only `bot-tts-stopped`) — copy the harness of `transport/ws/bot_interrupted_test.go`. `core/ttsunit`: the stopped frame of a finished unit carries the unit text; a unit cancelled by barge-in emits no stopped frame (existing behaviour, assert anyway). Client-js: `test_rtvi_function_calls.js` collects `onBotTtsText` texts into `EVENTS.tts`; `TestMCPCallPipecatClientSeesFunctionCallEvents` asserts `got.TTS` contains a text containing `barrier` (the echo reply "You said: barrier", spoken, so it went through TTS) and every entry is non-empty. All red.
- [ ] **Step 3:** implement. `cd core && go test -race ./ttsunit/ ./...`; `go test -race ./transport/... ./tools/compat/...`; `cd server && go test -race -count=1 -tags xaispeech -run 'MCPCall|BargeIn|ToolEvents' .`; provider modules `go test -race ./...` (they embed ttsunit; nothing should change).
- [ ] **Step 4 (Playwright):** `42-tool-calls.spec.ts`, after the barrier reply: `page.getByLabel("Filter events").fill("bot-tts-text")` → the RTVI event log has ≥ 1 `code` with `bot-tts-text` (`expect(...locator("code").first()).toHaveText("bot-tts-text")`), then restore the filter. No new spec.
- [ ] **Step 5 (docs):** `pipecat-compatibility.md:38`: replace "`bot-tts-text` and `vad-*` messages are not emitted." with "`bot-tts-text` (`{text}`, the spoken unit text after cleaning) is sent once per synthesis unit when its synthesis completes, just before `bot-tts-stopped`; a unit cut off before then sends none, and it is not held back until `bot-started-speaking` as Pipecat does. `bot-output` and `vad-*` messages are not emitted." Matrix row "RTVI wire": add the golden and the client-js test to Evidence.
- [ ] **Commit(s):** `test(compat): Pipecat golden for bot-tts-text`; `feat(transport): RTVI bot-tts-text per synthesized unit (B8)`. **Gate** (+`loadtest-smoke`; per-commit compile check).

---

## Task 7.5 — Ask for the microphone before the voice token (ledger 4.6b)

**Files:** `ui/src/voice/VoiceClient.ts:233-275`, `ui/src/voice/VoiceClient.test.ts`.

New order: state `requesting_token` (kept; the UI label is generic) → **Step 1 mic** (`startCapture`) → abandoned? return → **Step 2 token** (on failure: `audioController.stopCapture()` before the error state) → abandoned? return (mic already released by `disconnect`) → Step 3 socket. `sessionId` set after the token as today.

- [ ] **Step 1 (RED):** rewrite `VoiceClient.test.ts:30-52` for the new order: (a) "asks for the mic first and the token only after it is granted" (`issueVoiceToken` not called while `capture` is pending; called once after it resolves); (b) "a mic denied asks for no token" (`startCapture` rejects `permission_denied` → error category `mic`, `issueVoiceToken` never called); (c) "a token failure after the mic releases the mic" (`stopCapture` called, error category `token`); (d) disconnect while the mic opens → no token request, no socket; (e) disconnect while the token is fetched (mic open) → mic released (`destroy`), no socket. Fix `:95-106` (draft token) by using the stubbed `setup()` audio controller with `capture.resolve()`. (a)–(c) fail today.
- [ ] **Step 2:** swap the two blocks; keep every `attempt !== this.attempt` guard after each await. Vitest green.
- [ ] **E2E coverage:** every Playwright spec that starts a call (`startCall`: 12 spec files in `e2e/tests-flows` today, plus 44) exercises the new order with Chrome's fake mic; the denied path cannot be produced with `--use-fake-ui-for-media-stream` (auto-grant), so it is Vitest-only (recorded).
- [ ] **Commit:** `fix(ui): ask for the microphone before minting the one-minute voice token`. **Gate.**

---

## Task 7.6 — Row menu closes when focus leaves; key-bar copy (ledger 1.7 (b), 5.6)

**Files:** `ui/src/routes/flows/FlowsView.tsx:197-219` (+ test), `ui/src/styles.css:1975` (drop the ponytail note), `ui/src/app/App.tsx:203` (+ `App.test.tsx:31,73`), `e2e/tests-flows/20-flow-lifecycle.spec.ts:249-258`.

- [ ] **Step 1 (RED, Vitest):** FlowsView: open "More actions for X" (click summary) → `details` open; Escape → closed and focus on the summary (pins today's behaviour); open, then focus another control outside the menu (`user.tab()` past it / `user.click(document.body)`) → closed; Tab from the summary into "Duplicate X" keeps it open. App: copy "The key stays in this tab only, until you sign out or reload." (update `App.test.tsx:31,73`).
- [ ] **Step 2:** `<details onBlur={(e) => { if (!e.currentTarget.contains(e.relatedTarget as Node | null)) e.currentTarget.open = false; }}>` (focus leaving = outside click on focusable things and Tab-away; a click on non-focusable page background still leaves it open — acceptable, `ponytail:` comment names it). No `aria-expanded` added: native `<summary>` exposes the expanded state. Copy change in App.tsx.
- [ ] **Step 3 (Playwright):** in `20-flow-lifecycle.spec.ts` before "Duplicate": open the row menu, press Escape → the Duplicate button hidden and the summary focused (`toBeFocused()`); reopen and continue as today.
- [ ] **Commit:** `fix(ui): the flow row menu closes when focus leaves it; key-bar says sign out clears the key`. **Gate.**

---

## Task 7.7 — Idle lines on the call page (Phase 6 user-visible effect)

**Files:** `ui/src/routes/sessions/callStats.test.ts`, `ui/src/routes/sessions/CallView.test.tsx`, `e2e/tests-flows/38-call-behavior.spec.ts` (end of the first test). No product change expected; if a test exposes a rendering bug, fix it in `callStats.ts` in the same commit.

- [ ] **Step 1 (tests that pin today):** `turnRows`/`transcript` with a say-turn fixture (`transcript: ""`, `assistant_text: "Are you still there?"`, no `latencies`) and a goodbye turn: rows show the lines, no "Replied in" note, transcript lines are the step's (not "Caller"), the median reply ignores them; a note-triggered turn (`transcript: "[async tool result:context_note]"`, `assistant_text: "…"`) shows only the reply (no caller line). CallView: an ended `silent_caller` call shows "Caller went silent" (`callStats.ts:204`). Expected green on base (characterization); a red one is a bug to fix here.
- [ ] **Step 2 (Playwright):** in `38-call-behavior.spec.ts`, read the session id from `session-summary` right after "the draft call asked and hung up"; at the end of the test, `openCallPage(page, stack, id)` (7.1 helper) → the Transcript region (`getByRole("list", {name: "Transcript"})`) contains `PROMPT` and `BYE` as agent lines, no "Caller" line, and the figures show "Caller went silent".
- [ ] **Commit:** `test(ui): idle prompts and goodbyes read as the agent's lines on the call page`. **Gate.**

---

## Task 7.8 — Phase gate, adversarial review, PR notes

- [ ] **Step 1:** `git diff --stat $(cat $P/base-7.0)..HEAD | tail -1`; per-area numstat (ui, e2e, Go non-test, Go test, docs).
- [ ] **Step 2:** full gate block once more plus `make test-postgres` (guard only; expected unchanged) and `make loadtest-smoke`; Playwright run twice in a row (`make e2e-fake` ×2) to catch order/timing flakes in the new specs (44, 70, edited 10/20/38/42); report both.
- [ ] **Step 3: adversarial review (independent agent, opus) of `main...HEAD`.** Hunt for:
  1. **UI/server contract drift:** each new zod schema vs the Go struct tags (`acceptedResponse`, `Readiness.MissingEnv`); zod stricter than the server (enums, required fields the server omits, `accepted` type); the paging loop vs `ParsePagination` bounds and `ORDER BY id`; error-code → copy mapping vs `server/live_calls_api.go` (every status reachable: 400, 404×3 texts, 409, 429×2); 405 impossible from the UI.
  2. **Privacy/XSS:** operator note and `bot-tts-text` rendered only through React text nodes; no note text in URLs, `localStorage`, query keys, `console`, Playwright evidence names; note cleared on success; the mutation cache cleared on sign-out (Phase 5 pin still green); server logs unchanged (counts only); no TTS text in traces, metrics or stored rows; the management key not newly copied into any state.
  3. **a11y regressions:** labels and accessible names (textarea, checkbox, buttons, region heading), focus after send and after an error, `role="status"`/`role="alert"` not both on one change, keyboard order, the row menu's focus-out close not closing when moving into its own items, contrast via tokens (`tokens.contrast.test.ts`).
  4. **Playwright determinism:** no `waitForTimeout`/sleep in new or edited specs; every assertion waits on state; the second-page key handling; 70-many-flows leaves no expectations for other specs (runs last; names unique); route mock glob matches the paged URLs; `bot-tts-text` assertion independent of event-log order.
  5. **VoiceClient ordering:** every await guarded by `attempt`; mic released on token failure, disconnect during either step, and never released for a newer attempt; draft token still requested only with a draft.
  6. **B8 wire:** message shape equals the golden; order `bot-tts-text` before `bot-tts-stopped`; no message for a cancelled unit; failover (`core/tts_failover.go:295`) passes the field through untouched; `tools/provtest/fake.go:184` left without text is harmless; text size within `MaxMessageDataBytes`; idle/greeting `Say` units also produce it (expected, document).
  7. **YAGNI / knip:** no new exports without a consumer (knip empty), no hook or helper with one caller except `openCallPage` (two: 7.1, 7.7), no new dependency, `FLOW_PAGES_MAX` the only knob.
  8. **Error-state UX:** a 429 with `Retry-After` does not loop; a 404 refetches the session once; the controls disappear when the poll sees `ended`; a failing `getFlows` page shows the existing error state, not a partial list.
- [ ] **Step 4:** resolve findings (fix round = own commits + scoped re-review). Ledger line + `p7/pr-notes.md`: (a) user-visible changes (call page "Guide this call"; missing variables on not-ready flows; all flows listed up to 1000; mic asked before the token; row menu closes on focus-out; key-bar copy); (b) wire change: `bot-tts-text` (shape, timing, declared difference R5); (c) library API: `core.TTSStoppedFrame.Text`, `rtvi.TypeBotTTSText`, `(*rtvi.Handler).FormatBotTTSText`; (d) decisions U1/U2 as answered, rulings R1–R8; (e) skipped items (below); (f) carried (R8).

---

## Skipped (verified on dbcb21f)

| Item | Reason |
|---|---|
| B2 flow version history | D-0.6 "leave"; since then a read-only `GET /flows/{id}/versions/{n}` exists (`docs/api/v1.md:18`) and the editor uses it (`FlowEditor.tsx:181`); rollback = PUT that config. No list endpoint, no UI planned. |
| B3 retention/recordings API | Config-only by design (master plan self-review). |
| B6 catalog honesty (xAI speech) | Done before the review: `not_in_build` (`server/api/providers.go:66-67`, cc8e77f 2026-10-11), `TestProvidersNotInBuild`, UI offers only available services (`StageFields.tsx:24`); docs `docs/providers.md:5`. |
| B7 Deepgram `STTCounts` | Done in Phase 2 (runner `STTCounts`, `core/sttunit/runner.go:183`). |
| B9 idle `user_timeout` e2e | Covered: `TestCallBehaviorAFlowsQuietCallerSettingsPromptThenEndTheCall` (`server/call_behavior_e2e_test.go:63`, in `make test-e2e-go`) asserts the prompt, goodbye, hang-up and metric; Playwright `38-call-behavior.spec.ts:62-65`; storage by `TestCallBehaviorIdleLinesAreInTheCallsTurns`. 7.7 adds the call-page view. |
| B10 sign-out | Done in Phase 5.6b; only the key-bar copy remains (7.6). |
| Traces `arguments_summary/result_summary` in the UI | Removed in Phase 5.6a; `git grep` finds only the unused DB columns (a migration, out of scope). |
| MCP enable/disable error mapping (ledger 1.5a) | Done: store failures are 500 `internal_error` (`docs/api/v1.md:23-24`, `server/mcp/http.go:26,269`). |
| `offline` reachability (ledger 1.7 (a)) | Pending U1; recommended skip (see U1). |
| `bot-output`, word-level TTS text, `vad-*` | Not in B8's scope; separate RTVI messages; Phase 8.4 matrix tags (R5, R8). |
