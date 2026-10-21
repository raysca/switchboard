> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# Rename agent/node/handoff → flow/step/route (S6) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One vocabulary everywhere: a stored **flow** (not agent) made of **steps** (not nodes) joined by **routes** (not handoffs/edges), in the database schema, Go identifiers, the core library API, recording metadata, tests, docs and tooling. No behavior changes.

**Architecture:** The API and console already say flow/step/route. What is left is internal: the SQL schema (`agents`, `agent_versions`, `agent_mcp_tools`, `agent_id`, `agent_version_id`), `core`'s `Handoff*`/`NodeStages`/`BuildNodeFunc`, `core/record`'s `AgentID`/`agent_id`, tool and test helpers, a legacy config-key upgrade path, and prose. **Nothing is deployed**, so there is **no data migration**: the migration files are edited in place (a database created by an older build is simply recreated). The legacy `entry/nodes/edges/handoff_cue` config upgrade exists only for stored versions that cannot exist, so it is deleted with the rename.

**Tech Stack:** Go (`core`, `server`, `tools`, `providers`), SQLite + Postgres migrations (goose), TypeScript/React, Playwright.

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (S6). Decision (user): rename everything; no complex migration because nothing is deployed.

## Global Constraints

- **No behavior change.** Every existing test passes with only names changed. A rename that needs a logic edit is a ruling in the ledger.
- Word-boundary renames only (`\b`), never blind substring replaces: `magenta`, `dragenter`, `ledger`, `acknowledge`, `wedged`, `stateNode`/`parentNode` (React/DOM `Node`) and `ReactNode` must survive.
- "agent" stays where it means the **voice agent as a concept** in prose ("the agent says…", "voice agent"); it goes where it names the stored object or an identifier. Judge each prose hit; record the kept ones in the ledger.
- Tool names that say "agent" as a product term (`agent-matrix`, `.agent-matrix-out`, `tools/livecheck` "agent-level") are **kept** (Ruling 3).
- Both dialects: the schema rename is verified on SQLite and Postgres (`make test-postgres`).
- Conventional commits; one commit per task; the whole branch is reviewed by an independent Opus reviewer at the end (AGENTS.md).

## Review Focus

1. **A missed SQL name** (a trigger, an index, a FK, a Postgres function body, a fixture migration) that only fails at runtime on one dialect. Pinned by the sweep grep in Task 1 and by running both dialects.
2. **A renamed exported `core` name still referenced from a non-Go place** (docs, examples, `tools/`, comments, test names), or a lowercase variable left half-renamed (`nodeID` next to `stepID`).
3. **Recording metadata and logs:** `agent_id` in recorded file metadata and any log field or metric label; renamed consistently (a reader of an old recording is not a concern: none exist).
4. **Deleted legacy-key upgrade:** a config with `nodes`/`edges` is now rejected like any unknown field, with a clear message; no test or doc still mentions the old keys.
5. **Over-reach:** a replaced word that was not this concept (DOM `Node`, `magenta`, "edge" meaning boundary, "agent" meaning voice agent).

## Rulings made while planning

