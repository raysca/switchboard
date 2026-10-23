# Switchboard next implementation phases

## Purpose

This document groups the next product and runtime capabilities into sequenced epics. It is a planning baseline, not a commitment to a release date. Telephony integrations are excluded; the focus is the web voice experience, agent authoring, operations, and deployment.

The roadmap builds on Switchboard's existing Go voice runtime, stored/versioned agents, multi-step handoffs, MCP tool support, call history, local recording, PII redaction, per-session diagnostic timeline, Prometheus metrics, and OpenTelemetry. The aim is to turn those runtime capabilities into a coherent platform experience while adding the missing workflows.

### Status key

- **Present:** implemented and usable in the current product.
- **Partial:** a useful base exists, but the target experience or coverage is incomplete.
- **Planned:** no complete first-class implementation is currently identified.

## Current baseline and terminology

| Capability | Current status | Planning note |
|---|---|---|
| Stored agent versions and multi-step handoffs | Present | Calls pin the agent version they start with. |
| Flow as the primary authoring model | Partial | Multi-step agents already form flows, but the product model and naming still center on agents. |
| Orphaned agents | Partial | Define and implement lifecycle behavior as part of the flow migration. |
| Text-only agent (no STT/TTS voice pipeline) | To verify | The original note says “done”; retain as completed only after confirming a supported configuration and user path. |
| Call recording | Partial | Local WAV sink, retention, caps, and agent opt-in exist; storage is local and unencrypted at rest. |
| PII scrubbing | Partial | Heuristic text redaction exists for selected sinks and patterns; coverage and configuration are incomplete. |
| Admin timeline and observability | Partial | Diagnostic timeline, Prometheus, and optional OTel export exist; there is no proper UI for exploring OTel traces. |
| Language configuration | Partial | Language options are provider/model dependent; consistent language support and validation need definition. |
| External MCP servers and tools | Present | MCP is a usable extension point; an app catalog/connection UX and built-in knowledge service are separate work. |
| HTTP endpoint tools | To verify | Decide whether this means first-class configurable HTTP tools, in addition to HTTP-based MCP transports. |
| Diarization | Partial | Provider-level building blocks exist; agent configuration, normalized transcript data, and UI support need verification. |
| Structured/schema output | To verify | Confirm provider and client support before scoping the common contract. |
| Image input | Planned | No first-class multimodal image path has been identified. |
| Session resume | Planned | Define reconnect semantics, persistence needs, and behavior across server/container restarts. |

## Phase 1 — Flows and agent authoring foundation

**Description:** Establish the product’s central concept and vocabulary before adding more builder features. This phase makes a flow the versioned, publishable unit and gives existing agents a clear place in that model.

**Outcome:** Users build, publish, test, and operate flows as the primary unit. Existing agents migrate cleanly into this model.

**Decide before implementation**

- Is a flow simply the renamed agent graph, or can a flow compose reusable agents/subflows?
- Does “agent” remain a runtime entity, become a flow step type, or remain only as a compatibility/API term?
- What is the intended behavior for an orphaned agent: archive, delete, detach, or migrate automatically?
- Does “live implementation” mean the end-to-end web demo described here, or a different live-call workflow?
- Is text-only/no-voice-pipeline mode already complete, and what UI/API path demonstrates it?

**Scope**

- Refactor agent authoring and runtime definitions around flows; retain “agent” as a step/component where useful.
- Establish flow identity, versioning, publish/rollback behavior, validation, and ownership.
- Define orphaned-agent handling: migration, deletion, references, and compatibility for existing calls and stored versions.
- Keep call-level pinning so edits never mutate an active call.
- Define the “live implementation” milestone as a working end-to-end flow that can be configured, published, launched in the web client, and inspected during/after a call. If “live implementation” means a different feature, update this scope before implementation.
- Confirm and document the already-completed text-only/no-voice-pipeline path, or schedule it here if it is not complete.

**Acceptance criteria**

- A user can create, validate, publish, run, and roll back a multi-step flow in the UI and API.
- Existing stored agents and versions have a documented, non-destructive migration path.
- Orphaned references are detected and surfaced with a safe repair or deletion path.
- A published flow version is immutable; active calls remain pinned to their starting version.
- The end-to-end live demo can be launched and inspected without editing database records manually.
- Text-only mode, if marked done, can run without initializing STT or TTS and has documented API/UI behavior.

## Phase 2 — Knowledge, tools, and skills integrations

**Description:** Give flows useful access to business systems and knowledge while keeping setup manageable for demo users and safe for operators. Reuse MCP where it fits, and add direct HTTP configuration only where it meaningfully improves usability.

**Outcome:** A flow can securely retrieve business knowledge and invoke external capabilities using consistent, discoverable configuration.

