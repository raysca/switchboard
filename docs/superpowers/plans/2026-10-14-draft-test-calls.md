# Test an Unpublished Draft Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** In the flow editor, call the flow **as it is on the canvas**, without publishing: a voice token that carries an inline draft config, a server that validates it exactly as a publish would and runs the call on it, and a Test panel that says plainly "this runs your unpublished changes". Draft calls are recorded as calls marked `draft`, so their traces and transcripts are available and they are easy to tell from real calls.

**Architecture:** `POST /api/v1/voice-tokens` accepts an optional `draft: { config }`. The server validates it with the publish rules (`Decode`, providers attached, step tools a subset of the flow's attached MCP tools), computes its readiness, and holds it **in memory only**, keyed by the token's `jti` (the session id), for the token's lifetime, single use, bounded. When that token connects, `resolveCall` finds the draft by `jti` and starts the session on it instead of the flow's current version. The session row is a normal session pinned to the flow's **current version as its base** (the foreign key stays valid, MCP pins keep working), with its `config_hash` set to the draft's hash and a new `draft` flag. Nothing about the flow, its versions or its storage changes.

**Tech Stack:** Go (`server/storage`, `server/flow`, `server/runtime`, `server`), SQL migration (SQLite + Postgres), React 19 + TypeScript (`ui/`), Playwright (`e2e/tests-flows`, echo provider).

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md`, Section 5, item S1. Decisions with the user: an **inline-config token** (nothing persisted as a version, no draft endpoints); provider secrets stay environment-only (no change).

## What a draft call can and cannot do (stated, not hidden)

- It runs the **canvas config** with the flow's **currently attached MCP tools**: a draft cannot add or change MCP tool selections (that is a publish, `SetMCPTools`); a step may list only tools the flow already has, the same check publish applies (`checkStepTools`).
- It needs a **callable** flow like a real call: providers available in the environment, no tool problems. If the draft is not ready the token request answers `409 flow_not_ready` with the readiness reasons, so the editor shows why.
- It is **not a version**: no number, no rollback, nothing in "Versions". It appears in Calls with a `Draft` chip, based on the version it was started from.
- The draft lives in server memory for at most the token lifetime. A restart or an expired token loses it; the editor simply requests a new one.

## Global Constraints

- The draft is validated server-side with the same functions a publish uses; the client's idea of validity is never trusted. Provider secrets are never part of any request or response.
- Memory is bounded: at most 32 outstanding drafts, each at most the config size limit publish enforces; entries expire with their token and are removed on first use. Over the cap: `429 too_many_drafts`.
- SQL runs unchanged on SQLite and Postgres (`r.s.bind`, `boolValue(dialect, …)`, `utc(...)`); the migration exists in both directories. Existing rows get `draft = false`.
- UI: tokens only for colour (no hex in TSX/CSS), sentence case, existing primitives; the Draft chip uses the neutral `Status` primitive. Copy is honest: "Runs your unpublished changes", never "saved".
- The existing published-version test keeps working unchanged when there are no unpublished changes.
- Conventional commits ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`; gates: `cd server && go test -race ./...`, `cd ui && npx vitest run && npx tsc --noEmit`, `make e2e-echo`.

## Review Focus

1. **A draft can never run unvalidated or leak into real calls.** A token without a draft entry is untouched; a draft is consumed once; an expired or unknown `jti` falls back to the normal path only when the token never had a draft (a draft token whose entry is gone must fail, not run the published version silently). Pinned in Task 4.
2. **A draft cannot reach beyond the flow:** unattached provider, step tool not attached to the flow, invalid config, oversized config, another flow's id. Pinned in Tasks 3 and 4.
3. **Honest recording.** The draft session is marked `draft`, based on the current version, its `config_hash` is the draft's; a publish during the call neither changes nor ends it. Pinned in Tasks 1 and 4.
4. **Bounds and cleanup.** The cap, expiry and single use; a connect that never happens frees its entry. Pinned in Task 4.
5. **The editor says what it is testing.** With unpublished changes the panel says it runs them and the header says "Testing draft"; with none it behaves as today; a draft call's stale-version warning never fires. Pinned in Task 6.

---

### Task 1: The `draft` flag on sessions, and an exported config hash

**Files:**
- Create: `server/migrations/sqlite/006_session_draft.sql`, `server/migrations/postgres/006_session_draft.sql`
- Modify: `server/storage/history.go` (`SessionRecord`, `Create`, `Get`, `List`), `server/storage/storage.go` (export the hash), `server/storage/storage_test.go` (the migration-version assertion)
- Test: `server/storage/draft_test.go` (create)

**Interfaces:**
- Produces: `SessionRecord.Draft bool`; `storage.ConfigHash(raw []byte) (hash string, canonical []byte, err error)` (the existing `hashJSON`, exported).

- [ ] **Step 1: Write the failing tests** — `server/storage/draft_test.go`:

```go
package storage

import (
	"context"
	"testing"
	"time"
)

func TestSessionDraftFlagRoundTripsAndDefaultsToFalse(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) { // eachStore, seedSession: server/storage/orphans_test.go
		ctx := context.Background()
		real := seedSession(t, s, time.Now(), "active")
		if got, err := s.Sessions().Get(ctx, real); err != nil || got.Draft {
			t.Fatalf("a normal session must not be a draft: %+v err %v", got, err)
		}
		a := Flow{ID: NewID(), Name: "D", Enabled: true, CreatedAt: time.Now()}
		if err := s.Flows().Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		v, err := s.Flows().PublishVersion(ctx, a.ID, 1, []byte(`{}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		hash, _, err := ConfigHash([]byte(`{"draft":true}`))
		if err != nil {
			t.Fatal(err)
		}
		id := NewID()
		if err := s.Sessions().Create(ctx, SessionRecord{ID: id, FlowID: a.ID, FlowVersionID: v.ID, ConfigHash: hash, StartedAt: time.Now(), Status: "active", Draft: true}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Sessions().Get(ctx, id)
		if err != nil || !got.Draft || got.FlowVersionID != v.ID || got.ConfigHash != hash {
			t.Fatalf("draft session %+v err %v", got, err)
		}
		list, err := s.Sessions().List(ctx, SessionFilter{FlowID: a.ID})
		if err != nil || len(list) != 1 || !list[0].Draft {
			t.Fatalf("list %+v err %v", list, err)
		}
	})
}

