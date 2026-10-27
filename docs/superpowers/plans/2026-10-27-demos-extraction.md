# Demos extraction and optional demo deployments — plan

Date: 2026-10-27. Branch: `demos-extraction`. Spec:
[2026-10-27-demos-extraction-design.md](../specs/2026-10-27-demos-extraction-design.md).

Slices are ordered by dependency; write scopes are disjoint. The Lead owns the server/demo
boundary and the seed core so the two central pieces cannot race.

## S0 — Spec review (before code)

Independent reviewer read the spec and plan, read-only. Verdict: not ready; the reset
algorithm could not pass its own twice-run test, the package layout could not compile, and
the deployment/e2e preconditions were missing. Resolutions (all applied above):

| # | Severity | Finding | Resolution |
|---|---|---|---|
| 1 | Critical | Wipe deleted the token secret while an MCP server still named it (`409 secret_in_use`). | New order: archive live agents → `detach-delete` MCP servers and poll until gone → delete secrets (404 ok) → HTTP tools. |
| 2 | Critical | HTTP tools can never be deleted: the server counts immutable historical pins. | Seed never deletes HTTP tools; it lists by exact name, `PUT`s the expected definition when needed, and pins the returned current hash (create only when absent). Documented as the one reuse required by server immutability. |
| 3 | High | The "publish a minimal version" detach fails on archived agents, so a failed run cannot be repaired. | Wipe archives live agents only; MCP `detach-delete` handles pins (including archived agents) at storage level. The detach-config path is removed from the algorithm. |
| 4 | High | `demos` registry importing demo packages that import `demos` is an import cycle; `seed` is `package main` and not importable. | Add `demos/spec` for the shared types; demo packages import `spec`; `demos` registry and `demos/seed` import `spec`/`demos`. |
| 5 | High | No shipped deployment sets `SWITCHBOARD_SECRETS_KEY`, so auto-seed gets 503. | Add the secret to `bootstrap.sh`, the Cloud Run manifest and the Cloudflare Worker env; document it as a seed prerequisite. |
| 6 | High | Removing `demo.auto_install` leaves the deployed SQLite Cloud Run profile empty. | Documented as intended clean-room behaviour; Postgres recommended, or run the demo pipeline after deploy. Deploy-workflow text updated. |
| 7 | High | Global e2e seeding breaks `05-first-run` (needs empty store) and `50-screens` (archives all agents). | No global seed; `60-dental`/`61-atlas` seed in `beforeAll`; reorder/renumber if needed. |
| 8 | High | Atlas MCP binds loopback by default, so containers cannot reach it. | Container manifests/Dockerfile set `ATLAS_MCP_ADDR=0.0.0.0:$PORT`; static test covers it. |
| 9 | High | Demo deployer identity and token/management-key sources were unspecified and not permitted. | Define per-demo token secrets and management-key source per target; extend bootstrap IAM; Cloudflare requires API token/account; workflow masks secrets; no generated tokens. |
| 10 | Medium-High | Cloudflare `/tmp` SQLite discards seeded state. | Cloudflare demo target requires external Postgres + persistent secrets key; ephemeral profile is unsupported for demos. |
| 11 | Medium | The module "boundary for free" claim is false/unenforced. | Add `go list -deps ./server/...` boundary check to the gate; correct the rationale. |
| 12 | Medium | Acceptance "no demo API names" contradicts the 410 shim. | Reworded: the 410 shim is the only demo-named route and is tested. |
| 13 | Medium | Duplicate resources/pagination could break "one live demo". | Seed pages every agent list, acts on all exact-name matches, verifies by created id; workflow concurrency group. |
| 14 | Medium | Token-secret write could replace another server's credential. | Namespaced `demo-<demo>-<purpose>` names; seed refuses a name used by a non-demo MCP server. |
| 15 | Medium | Plan scopes missed config, tools/eval, server tests, rehearsal, Makefile gofmt, link-checker roots, go.work.sum. | Scopes below expanded; these files are explicitly in S1/S3/S4. |
| 16 | Medium | Seed needs API forms not documented as client contract (`POST /agents` with `mcp_tools`, MCP `enabled:true`). | Document both in `docs/api/v1.md`; seed asserts `enabled` and readiness. |
| 17 | Medium | Fake-API tests alone would not catch the reset bugs. | Add a real integration test (server + SQLite + MCP fixture, seed twice) and a demo image CI build. |
| 18 | Low | `--token-secret` literal and confirmation semantics were wrong. | Tokens only via env/token file; destructive run on an existing demo requires `--yes`; printed commands use env indirection. |
| 19 | Low | Seed Dockerfile/Job duplicated the workflow. | Dropped; seed runs as a workflow step / `go run ./demos/seed`. |
| 20 | Low | Path/docs slips (Wrangler path, stale docs files). | Corrected paths; S4 lists every doc to rewrite, not only move. |