**Decide before implementation**

- Should the built-in knowledge capability be an MCP server, an internal service/API, or both? Who owns its lifecycle and data?
- Which document formats, size limits, update/delete semantics, and retrieval modes are needed for the first version?
- Does “MCP apps” mean an in-product directory/catalog, one-click hosted connectors, or a UI for configuring user-provided MCP servers?
- What does “Skills integration” refer to in this product: reusable prompt/workflow packages, an external skills protocol, or a specific platform’s skills?
- Which HTTP auth methods are required initially, and where will per-connection credentials live?
- Should HTTP tools and MCP tools share one authoring and permission model?

**Scope**

- Provide first-class HTTP endpoint tools with method, URL, headers/auth reference, request schema, timeout, and response mapping.
- Improve MCP app setup and lifecycle: discover/connect servers, inspect tools, handle credentials, and show connection health.
- Add a built-in knowledge service or knowledge MCP for ingesting, indexing, searching, and updating documents.
- Define Skills integration: how skills are discovered, attached to a flow/step, versioned, and invoked. Keep the integration provider-neutral where practical.
- Add retrieval and tool-call traces to the call timeline for debugging.

**Acceptance criteria**

- A flow author can configure and validate an HTTP tool without writing a custom MCP server.
- Secrets used by HTTP/MCP connections are stored through the deployment's secret mechanism and never returned by management APIs.
- MCP app setup reports reachable/unreachable state and exposes only tools explicitly selected for the flow step.
- Knowledge sources can be added, updated, removed, and queried; results include provenance that the agent can cite or expose.
- Knowledge retrieval and tool execution have bounded timeouts, response sizes, and concurrency, with actionable failures in traces.
- Skills can be attached to a flow version reproducibly, and publishing pins the selected skill version/configuration.

## Phase 3 — Session continuity and multimodal contracts

**Description:** Extend the client/server call contract beyond a single uninterrupted audio session. Resolve what “resume” promises, then add image and structured-output capabilities with explicit provider limits and predictable client behavior.

**Outcome:** Calls can recover from transient client disconnects and support structured or image-bearing user input through explicit contracts.

**Decide before implementation**

- Does session resume cover only a brief browser/network reconnect, or recovery after server/container restart as well?
- Which state must survive: transcript/context, active flow step, pending tool call, and/or buffered audio? How long is a resumable session retained?
- Is image input needed during a live voice call, in a text-only turn, or both? What is the expected image retention/deletion policy?
- Which clients must carry images: the current Pipecat-compatible client, a Switchboard-specific web client, or both?
- Should schema output constrain the assistant’s user-facing response, return a separate structured payload, or support both modes?
- What schema standard and validation behavior should be supported, and what happens when a provider cannot enforce it natively?

**Scope**

- Session resume: reconnect authentication, session ownership, replay boundary, duplicate-frame protection, expiry, and behavior when the server process has restarted.
- Image input: client transport representation, size/type limits, storage policy, model capability negotiation, and fallback behavior.
- Schema output: JSON Schema (or a deliberately smaller contract), validation, retry/failure behavior, and how structured results reach the client and call history.
- Normalize provider capability discovery so the UI can distinguish supported, unsupported, and provider-dependent features.

**Acceptance criteria**

- A client can reconnect to an eligible live session using an expiring, scoped credential without creating a second call.
- Resume behavior defines what happens to missed events/audio and rejects stale or duplicate client messages safely.
- Resume works across the documented deployment boundary; if process/container restart recovery is not supported, the UI/API reports that limit clearly.
- Image requests enforce content type and byte limits, redact or avoid logging image payloads, and fail clearly when a selected provider cannot accept images.
- Schema-constrained responses are validated against the configured schema and have deterministic behavior for invalid output.
- Existing RTVI clients remain compatible when they do not use these new capabilities.

## Phase 4 — Recording, privacy, and durable call data

**Description:** Make call artifacts safe and durable enough for hosted use. This phase addresses storage and lifecycle guarantees for recordings and sensitive data, which become more important as resume, images, and analytics are added.

**Outcome:** Call artifacts have explicit retention, access, and privacy controls suitable for hosted demos and production deployments.

**Decide before implementation**

- Which storage backends are launch requirements: Cloudflare R2, S3-compatible storage, local disk, or more than one?
- Are recordings enabled by default, per workspace, per flow, or only by explicit call-level consent?
- What is the required disclosure behavior, and must the system verify that the disclosure was actually played?
- Which PII classes are in scope, and should scrubbing happen before persistence, at read/export time, or both?
- Should redaction be configurable per tenant/flow, and how are false positives and missed detections handled?
- What are the expected retention, legal hold, export, and deletion guarantees for calls and recordings?

**Scope**

