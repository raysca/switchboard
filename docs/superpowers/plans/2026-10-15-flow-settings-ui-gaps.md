# Flow Settings the Editor Is Missing (UI only) Implementation Plan — with defaults, a Basic/Advanced split and echo-provider e2e

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The flow editor can set every flow-config setting the server already accepts: the summarisation details, `stt_p99_ms`, which MCP tools run in the background (`async_tools`), and the failover policy (`failover.on`). No server change.

**Architecture:** All four are already validated by `server/flow` and carried in the stored config; the editor just never writes them. Each gets a labelled control in the panel where it belongs (Flow settings for the call-level ones, the step's Advanced tab for the per-step one, the MCP tool picker for background tools), mirrors the server's validation in `model.ts` so Publish is blocked with a reason, and reads back what a stored flow already carries. A blank field removes the key (the server default applies), exactly as the existing `vad_stop_ms` field does.

**Tech Stack:** React 19 + TypeScript + vitest + Playwright (echo suite).

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (flow editor); server rules in `server/flow/config.go`, `server/flow/fallback.go`.

## Defaults (what every control means when left alone)

An untouched control writes **nothing**; the server default applies, and the control shows it as its placeholder or hint (so the page always says what will happen). New flows (blank, copy, First run) start with none of these keys.

| Setting | Where | Default when unset | Shown as |
|---|---|---|---|
| Summarise older turns | Advanced · Memory | off | unchecked |
| Summarise when history reaches | Advanced · Memory | three quarters of Max tokens | placeholder `3000` (computed from the Max tokens field, 4000 by default) |
| Keep the latest turns | Advanced · Memory | 4 | placeholder `4` |
| Summary instructions | Advanced · Memory | the built-in prompt | placeholder `Built-in instructions` |
| Wait for a late transcript | Advanced · Listening | the server's per-provider value (about 500 ms to 1.5 s) | placeholder `Server default` |
| Failover on | step Advanced tab | timeouts, server errors, open circuit breaker | the three boxes checked |
| Runs in the background | MCP tool picker | off (the model waits for the result) | unchecked |

## Basic and Advanced

Flow settings today is one long list. It is split so a first-time operator sees only what a typical flow needs, and everything that is not strictly necessary sits under one collapsed **Advanced** group:

- **Always visible (Basic):** Flow name, Description, Listening (STT provider, model, language), Recording (with its disclosure confirmation; it is a compliance choice, not a tuning knob).
- **Advanced (collapsed by default):** Turn-taking (wait after the caller stops), Late-transcript wait, Memory (max messages, max tokens, summarisation and its details), Route sound. (The silent-caller and audio groups join it in the per-flow call-behavior spec.)
- **Step panel:** Basics, Routes and Tools stay as they are; the Advanced tab already holds fallbacks, tuning and (new) the failover policy.

## Setting help (the hint feature)

Obscure settings get an inline **explain this** affordance, not just a one-line hint:

- A small `?` button after the field label (`aria-label="Explain <label>"`, `aria-expanded`, `aria-controls`) toggles a short panel under the field. It is a button, not a hover tooltip, so it works with a keyboard, a screen reader and touch; `Escape` closes it and returns focus to the button. Only one panel per field; panels are independent (several can be open).
- A panel has up to four parts, in this order: **What it does** (one or two plain sentences), **When to change it**, **Default and range** (always present, with the unit), and, for risky settings, **Careful** (the cost or trap). No jargon without a gloss, no marketing words.
- The one-line hint under a field (placeholders, "Empty: …") stays: it states the default at a glance; the panel explains.
- **One source of truth:** `ui/src/routes/flows/editor/settingHelp.ts` exports `SETTING_HELP: Record<SettingId, { what: string; when?: string; defaults: string; careful?: string }>` and a `<SettingHelp id={...} />` component. Controls reference an id; no copy is written inside JSX.
- Guards: a test fails if a `SettingId` has no entry, if an entry is never referenced by a control (dead copy), or if an entry lacks `what` or `defaults`; another asserts no panel is open on first render and that every `SettingHelp` renders a labelled button.
- Applies to every Flow settings, step Advanced and tool picker control in this plan **and** to the older obscure ones (wait after the caller stops, memory limits, route sound, tool calls at once, clean text, fallbacks, recording disclosure); the per-flow call-behavior spec adds its fields to the same file.

### Copy (the first entries; the implementer writes the rest in the same voice)

| Id | What it does | When to change it | Default and range | Careful |
|---|---|---|---|---|
| `vad_stop_ms` Wait after the caller stops | How long the call waits in silence before deciding the caller has finished speaking. | Shorter for snappier replies; longer for slow or hesitant speakers. | 200–2000 ms. Empty: the server default. | Too short cuts people off mid-sentence. |
| `stt_p99_ms` Wait for a late transcript | How long the call waits for the speech-to-text service's final transcript after the caller stops. | Raise it if replies sometimes start without the caller's last words. | 100–10000 ms. Empty: the server's value for your provider. | Too high adds that delay to every reply that waits. |
| `context.max_messages` / `max_tokens` Memory | How much of the conversation the model is shown; older turns drop off past the limit. | Raise for long, detailed calls; lower to cut cost and speed up replies. | 20 messages and 4000 tokens. Max messages is at least 2. | More memory costs more and slows the model. |
| `summarization.enabled` Summarise older turns | Condenses the oldest turns into short notes the model keeps, instead of forgetting them. | Turn on for long calls where early details still matter. | Off. | Uses an extra model call when it runs. |
| `summarization.trigger_tokens` | The history size at which summarising starts. | Lower to summarise earlier. | Empty: three quarters of Max tokens. Must be below Max tokens. | |
| `summarization.keep_turns` | How many of the newest turns are kept word for word. | Raise if the model loses recent detail. | Empty: 4. | |
| `summarization.prompt` Summary instructions | Tells the model what to keep in the notes (names, amounts, promises). | When the built-in notes miss what your calls need. | Empty: the built-in instructions. At most 2048 bytes. | The call transcript is always sent as untrusted data. |
| `recording.enabled` / disclosure | Saves the call's audio on the server (only if the server enables recording too). | Only if you need recordings. | Off. | Tick the disclosure only if callers are actually told: it is your compliance statement. |
| `route_cue` Route sound | A short sound played while the call moves between steps, so the caller does not hear silence. | When a transfer takes more than a moment. | None. The name must be a sound your server has. | |
| `llm.max_parallel` Tool calls at once | How many tool calls the model may run side by side in one round. | Raise when tools are independent lookups. | 1 (one after another). 1–16. | Parallel calls load your tool servers harder. |
| `tts.clean_text` Clean text before speaking | Removes markdown, links and emoji so the voice does not read symbols aloud. | Turn off only if your text is already speech-ready. | On. | |
| fallbacks | Backup providers tried in order when the main one fails. | For flows that must not drop calls on a provider outage. | None. Up to 2 per stage. | Each fallback needs its own credentials on the server. |
| `failover.on` When to move to a fallback | Which kinds of failure make a stage switch to its next fallback. | Rarely. Add *Rejected requests* only if a bad request to one provider may succeed on another. | Timeouts, server errors, open circuit breaker. | Rejected requests are usually your bug, not an outage; switching can hide it. |
| `async_tools` Runs in the background | The model gets an immediate "working on it" and is told the result when it arrives. | For slow tools (a search or a booking) so the caller is not left waiting in silence. | Off. | The model must handle a late answer gracefully. |

---

### Task 0b: The setting-help component and its guards

**Files:**
- Create: `ui/src/routes/flows/editor/settingHelp.ts`, `ui/src/routes/flows/editor/SettingHelp.tsx`
- Modify: `ui/src/styles.css` (a few tokenised rules: no shadows)
- Test: `ui/src/routes/flows/editor/SettingHelp.test.tsx`, `settingHelp.test.ts`

**Interfaces:**
- Produces: `type SettingId` (a string union of every id in the copy table, extended by later tasks), `SETTING_HELP`, `export function SettingHelp({ id }: { id: SettingId }): JSX.Element`.

- [ ] **Step 1: Write the failing tests.**
  - `SettingHelp.test.tsx`: renders a button named `Explain <label>` (the label comes from the entry's `label`), closed with `aria-expanded="false"`; click or Enter/Space opens a region (`role="region"`, labelled by the button) showing the parts present (`What it does`, `When to change it`, `Default and range`, `Careful` only when the entry has it); `Escape` closes it and focuses the button; a second click closes it; two instances open independently; nothing is open on first render.
  - `settingHelp.test.ts`: every key of the copy table above exists; every entry has `what` and `defaults`; no entry text is empty or contains `TODO`; the exported ids equal the ids referenced by the controls (scan the `ui/src/routes/flows` sources for `<SettingHelp id="…"`) — an unreferenced entry or an unknown id fails.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/routes/flows/editor/SettingHelp.test.tsx src/routes/flows/editor/settingHelp.test.ts` — Expected: FAIL.
- [ ] **Step 3: Implement.** `settingHelp.ts` holds the table above (every cell as a string; `label` added per entry); `SettingHelp.tsx` is a `useId`/`useState` button + region, Escape handled on the region and the button; styles reuse `--rule`, `--ink-2`, the existing `.note` shape.
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS (the "referenced by a control" check is pending until Task 1–3 wire the controls: until then it asserts the **table** only; the reference check is switched on in Task 4 Step 3 and listed there).
- [ ] **Step 5: Commit** `feat(ui): an explain-this button for obscure settings, with one source of copy`.

---

### Task 0c: Wire help into the existing obscure controls

**Files:**
- Modify: `FlowSettingsPanel.tsx`, `StepAdvanced.tsx`, `ToolPicker.tsx` (labels of the controls named in the copy table)
- Test: the same files' tests

- [ ] **Step 1: Write the failing tests.** In each panel, the controls named in the copy table that already exist (wait after the caller stops, Max messages, Max tokens, Summarise older turns, Record calls and its disclosure, route sound, tool calls at once, clean text, fallbacks) have an `Explain …` button next to the label; clicking one shows the entry's text; the field's own label and hint are unchanged (existing tests keep passing); `Required` in the tool picker gets none.
- [ ] **Step 2: Run to see them fail.** Expected: FAIL.
- [ ] **Step 3: Implement.** Add `<SettingHelp id=… />` after each label's text, inside the existing `<label>` row but outside the input's accessible name (a sibling element, so the label text the tests find is unchanged).
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): explain-this buttons on the existing obscure settings`.

---

## Global Constraints

- Editor-only: no Go changes. If a rule the UI must mirror is not what the server enforces, the server wins and the mismatch is a ledger ruling.
- A blank numeric/text field **removes the key**; the server default applies (the pattern of `vad_stop_ms`, `numberOrNothing`).
- Call-level settings (`context`, `stt_p99_ms`, `async_tools`) are stored on the **start step** (`CALL_LEVEL` in `ui/src/routes/flows/editor/model.ts`); `toPayload` strips them from other steps.
- **Echo-provider proof:** every new control has a browser e2e on the echo providers (`make e2e-echo`): deterministic, free, no credentials. The same specs stay free of echo-only assumptions where cheap, but no live run is required.
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

### Task 0: Basic and Advanced in Flow settings

**Files:**
- Modify: `ui/src/routes/flows/editor/FlowSettingsPanel.tsx`, `ui/src/styles.css` (only if a rule is needed; tokens, no shadows)
- Test: `ui/src/routes/flows/editor/FlowSettingsPanel.test.tsx`

- [ ] **Step 1: Write the failing tests.**
  - The panel shows Flow name, Description, Listening and Recording without opening anything; Turn-taking, Memory and Route sound are inside a `<details>` whose summary reads `Advanced`; it is **closed** for a new or default flow.
  - The summary says how many advanced settings are set: `Advanced` alone when none, `Advanced · 2 set` when two keys (`vad_stop_ms`, `context`, `stt_p99_ms`, `route_cue`) are present.
  - A stored flow that already sets any advanced value opens with `Advanced` **expanded**, so no stored value is hidden.
  - A flow issue that belongs to an advanced field (for example `Summarise-at must be below Max tokens`) forces `Advanced` open and the issue list names the field; opening and closing is keyboard operable (native `<details>`).
  - Every existing control keeps its label, so the existing Flow settings tests pass with the group opened first where they touch an advanced field.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/routes/flows/editor/FlowSettingsPanel.test.tsx` — Expected: FAIL.
- [ ] **Step 3: Implement.** Wrap the advanced fieldsets in `<details className="advanced" open={forced || hasAdvancedValue}>` with a controlled `open` that the user can still toggle (state initialised from the two conditions, forced open by an advanced issue). No new copy beyond the summary.
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): Flow settings keeps the essentials up front and the rest under Advanced`.

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
- [ ] **Step 3: Implement.** Each new field carries its `<SettingHelp id=… />` (ids `summarization.trigger_tokens`, `summarization.keep_turns`, `summarization.prompt`, `stt_p99_ms`). In the panel, the summarisation block renders its three fields when `rec(context.summarization).enabled === true`, writing through `setContext`; reuse `numberOrNothing`; the `stt_p99_ms` input goes under Listening and writes via `setKey("stt_p99_ms", numberOrNothing(raw))`. `flowIssues` gains the checks above (effective max tokens: `context.max_tokens` or 4000 when blank, as `DefaultMaxTokens`).
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
- [ ] **Step 3: Implement.** The group carries `<SettingHelp id="failover.on" />`. A small `FailoverPolicy` component inside `StepAdvanced.tsx` reading `rec(step.config.failover).on`, writing through the same `patch` the file's other controls use; the default comparison is order-insensitive.
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
- [ ] **Step 3: Implement.** The section carries `<SettingHelp id="async_tools" />`. The editor reads `async_tools` from the start step's config; `ToolPicker` renders the checkbox next to `Required`; the change handler writes `async_tools` (or removes the key when empty) on the start step through `updateStep`; `applySelection` filters it; `diff.ts` gets the `FLOW_SETTINGS` entry (`["async_tools","background tools"]`).
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): mark which MCP tools run in the background`.

---

### Task 4: The browser proves it (echo providers), docs, gate, review

**Files:**
- Create: `e2e/tests-flows/36-flow-settings.spec.ts`
- Modify: `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (the flow editor's settings list)

- [ ] **Step 1: Write the spec **: create a flow through the API with `stage()`; open Flow settings, confirm `Advanced` is collapsed, open it, tick `Summarise older turns`, set `Summarise when history reaches` to 500 and `Max tokens` 1000, set `Wait for a late transcript` to 800; open the step's Advanced tab and tick the client-error failover class; tick `Runs in the background` on a tool chosen from a server connected by the spec (`/ok`, no call is made); publish; assert through `GET /flows` that the version config carries `context.summarization.trigger_tokens: 500`, `stt_p99_ms: 800` and `failover.on` with four classes; reopen (in-app) and see the controls show the stored values with `Advanced` already expanded; set a trigger above Max tokens and see Publish disabled with the reason. **Then place a call with these settings on the published flow** (`openFlowTest`, `startCall`, `sendText("Say hello")`) and expect any bot reply (`botReplies(page).first()` visible and the labelled echo reply): this proves the server accepts and runs a flow carrying every new key.
- [ ] **Step 2: Run** `make e2e-echo` — Expected: all green. Fix real failures in the code, not the spec.
- [ ] **Step 2b: Switch the guard on.** In `settingHelp.test.ts` enable the "every entry is referenced by a control and every referenced id exists" check (it was table-only until now); in the e2e, open one `Explain …` panel (`Explain Wait for a late transcript`), see its `Default and range` text and close it with Escape.
- [ ] **Step 3: Full gate.** `cd ui && npx vitest run && npx tsc --noEmit`; `make e2e-echo`; — Expected: all green. (No Go change: the Go suites only need to still pass.)
- [ ] **Step 4: Independent review** (AGENTS.md): a fresh Opus reviewer on `main..HEAD` with the Review Focus items; it also checks bloat and dead code introduced by the branch.
- [ ] **Step 5: Commit** `docs: the flow editor sets summarisation details, the late-transcript wait, background tools and the failover policy`.
