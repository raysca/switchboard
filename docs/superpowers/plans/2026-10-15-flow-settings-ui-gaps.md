# Flow Settings the Editor Is Missing (UI only) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The flow editor can set every flow-config setting the server already accepts: the summarisation details, `stt_p99_ms`, which MCP tools run in the background (`async_tools`), and the failover policy (`failover.on`). No server change.

**Architecture:** All four are already validated by `server/flow` and carried in the stored config; the editor just never writes them. Each gets a labelled control in the panel where it belongs (Flow settings for the call-level ones, the step's Advanced tab for the per-step one, the MCP tool picker for background tools), mirrors the server's validation in `model.ts` so Publish is blocked with a reason, and reads back what a stored flow already carries. A blank field removes the key (the server default applies), exactly as the existing `vad_stop_ms` field does.

**Tech Stack:** React 19 + TypeScript + vitest + Playwright (echo suite).

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (flow editor); server rules in `server/flow/config.go`, `server/flow/fallback.go`.

## Global Constraints

- Editor-only: no Go changes. If a rule the UI must mirror is not what the server enforces, the server wins and the mismatch is a ledger ruling.
- A blank numeric/text field **removes the key**; the server default applies (the pattern of `vad_stop_ms`, `numberOrNothing`).
- Call-level settings (`context`, `stt_p99_ms`, `async_tools`) are stored on the **start step** (`CALL_LEVEL` in `ui/src/routes/flows/editor/model.ts`); `toPayload` strips them from other steps.
- Only controls for settings the server accepts; no invented knobs. Every control has a visible label and a hint with the unit and the default; errors name the field. No shadows, tokens only (the design-rule test).

## Review Focus

1. **Round trip:** a stored flow carrying any of these keys opens with the control showing the stored value, and publishing without touching it sends the same value (no silent drop or rewrite). Pinned per task.
2. **Mirror of server validation** (`trigger_tokens` below the effective `max_tokens`, `prompt` at most 2048 bytes, `stt_p99_ms` 100–10000, `failover.on` only the four known classes, `async_tools` unique `<server>/<tool>`): Publish is blocked with a reason before the server would say 400. Pinned per task.
3. **Cascades:** removing an MCP tool from the flow also removes it from `async_tools`; turning summarisation off drops its block.
4. **Defaults are not written:** a failover policy equal to the default (timeout, server error, circuit open) is omitted, so an untouched flow's version hash does not change when it is opened and republished.
5. **Flat one-step flows** keep their flat config (these keys sit at the top level there, as `vad_stop_ms` does).

## Rulings made while planning

1. **Failover policy is per step** (it is on each step's `Config`, not in `validateCallLevel`'s list), so it goes in the step panel's Advanced tab beside the fallback lists, not in Flow settings. Fallbacks are optional, so the control says it only matters when a stage has fallbacks.
2. **At least one class stays checked**: an empty `failover.on` means "default" on the server, so an all-unchecked state would silently re-enable the defaults. The last checked class is disabled.
3. **Unchecking "Summarise older turns" removes the whole `summarization` block** (today's behavior), including the details typed under it; no hidden retained values.
4. **`async_tools` is chosen in the MCP tool picker** (a per-tool "Runs in the background" checkbox beside "Required"), because it names flow tools; it is stored on the start step.

---

### Task 1: Summarisation details and `stt_p99_ms` (Flow settings)

**Files:**
- Modify: `ui/src/routes/flows/editor/FlowSettingsPanel.tsx`, `ui/src/routes/flows/editor/model.ts` (`flowIssues`)
- Test: `ui/src/routes/flows/editor/FlowSettingsPanel.test.tsx`, `model.test.ts`

**Interfaces:**
- Produces: `MAX_SUMMARY_PROMPT_BYTES = 2048`, `STT_P99_RANGE = [100, 10000]` in `model.ts`; `flowIssues` returns the new messages.

- [ ] **Step 1: Write the failing tests.**
  - Panel: with "Summarise older turns" checked, three fields appear: `Summarise when history reaches (tokens)` (hint `Empty: three quarters of Max tokens.`), `Keep the latest turns` (hint `Empty: 4.`), `Summary instructions` (textarea, hint `Empty: the built-in prompt. At most 2048 bytes.`); typing sets `context.summarization.trigger_tokens`, `keep_turns`, `prompt` (numbers as integers); blanking one removes its key; unchecking removes the whole block and hides the fields; a stored `{enabled:true, trigger_tokens:900, keep_turns:2, prompt:"Notes."}` opens showing those values and Publish payload equals it.
  - `stt_p99_ms`: the Listening group has `Wait for a late transcript (ms)` (hint `100 to 10000. Empty: the server default.`); typing sets `stt_p99_ms`; blank removes it; a stored value round-trips.
  - `model.test.ts` (`flowIssues`): `trigger_tokens` ≥ effective max tokens (`context.max_tokens`, default 4000) → `Summarise-at must be below Max tokens (4000).`; a negative `keep_turns` or `trigger_tokens`; a `prompt` of 2049 bytes (and `é`×1025) → `Summary instructions are over 2048 bytes.`; `stt_p99_ms` 99 or 10001 → `Late-transcript wait must be between 100 and 10000 ms.`; valid values give no issue; summarisation disabled (no block) gives none.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/routes/flows/editor` — Expected: FAIL.
- [ ] **Step 3: Implement.** In the panel, the summarisation block renders its three fields when `rec(context.summarization).enabled === true`, writing through `setContext`; reuse `numberOrNothing`; the `stt_p99_ms` input goes under Listening and writes via `setKey("stt_p99_ms", numberOrNothing(raw))`. `flowIssues` gains the checks above (effective max tokens: `context.max_tokens` or 4000 when blank, as `DefaultMaxTokens`).
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): summarisation details and the late-transcript wait in Flow settings`.

---

### Task 2: The failover policy (step Advanced tab)

**Files:**
- Modify: `ui/src/routes/flows/editor/StepAdvanced.tsx`, `ui/src/routes/flows/editor/model.ts`
- Test: `ui/src/routes/flows/editor/StepAdvanced.test.tsx`, `model.test.ts`

**Interfaces:**
- Produces: `FAILOVER_CLASSES = [{id:"timeout",label:"Timeouts"},{id:"server_error",label:"Server errors and dropped connections"},{id:"circuit_open",label:"The provider's circuit breaker is open"},{id:"client_error",label:"Rejected requests (bad request, bad key)"}]`, `DEFAULT_FAILOVER = ["timeout","server_error","circuit_open"]`.

- [ ] **Step 1: Write the failing tests.**
  - A `Failover` group in the Advanced tab (`When to move to a fallback`) with four labelled checkboxes; a step with no `failover` shows the three defaults checked and `client_error` unchecked, and the hint `Only matters when a stage has fallbacks.`; checking `client_error` writes `failover: { on: ["timeout","server_error","circuit_open","client_error"] }` (in the order of `FAILOVER_CLASSES`); returning to exactly the default removes the `failover` key (Review Focus 4); the last checked box is disabled; a stored `failover: {on:["client_error"]}` opens showing only that one checked and republishes unchanged.
  - `model.ts` `stepIssues`: an unknown class in `failover.on` (from a hand-edited stored config) → an `advanced` issue `Failover lists an unknown error class.`.
- [ ] **Step 2: Run to see them fail.** Expected: FAIL.
- [ ] **Step 3: Implement.** A small `FailoverPolicy` component inside `StepAdvanced.tsx` reading `rec(step.config.failover).on`, writing through the same `patch` the file's other controls use; the default comparison is order-insensitive.
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): choose which errors move a stage to its fallback`.

---

### Task 3: Background tools (`async_tools`) in the tool picker

**Files:**
- Modify: `ui/src/routes/flows/editor/ToolPicker.tsx`, `ui/src/routes/flows/editor/FlowEditor.tsx` (state and cascade), `ui/src/routes/flows/editor/model.ts` (`applySelection`)
- Test: `ui/src/routes/flows/editor/ToolPicker.test.tsx`, `FlowEditor.test.tsx`, `model.test.ts`

**Interfaces:**
- Produces: `ToolPicker` props gain `asyncKeys: string[]` and `onAsyncChange: (keys: string[]) => void`; `applySelection(g, next)` also drops removed keys from the start step's `async_tools`.

- [ ] **Step 1: Write the failing tests.**
  - Picker: a chosen tool shows a checkbox `Runs in the background` (accessible name `Background: <tool>`; hint on the section: `The model gets an immediate acknowledgement and is told the result when it arrives.`); toggling calls `onAsyncChange` with the keys (`<server>/<tool>`, order of the selection); a tool that is not chosen has no such checkbox; unchoosing a tool calls `onAsyncChange` without it.
  - `model.test.ts`: `applySelection` removes a dropped tool from `async_tools` on the start step and leaves the others; the payload carries `async_tools` on the start step only and omits the key when empty.
  - `FlowEditor.test.tsx`: ticking Background on a chosen tool makes the draft "unpublished", Publish sends `config.async_tools: ["crm/echo"]` (start step), and the Publish dialog lists `Flow settings: background tools changed`; a stored flow with `async_tools` opens with the box ticked.
- [ ] **Step 2: Run to see them fail.** Expected: FAIL.
- [ ] **Step 3: Implement.** The editor reads `async_tools` from the start step's config; `ToolPicker` renders the checkbox next to `Required`; the change handler writes `async_tools` (or removes the key when empty) on the start step through `updateStep`; `applySelection` filters it; `diff.ts` gets the `FLOW_SETTINGS` entry (`["async_tools","background tools"]`).
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): mark which MCP tools run in the background`.

---

### Task 4: The browser proves it, docs, gate, review

**Files:**
- Create: `e2e/tests-flows/36-flow-settings.spec.ts`
- Modify: `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (the flow editor's settings list)

- [ ] **Step 1: Write the spec** (echo only): create a flow through the API; open Flow settings, tick `Summarise older turns`, set `Summarise when history reaches` to 500 and `Max tokens` 1000, set `Wait for a late transcript` to 800; open the step's Advanced tab and tick the client-error failover class; publish; assert through `GET /flows` that the version config carries `context.summarization.trigger_tokens: 500`, `stt_p99_ms: 800` and `failover.on` with four classes; reopen (in-app) and see the controls show the stored values; set a trigger above Max tokens and see Publish disabled with the reason.
- [ ] **Step 2: Run** `make e2e-echo` — Expected: all green. Fix real failures in the code, not the spec.
- [ ] **Step 3: Full gate.** `cd ui && npx vitest run && npx tsc --noEmit`; `make e2e-echo` — Expected: all green. (No Go change: the Go suites only need to still pass.)
- [ ] **Step 4: Independent review** (AGENTS.md): a fresh Opus reviewer on `main..HEAD` with the Review Focus items; it also checks bloat and dead code introduced by the branch.
- [ ] **Step 5: Commit** `docs: the flow editor sets summarisation details, the late-transcript wait, background tools and the failover policy`.