1. **New names (core):** `Handoff`→`Router`, `NewHandoff`→`NewRouter`, `HandoffConfig`→`RouterConfig`, `HandoffEvent`→`RouteEvent`, `HandoffEventKind`→`RouteEventKind`, `HandoffCommitted/Preparing/Active/Failed/Closed`→`RouteCommitted/RoutePreparing/RouteActive/RouteFailed/RouteClosed`, `ErrHandoffPending/Flipped/Limit`→`ErrRoutePending/ErrRouteFlipped/ErrRouteLimit`, `NodeStages`→`StepStages`, `BuildNodeFunc`→`BuildStepFunc`, parameters/fields `nodeID`→`stepID`, file `core/handoff.go`→`core/router.go` (and its test). Verified free: none of these names exists today. The server already uses route vocabulary (`call.OnRoute`, `EventRoute`, route metrics), so this removes the last translation.
2. **Schema:** `agents`→`flows`, `agent_versions`→`flow_versions`, `agent_mcp_tools`→`flow_mcp_tools`, `agent_id`→`flow_id`, `agent_version_id`→`flow_version_id`, with every trigger, index, constraint and Postgres function carrying the old word renamed the same way (`agent_versions_immutable_update`→`flow_versions_immutable_update`, …). Migration numbering is unchanged; files are edited in place.
3. **Kept on purpose:** the `agent-matrix` tooling and its output directory (a product term for the agent-level matrix harness), and "agent" in prose that means the voice agent. Cost if wrong: two names remain; renaming them later is a mechanical follow-up.
4. **`core/record`:** `AgentID`→`FlowID`, JSON `agent_id`→`flow_id` (recording metadata); `tools/loadtest` `AgentID`→`FlowID`.
5. **Legacy keys deleted:** `upgradeKeys`/`legacyKeys` (server), `LEGACY_KEYS` and its transform (UI), their tests and the doc sentence; `handoff_cue` too.
6. **Local databases:** `.govoice-dev/govoice.db*` and any other database made by an older build must be deleted and recreated (`make dev` does so when the file is absent). Stated in the final message; no code handles it.

---

### Task 1: The schema and every SQL string

**Files:**
- Modify: `server/migrations/sqlite/001_initial.sql`, `002…006` and `server/migrations/postgres/001…006` (every occurrence), `server/storage/*.go` (SQL strings, column scans, comments), `server/retention/*.go`, `server/storage/*_test.go`, fixtures under `server/storage/testdata` if any
- Test: the existing storage, retention and server suites on both dialects

**Interfaces:**
- Produces: tables `flows`, `flow_versions`, `flow_mcp_tools`; columns `flow_id`, `flow_version_id`; the Go structs already say `FlowID`/`FlowVersionID` (no Go API change).

- [ ] **Step 1: Inventory** (record the counts in the ledger): `grep -rInE "agents?_?[a-z_]*" server/migrations server/storage server/retention server/*.go | grep -iE "agent" | grep -viE "magenta|voice agent"` — every hit is a candidate.
- [ ] **Step 2: Write the guard first.** Add `TestSchemaUsesFlowVocabulary` in `server/storage/schema_names_test.go` (`eachStore`): open the store, then read the table and column names from the catalog (`sqlite_master`/`pragma_table_info` on SQLite; `information_schema.tables/columns` on Postgres) and fail if any name contains `agent`; assert `flows`, `flow_versions`, `flow_mcp_tools` exist and `sessions` has `flow_id` and `flow_version_id`. Run it: Expected FAIL (names still say agent).
- [ ] **Step 3: Rename.** In every migration file (sqlite and postgres, up and down sections, triggers, the Postgres `check_session_provenance` function bodies in 001 and 006): `agent_version_id`→`flow_version_id`, `agent_versions`→`flow_versions`, `agent_mcp_tools`→`flow_mcp_tools`, `agent_id`→`flow_id`, `\bagents\b`→`flows`, and trigger/index names with the same stems (`sed -E -i '' 's/agent_version_id/flow_version_id/g; s/agent_versions/flow_versions/g; s/agent_mcp_tools/flow_mcp_tools/g; s/agent_id/flow_id/g; s/\bagents\b/flows/g'` on the SQL files only, then read the diff). Apply the same four substitutions to the Go SQL strings and comments in `server/storage`, `server/retention` and their tests; table aliases that are single letters stay.
- [ ] **Step 4: Verify.** `cd server && go test -race -count=1 ./storage ./retention .` (SQLite) and `make test-postgres` — Expected: PASS including the guard; `grep -rIniE "agent" server/migrations server/storage server/retention` returns only prose kept on purpose (ledger them).
- [ ] **Step 5: Commit** `refactor(storage): flows, flow_versions, flow_mcp_tools and flow_id replace the agent names in the schema`.

---

### Task 2: The core library API (handoff → route, node → step)