- Replace or complement the local recording sink with a durable object-storage interface suitable for Cloudflare R2 and other S3-compatible stores.
- Encrypt recordings at rest where the storage service does not provide an adequate managed guarantee; define access controls and signed playback/download URLs.
- Complete PII scrubbing policy across transcripts, extracted fields, logs, traces, exports, recordings metadata, and tool payloads.
- Make recording disclosure and consent behavior explicit, beyond a configuration acknowledgement.
- Document storage durability expectations for SQLite demo deployments versus production database/object storage.

**Acceptance criteria**

- Recording can be enabled per flow and globally, and stored in configured durable storage with bounded size/duration and retention cleanup.
- Only authorized users can access recordings; URLs expire and access is auditable.
- PII policy identifies what is scrubbed, where, and which source data remains by design; tests cover each configured sink.
- Recording disclosure behavior is configurable and cannot silently claim that disclosure played when it did not.
- Restart/redeploy behavior and deletion/retention guarantees are documented for each supported storage profile.

## Phase 5 — Voice quality, languages, and conversation harness

**Description:** Improve the builder’s ability to configure and diagnose speech behavior across providers. The harness should make voice issues repeatable; language, dictionary, and diarization controls should reveal provider differences instead of hiding them.

**Outcome:** Builders can tune speech behavior and reproduce conversation issues across supported providers and languages.

**Decide before implementation**

- Is the harness a developer-only tool, an admin UI feature, or both? Does “live implementation” belong here instead of Phase 1?
- Should scenarios use microphone input, uploaded audio, scripted text, or all three?
- Does the dictionary configure STT recognition hints, TTS pronunciation, LLM terminology, or a shared vocabulary mapped to provider-specific controls?
- Is diarization needed during live calls, only in post-call transcripts, or both? Which provider is the first supported target?
- Which languages are launch priorities, and is support defined by language code, dialect/locale, or end-to-end tested provider combinations?
- Should unsupported language/provider combinations block publishing or show a warning?

**Scope**

- Refine the voice harness for repeatable browser calls, scenario setup, transcript/audio capture, provider selection, and issue export.
- Add custom vocabulary/dictionary support for pronunciation, names, acronyms, and domain terms, mapping to provider-specific features where available.
- Surface diarization as a normalized optional capability, including speaker labels in transcripts and supported recordings.
- Define a language support matrix for STT, LLM, and TTS combinations; make unsupported combinations visible before publish.
- Include the text-only path in the harness so voice and non-voice flows can be exercised consistently.

**Acceptance criteria**

- A developer can replay or repeat a documented scenario against a chosen flow version and provider configuration.
- Harness output includes transcript, timestamps where available, and correlation IDs without exposing secrets.
- Dictionary entries are versioned with the flow and clearly report providers that cannot consume them.
- Diarization speaker labels are normalized and survive storage/export when supported; unsupported providers do not fabricate labels.
- The UI shows a verified language compatibility matrix for selected STT/LLM/TTS choices and refuses or warns on invalid combinations.

## Phase 6 — Evals, simulations, and release quality gates

**Description:** Turn the harness and flow versioning into a quality loop. Start with reproducible text and tool behavior checks, then add audio/voice simulation where it can produce reliable evidence.

**Outcome:** Flow changes can be checked against saved scenarios before publication and regression-tested over time.

**Decide before implementation**

- What should be simulated first: model decisions and tool calls, complete voice turns, or full audio conversations?
- Which checks are deterministic assertions versus model-graded judgments, and which model/provider pays for grading?
- Are eval suites owned by a workspace, a flow, or both? How are they versioned with flow changes?
- Should failed evals block publishing by default, or should each workspace/flow opt into gates?
- How will real-call failures become sanitized regression fixtures without retaining unnecessary customer content?
- What CI interface is required: CLI, API, GitHub Actions example, or all three?

**Scope**

- Create saved eval cases and suites covering expected responses, tool arguments, structured output, handoffs, and safety/privacy expectations.
- Add text-level mock conversations first, then simulated voice conversations through the harness where practical.
- Store run history by flow version with failure details and reproducible configuration.
- Support local/CI execution and make publication gates configurable.
- Allow a real-call issue to be converted into a sanitized regression scenario.

**Acceptance criteria**

- Evals can be created, edited, grouped, and run against an immutable flow version.
- Runs report pass/fail, criterion-level results, transcript/tool activity, and the model/provider configuration used.
- Suites can run from CI with machine-readable results and deterministic mock-provider coverage.
- A configured release gate blocks publication when required evals fail, with an explicit override recorded in the audit trail.
- Sensitive call content is not copied into eval fixtures without an explicit redaction/sanitization step.

## Phase 7 — Admin operations, analytics, and cost

