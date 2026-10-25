# Switchboard next implementation phases

## Purpose

This document groups the next product and runtime capabilities into sequenced epics. It is a planning baseline, not a commitment to a release date. Telephony integrations are excluded; the focus is the web voice experience, agent authoring, operations, and deployment.

It was last checked against the code on 2026-10-23 (`main` at `c5a3c1bb`, plus the unmerged eval work on `feat/phase-11`). Each baseline row names the evidence, so a later reader can re-check it.

### Status key

- **Present:** implemented and usable in the current product.
- **Partial:** a useful base exists, but the target experience or coverage is incomplete.
- **Planned:** no implementation exists yet.
- **In progress:** work exists on a branch that is not yet on `main`.

## Terminology

The product model is the **agent**: a stored, versioned configuration made of one or more **steps** joined by **handoffs**. The earlier names for these were retired from the API, schema, UI and docs, and guard tests keep them out (`core/naming_test.go`, `server/storage/schema_names_test.go`, `scripts/check-vocabulary.mjs`). This roadmap uses the current words. An earlier draft treated the old name as a future concept that would replace "agent"; that decision is closed.

## Current baseline

| Capability | Status | Evidence and remaining gap |
|---|---|---|
| Agents as the authoring model | Present | Every save that changes config or tools publishes the next immutable version (`MutateAndPublish`, `server/agent/service.go`; SQLite triggers in `server/migrations/sqlite/001_initial.sql`). Publish validates schema, providers and tools, and a readiness state gates calls. The UI has the editor, a publish dialog with a diff and the number of calls in progress, and a one-step restore of the previous version. Gaps: there is no separate draft entity (every save is a publish), no endpoint that lists versions (only `GET /agents/{id}/versions/{n}`), and rollback is a client-side re-save of the previous version. |
| Call pinning | Present | A call pins the version it starts with; a publish never changes a live call. |
| Draft test calls | Present | `draft.config` on `POST /voice-tokens` tests an unpublished configuration; drafts are held in memory only (32 at most). Not in the original roadmap. |
| Agent lifecycle (archive, restore, versions) | Present | `POST /agents/{id}/archive` disables an agent; `POST /agents/{id}/unarchive` brings it back disabled; `GET /agents/{id}/versions` lists its publishes and `POST /agents/{id}/versions/{n}/restore` publishes an earlier one again as the next version; the editor has a Version history list and the agents page an archived list with Unarchive. There is no delete: versions are immutable and calls refer to them, so archive is the way to retire an agent. Reference detection exists for MCP servers and HTTP tools (`mcp_server_in_use`, `http_tool_in_use`). |
| Text-only agent | Present | The keyless `silence` provider (`server/registry_silence.go`, `docs/providers.md#silence-text-only`, `docs/decisions/silence-provider.md`). The console hides the microphone for these agents; `examples/rtvi_client/text.html` is a browser page using the browser's own speech. Limit: STT and TTS processors still start on in-process backends, so "runs without initialising STT or TTS" is true in effect, not literally. |
| Call recording | Partial | `core/record` has a `Sink` interface and a local WAV sink with a JSON sidecar; `server/recording.go` sets directory, retention days, maximum duration and size, and a janitor enforces retention. An agent opts in with `disclosure_confirmed`. Missing: object storage (S3 or R2 is interface only), encryption at rest, signed URLs, any API or UI to list, play or download a recording (a recording is removed with its call), access audit, and any check that a spoken disclosure was played. |
| PII scrubbing | Partial | `core/redact` is wired through `privacy.redaction` (default off; DTMF masking on) into stored turn history, the recording sidecar, timeline error text, session logs and OTel content attributes. Tool arguments and results are never stored or traced. `docs/privacy.md` lists what is not covered: the live model context, what providers receive, the client transcription message, recording audio and data stored before enabling. Missing: a per-agent policy (it is server-wide), read-time redaction, and detection beyond regular expressions. |
| Data retention | Partial | One server-wide job (`server/retention`, [privacy.md](privacy.md#server-data-retention-and-privacy-controls)) with two independent lifetimes: conversational content (`content_retention_days`: transcripts and assistant text are blanked) and operational metadata (`metadata_retention_days`: sessions, turns, spans and tool-call rows are deleted), run every `retention.interval` in bounded batches; active and draining sessions are never touched; agent versions and tool definitions are immutable and kept. Recordings have their own janitor (`recording.retention_days`, by file time, never longer than content retention). Purging is off unless `retention.enabled` is set (on in `production.yaml` and the Cloud Run profiles). `GET /api/v1/retention` shows the policy in effect and the last run, a dry run counts what the next run would remove, runs and removals are metrics, a `GovoiceRetentionStalled` alert watches the job, and a configuration that keeps content longer than metadata (or recordings longer than content) is refused naming both values. A call can be held (the job and the recording janitor skip it) and one call, or every call of a caller label, can be erased on request with its recording on that instance; every hold, release and erasure is in an audit history without content ([privacy.md](privacy.md#holds-and-erasure)). Missing: a policy per agent or per data class, an export before erasure, cross-instance recording erasure, and a dashboard view of the policy. Data outside the database (log files, an OTel backend, backups) is not purged by it. |
| Observability | Partial | Prometheus (`server/metrics.go`, 14 alert rules in `deploy/alerts/govoice.rules.yml`); OTel export with one trace per call (`server/otelexport`); stored per-call spans, events and tool calls at `GET /sessions/{id}/traces`, shown by `TurnTimeline` and `ReplyWaterfall`; and an in-memory timeline at `/debug/timeline` (admin listener only, lost on restart, no UI). Missing: a trace explorer, the OTel `trace_id` stored on the session (so there is no call-to-trace link), and any "trace unavailable" reporting. |
| Language configuration | Partial | Only STT has a language setting (start step), shown as a dropdown. TTS has none; the model language is left to the prompt. Catalog `languages` lists are short and do not gate anything. No compatibility matrix and no publish-time check. |
| MCP servers and tools | Present | Management API (`docs/api/mcp.md`): create, update, enable, validate, test a tool, `detach-delete`; background health sync; schema-hash-pinned tool selection per agent version and a per-step subset; drift review. UI at `/mcp-servers`. Credentials are environment variable names only (`MCP_TOKEN_*`) and never returned. Missing: an app catalog or one-click connectors, OAuth (static bearer over Streamable HTTP only), and a secret store beyond the environment. |
| HTTP endpoint tools | Present | `/api/v1/http-tools` ([api/http-tools.md](api/http-tools.md)): method, URL with path placeholders, static headers, one secret header, input schema, timeout, response cap and response pick; immutable definitions pinned by agent versions; test calls; an HTTP tools page and a section in the agent's tool picker. Missing: background (async) calls, several credentials per tool, OAuth. |
| Knowledge service | Planned | No retrieval or embedding code. |
| Skills integration | Planned | No code. The term still needs a definition (see Phase 2). |
| Diarization | Planned | Only generated Riva protobuf fields. No configuration, transcript field or UI. |
| Custom vocabulary (dictionary) | Planned | Nothing beyond ElevenLabs voice settings. |
| Structured output | Planned | No `response_format` or schema use in providers or `server/`. |
| Image input | Planned | No image frame or client path. |
| Session resume | Planned | Tokens are single-use and tied to one process; context is not carried over and the client starts a new call (`docs/operations.md`). The limit is already documented. |
| Voice harness | Partial | `tools/livecheck` with `live-check`, `agent-matrix` and `live-critical` (paid, capped by `MAX_CALLS`, run by `.github/workflows/live.yml`); free in-process `live-check-selftest`, `eval-pipecat` and `eval-turn`; the console voice and text test; the echo provider. |
| Agent evals | In progress | `feat/phase-11` has the case loader, provider rewrite (silence speech, offline or echo model), scorer, a shared RTVI text client and a refactored `livecheck`. The runner, report and self-test are uncommitted there. Missing: saved cases, a make target and CI job, run history, any UI or API, and a publish gate. The committed entry point refers to `docs/operations/agent-evals.md`, which does not exist yet. |
| Operator live-call control | Present | `POST /sessions/{id}/context` and `/run`, `LiveCallControls`. Not in the original roadmap. |
| Dashboard and call inspection | Partial | Dashboard with live calls, capacity, turns, errors, recent calls, readiness and the reply-time chart (axis, slow and typical lines, keyboard tooltip, table view; `main`). Calls list and detail with transcript, waterfall and timeline. The list filters on `status` and `agent_id` only. |
| Cost, summaries, extraction | Planned | `core.UsageMetricsFrame` is not consumed by `server/`. `safe_summary` is a status string, not a generated summary. |
| Alerts, exports | Planned | Prometheus rules are the only alerting. There is no webhook, notification or export feature. |
| Secrets for tools and MCP servers | Present | Named secrets resolved at call time from `MCP_TOKEN_*` and `HTTP_TOKEN_*` variables, a secrets directory, or an AES-256-GCM encrypted store in the database ([api/secrets.md](api/secrets.md), [security/credentials.md](security/credentials.md)); a Secrets page; keyring rotation by restart. A persistent audit trail records every create, replace and delete of a stored secret (name and request, never a value). Missing: per-tenant scoping, and retention for the audit table. |
| Authentication and tenancy | Partial | One shared management key (at least 32 bytes), magic-link admin sign-in with `public_admin`, and a first-run onboarding wizard. No users, roles or tenants. |
| Deployment profiles | Present | Cloud Run (Cloud SQL Postgres, or a SQLite demo on `/tmp`) and a Cloudflare container demo (one Durable Object container, two sessions, ephemeral SQLite). Both are demo-grade for storage: data and recordings are lost on restart; no multi-instance resume. |

Also present and not in the original roadmap: STT, TTS and model failover with circuit breakers; the offline speech stack; xAI, NVIDIA, Deepgram, Cartesia and ElevenLabs providers; handoff cues, background audio and DTMF; smart turn detection and idle handling; context summarisation; admission control, rate limits and drain semantics; Postgres with forward-only migrations and a metadata versus content retention job; and the Pipecat parity matrix with turn-quality and performance gates.

## Phase 1 — Agent lifecycle polish

**Status:** the version list, restore as a new version, and unarchive are built (see the baseline row). Decided: every save stays a publish (draft test calls cover trying a change first), and there is no agent delete, because versions are immutable and calls refer to them; archive retires an agent and keeps its history. Still open: the "what does this affect" summary for archive and for an MCP detach, a record on a restored version of which version it copied, and the text-only startup point below.

**Description:** The authoring foundation is built. This phase closes the lifecycle gaps that remain, so publishing and rollback are first-class instead of side effects of saving.

**Outcome:** Users can see an agent's version history, roll back deliberately, and retire or restore an agent without orphaning anything.

**Decide before implementation**

- Is a draft a server entity (saved but not live) or does the current "every save publishes" model stay? Draft test calls already cover most of the need.
- What does delete mean for an agent with call history: refuse, or delete after the retention period?
- Should an archive or an MCP detach list the agents it touches before it runs?

**Scope**

- A versions list endpoint and a history view; rollback as an explicit action that publishes a copy of the chosen version, with the audit record saying so.
- Unarchive; delete under the rule decided above.
- A "what does this affect" summary for archive and for MCP detach.
- Make the text-only path skip STT and TTS startup, or reword the documentation so it does not claim it.

**Acceptance criteria**

- A user can list versions, view any of them, and roll back to one in the UI and API.
- An archived agent can be restored; a deleted one leaves call history intact and readable.
- Active calls stay pinned to their starting version through every action above.

## Phase 2 — Knowledge, tools, and skills integrations

**Status:** HTTP tools and cloud-agnostic secrets are built (design record: [decisions/http-tools.md](decisions/http-tools.md)). Knowledge and skills remain.

**Description:** Give agents access to business systems and knowledge while keeping setup manageable for demo users and safe for operators. MCP and HTTP tools work; decide the knowledge and skills approach.

**Outcome:** An agent can retrieve business knowledge and call external services using consistent, discoverable configuration.

**Decide before implementation**

- Should HTTP tools share one tool list and permission model with MCP tools, or have their own?
- Which HTTP auth methods does the first version need (bearer from an environment variable only, or also API-key headers and basic auth)?
- Should the knowledge capability be an MCP server, an internal service, or both? Who owns its data and lifecycle?
- What does "skills" mean here: reusable prompt or workflow packages, an external skills protocol, or a specific platform's skills?
- Does "MCP apps" mean a catalog, one-click hosted connectors, or only the existing UI for user-provided servers?

**Scope**

- First-class HTTP tools: method, URL, headers, an auth reference by environment variable name, a request schema, a timeout, a response size cap and response mapping. They appear in the tool picker and are pinned in the agent version like MCP tools.
- MCP gaps: OAuth, an optional catalog.
- A knowledge service (ingest, index, search, update, remove) with provenance in results.
- A definition of skills and how they attach to a step.
- Retrieval and tool-call details in the call timeline (tool name, status and duration are stored today; arguments and results are deliberately not).

**Acceptance criteria**

- An author can configure, test and validate an HTTP tool without writing an MCP server.
- Secrets used by HTTP and MCP connections come from the deployment's secret mechanism and are never returned by the API.
- Tools, retrieval and requests have bounded timeouts, response sizes and concurrency, with actionable failures in traces.
- Knowledge results carry provenance the agent can cite.
- Whatever is selected is pinned by the published agent version.

## Phase 3 — Session continuity and multimodal contracts

**Description:** Extend the client and server call contract beyond one uninterrupted session, then add image and structured-output capabilities with explicit provider limits. All four are Planned.

**Outcome:** Calls can recover from short client disconnects and accept structured or image-bearing input through explicit contracts.

**Decide before implementation**

- Does resume cover only a brief browser or network reconnect, or also recovery after a restart? The documented limit today is "a new call".
- Which state must survive (transcript and context, active step, pending tool call, buffered audio) and for how long?
- Is image input for live voice calls, text turns or both, and which clients must carry it? What is the retention policy?
- Should schema output constrain the user-facing reply, return a separate payload, or both? Which schema standard, and what happens when a provider cannot enforce it?

**Scope**

- Resume: reconnect credential, session ownership, replay boundary, duplicate protection, expiry, and behaviour after a restart.
- Image input: transport representation, size and type limits, model capability check, storage and logging policy, fallback.
- Schema output: validation, retry or failure behaviour, and how results reach the client and call history.
- Provider capability discovery so the UI can tell supported, unsupported and provider-dependent features apart.

**Acceptance criteria**

- A client reconnects to an eligible live session with an expiring scoped credential without creating a second call.
- Missed events, stale or duplicate messages are handled and documented; any restart limit is reported clearly.
- Image requests enforce type and byte limits, do not log payloads, and fail clearly when the model cannot take images.
- Schema-constrained replies are validated, with deterministic behaviour for invalid output.
- Existing RTVI clients that do not use these features keep working.

## Phase 4 — Recording, privacy, and durable call data

**Description:** Make call artifacts safe and durable enough for hosted use. Today recordings are local, unencrypted files with no way to reach them through the product.

**Outcome:** Call artifacts have explicit retention, access and privacy controls suitable for hosted demos and production.

**Decide before implementation**

- Which storage backends are launch requirements (R2, S3-compatible, local disk)?
- Is recording enabled globally, per agent, or only with call-level consent? Must the system verify that the disclosure played?
- Which PII classes are in scope, and does scrubbing happen before storage, at read time, or both? Per agent or server-wide?
- Which retention, legal hold, export and deletion guarantees apply to recordings? The policy itself is Phase 8; recordings follow it.

**Scope**

- An object-storage sink behind the existing `Sink` interface, for R2 and S3-compatible stores.
- Encryption at rest where the store does not provide it; signed, expiring playback and download URLs; an API and UI to list, play and delete recordings; an access audit.
- Close the PII gaps listed in `docs/privacy.md`, and add a per-agent policy and an export path that applies it.
- Recording disclosure that is played and recorded as played, not only acknowledged in configuration.
- Document durability for the SQLite demo profiles versus production storage.
- **Deferred, planned:** opt-in capture of tool arguments and results, off by default and set per agent. Today the recorder (`turnRecorder`, `server/call.go`) stores only a tool call's name, status and timing; `tool_calls.arguments_summary` and `result_summary` exist in the schema but are written empty, and traces and OTel drop `tool.arguments` and `tool.result` by policy (`docs/privacy.md`). Capture would fill those columns through `core/redact` with a size cap, extend the retention purge to them, add the fields to the API and the trace viewer, and relax the privacy statements and tests deliberately. Evaluation scorers are written against optional `Arguments` and `Result` fields on the tool-call input so tool-call accuracy and live correctness gain argument-level checks when capture lands, with no scorer changes.

**Acceptance criteria**

- Recording can be enabled globally and per agent, stored durably, and cleaned up by retention.
- Only authorised users reach recordings; URLs expire and access is auditable.
- The PII policy states what is scrubbed, where, and what remains by design, with a test for each sink.
- Disclosure cannot claim to have played when it did not.
- Restart, deletion and retention behaviour is documented for each storage profile.

## Phase 5 — Voice quality, languages, and conversation harness

**Description:** Improve how builders configure and diagnose speech behaviour. A harness (`livecheck`, the console voice and text test, the echo provider) exists; this phase makes issues repeatable and provider differences visible.

**Outcome:** Builders can tune speech behaviour and reproduce conversation problems across providers and languages.

**Decide before implementation**

- Is the harness developer-only, an admin UI feature, or both?
- Should scenarios use microphone input, uploaded audio, scripted text, or all three?
- Does the dictionary configure recognition hints, pronunciation, model terminology, or a shared vocabulary mapped per provider?
- Is diarization needed in live calls, post-call transcripts, or both, and for which provider first?
- Is language support defined by code, locale, or tested provider combinations? Should an unsupported combination block publish or warn?

**Scope**

- Repeatable scenarios against a chosen agent version and provider set, with transcript, timestamps, correlation IDs and an issue export that carries no secrets. The text-only path is part of it.
- Custom vocabulary, versioned with the agent, reporting providers that cannot use it.
- Diarization as an optional normalised capability; no invented labels for providers that lack it.
- A language field on TTS, a verified compatibility matrix for STT, model and TTS choices, and a publish-time warning or block.

**Acceptance criteria**

- A developer can repeat a documented scenario against a chosen agent version and provider configuration.
- Dictionary entries and speaker labels are normalised, stored and exported where supported.
- The UI shows the language matrix for the selected providers and warns or refuses invalid combinations.

## Phase 6 — Evals, simulations, and release quality gates

**Description:** Turn the harness and agent versions into a quality loop. The building blocks are on `feat/phase-11`; this phase finishes and productises them.

**Outcome:** Agent changes can be checked against saved scenarios before publication and regression-tested over time.

**Decide before implementation**

- What is simulated first: model decisions and tool calls, complete voice turns, or full audio?
- Which checks are deterministic and which model-graded, and who pays for grading?
- Do eval suites belong to the workspace, an agent, or both, and how are they versioned?
- Do failed evals block publishing by default, or is the gate opt-in?
- How does a real-call failure become a sanitised fixture?

**Scope**

- Finish the eval runner and report on `feat/phase-11`, write `docs/operations/agent-evals.md`, commit saved cases, and add a make target and a CI job (free, deterministic echo or silence providers).
- Persist run history by agent version, with criterion-level results and the provider configuration used.
- A configurable publish gate with an audited override.
- Convert a real call into a sanitised scenario with an explicit redaction step.
- Scorers (correctness, tool-call accuracy, answer relevancy, toxicity) share one interface for suites and for sampled live calls. Until the tool capture in Phase 4 exists, tool-call accuracy judges tool names, order and outcomes only, and live correctness sees the transcript, the step path and tool names.

**Acceptance criteria**

- Eval cases run against an immutable agent version and report pass or fail per criterion, with transcript, tool activity and configuration.
- Suites run in CI with machine-readable results and deterministic mock coverage.
- A configured gate blocks publish on a failing required suite, and the override is recorded.
- Call content enters a fixture only through an explicit sanitisation step.

## Phase 7 — Admin operations, analytics, and cost

**Description:** Bring what operators need into one review and reporting experience. A call page with transcript, waterfall and stored spans already exists; cost, outcomes, trace exploration and alerts do not.

**Outcome:** Operators can see what happened on a call, how agents perform, and what they cost, from one admin experience.

**Decide before implementation**

- Which roles need access to content, recordings, costs and diagnostics? Is multi-user access in scope? Today there is one shared key.
- Which outcomes matter first (completed, escalated, failed, booked, qualified)? Are summaries generated for every call or per agent?
- Which rate source is authoritative, and how are unknown or negotiated rates shown? What cost detail is required (call, stage, turn)?
- Should the trace UI query an existing OTel backend or include its own storage? Which backend first? Only Switchboard spans, or correlated downstream spans too?
- Which alert destinations first, and per workspace or per agent?

**Scope**

- Store the OTel trace id on the session so a call links to its trace; then a trace explorer (span tree, attributes, events, timing) or a link-out, as decided above, with explicit "unavailable" states for sampling, expiry and connectivity.
- Consume `core.UsageMetricsFrame` for per-call and aggregated cost with estimate and unknown flags.
- Post-call summaries and structured extraction stored apart from the transcript and labelled with the agent version and schema.
- Dashboards and filters for quality, completion, errors, latency, usage and cost, with export; richer call-list filters.
- Alert hooks for failures and thresholds, bounded, retry-safe and never blocking a live call.
- Keep Prometheus and OTel for external observability.

**Acceptance criteria**

- An operator finds a call and sees transcript, step path, timestamps, tool events, any recording and the failure reason in one place, and can move between the call and its trace.
- Cost records attribute usage to call, agent version, stage and provider or model, and mark estimates and unknown rates.
- Aggregates reconcile with stored records within documented tolerances; exports respect access controls and the PII policy.

## Phase 8 — Data retention policy and lifecycle

**Status:** two slices are built: validation that names both values, a dry run, a status endpoint with the last run, metrics and a stalled-job alert; then holds and erasure of one call or one caller, with an audit history, recordings named by call so a hold and an erasure reach them, and the console's call page (see the baseline row and [decisions/retention-holds-erasure.md](decisions/retention-holds-erasure.md)). Still to do: the per-class policy model and per-agent overrides, an export before erasure, later data classes under the same report, the dashboard view of the policy, and production profiles requiring an explicit choice.

**Description:** Turn today's single purge job into an explicit, configurable and verifiable retention policy for every kind of data the product keeps. The job and its two lifetimes exist; what is missing is a policy that names each data class, can be set per agent where that is safe, can be paused for a legal hold, can erase one call or one caller on request, and proves what it did. Later phases add data (summaries, extracted outcomes, cost records, eval fixtures, recordings in object storage, an audit trail); each of them needs a retention rule from the start, so this phase defines the model and the configuration before they land.

**Outcome:** An operator can state, read and verify how long each kind of data is kept, change it without editing code, hold or erase specific data, and see that the purge ran.

**Decide before implementation**

- **Data classes.** Which classes get their own lifetime: transcripts and assistant text, post-call summaries and extracted outcomes, recordings and their metadata, session and turn metadata, spans and events, tool-call rows, the `caller` label, eval run history and fixtures, cost and usage records, the audit trail, draft test calls? Which are kept for as long as the agent version they belong to (agent versions, HTTP tool definitions, MCP discoveries)?
- **Granularity.** Server-wide only, or also per agent? If an agent may override, may it only shorten the server's lifetime (the safe default), or also lengthen it? What is the rule when a call crosses agents?
- **Defaults.** Purging is off unless enabled. Should a production profile refuse to start without an explicit retention choice? What do the demo profiles keep (the Cloud Run demos keep content for a day), and are draft test calls kept shorter than real calls?
- **Holds and requests.** Does a hold apply to one call, one caller or one agent? Who may place and lift one, and is that recorded? Is erasing a call a hard delete or a content wipe that keeps the metadata row? Is there an export before deletion, and in what format?
- **What the product cannot purge.** Backups and database snapshots, log files, an external OpenTelemetry backend and provider-side retention outlive the job. How is that stated, and can content capture to those sinks be refused when the policy is short?
- **Changes.** Is the policy read at start-up only, or editable through the management API and dashboard with an effective date? Does shortening a lifetime purge what is already older, and does lengthening one restore anything (it cannot)?

**Scope**

- **Policy model.** A table of data classes, each with a lifetime and the rule that applies to it, validated at start-up: content is never kept longer than metadata, recordings never longer than content, a derived record (a summary) never longer than the transcript it came from unless the policy says so. One document, [privacy.md](privacy.md), lists every class and where it is stored.
- **Configuration.** The `retention` section grows per-class lifetimes, with the existing `content_retention_days` and `metadata_retention_days` kept as the defaults they are today. Per-agent overrides (shorter only, to begin with) live in the agent version, so the policy a call ran under is pinned like everything else. Production profiles require an explicit choice.
- **Holds and erasure.** A hold flag on a call or a caller that the purge skips; `DELETE` of one call and of everything a caller label produced, with an export first when asked; both recorded in the audit trail without the content.
- **The job.** A dry run that reports counts without deleting; counts of what each run purged by class, the age of the oldest remaining row and the time of the last run, as metrics and in the API; an alert rule for a stalled or failing job; recordings and object storage brought under the same schedule and report.
- **Dashboard.** A Retention view showing the policy in effect, per class and per agent, the last and next run, what it removed, and any holds; edits where the model allows them.
- **Documentation.** A table of what is stored, where, for how long and who can read it, the list of what the job cannot reach, and the backup guidance for each storage profile.

**Acceptance criteria**

- Every data class the product stores has a documented lifetime and a test that shows it is purged on schedule, including recordings and any new derived record.
- A configuration that keeps content longer than metadata, or recordings longer than content, is refused at start-up with a message naming both values.
- An agent that sets a shorter lifetime has its calls purged on that schedule, and a call's lifetime is fixed by the agent version it ran under.
- A held call or caller survives a purge that would otherwise remove it; lifting the hold lets the next run remove it. Both actions are in the audit trail.
- Erasing a call or a caller removes its content, recording and derived records, leaves no row that identifies the caller beyond the audit entry, and is reported in the response by class.
- A dry run reports exactly what the next real run would remove, and the run's counts match the rows actually gone.
- A job that has not completed within a configured multiple of its interval raises the documented alert.
- The privacy documentation states what retention cannot reach (backups, logs, external trace backends, provider retention) for each deployment profile.

## Cross-cutting requirements

- Keep `core/` independent of server, storage and UI concerns; do not add a `core/` capability the server does not use on a supported call path.
- Keep queues, retained media, transcript size and background jobs bounded; propagate cancellation and close owned resources.
- Keep provider and connection credentials in the environment or deployment secret mechanism; never persist them in agent versions or return them from APIs.
- Treat tenancy, authorisation, retention and redaction as requirements of every new data surface.
- Do not promise cross-instance session resume until shared state and request distribution exist and are verified.
- Keep the Cloud Run SQLite and Cloudflare setups labelled as demo profiles: durable recordings, resume across restarts and multi-instance use need shared persistence.

## Suggested delivery order

1. **Phase 2:** HTTP tools and secrets are done; knowledge and skills follow.
2. **Phase 6:** finish the eval work already under way on `feat/phase-11` (merge or rebase it onto `main` first).
3. **Phase 1:** small lifecycle polish; can run alongside either.
4. **Phase 8 (the policy model and configuration first)**, then **Phase 7 (cost and trace link first)** and **Phase 4:** all three add data, so define how long each class is kept (Phase 8) before storing it; Phase 4 and Phase 7 also want a decision on storage and access control, so settle the tenancy and storage contracts before building either.
5. **Phase 3** after the agent model and recordings are stable.
6. **Phase 5** continuously, as the provider matrix grows.

Phases can overlap where dependencies permit. No phase is complete until its acceptance criteria are demonstrated on the supported deployment profile and documented.
