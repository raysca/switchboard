# Switchboard console: flows, steps and routes — design

**Status:** approved direction (canvas reviewed 2026-10-12). Builds on `2026-10-12-ui-switchboard-design.md` (tokens, type, motion), which still governs visual rules.

**Design source:** the "Switchboard Console Designs" canvas — https://claude.ai/artifact/N7YEejiyPD7bLb65uUUuwM (private to the owner). Board numbers below refer to it.

## 1. Vocabulary

The product names what callers reach a **Flow**. A flow has **Steps** joined by **Routes**.

| UI word | Meaning | Code today |
|---|---|---|
| Flow | What callers reach; created, tested, published in versions | `storage.Agent`, `agent.Graph`, `/api/v1/agents` |
| Step | One part of a flow with its own instructions, model, voice and tools (max 8) | `agent.Node`, `GraphNode` |
| Route | A plain-language condition that moves the caller to another step | `agent.Edge`, "handoff", `GraphEdge` |
| Start step | The step every call begins on | `Graph.Entry` |
| Flow settings | Shared by the whole call: listening (STT), turn-taking, memory, recording, tool access, route sound | entry-node call-level config (`CALL_LEVEL` in `ui/src/routes/agents/graph/model.ts`) |
| Version | Immutable published config; calls keep the version they started on | `AgentVersion` |

Rules: never say "agent" or "node" in UI copy for these concepts ("agent" may still mean a human taking over). The code rename (Go, API, storage) is a separate task; UI work uses the new words in copy and keeps API field names until that rename lands.

## 2. Authoring model: one editor, the graph never leaves the screen

The flow editor (boards 6–10) is one page: header, toolbar, graph canvas, and a right-hand **side panel** that slides over the canvas. Opening any panel never navigates away.

- **Header:** flow name, version chip (`v7` / `v7 · unpublished changes` / `New flow`), a state line (`No steps yet` / `2 steps need setup` / `Ready to test`), `Flow settings`, `Test call`, `Publish`.
- **Toolbar:** `Add step` and `N of 8 steps`.
- **Blank flow:** a centred card "Start with one step." with `Add first step`.
- **Adding a step** opens its panel immediately on the Basics tab with the name selected. The first step is blank; later steps start from the start step's model and voice (valid at once, as today's `addNode` does).
- **Step cards** show name, `START` badge, `model · voice`, and a status line with priority: live call state (`Speaking`, `Listening`, `Preparing`, `Visited`) > `Needs setup` (dashed border, see Section 4) > `No route leads here` (unreachable, non-blocking) > `Ready`.
- **Routes** are drawn by dragging from a step's source handle to another step (React Flow `onConnect`) or with React Flow's click-to-connect, or from the Routes tab (`Route to` select + `Add route`) for keyboard users. A new route has an **empty condition**: drawn dashed with the label `Set a condition`, and the source step's panel opens on the Routes tab with the condition field focused.

### Step panel tabs

| Tab | Contents | Required for "Ready" |
|---|---|---|
| Basics | Step name, Instructions (`llm.system_prompt`), Model (LLM provider + model), Voice (TTS provider + voice) | name, LLM provider, LLM model, TTS provider |
| Routes (count) | Each outgoing route's condition, remove, `Route to` + `Add route`, `Route to a new step` | every outgoing route has a condition |
| Tools (count) | Checkboxes over the flow's selected MCP tools (`GraphNode.tools`) | no |
| Advanced | Model fallbacks, voice fallbacks (`llm.fallbacks`, `tts.fallbacks`), Tool calls at once (`llm.max_parallel`, 1–16), Clean text before speaking (`tts.clean_text`) | no |

A tab with an unmet requirement shows a `--bad` marker with the accessible name "needs attention". Opening a step lands on the first tab with a problem, else Basics (re-selecting the same step keeps the current tab). `Widen` toggles the panel between 440px and 720px. Footer: `Delete step` (not on the start step), `Make start step` (not on the start step), `Done`.

### Flow settings panel

Name and description; Listening (STT provider/model/language); Turn-taking (wait after the caller stops, `vad_stop_ms`, 200–2000); Memory (`context.max_messages`, `context.max_tokens`, summarise older turns); Recording (`recording.enabled`, `recording.disclosure_confirmed`); Route sound (`handoff_cue.name`, `after_ms`). A missing STT provider is a flow-level problem shown in the header state.

### Test call panel

`Test call` opens a panel containing the existing voice test (`AgentVoiceTest`): status, meters, transcript, mute, end. While a call runs, the **main canvas** shows it: the active step pulses (lamp), visited steps are marked, the route just taken draws in (existing cable motion), using the server's handoff trace spans (`deriveView`). Ending the call keeps the transcript in the panel.

**Constraint (server today):** a voice token runs the flow's *published* version. Until the server can test a draft (S1), the panel says `This call runs v7, the published version. Publish to hear your changes.` whenever the draft differs, and `Test call` is disabled for a never-published flow (`Publish once to test`).

### Publish