**Description:** Bring the information operators need into one call-review and reporting experience. Preserve external metrics/tracing integrations while adding product-level answers about outcomes, reliability, and spend.

**Outcome:** Operators can understand what happened on a call, how flows perform, and what they cost from one admin experience.

**Decide before implementation**

- Which roles need access to call content, recordings, costs, and diagnostic events? Is multi-user/team access in scope for these phases?
- Which call outcomes matter for the first dashboard (completed, escalated, failed, booked, qualified, etc.)?
- Are summaries and extracted outcomes generated automatically for every call or configured per flow?
- Which provider rate source is authoritative, and how should cached, negotiated, or unknown rates be represented?
- What cost detail is required: estimate per call, per stage, per turn, per tenant, or all of these?
- Which alert destinations are needed first, and should notifications be configurable per workspace or flow?
- Is the admin timeline for incident diagnosis, product analytics, or both? Which events should be persisted beyond the current in-memory window?
- Should the trace UI query an existing OpenTelemetry backend (for example, a configured collector/trace store), or should Switchboard include/manage trace storage? Which backend(s) must be supported first?
- Should the UI show only Switchboard spans or also correlated provider, MCP, and downstream service spans when they share trace context?
- What trace retention, sampling, access-control, and PII-redaction rules apply in the UI?

**Scope**

- Turn the existing diagnostic timeline into an admin call-inspection experience alongside transcript, recording, flow path, tool/MCP events, and extracted outcomes.
- Provide a proper OpenTelemetry trace UI: browse traces and spans, inspect attributes/events and timing, and navigate from a call to its related trace using shared identifiers. This is a trace-exploration product feature, not merely a timeline skin or an OTLP export toggle.
- Add post-call summaries and configurable structured extraction for outcomes relevant to the flow.
- Add per-call and aggregated cost calculations with provider/model rates, usage attribution, and clear handling for unknown or estimated prices.
- Build dashboards and filters for quality, completion, errors, latency, usage, and cost; allow export.
- Add alert hooks for operational failures and configured quality/cost thresholds.
- Preserve Prometheus and OpenTelemetry for operators who use external observability systems.

**Acceptance criteria**

- An operator can find a call and inspect its transcript, flow path, timestamps, tool events, available recording, and failure reason in one place.
- An authorized operator can open the call's related OpenTelemetry trace, inspect its span tree, timing, attributes, and events, and navigate between the trace and call record.
- The trace UI clearly reports when a trace is unavailable because of sampling, expiry, backend connectivity, or missing correlation data; it does not imply that an incomplete trace is complete.
- Summary/extraction outputs are stored separately from raw transcript and labeled with the flow version and extraction schema.
- Cost records attribute usage to call, flow version, stage, provider/model, and mark estimates/unknown rates explicitly.
- Dashboard aggregates reconcile with stored call/usage records within documented tolerances.
- Filters and exports respect tenant/role access controls and PII policy.
- Alert delivery is bounded, retry-safe, and does not block live calls.

## Cross-cutting requirements

- Keep the `core/` library independent of server, storage, and UI concerns; implement product workflows in `server/` and clients.
- Keep queues, retained media, transcript/context sizes, and background jobs bounded. Propagate cancellation and close owned resources.
- Keep provider credentials environment/deployment managed; never persist secrets in flow versions or expose them in APIs.
- Treat tenancy, authorization, retention, and redaction as requirements for every new data surface, not a later hardening task.
- Do not promise cross-instance session resume until shared state and routing are implemented and verified.
- Keep the Cloudflare SQLite/ephemeral-storage setup labeled as a demo profile. Durable recordings, resume across restarts, and multi-instance production use need suitable shared persistence.
- Do not add a capability to `core/` unless the server runtime consumes it on a supported call path.

## Suggested delivery order

1. **Phase 1: Flows and authoring foundation** — settle the domain model before adding more configuration surfaces.
2. **Phase 2: Knowledge, tools, and skills** — makes flows useful with real business data and actions.
3. **Phase 3: Session continuity and multimodal contracts** — add new runtime/client contracts after flow versioning is stable.
4. **Phase 4: Recording, privacy, and durable call data** — required before broad hosted use of richer session data.
5. **Phase 5: Voice quality, languages, and harness** — improve configuration and reproducibility across the provider matrix.
6. **Phase 6: Evals and simulations** — use the stable flow model and harness as eval targets; CI gates publication.
7. **Phase 7: Admin operations, analytics, and cost** — complete the operational feedback loop over durable call data.

Phases can overlap where dependencies permit. In particular, define the storage, tenancy, and privacy contracts during Phase 1 even if the full Phase 4 implementation lands later. No phase is complete until its acceptance criteria are demonstrated on the supported deployment profile and documented.
