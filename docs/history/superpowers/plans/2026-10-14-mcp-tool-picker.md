> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# MCP Tool Picker in the Flow Designer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** In the flow designer, an operator can choose which MCP servers' tools the flow may call (from the step's Tools tab), then choose per step which of them it uses, and Publish and Test call both carry that choice.

**Architecture:** Today the flow's selected MCP tools (`mcp_tools` on a version: server, tool, pinned schema hash, required) can only be changed by `PUT /flows/{id}/mcp-tools`, which publishes a version at once, so the designer can only tick tools that are already attached. The selection becomes part of the draft: `PUT /flows/{id}` accepts an optional `mcp_tools` beside `config` and publishes both as ONE version (validated exactly as `mcp-tools` validates), and `POST /voice-tokens` `draft` accepts it too so a draft call can use a tool that is not attached yet. The editor keeps the selection in its state, shows a picker built from the servers' latest discoveries, cascades removals into the steps, and sends it on Publish and on Test call.

**Tech Stack:** Go (`server/storage`, `server/flow`, `server/api`), React 19 + TypeScript + vitest, Playwright echo suite.

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (flow editor, Tools tab; S1 draft calls). This plan lifts S1's limit "a draft can use only the tools already attached" and fixes the dead-end copy "Choose MCP tools for the flow first".

## Global Constraints