## S1 — Extract demos, seed client, clean-room server (Lead)

Write scope: `demos/` (new), `server/` (removals/edits and the affected tests:
`demo*_test.go`, `handoff_obs_test.go`, `api_routes_table_test.go`, the server demo files),
`cmd/` (removals), `config/` (remove demo blocks), `tools/eval/agents/` (case move + test
paths), `go.mod`/`go.sum`/`go.work.sum`, `Makefile`, `scripts/deadcode.sh`,
`scripts/check-links.mjs` (add `demos` to the top-level paths), UI demo files.

1. Move `server/mcp/dentalmcp` → `demos/dental/mcp/dentalmcp`, same for atlas;
   `cmd/dental-mcp`/`cmd/atlas-mcp` → `demos/<name>/cmd/...`.
2. Move the agent builders from `server/demo/dental.go|atlas.go` into
   `demos/dental/demo.go`/`demos/atlas/demo.go`, replacing server imports with `demos/spec`
   DTOs/JSON. Add `demos/spec/spec.go` and `demos/registry.go`.
3. Add `demos/seed`: HTTP client DTOs; wipe (archive live exact-name agents → detach-delete
   MCP servers and poll → delete token secrets); create (token secrets → MCP servers with
   `enabled:true` → validate/discovery → HTTP tools list/PUT/create → agent with pins);
   verify by created id/readiness; `--wipe-only`, `--dry-run`, `--yes`, `--wait`, `--json`.
   Unit tests with a fake management API for wipe/create/idempotency/dry-run/safety, and a
   real integration test starting a server (SQLite, echo/no-provider config) plus the moved
   MCP fixture, seeding twice and asserting one live, ready agent.
4. Delete `server/demo/`, `server/demo_api.go` and tests; add a fixed `410 demo_moved`
   response for `/api/v1/demo*` (one-release compatibility), with a test.
5. Remove `demo` config and its validation, update shipped `config/*.yaml`.
6. Remove the Dashboard demo card and demo client methods/tests; keep the rest of the UI.
7. Move `tools/eval/agents/cases-dental` → `demos/dental/eval/`; update
   `tools/eval/agents/main_test.go`, `cases/README.md` and the case header; update Makefile
   (`dental-mcp`, `atlas-mcp`, `seed`, `eval-agents` case path), `gofmt -l` paths, deadcode
   roots and link-checker top-level paths. Add the `go list -deps ./server/...` boundary
   check to the Makefile/CI.
8. Demo unit tests: config decodes to the same step/handoff/tool counts (using
   `server/agent` in tests); `demos/spec`/registry tests; seed tests above.

Gate for S1: `go build ./...`, `go test ./...` in root and server, UI unit tests, `gofmt`.

## S2 — e2e and UI behaviour (build teammate; write scope `e2e/`)

Depends on S1's `bin/seed` CLI and demo fixtures.

1. `e2e/global-setup-agents.ts`: build `bin/seed` (or use `go run`) and the moved demo MCP
   binaries; start the fixtures; pass only generic secrets to the server. Do **not** seed
   globally.