**Files:**
- Rename: `core/handoff.go`→`core/router.go`, `core/handoff_test.go`→`core/router_test.go`
- Modify: every Go file naming the Ruling-1 symbols: `grep -rlE "Handoff|NodeStages|BuildNodeFunc|nodeID" core server transport tools providers examples --include='*.go'` (about 30 files), plus comments
- Test: the existing `core` and `server` suites

**Interfaces:**
- Produces: the names in Ruling 1, with identical signatures and behavior.

- [ ] **Step 1: Guard first.** Add `core/naming_test.go`: `TestNoHandoffOrNodeVocabularyInTheCoreAPI` parses the package's exported identifiers with `go/parser` + `go/ast` (non-test files) and fails if any exported name contains `Handoff` or equals/contains `NodeStages`/`BuildNodeFunc`. Run: Expected FAIL.
- [ ] **Step 2: Rename with word boundaries.** From the repo root, over `*.go` files only, in this order (longest first): `HandoffEventKind`→`RouteEventKind`; `HandoffEvent`→`RouteEvent`; `HandoffConfig`→`RouterConfig`; `NewHandoff`→`NewRouter`; `ErrHandoffPending`→`ErrRoutePending`, `ErrHandoffFlipped`→`ErrRouteFlipped`, `ErrHandoffLimit`→`ErrRouteLimit`; `HandoffCommitted/Preparing/Active/Failed/Closed`→`Route…`; `\bHandoff\b`→`Router`; `NodeStages`→`StepStages`; `BuildNodeFunc`→`BuildStepFunc`; `\bnodeID\b`→`stepID`. Use `gofmt -r` where an identifier-aware rewrite is clearer; otherwise `perl -pi -e 's/\bOLD\b/NEW/g'`. Then `git mv` the two files.
- [ ] **Step 3: Test and variable names.** Rename test helpers and test names that carry the words (`newHandoffEnv`, `handoffEnv`, `handoffSession`, `TestHandoff…`) to `Route`/`Router` forms; fix lowercase `handoff` in comments and local names by hand (`handoff`→`route` for the act, `router` for the object); read each hit, do not blind-replace prose.
- [ ] **Step 4: Verify.** `gofmt -l .`; `go vet ./...` in `core`, `server`, `tools`, `providers/...`, `examples`; `cd core && go test -race -count=1 ./...`; `cd server && go test -race -count=1 ./...` — Expected: PASS, including the guard; `grep -rIniE "handoff" core server transport tools providers examples cmd --include='*.go'` returns nothing or only kept comments (ledger them).
- [ ] **Step 5: Commit** `refactor(core): the handoff API becomes Router/Route*; nodes become steps`.

---

### Task 3: Remaining identifiers, recording metadata and the legacy keys

**Files:**
- Modify: `core/record/recorder.go` (+ its tests), `server/recording.go`, `tools/loadtest/client.go`, `tools/loadtest/main.go`, test helpers named `agentWith`/`agentHarness` and similar (`grep -rIlE "\bagent[A-Z][A-Za-z]*\b|\bAgent[A-Z][A-Za-z]*\b" server core tools e2e ui/src --include='*.go' --include='*.ts' --include='*.tsx'`), `server/flow/definition.go` (+ tests), `server/flow/service.go` (the `upgradeKeys` call), `ui/src/api/client.ts` (+ test), `docs/api/v1.md`
- Test: `core/record`, `server/flow`, `ui` vitest