func TestConfigHashMatchesAPublishedVersionsHash(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := Flow{ID: NewID(), Name: "H", Enabled: true, CreatedAt: time.Now()}
	if err := s.Flows().Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, err := s.Flows().PublishVersion(ctx, a.ID, 1, []byte(`{"b":2,"a":1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h, _, err := ConfigHash([]byte(`{ "a": 1, "b": 2 }`)); err != nil || h != v.ConfigHash {
		t.Fatalf("hash %q err %v, want the published %q (same canonical form)", h, err, v.ConfigHash)
	}
}
```

Also change the migration test's expected version in `TestMigrationEmptyAndIdempotentReopen` from `5` to `6`.

- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -run 'TestSessionDraft|TestConfigHash|TestMigrationEmpty' ./storage` — Expected: FAIL (`Draft` and `ConfigHash` undefined).

- [ ] **Step 3: Implement.** Migrations (follow the goose format of `005_agent_mcp_review.sql`; copy its `-- +goose Up/Down` markers):

SQLite: `ALTER TABLE sessions ADD COLUMN draft INTEGER NOT NULL DEFAULT 0;`
Postgres: `ALTER TABLE sessions ADD COLUMN draft BOOLEAN NOT NULL DEFAULT FALSE;`

(Down: SQLite `ALTER TABLE sessions DROP COLUMN draft;` works on SQLite ≥ 3.35; if the repo's driver is older, mirror how 005's Down handles it.) In `storage.go` rename `hashJSON` → `ConfigHash` (exported, same body) and update its callers. In `history.go` add `Draft bool` to `SessionRecord`; `Create` inserts `draft` with `boolValue(r.s.dialect, v.Draft)` as an extra column and value; `Get` and `List` select `draft` and scan it the way `turns.interrupted` or `agent_versions.mcp_review_required` are scanned (read how those handle both dialects' bool scan and copy it).

- [ ] **Step 4: Run to see them pass, then the package.** `cd server && go test -race -count=1 ./storage 2>&1 | tail -3` — Expected: PASS. If `TEST_POSTGRES_DSN` is set (docker `postgres:16-alpine`), run it there too.
- [ ] **Step 5: Commit** `feat(storage): sessions carry a draft flag; ConfigHash is exported`.

---

### Task 2: The API shows `draft`

**Files:**
- Modify: `server/api/traces.go` (`SessionDTO`, its mapping), `ui/src/api/client.ts` (the session schema)
- Test: `server/api/traces_test.go`, `ui/src/api/client.test.ts`

**Interfaces:**
- Produces: `SessionDTO.Draft bool` as `"draft"`; the UI session type has `draft: boolean` (default `false` when the field is absent, so older servers keep working).

- [ ] **Step 1: Failing tests.** In `traces_test.go`, add a case to the sessions list test: a stored session with `Draft: true` is returned with `"draft":true` and a normal one with `"draft":false`. In `client.test.ts`, parse a session payload without `draft` → `draft === false`, and with `draft: true` → `true`.
- [ ] **Step 2: Run to see them fail.** `cd server && go test -run Sessions ./api`; `cd ui && npx vitest run src/api/client.test.ts` — Expected: FAIL.
- [ ] **Step 3: Implement.** Add `Draft bool \`json:"draft"\`` to `SessionDTO` and set it where `SessionDTO` is built from `storage.SessionRecord`; in `client.ts`'s session zod schema add `draft: z.boolean().default(false)`.
- [ ] **Step 4: Run to see them pass.** Same commands — Expected: PASS; `cd ui && npx tsc --noEmit` clean.
- [ ] **Step 5: Commit** `feat(api): sessions say whether they were a draft test`.

---

### Task 3: `flow.Service.ResolveDraft`

**Files:**
- Modify: `server/flow/service.go` (after `ResolveCurrent`; add `Draft bool` to `Snapshot`; make `StartCapturedSession` set `Draft` on the stored record)
- Test: `server/flow/draft_test.go` (create)

**Interfaces:**
- Consumes: `normalizeConfig`, `Decode`, `Problems`, `checkConfigTools`, `flowReadiness`, `toVersion`, `storage.ConfigHash` (Task 1).
- Produces: `func (s *Service) ResolveDraft(ctx context.Context, id string, config json.RawMessage) (Snapshot, Readiness, error)` — validates `config` like a publish for flow `id`, returns a `Snapshot{FlowID, Version: <the current version with Config and ConfigHash replaced by the draft's>, ConfigHash: <draft hash>, MCPTools: <current attached tools>, Draft: true}` and the draft's `Readiness`. Errors: `ErrInvalidConfig` / `ErrNotAttached` (same values `Create`/`Update` return), `sql.ErrNoRows` when the flow does not exist, an error for an archived or disabled flow.

- [ ] **Step 1: Write the failing tests** — `server/flow/draft_test.go` (reuse the service test harness of `service_test.go`: read how it builds a `Service` over a SQLite store with an availability function; copy it):

```go
func TestResolveDraftReturnsTheDraftOnTheCurrentVersion(t *testing.T) // flow published with prompt A; draft changes the llm system_prompt to B: Snapshot.Draft true, Snapshot.Version.ID == current version id, Version.Number == current number, Config contains B, ConfigHash == storage.ConfigHash(draft) and != the published hash, MCPTools == the flow's attached tools; the flow's stored current version is unchanged afterwards
func TestResolveDraftRejectsWhatAPublishWouldReject(t *testing.T)   // invalid JSON/config (ErrInvalidConfig), a provider the environment lacks (ErrNotAttached), a step tool the flow has not attached (checkStepTools error), a config over the size limit
func TestResolveDraftReportsReadiness(t *testing.T)                  // a draft that decodes but names a step with no llm: Readiness.State == ReadinessNotReady with reasons (no error: the caller decides)
func TestResolveDraftNeedsAUsableFlow(t *testing.T)                  // unknown id -> sql.ErrNoRows; archived or disabled -> error
func TestResolveDraftWritesNothing(t *testing.T)                     // the flow's version count and updated_at are unchanged
```

- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -run TestResolveDraft ./flow` — Expected: FAIL (`ResolveDraft` undefined).
- [ ] **Step 3: Implement** in `service.go`:

```go
// ResolveDraft validates config exactly as a publish of flow id would and returns what a call on it
// runs: the flow's current version with the draft's config and hash, and the flow's attached MCP
// tools. Nothing is written. A draft cannot change tool selections, only use the attached ones.
func (s *Service) ResolveDraft(ctx context.Context, id string, config json.RawMessage) (Snapshot, Readiness, error) {
	row, err := s.store.Flows().Current(ctx, id)
	if err != nil {
		return Snapshot{}, Readiness{}, err
	}
	if row.Archived || !row.Enabled || row.CurrentVersionID == "" {
		return Snapshot{}, Readiness{}, errors.New("flow unavailable")
	}
	canonical, err := s.attachedConfig(ctx, config)
	if err != nil {
		return Snapshot{}, Readiness{}, err
	}
	if err := s.checkConfigTools(ctx, id, canonical); err != nil {
		return Snapshot{}, Readiness{}, err
	}
	base, err := s.store.Flows().Version(ctx, row.CurrentVersionID)
	if err != nil {
		return Snapshot{}, Readiness{}, err
	}
	hash, canonical, err := storage.ConfigHash(canonical)
	if err != nil {
		return Snapshot{}, Readiness{}, err
	}
	draft := base
	draft.ConfigJSON, draft.ConfigHash = canonical, hash
	readiness, tools := s.flowReadiness(ctx, draft, row.Enabled, row.Archived)
	return Snapshot{FlowID: row.ID, Version: toVersion(draft), ConfigHash: hash, MCPTools: tools, Draft: true}, readiness, nil
}
```

Add `Draft bool \`json:"draft,omitempty"\`` to `Snapshot`, and in `StartCapturedSession` set `Draft: snapshot.Draft` on the `storage.SessionRecord` and use `snapshot.ConfigHash` (already used). Check `normalizeConfig` enforces the size limit publish uses; if it does not, add the explicit check here with the same constant publish uses (find it: `rg -n "len\(.*\) >" server/flow/*.go`).

- [ ] **Step 4: Run to see them pass, then the package.** `cd server && go test -race -count=1 ./flow ./storage 2>&1 | tail -3` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(flow): ResolveDraft — a draft config validated like a publish, on the current version`.

---

### Task 4: Draft tokens end to end on the server

**Files:**
- Create: `server/drafts.go`, `server/drafts_test.go`
- Modify: `server/runtime/mcp.go` (`StartSession` split), `server/api/api.go` (`VoiceTokenRequest.Draft`), `server/api_routes.go` (voice-tokens handler), `server/call.go` (`resolveCall`), `server/server.go` (the registry field, constructed in `NewServer`)

**Interfaces:**
- Consumes: `flows.ResolveDraft` (Task 3), `storage.ConfigHash` (Task 1).
- Produces:
  - `api.VoiceTokenRequest{ Principal, FlowID string; Draft *DraftRequest }` with `DraftRequest{ Config json.RawMessage \`json:"config"\` }`.
  - `type draftCalls struct` with `put(id string, d draftCall, now time.Time) error` (error `errTooManyDrafts` over 32 live entries, after dropping expired ones), `take(id string, now time.Time) (c draftCall, found, lost bool)` (single use; `lost` means the jti was issued as a draft but its entry is gone), where `draftCall{ flowID string; snapshot flow.Snapshot; expires time.Time }`; `(s *Server) drafts *draftCalls`.
  - `runtime.StartDraftSession(ctx, flows, mcpService, snapshot flow.Snapshot, sessionID string) (*Session, error)`; `StartSession` becomes `ResolveCurrent` + the shared `startFromSnapshot`.
  - `resolveCall` starts a draft session when `s.drafts.take(tokenID)` finds one; a token that was issued as a draft but whose entry is gone must **fail** (see Review Focus 1): record the draft `jti`s the registry has issued in a small bounded set (or keep a tombstone with the expiry) so `resolveCall` can tell "never a draft" from "draft lost".

- [ ] **Step 1: Write the failing tests** — `server/drafts_test.go` (use `apiTestServer`, `enableTestManagement`, `readyTestFlow`, `apiRequest`, `e2eMgmtKey`, and the WebSocket rig of `flow_e2e_test.go` — `newFlowRigE2E`, `dial`, `say`, `waitReplies` — configured with the echo provider and `label_replies` so a reply starts with the step's prompt):

```go
func TestDraftRegistryIsBoundedSingleUseAndExpires(t *testing.T)          // 32 live entries then errTooManyDrafts; take twice -> second is not found; an entry past its expiry is not found and frees its slot
func TestVoiceTokenWithADraftIsValidatedLikeAPublish(t *testing.T)        // 400 invalid_request for invalid config; 400/409 for an unattached provider; 409 flow_not_ready with details for a draft that decodes but is not ready; 404 for an unknown flow; 201 and a token otherwise; the flow's versions are unchanged
func TestVoiceTokenWithoutADraftIsUnchanged(t *testing.T)                  // existing behaviour: published version
func TestDraftCallRunsTheDraftNotThePublishedVersion(t *testing.T)         // publish prompt "PUBLISHED", request a draft token with prompt "DRAFT", dial: the first reply starts with "[DRAFT"; a second dial with a normal token starts with "[PUBLISHED"
func TestDraftCallIsRecordedAsADraftOnTheCurrentVersion(t *testing.T)      // after the call: the session row has Draft true, FlowVersionID == the flow's current version, ConfigHash == hash of the draft; GET /api/v1/sessions/<id> says draft:true
func TestDraftSurvivesAPublishDuringTheCall(t *testing.T)                  // publish v2 while the draft call is up: the call keeps answering with the draft prompt and is not ended
func TestALostDraftFailsInsteadOfRunningThePublishedVersion(t *testing.T)  // issue a draft token, drop its registry entry (expiry or restart), dial: the connect fails; the published version never answers
func TestADraftTokenIsSingleUse(t *testing.T)                              // the token's jti is single use already (token replay); a second dial fails and the entry is gone
```

- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -run 'TestDraft|TestVoiceTokenWith' .` — Expected: FAIL.
- [ ] **Step 3: Implement.** `server/drafts.go`:

```go
package server

import (
	"errors"
	"sync"
	"time"

	"govoice/server/flow"
)

// maxDrafts bounds the drafts waiting for their call to connect: each holds a config in memory.
const maxDrafts = 32

var errTooManyDrafts = errors.New("too many draft calls waiting to connect")

type draftCall struct {
	flowID   string
	snapshot flow.Snapshot
	expires  time.Time
}

// draftCalls holds validated drafts by the voice token's jti until the call connects or the token
// expires. In memory only: a draft is never a version.
type draftCalls struct {
	mu      sync.Mutex
	pending map[string]draftCall
	issued  map[string]time.Time // jti -> expiry, so a draft whose entry is gone is told apart from a normal token
}

func newDraftCalls() *draftCalls {
	return &draftCalls{pending: map[string]draftCall{}, issued: map[string]time.Time{}}
}

func (d *draftCalls) sweepLocked(now time.Time) {
	for id, c := range d.pending {
		if !now.Before(c.expires) {
			delete(d.pending, id)
		}
	}
	for id, exp := range d.issued {
		if !now.Before(exp) {
			delete(d.issued, id)
		}
	}
}

func (d *draftCalls) put(id string, c draftCall, now time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked(now)
	if len(d.pending) >= maxDrafts {
		return errTooManyDrafts
	}
	d.pending[id], d.issued[id] = c, c.expires
	return nil
}

// take returns the draft for id once. lost reports a token that was issued as a draft but whose
// draft is gone (expired, or the process restarted): the call must fail, not run the published flow.
func (d *draftCalls) take(id string, now time.Time) (c draftCall, found, lost bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked(now)
	if c, ok := d.pending[id]; ok {
		delete(d.pending, id)
		return c, true, false
	}
	_, issued := d.issued[id]
	return draftCall{}, false, issued
}
```

(The `issued` set exists for one narrow window: the token is still valid at connect but the entry was swept at the same expiry instant, which must fail rather than fall back to the published version. After a process restart the token's own `issuer_epoch` check already rejects every token the new process did not issue, so a restart needs no handling here. `issued` entries live until the token expires, so it is bounded by the issuance rate times the token lifetime; the management API is authenticated. If the final review shows the window cannot occur, delete the set.) `VoiceTokenRequest` gains `Draft *DraftRequest`. In the `voice-tokens` handler: when `input.Draft != nil`, after the existing flow lookup call `s.flows.ResolveDraft(r.Context(), input.FlowID, input.Draft.Config)`; map errors with the codes publish uses (`flow.ErrInvalidConfig`/`ErrNotAttached` → `400 invalid_request` with the message; unavailable flow → `409 flow_not_ready`); `!flowCallable(readiness.State)` → the same `409 flow_not_ready` JSON as today with `Details: readiness`; otherwise sign the token as before, then `s.drafts.put(claims.ID, draftCall{...expires: claims.ExpiresAt.Time}, now)`; `errTooManyDrafts` → `429 too_many_drafts`. Skip the existing `flowCallable(target.Readiness.State)` check for drafts (the published state is irrelevant to a draft). In `resolveCall`, before `runtime.StartSession`:

```go
	if c, found, lost := s.drafts.take(tokenID, time.Now()); found {
		rs, err = runtime.StartDraftSession(ctx, s.flows, s.mcpService, c.snapshot, sessionID)
	} else if lost {
		return CallSpec{}, nil, errors.New("draft expired")
	} else {
		rs, err = runtime.StartSession(ctx, s.flows, s.mcpService, flowID, sessionID)
	}
```

(declare `rs`/`err` accordingly). In `runtime/mcp.go` split `StartSession`: keep its signature; move everything after `ResolveCurrent` into `startFromSnapshot(ctx, flows, mcpService, snapshot, sessionID)`; `StartDraftSession` calls it directly. The captured-tool loop already reads `snapshot.MCPTools`, which for a draft is the flow's attached tools with live readiness.
- [ ] **Step 4: Run to see them pass, then the module.** `cd server && go test -race -count=1 ./... 2>&1 | grep -E "^(FAIL|--- FAIL|ok)"` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): voice tokens can carry a draft; the call runs it, recorded as a draft`.

---

### Task 5: The client sends the draft

**Files:**
- Modify: `ui/src/api/client.ts` (`issueVoiceToken`), `ui/src/voice/VoiceClient.ts` (`ConnectOptions.draft`, pass it through)
- Test: `ui/src/api/client.test.ts`, `ui/src/voice/VoiceClient.test.ts`

**Interfaces:**
- Produces: `issueVoiceToken(input: { principal: string; flow_id: string; draft?: { config: unknown } })`; `ConnectOptions.draft?: { config: unknown }`.

- [ ] **Step 1: Failing tests.** `client.test.ts`: `issueVoiceToken({principal, flow_id, draft: {config: {a: 1}}})` POSTs `/voice-tokens` with that body (assert the request body, as the neighbouring tests do); without `draft` the body has no `draft` key. `VoiceClient.test.ts`: `connect({flowId, draft})` calls `issueVoiceToken` with the draft; without it, without.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/api/client.test.ts src/voice` — Expected: FAIL.
- [ ] **Step 3: Implement.** Extend the input type; `VoiceClient.connect` forwards `options.draft` into `issueVoiceToken({ principal, flow_id: flowId, ...(options.draft ? { draft: options.draft } : {}) })`.
- [ ] **Step 4: Run to see them pass.** Same command plus `npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): the voice client can request a draft token`.

---

### Task 6: The editor tests what is on the canvas

**Files:**
- Modify: `ui/src/routes/flows/FlowVoiceTest.tsx` (a `draft` prop), `ui/src/routes/flows/editor/FlowEditor.tsx` (pass it; replace the "runs v{version}" note), `ui/src/routes/sessions/CallsView.tsx` and `CallView.tsx` (the Draft chip), `ui/src/styles.css` if the chip needs a rule
- Test: `ui/src/routes/flows/FlowVoiceTest.test.tsx`, `ui/src/routes/flows/editor/FlowEditor.test.tsx`, `ui/src/routes/sessions/CallsView.test.tsx`

**Interfaces:**
- Produces: `FlowVoiceTestProps.draft?: { config: unknown }`: when present the call is a draft test.

- [ ] **Step 1: Write the failing tests.**
  - `FlowVoiceTest.test.tsx`: with `draft`, clicking Start calls `connect` with `draft`; the header reads `Testing draft (based on v3)`; the stale-version warning ("This call is on v…") never shows for a draft call even after the flow's `current_version` changes; without `draft` the header is `Testing v3` and `connect` has no `draft`.
  - `FlowEditor.test.tsx`: with unpublished changes the Test call panel shows the note `This call runs your unpublished changes. They are not saved or published.` and passes `draft.config === toPayload(draft)`; with no changes it shows no note and passes no draft (today's behaviour, minus the old "Publish to hear your changes" note, which no longer applies); a draft with validation errors (`validate(g)` not empty) disables Start with the reason.
  - `CallsView.test.tsx` and the call detail test: a session with `draft: true` shows a `Draft` chip beside its name; a normal one does not.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/routes/flows src/routes/sessions` — Expected: FAIL.
- [ ] **Step 3: Implement.** In `FlowVoiceTest`: `handleConnect` passes `draft` into `voiceClient.connect`; the header text is `draft ? \`Testing draft (based on v${n})\` : \`Testing v${n}\``; `callIsStale` is `!draft && …`. In `FlowEditor`, where the call panel renders: compute `const draft = unpublished && issues.length === 0 ? { config: toPayload(g) } : undefined` (the same `g` and issue list the Publish button uses: read how `unpublished` and the validation state are derived around line 93 and reuse those exact values), pass it, and swap the note. In the Calls list and detail add `{session.draft && <Status tone="neutral">Draft</Status>}` next to the flow name.
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): the Test call panel runs the canvas draft; Calls marks draft calls`.

---

### Task 7: The browser proves it

**Files:**
- Create: `e2e/tests-flows/25-draft-test.spec.ts`

**Interfaces:**
- Consumes: the echo stack (`label_replies: true`: each reply begins with the step's system prompt in brackets), `e2e/lib/flows-helpers.ts` (`connectAdmin`, `api`, `openFlow`, `startCall`, `sendText`, `botReply`), `e2e/lib/evidence.ts` (`step`).

- [ ] **Step 1: Write the spec** (runs after the flow-lifecycle spec so a flow exists; create its own flow through the API to stay independent): create a flow whose prompt is `PUBLISHED PROMPT`; open it in the editor; change the step's Instructions to `DRAFT PROMPT` without publishing; open Test call: the note `This call runs your unpublished changes` is visible; `startCall`, `sendText("hello")`, expect a bot reply matching `/DRAFT PROMPT/`; disconnect; assert through the API that the flow still has one version with `PUBLISHED PROMPT`; open Calls: the new call has the `Draft` chip; then Publish, open Test call again: no note, and the reply matches `/DRAFT PROMPT/` (now published) without the chip on the new call. Take `step(page, …)` screenshots.
- [ ] **Step 2: Run** `make e2e-echo` — Expected: all green including the new spec. Fix real failures in the code, not the spec.
- [ ] **Step 3: Commit** `test(e2e): a draft test call runs the canvas, not the published version`.

---

### Task 8: Docs, spec, gate, independent review

**Files:**
- Modify: `docs/api/v1.md` (voice-tokens `draft`), `docs/operations_runbook.md` (a short note), `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (S1 row)

- [ ] **Step 1: Docs.** `docs/api/v1.md`: the `draft` field, its validation, the codes (`400 invalid_request`, `409 flow_not_ready`, `429 too_many_drafts`), the limits (attached tools only, 32 waiting, token lifetime, memory only) and that the session is recorded with `draft: true` based on the current version. Spec S1: done, with that scope. Runbook: "Draft test calls are held in memory until they connect; a restart loses them and the editor requests a new token."
- [ ] **Step 2: Full gate.** `cd server && go test -race -count=1 ./...`; `cd ui && npx vitest run && npx tsc --noEmit`; `make e2e-echo`; `gofmt -l server` — Expected: all green.
- [ ] **Step 3: Independent review** (AGENTS.md). Fresh reviewer on `main..HEAD`: the five Review Focus items; security (can a draft be used by a different principal or flow; token handling; config size and memory exhaustion; the `issued` set growth), concurrency (registry locking, take/put races, a publish racing the draft's validation), YAGNI (is the `issued` tombstone needed given the token epoch check — if the reviewer shows it is redundant, delete it), Postgres (run the storage tests with `TEST_POSTGRES_DSN`), UI honesty and a11y of the note and chip. Fix confirmed findings, each with a test that fails first.
- [ ] **Step 4: Commit** `docs: S1 done — draft test calls`.