2. `60-dental.spec.ts`/`61-atlas.spec.ts`: seed the demo in `beforeAll` through `bin/seed`
   (after `50-screens` has archived agents, so re-seeding is required); remove the Dashboard
   "Load demo" test; assert the seeded agent exists with the same steps/handoffs/pins; keep
   the call, canvas, call-path, transcript and end-reason assertions. Update evidence
   screenshots (Agents page / canvas / call; no demo card). If file ordering makes
   `05-first-run`/`50-screens` fragile, rename the demo specs after 50 and assert the empty
   store before the first demo spec.
3. Update `e2e/README.md` spec table; remove dead demo-card helpers/mocks.

Verification: `npm run typecheck`, `npx playwright test -c playwright.agents.config.ts`
(needs S1 merged into the worktree), plus one-port suite if touched.

## S3 — Deployment: clean server, optional demo pipelines, Cloud Run + Cloudflare (build teammate; write scope `deploy/`, `.github/workflows/`, `docs/deployment*`, `demos/*/deploy/`)

Depends on S1 for demo binary paths and the seed image contract; can write assets in parallel
after the spec review.

1. Clean the server pipeline: `.github/workflows/deploy.yml` no dental image/service/env;
   `deploy/gcp/*` demo manifests removed/moved; `bootstrap.sh` no dental service/secret but
   adds the persistent `SWITCHBOARD_SECRETS_KEY`; `deploy/gcp/switchboard.service.yaml` and
   the Cloudflare Worker env mount it; `deploy/smoke-cloudrun.sh` no demo expectation.
   Remove `demo:` blocks from `config/cloudrun*.yaml` and `deploy/rehearsal/server.yaml`;
   update `deploy/rehearsal/docker-compose.yaml`/README to use the demo deploy scripts or
   drop the dental service. Update `server/gcp_manifest_test.go`,
   `server/gcp_bootstrap_test.go`, `server/deploy_workflow_test.go`,
   `server/deploy_contract_test.go` (including the rehearsal comparison) and
   `server/cloudrun_config_test.go`.
2. Demo Cloud Run assets per demo (`demos/<name>/deploy/cloudrun/`): `Dockerfile.mcp`,
   `cloudrun.yaml`, `deploy.sh` (build/push, deploy with the demo token env, health, output
   URL). Atlas sets `ATLAS_MCP_ADDR=0.0.0.0:$PORT`.
3. Demo Cloudflare assets per demo (`demos/<name>/deploy/cloudflare/`): amd64
   `Dockerfile.mcp`, `wrangler.jsonc`, generic Worker proxy for `/mcp`, `/api/*`, `/health`;
   token secret handling; `ATLAS_MCP_ADDR` for Atlas. Existing server `deploy/cloudflare`
   gains `SWITCHBOARD_SECRETS_KEY` and the optional Postgres DSN.
4. `.github/workflows/deploy-demo.yml` + a dispatch entry: inputs demo/target/environment/
   server_admin_url/management_key_secret/mcp_base_url (Cloudflare)/auto_seed/image_tag;
   build the MCP image; deploy Cloud Run or Cloudflare; health; run `go run ./demos/seed`
   from the runner; fail on non-ready; print an env-indirection command when auto-seed is
   off. Define per-demo token secrets and management-key sources; extend bootstrap IAM for
   Secret Manager reads; mask all secrets. No seed image/Job.
5. CI/static tests: update the server deployment tests above; add CI jobs that build the
   seed binary and each demo MCP image, and static checks for the new demo manifests/workflow
   (no secrets, correct paths, Atlas ADDR). Add the demo-boundary check.
6. `docs/deployment.md` and `docs/deployment-cloudflare-plan.md`: two-pipeline model,
   auto-seed inputs, token/management-key sources, secrets-store prerequisite, Cloudflare
   external-Postgres requirement, teardown, and the SQLite Cloud Run empty-start change.

Verification: workflow/static tests, `actionlint` if available, cloudflare `wrangler` config
validated where possible without credentials.

## S4 — Docs (Lead or build teammate; write scope `README.md`, `docs/`, `demos/*/README.md`)