`Publish` opens a confirmation dialog: `Publish <name> v<n>?`; "New calls get v<n> straight away. Calls already in progress finish on the version they started with."; the list of changes since the published version (computed client-side); blocking problems (confirm disabled); `Keep editing` / `Publish v<n>`. Success shows `Published v<n>` (existing message) and a status banner on the flows list (board 12).

## 3. Other screens

| Board | Screen | Route | Notes |
|---|---|---|---|
| 0 | Connect, empty and error states | all | Not connected keeps "Enter a management key". Each empty/error state: what happened, what it means for live calls, one next step. |
| 1 | First run | `/onboarding` | 4 steps: voice stack (from detected providers) → flow basics → test call → connect (voice token + client snippet). Key paste (S3) shown only when the server supports it. |
| 2 | Dashboard | `/dashboard` | State line, figures, live calls, latency, providers, flows. Already largely built (commit `065cd69`). |
| 3–5, 12 | Flows list, empty state, New flow dialog, published banner | `/flows`, `/flows/new`, `/flows/:id` | **Done (plan 2).** Rows show status, version, steps/routes/tools and live calls; Enabled, Test, Duplicate, Archive. New flow asks for a name, then Blank or Copy (the canvas's voice-stack choice is left to First run). The banner appears in the editor after a publish. Draft badges, rollback, calls-today and Restore wait on S1, S2, S7, S8. |
| 6–10 | Flow editor | `/flows/:id`, `/flows/new` | Section 2. |
| 11–12 | Publish, v8 is live | — | Section 2. |
| 13–14 | Calls, Call detail | `/sessions`, `/sessions/:id` | **Done (plan 3).** The list filters by flow and Live/Ended (kept in the URL), pages with Newer/Older, and shows flow, version, duration, turns and outcome. The detail shows the figures, the path, and a transcript labelled by step with routes marked; the raw trace is behind "Technical trace". The API's statuses are only active, draining and ended. Caller, waterfall, totals, Path column and call control wait on S9–S13. |
| 15–16 | Providers, MCP tools | `/providers`, `/mcp-servers` | Provider detail panel; MCP schema-change review. |

## 4. Deviations from the canvas (the Switchboard spec wins)

- No all-caps eyebrow labels and no monospace labels: section labels are sentence case (`ui-switchboard-design.md`).
- No drop shadows or card boxes: the side panel is separated by a `--rule` border, not a shadow.
- Colours only from tokens; no hard-coded hex in TSX (guarded by `noHardcodedColors.test.ts`).
- Lamp amber stays reserved for the live call (active step, route just taken). "Needs setup" is a status: a dashed `--ink-2` border on the step card and the status text and tab marker in `--bad`. The empty-condition route is dashed `--ink-2` with the label `Set a condition`.
- The old agent detail screen's raw config JSON and MCP tools table are gone; the version and hash are in Flow settings and the tools are chosen per step.
- The Calls list drops the canvas's Search, When, Caller, Path and median-turn columns and the Export CSV, Listen in and End call buttons: the server records none of what they need (S9–S13). Outcome is a pressed-button group, not a select plus chips.
- Placeholder sample data on the canvas (latency numbers, provider names in mock rows) is illustrative only.

## 5. Server prerequisites (not UI work)

| Id | Need | Blocks |
|---|---|---|
| S1 | Test an unpublished draft (voice token for an inline config or a draft version) | Draft testing in the editor; until then the published-version note applies |
| S2 | Version list and rollback (`GET /agents/{id}/versions`, `POST …/rollback`) | Version history and "Roll back to v7" (canvas boards 8, 12) |
| S3 | Write provider credentials to the sealed keyring | Pasting keys on Providers and First run |
| S4 | Live call count per flow/version (operations status) | "9 calls finishing on v7" copy |
| S5 | Greeting / first line in config | First run step 2 "First thing it says" |
| S6 | Code rename agent/node/handoff → flow/step/route | Route paths and API names (`/flows`) |
| S7 | Per-flow call statistics (calls today, median reply time) | Those two figures on the Flows list rows |
| S8 | Unarchive a flow (`POST /agents/{id}/restore`) | The canvas's "Archived · 1 / Restore" section |
| S9 | Per-turn latency breakdown stored on turns (`latencies_json` is never written today) | The turn-latency waterfall and "median turn" (canvas boards 13, 14) |
| S10 | Caller identity and transport, and an end reason (hung up, failed, transferred), on sessions | The Caller column and a richer Outcome |
| S11 | Calls list totals and filters (`total`, `started_after`, text search) and CSV export | "of 1,284", When, Search, Export CSV |
| S12 | Version number and path on the session row (or the step id on each turn) | "v7" for old versions, the list's Path column, exact step labels |
| S13 | Live call control: listen in, end a call | The Listen in and End call buttons |

Until each lands, the UI omits the dependent control rather than faking it.

## 6. Plans

1. The unified flow editor (Section 2). **Done:** merged; its step-by-step plan is in git history.
2. **Done:** Flows list, New flow dialog, `/flows` routes, retire the agent screens, published banner.
3. **Done:** Calls list and call detail (boards 13–14).
4. Providers and MCP tools (boards 15–16).
5. First run and empty/error states (boards 0–1), after S3/S5 decisions.