- [ ] **Step 1: Failing tests.** In `server/flow/definition_test.go` replace the legacy-key upgrade test with `TestOldKeysAreRejectedLikeAnyUnknownField`: a config with `entry`, `nodes` and `edges` is rejected with `ErrInvalidConfig` naming the unknown field; in `core/record`, assert the metadata JSON has `flow_id` and no `agent_id`; in `ui/src/api/client.test.ts` replace the legacy-config test with one asserting a version config with `nodes` is **not** rewritten. Run them: Expected FAIL.
- [ ] **Step 2: Rename and delete.** `core/record`: `AgentID`→`FlowID`, JSON `agent_id`→`flow_id`; callers (`server/recording.go`, tests, `tools/*`). `tools/loadtest`: `AgentID`→`FlowID`. Test helpers: `agentWith`→`flowWith`, `agentHarness`→`flowHarness` and the like. Delete `legacyKeys`, `upgradeKeys` and its call in `server/flow/service.go` (`normalizeConfig`) and `definition.go`; delete `LEGACY_KEYS` and the transform in `client.ts`; remove the `handoff_cue` mention and the "versions stored before the rename" sentences from `docs/api/v1.md` and comments.
- [ ] **Step 3: Verify.** `go test -race -count=1 ./...` in `core`, `server`, `tools/...`; `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS; `grep -rIniE "\bagent_id\b|AgentID|upgradeKeys|LEGACY_KEYS|handoff_cue|\bnodes\b|\bedges\b" server core tools ui/src docs/api e2e` returns nothing that is not prose or an unrelated word (ledger the rest).
- [ ] **Step 4: Commit** `refactor: FlowID in recordings and tools; the legacy entry/nodes/edges config keys are gone`.

---

### Task 4: Docs, examples, scripts and prose

**Files:**
- Modify: `README.md`, `docs/*.md` (not `docs/superpowers/**`, which are dated records), `docs/api/v1.md`, `examples/**`, `scripts/*.sh`, `Makefile` comments, code comments
- Test: none beyond the sweep; examples must still build

- [ ] **Step 1: Sweep list.** `grep -rIniE "agent|handoff|\bnodes?\b|\bedges?\b" README.md docs/*.md docs/api examples scripts Makefile | grep -viE "magenta|voice agent|docs/superpowers"` — save the hits to the ledger with a verdict each: *rename* (names the stored object, a route, a step), *keep* (voice agent as a concept, the `agent-matrix` tooling, an unrelated word), *rewrite* (a sentence built around the old term).
- [ ] **Step 2: Apply.** Rename/rewrite the *rename* and *rewrite* hits by hand (no blind replace). `docs/pipecat-parity-matrix.md` and `docs/observability.md` keep Pipecat's own terms where they quote Pipecat's names (say so in a sentence); `README.md` and `docs/api/v1.md` use only flow/step/route. Examples (`examples/**`): rename variables and flags that say agent for the stored flow; they must still compile: `cd examples && go vet ./...` (or each example's module).
- [ ] **Step 3: Verify.** Re-run the Step-1 grep: only *keep* hits remain; `make verify` if present, else `go build ./...` over every module in `go.work`.
- [ ] **Step 4: Commit** `docs: flow, step and route replace agent, node and handoff`.

---

### Task 5: The gate and the review

- [ ] **Step 1: Full gate.** `gofmt -l .`; `go vet ./...` in every `go.work` module; `go test -race -count=1 ./...` in `core`, `server`, `providers/...`, `tools/...`; `make test-postgres`; `cd ui && npx vitest run && npx tsc --noEmit`; `make e2e-echo` — Expected: all green.
- [ ] **Step 2: Fresh database.** Delete `.govoice-dev/govoice.db*`, run `make dev` for a few seconds against the fresh schema (or the server's migration test) and confirm the flows list endpoint answers 200; note the delete-your-old-database requirement in the final message.
- [ ] **Step 3: Independent review** (AGENTS.md): a fresh Opus reviewer on `main..HEAD`, given the five Review Focus items and the Ruling list; it must grep for leftovers itself, run the two guard tests, and look for over-reach (a replaced word that was not this concept) and behavior changes hidden in the diff (any non-rename hunk). Fix confirmed findings, each with a test that fails first.
- [ ] **Step 4: Spec.** Mark S6 done in `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (the "Route paths and API names" blocker is gone) and note the kept `agent-matrix` naming.
- [ ] **Step 5: Commit** `docs: S6 done — one vocabulary`.