1. Move `docs/demo/dentistry.md` → `demos/dental/README.md`, `docs/demo/atlas.md` →
   `demos/atlas/README.md`; add `demos/README.md` (deploy + seed + wipe). Rewrite the moved
   docs: no Dashboard button, no server demo env vars, new seed command.
2. README "Try it", `docs/quickstart.md`, `docs/api/v1.md` (remove the demo section; document
   `POST /agents` with `mcp_tools` and MCP `enabled:true`; mention the 410),
   `docs/operations.md` (upgrade note), `docs/operations/agent-evals.md` case path,
   `docs/pipecat-parity-matrix.md` demo-installer references, `e2e/README.md`,
   `tools/eval/agents/cases/README.md`, `deploy/cloudflare/README.md`.
3. Remove stale demo env vars from deployment docs; document seed with host + management key,
   the secrets-store prerequisite and the token sources.

Verification: `node scripts/check-links.mjs --paths`, `node scripts/check-vocabulary.mjs`.

## S5 — Integration, independent code review, gate (Lead)

1. Integrate slices; `gofmt -w .`; targeted tests; UI tests.
2. Independent code review (fresh context, read-only, diff main...HEAD) against AGENTS.md:
   boundary, seed destructive safety, API compatibility, deployment workflows, docs.
3. Gate, one command at a time: `make verify`, `make e2e-echo`, `make deadcode`.
4. Copy e2e screenshots to `.govoice-dev/atlas-evidence/` (or a scratch dir) and write the
   evidence report; seed command transcript.
5. Merge `main` fast-forward, remove worktree/branch.

## Code review findings (independent, read-only) and resolution

Verdict before fixes: not ready to merge. The extraction, boundary, configs, e2e ordering and
server lifecycle were sound; the findings were seed-safety, a fail-open guard and demo IAM.
All are resolved in the tree except the explicitly deferred refactors.

| # | Severity | Finding | Resolution |
|---|---|---|---|
| 1 | High | A disabled demo HTTP tool was never re-enabled, so a re-seed could not reach readiness. | Seed enables an existing disabled tool and pins its current hash; test added. |
| 2 | High | Seed silently rewrote a same-named HTTP tool, breaking other agents that pinned it. | A differing definition now requires `--yes`; without it seed refuses and names the tool; test added. |
| 3 | Medium | `--wipe-only` could archive the demo and then fail on a secret another MCP server or HTTP tool owns. | `preflightWipe` checks current MCP servers and HTTP-tool definitions before any wipe; tests added. Historical HTTP-tool definitions are not enumerable and remain a documented residual. |
| 4 | Medium | A lost HTTP-tool create race failed instead of reusing the winner. | Seed re-lists on 409 and continues through the definition/enabled checks. |
| 5 | Medium | The boundary guard could fail open and missed `govoice/demos/*`. | The Makefile captures `go list` (fails on error) and matches `^govoice/demos(/|$)`. |
| 6 | Medium (security) | Public demo MCP services ran on the default compute SA. | Bootstrap creates a dedicated `demo-mcp-runtime` SA with only the two demo-token bindings; manifests and deploy scripts use it; tests/docs updated. |
| 7 | Medium (security) | `parseBaseURL` accepted URL userinfo and echoed raw values in errors. | Userinfo and `/mcp` are refused; error messages show a redacted URL; a table test asserts no secret/query is echoed. |
| 8 | Medium/Low | `.gocache/`/`.playwright/` are local work artifacts left in the tree. | Removed before merge; never staged. |
| 9 | Low | Manual seed docs omitted the demo token. | Rehearsal README/.env.example and the Cloudflare README now export it. |
| 10 | Low | Postgres upgrades leave the old demo not-ready. | `docs/operations.md` says to re-seed after upgrade. |
| 11 | Low | `--wait` 2m is shorter than a demo call. | The demo workflow passes `--wait 25m`; `server/deploy_workflow_test.go` asserts the flag. |
| 12 | Low | `--dry-run` skipped preflight and its plan could describe a run that would fail. | Dry-run runs the read-only preflight; messages/counts fixed; it reports a conflicting HTTP tool it would refuse without `--yes`; test added. |
| 13 | Low | Token/key values were not trimmed. | Both are trimmed before use; test added. |
| 14 | Low | Wipe matches names without checking shape, contrary to the design text. | The design now records name-based wipe and the HTTP-tool `--yes` conflict rule. |
| 15 | Low (unverified) | Cloudflare `wrangler secret put` before the first deploy could fail. | Deploy, put the secret, deploy again; still needs a live account run. |
| 16 | Low (unverified) | Demo Cloud Run deploy needed `resourcemanager.projects.get`. | Replaced with `demo-mcp-runtime@<project>`; no project describe call. |
| 17 | Low | Stale docs (README deployment wording, parity panic site, roadmap SQLite demo, design `Detach`/conflict text, deployment IAM claim). | All corrected. |
| 18 | Low | Tests missed disabled/conflicting tools, shared-secret wipe, concurrency. | Disabled/conflict/shared-secret tests added; the create-race path is implemented (no deterministic concurrency test); Postgres/active-call cases remain covered by the real integration test only indirectly. |
| 19 | Low | Bloat/duplication: 566-line deploy contract test, four similar Dockerfiles/two Workers, duplicated HTTP definition builder, placeholder `Demo.Agent` call for the name. | Deferred: mechanical refactors after the functional gate; recorded here as follow-ups. |