- Tokens for MCP servers are read only from `MCP_TOKEN_*` environment variables; the picker never shows or sends a token (AGENTS.md; memory `provider-secrets-env-only`).
- Dual dialect: every storage change runs on SQLite and Postgres (`eachStore` in `server/storage/orphans_test.go`; `make test-postgres`).
- Immutable versions: a publish that changes the selection is a normal new version (number +1); nothing mutates a stored version.
- Bounded input: at most 64 selected tools per flow (the selection is validated server-side; `api.DecodeJSON` already caps the body).
- Backward compatible API: `mcp_tools` absent on `PUT /flows/{id}` means "keep the current selection" (today's behaviour); present (even `[]`) means "replace".
- `core/` untouched; every change is consumed by `server/` and the console (AGENTS.md YAGNI).
- All Go passes `go test -race ./...`; `gofmt -w .`; conventional commits; final independent Opus review.

## Review Focus

1. **Removing a tool a step still uses.** Unticking a flow tool in the picker must also remove it from every step's tool list (otherwise the server rejects the publish with `step … lists tool … that the flow has not selected`). Pinned in Task 4 (UI) and Task 2 (server rejects it, 400).
2. **A selected tool whose server was disabled, deleted or re-synced** stays visible in the picker as unavailable (with the reason) instead of silently disappearing, and cannot be newly ticked. Pinned in Task 5.
3. **A schema that changed since it was pinned.** Selecting pins the hash from the latest discovery; a hash that moved between the picker loading and Publish is a `400 invalid_tool_selection` the dialog shows, not a silent re-pin. Pinned in Task 1 and Task 5.
4. **One-step (flat) flows.** A flat config has no per-step tool list: every selected tool is available to the lone step. Ticking tools in a flat flow must not create a steps-form config, and adding a second step later keeps the tools on the start step (existing behaviour, re-pinned in Task 4).
5. **Draft test calls with a changed selection** run the draft's tools, never the published ones, and a publish during the call changes nothing. Pinned in Task 3.

## Rulings made while planning (re-read before Task 1)

1. **The selection publishes atomically with the config** (one version), via `mcp_tools` on `PUT /flows/{id}`. Cost if wrong: none for existing callers (field optional). The separate `PUT /flows/{id}/mcp-tools` stays for the drift panel's "accept new schema".
2. **A publish that carries `mcp_tools` clears `mcp_review_required`**, like `PublishVersion` does today: choosing the tools again is the review. A publish without it still carries the flag forward.
3. **The picker lives in the step's Tools tab** (as asked), in a section titled "Flow tools: shared by every step", above the existing "Tools this step can call". Selection is flow-wide; the per-step list is the existing control.
4. **Selectable tools** are those in the latest discovery of enabled servers with `validation_state == "healthy"`. `required` is a per-tool checkbox, default off.
5. **Draft limit lifted, not loosened:** `draft.mcp_tools` is validated like a publish (server enabled, discovery, hash matches), and a draft call captures exactly the draft's tools.
6. **No new endpoint for the catalog:** the picker reads `GET /mcp-servers` and each server's detail (the MCP page already does).

---

### Task 1: Storage publishes a replacement selection with the config

**Files:**
- Modify: `server/storage/flows.go` (`FlowMutation`, `MutateAndPublish`), `server/storage/repositories.go` (`PublishVersion`: extract the validation loop)
- Test: `server/storage/flow_tools_test.go`

**Interfaces:**
- Produces:
  ```go
  type FlowMutation struct {
  	Name, Description *string
  	Enabled           *bool
  	Tools             *[]SelectedTool // nil keeps the current version's selection; non-nil (even empty) replaces it and clears mcp_review_required
  }
  func validateSelections(ctx context.Context, q queryer, dialect Dialect, selections []SelectedTool) error // the loop now in PublishVersion; returns ErrInvalidToolSelection
  const MaxSelectedTools = 64
  ```
  `queryer` is the small interface `QueryRowContext(ctx, query string, args ...any) *sql.Row` satisfied by `*sql.Tx` and `*sql.DB` (declare it in `repositories.go` if the package has none; grep first).

- [ ] **Step 1: Write the failing tests** (`eachStore`; seed a flow with one version and MCP servers via the helpers the existing MCP storage tests use: `grep -n "func seed" server/storage/*_test.go`):
  - publish with `Tools: &[]SelectedTool{{server A, "echo", hashA, false}}` and a config → new version N+1 whose `SelectedTools` is exactly that, and the previous version's selection is untouched;
  - `Tools: nil` copies the previous selection forward (today's behaviour);
  - `Tools: &[]SelectedTool{}` clears the selection;
  - a selection whose hash differs from the discovery's → `errors.Is(err, ErrInvalidToolSelection)`, **no version row, and a `Name` change in the same mutation is rolled back**;
  - unknown tool, disabled server, server with no discovery → `ErrInvalidToolSelection`;
  - more than 64 tools → `ErrInvalidToolSelection`;
  - a flow whose current version has `mcp_review_required` true: publish with `Tools` set → the new version's flag is false; publish with `Tools: nil` → true is carried forward.
- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -run 'FlowTools' ./storage` — Expected: FAIL (compile: no `Tools` field).
- [ ] **Step 3: Implement.** Extract the validation loop (`server/storage/repositories.go:104-130`) into `validateSelections` and call it from `PublishVersion` with the tx. In `MutateAndPublish`, inside the transaction before inserting the version: if `mutation.Tools != nil` → `len > MaxSelectedTools` is `ErrInvalidToolSelection`, run `validateSelections(ctx, tx, r.s.dialect, *mutation.Tools)`, set `reviewRequired = false`, insert the given rows into `agent_mcp_tools` instead of the `INSERT … SELECT` copy; otherwise keep today's copy. (A `Tools` set on a mutation with no config is not supported by this method: the service passes the current config, Task 2.)
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 ./storage` and `make test-postgres` — Expected: PASS on both dialects.
- [ ] **Step 5: Commit** `feat(storage): a publish can replace the flow's MCP tool selection atomically`.

---

### Task 2: `PUT /flows/{id}` accepts `mcp_tools`

**Files:**
- Modify: `server/flow/service.go` (`UpdateRequest`, `Update`, `checkConfigTools`), `server/api/flows.go` only if error mapping needs a case (it already maps `ErrInvalidToolSelection` to `400 invalid_tool_selection`)
- Test: `server/flow/update_tools_test.go`, `server/api/flows_test.go`

**Interfaces:**
- Consumes: `storage.FlowMutation.Tools` (Task 1).
- Produces:
  ```go
  type UpdateRequest struct {
  	Name, Description *string; Config json.RawMessage; Enabled *bool
  	MCPTools *[]ToolSelection `json:"mcp_tools,omitempty"` // nil keeps; non-nil replaces
  }
  func (s *Service) checkConfigTools(ctx context.Context, id string, config json.RawMessage, selected *[]ToolSelection) error // validates step tools against `selected` when non-nil, else the current version's
  ```

- [ ] **Step 1: Write the failing tests.**
  - service: `Update` with `Config` whose step lists `srv/echo` and `MCPTools` containing it → version N+1 has the tool and the step; the same call without `MCPTools` → rejected `step … lists tool … that the flow has not selected` (today's rule); `MCPTools` that drops a tool a step in `Config` still lists → `ErrInvalidConfig`-style rejection and nothing published; `MCPTools: &[]` with a config whose steps use no tools → clears; `MCPTools` with a hash that moved → `storage.ErrInvalidToolSelection`; `MCPTools` set and `Config` empty → publishes the current config with the new selection (the service reads the current version's `ConfigJSON`);
  - api: JSON round trip `{"config":{…},"mcp_tools":[{"mcp_server_id","remote_name","schema_hash","required"}]}` → 200 with `current_version.mcp_tools`; a stale hash → 400 `invalid_tool_selection`; `"mcp_tools": null` and absent both keep the selection.
- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -run 'UpdateTools|FlowsUpdateMCP' ./flow ./api` — Expected: FAIL.
- [ ] **Step 3: Implement.** In `Update`: when `req.MCPTools != nil`, `toStorageSelections(*req.MCPTools)` (structure check) → `mutation.Tools`; if `len(req.Config)==0`, load the current version's `ConfigJSON` and use it as `config` so a selection-only update still publishes; pass `req.MCPTools` to `checkConfigTools`, which builds `have` from it instead of `SelectedTools(current)` when non-nil. `ResolveDraft` (Task 3) calls the same helper.
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 ./flow ./api ./storage` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(api): a flow update can carry its MCP tool selection and publishes both as one version`.

---

### Task 3: A draft test call can use a changed selection

**Files:**
- Modify: `server/flow/service.go` (`ResolveDraft`), `server/api/api.go` (`DraftRequest`), `server/api_routes.go` (pass it), `server/storage` (a read-only `ValidateSelections(ctx, sels) error` on `FlowRepository`, wrapping `validateSelections` on `*sql.DB`)
- Test: `server/flow/draft_test.go`, `server/drafts_test.go`

**Interfaces:**
- Consumes: `checkConfigTools(…, selected *[]ToolSelection)` (Task 2), `validateSelections` (Task 1).
- Produces:
  ```go
  func (s *Service) ResolveDraft(ctx context.Context, id string, config json.RawMessage, tools *[]ToolSelection) (Snapshot, Readiness, error) // tools nil: the current version's selection
  type DraftRequest struct { Config json.RawMessage `json:"config"`; MCPTools *[]ToolSelection `json:"mcp_tools,omitempty"` }
  func (r FlowRepository) ValidateSelections(ctx context.Context, sels []SelectedTool) error
  ```

- [ ] **Step 1: Write the failing tests.**
  - `ResolveDraft` with a draft whose step uses `srv/echo` and `tools` containing it (not attached to the current version) → `Snapshot.MCPTools` lists it, readiness `ready`, nothing written (version count unchanged); the same draft without `tools` → rejected as an unselected step tool (S1 behaviour); a stale hash → `ErrInvalidToolSelection`; a disabled server → `ErrInvalidToolSelection`.
  - voice-token e2e (rig from `drafts_test.go` plus the MCP test server used by `grep -rn "MCPCall" server/*_test.go`): a flow with no tools, a draft that selects the test server's `echo` tool and whose echo-LLM phrase calls it → the call's session records the tool (`session_mcp_tools`) and the MCP server received the call; the published flow still has no tools; an ordinary call on the flow calls nothing.
- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -run 'ResolveDraft|DraftTool' ./flow .` — Expected: FAIL.
- [ ] **Step 3: Implement.** `ResolveDraft`: if `tools != nil` → `toStorageSelections`, `store.Flows().ValidateSelections`, build `draft.MCPTools` from them (replacing the base version's) before `flowReadiness`; pass `tools` to `checkConfigTools`. The handler (`api_routes.go`) passes `input.Draft.MCPTools`. `runtime.StartSnapshotSession` already captures `snapshot.MCPTools`, so the call needs no change; the session row keeps the current version id (S1 ruling) and the draft's hash.
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 ./...` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): a draft test call can select MCP tools the flow has not published`.

---

### Task 4: The editor holds the tool selection (client, state, cascade, payloads)

**Files:**
- Modify: `ui/src/api/client.ts` (`CreateFlowInput.mcp_tools?`, `UpdateFlowInput.mcp_tools?`, `issueVoiceToken` draft `mcp_tools?`), `ui/src/voice/VoiceClient.ts` (`ConnectOptions.draft?: { config: unknown; mcp_tools?: ToolSelection[] }`), `ui/src/routes/flows/FlowVoiceTest.tsx` (the `draft` prop type), `ui/src/routes/flows/editor/FlowEditor.tsx` (state, diff, payloads), `ui/src/routes/flows/editor/model.ts` (a pure `dropTool`)
- Test: `ui/src/routes/flows/editor/model.test.ts`, `FlowEditor.test.tsx`, `ui/src/api/client.test.ts`

**Interfaces:**
- Consumes: `ToolSelection` (`client.ts`), the API of Tasks 2 and 3.
- Produces:
  ```ts
  // model.ts: removes a tool from the flow's selection and from every step that lists it
  export function dropTool(g: FlowDraft, selection: ToolSelection[], key: string): { graph: FlowDraft; selection: ToolSelection[] }
  // FlowEditor state: const [tools, setTools] = useState<ToolSelection[]>(flow?.current_version?.mcp_tools ?? [])
  // the tool key everywhere is `${mcp_server_id}/${remote_name}`
  ```

- [ ] **Step 1: Write the failing tests.**
  - `model.test.ts`: `dropTool` removes `srv/echo` from the selection and from each step's `tools` and leaves other tools alone; dropping an unselected key changes nothing; a flat one-step flow stays flat (no `steps` form is created).
  - `client.test.ts`: `updateFlow(id, {config, mcp_tools})` and `createFlow` send `mcp_tools`; absent when not given; `issueVoiceToken` sends `draft.mcp_tools` when present.
  - `FlowEditor.test.tsx`: Publish sends `mcp_tools` equal to the selection (and omits it for a flow whose selection is unchanged? **No: always send it**, so a publish is explicit; assert it equals the current selection when nothing changed); a selection change alone makes the editor "unpublished" (the `v1 · unpublished changes` chip and the Publish dialog's changes list names `Added tool crm/lookup` / `Removed tool …`); the Test call panel passes `draft.mcp_tools`; the existing "keeps the flow's tools on the start step when a step is added" test still passes.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/routes/flows src/api` — Expected: FAIL.
- [ ] **Step 3: Implement.** `tools` state initialised from `flow.current_version.mcp_tools`; `flowTools` strings derive from it; `canonical(toPayload(graph))`-style diff gains `JSON.stringify(sortedKeys(tools))` compared with `published` tools; `publish()` adds `mcp_tools: tools`; `draft = { config, mcp_tools: tools }` for the Test call; the changes list in `PublishDialog` (`grep -n "Added step" ui/src/routes/flows/editor/*.ts*`) gains the two tool lines. A flat flow mirrors the selection onto its main step so "with one step it can call every tool" stays true.
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): the editor keeps the flow's MCP tool selection and sends it on publish and test`.

---

### Task 5: The picker in the Tools tab

**Files:**
- Create: `ui/src/routes/flows/editor/ToolPicker.tsx`, `ui/src/routes/flows/editor/useMCPCatalog.ts`
- Modify: `ui/src/routes/flows/editor/StepPanel.tsx` (the Tools tab), `ui/src/routes/flows/editor/FlowEditor.tsx` (props), `ui/src/styles.css` (only if a rule is needed; no shadows, tokens only)
- Test: `ui/src/routes/flows/editor/ToolPicker.test.tsx`, `StepPanel.test.tsx` (or the FlowEditor test, whichever already renders the Tools tab)

**Interfaces:**
- Consumes: `client.getMCPServers()`, `client.getMCPServer(id)` (`discovery.tools[]`: `remote_name`, `description`, `schema_hash`), `dropTool` and the `tools` state (Task 4).
- Produces:
  ```ts
  export type CatalogServer = { id: string; name: string; usable: boolean; reason?: string; tools: { remote_name: string; description: string; schema_hash: string }[] }
  export function useMCPCatalog(client: ApiClient, enabled: boolean): { servers: CatalogServer[]; loading: boolean; error: ApiError | null }
  export function ToolPicker(props: { catalog: ReturnType<typeof useMCPCatalog>; selected: ToolSelection[]; onChange: (next: ToolSelection[]) => void; onDrop: (key: string) => void }): JSX.Element
  ```
  `usable` is `enabled && validation_state === "healthy" && discovery present`; `reason` is `Disabled`, `Not reachable`, or `Not synced yet`.

- [ ] **Step 1: Write the failing tests** (mock `client` like `MCPView.test.tsx` does):
  - the Tools tab shows a `Flow tools` section ("Shared by every step. Each step chooses which of these it can call.") above `Tools this step can call`; with no servers it says `No MCP server is connected.` and links `Connect a server` to `/mcp-servers`, and the old dead-end sentence is gone;
  - a server with two tools lists them as checkboxes named by tool (`echo`, `lookup_order`) with their descriptions; ticking `echo` calls `onChange` with `{mcp_server_id, remote_name: "echo", schema_hash: <the discovery's>, required: false}`; a `Required` checkbox appears for a ticked tool and flips `required`;
  - unticking calls `onDrop("srv/echo")` (the cascade lives in the editor);
  - a selected tool of a disabled server is listed, disabled, with `Disabled`, and can still be unticked; a selected tool absent from the discovery says `No longer offered`;
  - a server whose detail failed to load shows its name with `Could not load tools` and a Try again button; the picker keeps working for the others;
  - loading shows `Loading MCP servers`; the picker makes no request until the Tools tab is opened (assert `getMCPServers` not called on first render of another tab);
  - the checkboxes are labelled, grouped by server in `<fieldset><legend>`, and reachable by keyboard.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/routes/flows/editor` — Expected: FAIL.
- [ ] **Step 3: Implement.** `useMCPCatalog` is two `useQuery`/`useQueries` calls (the same keys the MCP page uses, `["mcp-servers"]` and `["mcp-server", id]`, so they share the cache), enabled only when the Tools tab is open. `ToolPicker` renders one `<fieldset>` per server; the selection's hash is the discovery's at tick time. In `StepPanel`, the Tools tab renders `ToolPicker` then the existing per-step fieldset (which now lists `tools` from state).
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS (including the design-rule test that forbids shadows and hardcoded colours).
- [ ] **Step 5: Commit** `feat(ui): choose a flow's MCP tools from the step's Tools tab`.

---

### Task 6: The browser proves it, docs, gate, review

**Files:**
- Create: `e2e/tests-flows/32-tool-picker.spec.ts`
- Modify: `docs/api/v1.md` (`mcp_tools` on `PUT /flows/{id}`, on `draft`), `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (Tools tab; S1 limit lifted)

- [ ] **Step 1: Write the spec** (echo only; the MCP test servers are started by the global setup; helpers in `e2e/lib/mcp-helpers.ts`): connect the `ok` test server through the MCP page, create a one-step flow through the API (no tools), open it, open the step, open the Tools tab: the server and its `echo` tool are listed; tick `echo`, tick it under `Tools this step can call` if the flow is multi-step (use a two-step flow so the per-step list shows); open Test call: the note says unpublished changes; start the call and send the echo-LLM phrase that triggers the tool (see `mcpToolPhrases`, `e2e/tests-flows/30-mcp.spec.ts` for the phrase and the `lastMCPCall` assertion): the MCP server records the call and the transcript shows the result; the published flow still has no tools (`GET /flows`); Publish: one new version with `mcp_tools` and the step's `tools`; reload via in-app navigation and the tick persists; untick the tool: the step's tick disappears with it; Publish: the version has neither. `step(page, …)` screenshots.
- [ ] **Step 2: Run** `make e2e-echo` — Expected: all green. Fix real failures in the code, not the spec.
- [ ] **Step 3: Docs.** `docs/api/v1.md`: `mcp_tools` on update (absent keeps, present replaces, one version, validated like `mcp-tools`, errors `400 invalid_tool_selection`, at most 64), on `draft` (validated like a publish, call uses exactly the draft's tools). Spec: Tools tab text; S1 "a draft can use only the flow's attached tools" is replaced by "a draft may select tools".
- [ ] **Step 4: Full gate.** `gofmt -l server`; `cd server && go test -race -count=1 ./...`; `make test-postgres`; `cd ui && npx vitest run && npx tsc --noEmit`; `make e2e-echo` — Expected: all green.
- [ ] **Step 5: Independent review** (AGENTS.md): fresh Opus reviewer on `main..HEAD` with the five Review Focus items; transaction scope (the validation and the version insert in one tx; the name rollback), the `mcp_review_required` rule (Ruling 2: can a client clear a review by sending the same selection back, and is that intended?), `ValidateSelections` on a draft versus a concurrent server delete, the 64 cap, the one-step flat flow, UI honesty (no hidden requests, loading and error states, a11y of the grouped checkboxes), YAGNI (is `required` needed in the picker; is `useMCPCatalog` duplicating `MCPView`'s queries). Fix confirmed findings, each with a test that fails first.
- [ ] **Step 6: Commit** `docs: MCP tool picker done — tools chosen in the designer publish and test with the flow`.
