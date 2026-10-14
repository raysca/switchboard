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
| 0 | Connect, empty and error states | all | **Done (plan 7).** Not connected is a dark management-key strip. Read failures say what happened and that live calls are not affected, and show status, code and request id with Try again and Copy details. Tables load as skeleton rows. The Dashboard shows "Two steps to your first call" until a provider is detected and a flow exists. The provider-outage banner waits on S15. |
| 1 | First run | `/onboarding` | **Done (plan 7).** Four steps with the step in the URL: voice stack (one-provider stacks from the catalog defaults; keys are set on the server, not pasted: S3), flow (name, voice, what it should do; no greeting field: S5), test call (the published v1; no reply-time chips: S9), connect (token request and web client with the real flow id). The flow is created as v1 and is live: there are no drafts (S1). |
| 2 | Dashboard | `/dashboard` | State line, figures, live calls, latency, providers, flows. Already largely built (commit `065cd69`). |
| 3–5, 12 | Flows list, empty state, New flow dialog, published banner | `/flows`, `/flows/new`, `/flows/:id` | **Done (plan 2).** Rows show status, version, steps/routes/tools and live calls; Enabled, Test, Duplicate, Archive. New flow asks for a name, then Blank or Copy (the canvas's voice-stack choice is left to First run). The banner appears in the editor after a publish. Draft badges, rollback, calls-today and Restore wait on S1, S2, S7, S8. Brought to the canvas's layout in plan 6 (flow screens parity). |
| 6–10 | Flow editor | `/flows/:id`, `/flows/new` | Section 2. Brought to the canvas's layout in plan 6 (flow screens parity). |
| 11–12 | Publish, v8 is live | — | Section 2. Brought to the canvas's layout in plan 6 (flow screens parity). |
| 13–14 | Calls, Call detail | `/sessions`, `/sessions/:id` | **Done (plan 3).** The list filters by flow and Live/Ended (kept in the URL), pages with Newer/Older, and shows flow, version, duration, turns and outcome. The detail shows the figures, the path, and a transcript labelled by step with routes marked; the raw trace is behind "Technical trace". The API's statuses are only active, draining and ended. Caller, waterfall, totals, Path column and call control wait on S9–S13. |
| 15 | Providers | `/providers` | **Done (plan 4).** A list filtered by service (kept in the URL) beside a panel with the credential source (the variables the server reads, and which are still missing), the catalog, and the flows using the provider. Read-only: no key field until S3; p50, Last hour and Test connection wait on S15, S16. |
| 16 | MCP tools | `/mcp-servers`, `/mcp-servers/:id` | **Done (plan 5).** Servers beside the selected server's facts, the flows that use it and a tools table; schema changes are reviewed per flow and accepted through `PUT /agents/{id}/mcp-tools`; connect and delete are dialogs. Server states now read in the server's own terms (the old page tested for states the server never sends). Edit connection, the diff and "Keep pinned" wait on S17, S18. |

## 4. Deviations from the canvas (the Switchboard spec wins)

- First run offers one-provider voice stacks only (no mixed "Fastest replies"), no key paste (S3), no "first thing it says" (S5), no draft wording (S1) and no latency chips (S9). The error panel reassurance appears only on read failures of the management API.
- No all-caps eyebrow labels and no monospace labels: section labels are sentence case (`ui-switchboard-design.md`).
- No drop shadows or card boxes: the side panel is separated by a `--rule` border, not a shadow.
- The Calls list's filter pills keep the canvas's fully rounded shape (`border-radius: 999px`, the one exception to the single-radius rule); their touch target is still 44px.
- Colours only from tokens; no hard-coded hex in TSX (guarded by `noHardcodedColors.test.ts`).
- Lamp amber stays reserved for the live call (active step, route just taken). "Needs setup" is a status: a dashed `--ink-2` border on the step card and the status text and tab marker in `--bad`. The empty-condition route is dashed `--ink-2` with the label `Set a condition`.
- The old agent detail screen's raw config JSON and MCP tools table are gone; the version and hash are in Flow settings and the tools are chosen per step.
- The Calls list drops the canvas's Search, When, Caller, Path and median-turn columns and the Export CSV, Listen in and End call buttons: the server records none of what they need (S9–S13). Outcome is a pressed-button group, not a select plus chips.
- The Providers panel shows the credential's source and which variables are set, not the canvas's key input, "Save to keyring" or "Test connection" (S3, S16), and no p50 or "Last hour" (S15). Selected rows are outlined in --caller, not boxed with a shadow.
- The MCP tools screen's schema-change review says what really happens: a required tool whose schema changed stops the flow taking calls until it is re-pinned (the canvas says flows pinned to the old hash keep working, which the server does not do). It offers "Accept new schema" per flow, not "Keep pinned" (S17), and shows the current schema without a diff.
- The Test call panel follows the canvas (dark header with version and clock, controls, step-labelled transcript) without the waveform and the "median reply" figures (S9), and without a halo or shadow. The Publish dialog follows the canvas without the history note and the "roll back to vN" line (S2), and lists only the checks the editor really makes. The Flow settings footer says the changes are part of the draft until published (S1); it does not say "saved".
- Placeholder sample data on the canvas (latency numbers, provider names in mock rows) is illustrative only.

## 5. Server prerequisites (not UI work)

| Id | Need | Blocks |
|---|---|---|
| S1 | Test an unpublished draft (voice token for an inline config or a draft version) | Draft testing in the editor; until then the published-version note applies |
| S2 | Version list and rollback (`GET /agents/{id}/versions`, `POST …/rollback`) | Version history and "Roll back to v7" (canvas boards 8, 12) |
| S3 | ~~Write provider credentials to the sealed keyring~~ **Decided against:** providers are preconfigured and their secrets are only ever read from the environment; the console never accepts a key | Nothing: Providers and First run name the variable to set, permanently |
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
| S14 | End orphaned sessions at startup (a crash leaves `status=active` forever; nothing reconciles them) | A trustworthy Live lamp, live counts and call clocks |
| S15 | Provider health in the management API: requests, errors, median latency and circuit-breaker state per provider (today only in Prometheus `/metrics`) | The p50 column and the panel's "Last hour" figures (canvas board 15) |
| S16 | Test a provider's connection (`POST /providers/{id}/test`) | The panel's "Test connection" button |
| S17 | Read a pinned tool schema by hash (`GET /mcp-servers/{id}/tools/{tool}/schemas/{hash}`); the server stores them but exposes none | The red/green schema diff and "Keep pinned" (canvas board 16) |
| S18 | Update an MCP server's connection (endpoint, token variable) | The "Edit connection" button |

Until each lands, the UI omits the dependent control rather than faking it.

## 6. Plans

1. The unified flow editor (Section 2). **Done:** merged; its step-by-step plan is in git history.
2. **Done:** Flows list, New flow dialog, `/flows` routes, retire the agent screens, published banner.
3. **Done:** Calls list and call detail (boards 13–14).
4. **Done:** Providers (board 15).
5. **Done:** MCP tools (board 16).
6. **Done:** flow screens parity (boards 3–12): full-bleed editor, step cards, rebuilt Test call panel, Flows header and empty state, New flow cards, Publish checks.
7. **Done:** First run and the empty and error states (boards 0–1).