## Plan-to-build deviations

- The seed sends the server's actual MCP transport constant `streamable-http` (the plan's
  underscore spelling is rejected by `POST /api/v1/mcp-servers`).
- The seed ignores extra `--mcp logical=url` values so the shared `make seed` target can pass
  both demo bases; missing/unknown required logicals are still refused.
- Wipe resolves the demo agent name through `Demo.Agent` with placeholder ids; no placeholder
  is ever written.
- The seed's real integration test wraps the real dental definition in echo-provider stages:
  the shipped dental agent uses xAI TTS, which the default test build does not register, so a
  ready agent is impossible otherwise. MCP discovery, secret, tool and agent/readiness paths
  remain real.
- The `/api/v1/demo*` 410 shim is matched before the management-availability prefix table, so
  it answers 410 even when management is disabled; the existing route-table test was updated
  and a dedicated 410 test added.
- `config/testdata/effective_*.json` goldens were regenerated for the removed `demo` block.
- The e2e suite seeds per demo spec in `beforeAll` (not globally), so `05-first-run` still sees
  an empty store and `50-screens` archiving agents is followed by a reseed.
- The Cloudflare demo path ships a `cloudflare-demo-postgres.yaml` profile; the Worker selects
  it when `GOVOICE_DATABASE_DSN` is a Postgres DSN (the server has no dialect env switch).
- Cloud Run demo MCP services run on the dedicated `demo-mcp-runtime` account and can read
  only their demo token secret; the deployer gets `iam.serviceAccountUser` on it.
- Deployment workflow/actionlint/Cloud Run/Cloudflare/Wrangler runs were not executable in
  this environment; static tests and local binary probes cover the checked-in assets.

## Risks

- Wipe ordering: agent pins must be removed before tools/servers can be deleted; active calls
  can block; seed must retry/report without leaving a live demo in a half state.
- Cloudflare Containers are ephemeral: demo state does not survive container restarts without
  external Postgres. Documented, not solved in code.
- Workflow changes cannot be fully exercised without cloud credentials; static tests +
  local (rehearsal) checks must cover paths, and the report must state what was not run
  (Cloud Run deploy, Cloudflare deploy, WIF/IAM, Wrangler containers, real-cluster seed).
- E2E seeding adds time and sequencing to global setup; seed failure must fail the stack
  loudly, not silently skip demos.

## Out of scope

- Porting demo MCPs to TypeScript, agent hard delete, call-history purge, persistent storage
  changes for the server, generic ownership labels.
