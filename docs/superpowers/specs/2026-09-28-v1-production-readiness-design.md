# V1 Production Readiness Remediation Design

Date: 2026-09-28

Status: approved design, awaiting written-spec review

Baseline commit: `57463b23a2ec8d2f2bff37365eb5cc5092bbabb0`

## Purpose

Bring Voice Framework V1 to an evidence-backed production release for end-to-end calls over the pinned Pipecat WebSocket transport. The work fixes known correctness, security, lifecycle, release-engineering, deployment, and documentation blockers without adding WebRTC, other transports, or post-V1 xAI speech support.

Production readiness means more than a green unit-test suite. The shipped implementation, compatibility lock, configuration, deployment artifacts, operational documentation, and release evidence must agree. Requirements that need real credentials, browser audio, target Linux hardware, load generation, or network impairment must be run and recorded before the release is declared ready.

## Product Contract Decision

V1 uses WebSocket transport exclusively. The server guarantees:

- interruption advances the generation fence before any late provider result can mutate the next turn;
- queued and in-flight server audio for the interrupted generation is cancelled or the affected connection is terminated;
- the server-local flush acknowledgement reaches the assistant context within the 200 ms interruption SLO;
- no old-generation audio is accepted by transport after the barrier; and
- partial assistant context contains only complete synthesis units whose final audio write succeeded before interruption.

The pinned unmodified Pipecat WebSocket client does not expose a wire-triggered operation that clears audio already buffered in its local media manager. V1 therefore does not promise immediate removal of client-buffered playback. The specification, compatibility lock, ADR, probe, README, and release evidence must state this limitation consistently. Client playout clearing through WebRTC or another transport remains V2 work.

This is a formal product-contract revision, not a claim that the compatibility probe passed.

## Design Principles

1. Fix correctness at the owning boundary. Session generation rules remain in core; socket and pacing behavior remain in the WebSocket transport; provider readiness remains in the registry/server.
2. Make lifecycle transitions one-shot and explicit. A socket owns at most one voice session, `bot-ready` means that session is running, and teardown releases every reservation exactly once.
3. Keep the session run loop non-blocking. Potentially slow socket operations execute outside inline frame processing and re-enter through bounded control results.
4. Fail closed in production. Invalid Origin policy, missing configured credentials, incompatible contract values, unavailable capacity, and failed release checks stop admission or startup.
5. Prefer one source of truth. Manifest values are machine-checked against production configuration and code defaults; admission counters and metrics have one owner.
6. Preserve V1 scope. Experimental xAI STT/TTS stays build-tagged and non-release-gating. No WebRTC, SIP, persistent state, control plane, or unrelated refactor is introduced.
7. Evidence precedes claims. Documentation may describe only tests and deployments that were actually run.

## Remediation Architecture

### 1. WebSocket readiness and session ownership

The RTVI handler will reject or idempotently ignore a second `client-ready`; it must never call the allocation callback twice. The transport will expose an explicit readiness state transition guarded by a one-shot primitive.

For the first compatible `client-ready`, the transport will ask the server to reserve admission, construct the requested pipeline, attach processors, and start the session. Only after this succeeds will it send `bot-ready`. Failure will send a safe RTVI error when possible, close with a policy/service code, and release all reservations.

The connection will hold exactly one immutable session pointer after publication. Duplicate-ready, concurrent close, startup failure, and normal teardown tests will prove that no session or provider resource is orphaned.

### 2. Interruption and control-write path

Interruption handling will prioritize the server-local audio barrier over client notification. The transport will begin pacer flush immediately without waiting for `writeMu` or an informational RTVI write.

`FlushProcessedFrame` will carry the interrupted generation as payload but re-enter the session using a non-stale envelope generation. Assistant context will match the payload against its pending barrier. A failed or timed-out pacer flush terminates the affected connection explicitly.

Control-plane WebSocket messages will use a bounded serialized writer outside the session run loop. Completion errors will re-enter as fatal transport errors or close the connection. The design will not add a second unbounded queue: control output receives a small explicit byte/message budget and shutdown semantics.

### 3. Core startup and teardown

`Session.Start` will be transactional. If an attachment fails, already-attached processors are closed, terminal channels are completed, state becomes failed, and `Close` cannot wait forever on a run loop that never started. Starting or closing more than once returns deterministic results.

### 4. Authentication, Origin, and connection lifecycle

JWT parsing will use the single clock-skew value frozen in the compatibility lock for `exp`, `nbf`, and custom `iat`/restart checks. Production configuration and examples will use the same value.

Production defaults will not include wildcard origins or implicitly allow missing Origin headers. Missing Origin support will require an explicit, named non-browser override.

All locked WebSocket lifecycle values will be represented in server YAML: upgrade, RTVI-ready, read, idle, ping, write, close, encoded-message, decoded-audio, pending-handshake, active-session, per-source, and rate limits. A ping task will keep healthy muted clients alive and bound half-open detection.

### 5. Admission, readiness, and provider initialization

Pending-handshake accounting will have one owner in the transport and will report live gauge/rejection metrics through structured observation callbacks. Duplicate unused admission-controller handshake state will be removed.

Checks that can be performed before upgrade—source rate, socket capacity, token, Origin, pipeline authorization, and known global admission state—remain pre-upgrade. Principal/VAD/provider reservations that depend on the authenticated identity will be acquired before `bot-ready` and before provider work begins. Post-upgrade rejection will use an RTVI error and a service-appropriate close.

Every provider referenced by a configured production pipeline must initialize its required local credentials and client construction successfully. Missing or structurally invalid credentials fail startup with a provider-specific diagnostic. Transient remote outages feed circuit/readiness state rather than creating a restart loop.

Configured STT concurrency and rotation headroom will produce the effective session ceiling. Documentation and metrics will report that minimum rather than the nominal host session limit.

### 6. Contract, modules, and release engineering

The compatibility verifier will compare the lock with:

- Go and npm dependency versions and integrity;
- Pipecat gitlink commit;
- token claims and skew;
- production timeouts, sizes, rates, and admission limits;
- Google TTS encoding/rate behavior;
- ONNX Runtime and Silero coordinates/checksums; and
- Linux architecture/libc declarations.

The repository will generate a standard CycloneDX or SPDX SBOM with no developer-local paths. Release tools and scanner images will be version/digest pinned. CI will run module builds, vet/race tests, compatibility fixtures, contract validation, release-tag suites, image build/smoke, dependency scanning, container scanning, and SBOM validation.

The existing multi-module architecture will be retained because it is required by the V1 specification. Before publishing, placeholder module names and local release-time replacements will be changed to a user-supplied canonical module path and independently consumed from a clean external module. No canonical path will be invented.

### 7. Deployment and documentation

The quickstart will use the actual YAML schema, environment names, token claims, pinned client constructor, and connection API. A script/test will execute all offline steps and a credential-gated test will prove the real two-turn call.

Compose, Nginx, Docker, systemd, model/library installation paths, TLS mounts, read-only filesystem requirements, and shutdown grace periods will agree. Supervisor grace periods must exceed the application drain deadline plus forced-cleanup margin.

Capacity, SLO, security-scan, image-digest, and release-evidence documents will distinguish design limits, local tests, live-provider tests, target-Linux tests, and unverified criteria. Historical results will not be presented as current certification.

### 8. Observability

Metrics and bounded timelines will cover every V1-required transition used for operations and certification: startup/provider readiness, VAD capacity/faults, admission reasons, socket/control-write latency and failure, buffered audio, interruption/flush latency, provider retries/circuit transitions, STT rotation, context eviction, stale output, teardown, leaked work, and diagnostic shedding. No transcript or raw audio content will be exported.

## Test and Review Strategy

Each implementation task follows red-green-refactor and receives a scoped independent review before the next task begins.

Deterministic gates include:

- duplicate `client-ready` and one-session-per-socket integration tests;
- real WebSocket transport interruption/flush acknowledgement tests;
- stalled audio/control write and failed-write tests;
- transactional session-start failure tests;
- token leeway and strict production Origin tables;
- ping, half-open, idle, timeout, and locked-value contract tests;
- configured-provider startup failure and partial-outage readiness tests;
- admission saturation and observable-reason tests;
- clean-checkout module, manifest, SBOM, image, Compose, and systemd static/smoke tests;
- quickstart command/API regression tests; and
- metric/timeline event assertions without sensitive content.

The final independent reviewer receives the complete diff plus the authoritative V1 specification, implementation plan, compatibility lock, recorded WebSocket limitation, and pinned Pipecat sources. All Critical and Important findings must be fixed and re-reviewed.

## External Certification

The following cannot be certified by unit tests or code inspection:

- real Google STT/LLM/TTS and xAI LLM calls using release credentials;
- a 30-minute call crossing at least five configured STT rotations;
- browser/device/acoustic behavior with the pinned client;
- amd64 and arm64 target-Linux container/systemd deployment;
- worker-process native-fault isolation under concurrent real calls;
- latency, jitter, bandwidth, stall, half-open, and slow-client impairment tests;
- p95/p99 release SLOs and bounded memory under load; and
- end-to-end capacity on target hardware, reported as the minimum of all active limits.

These tests will be automated where practical and recorded as pending when infrastructure or credentials are absent. The release must not be called production-ready until mandatory external certification is present or the corresponding requirement is explicitly revised by the product owner.

## Success Criteria

V1 is ready only when:

1. Every code, security, lifecycle, deployment, documentation, and release-tool blocker in the implementation plan is complete with passing regression evidence.
2. The compatibility lock, specification, production configuration, shipped binaries, container and documentation agree.
3. A clean checkout reproduces builds, tests, fixtures, SBOM, scans, image and smoke checks.
4. The quickstart reaches a real two-turn WebSocket voice call.
5. Mandatory external scenarios and SLOs are recorded from the target environment.
6. An independent final review against the V1 specification and pinned Pipecat feature set reports no unresolved Critical or Important findings.

## Non-Goals

- WebRTC or any transport other than WebSocket.
- Immediate clearing of audio already buffered by the pinned unmodified WebSocket client.
- xAI STT/TTS production support.
- Distributed session migration, multi-host orchestration, control plane, billing, persistent conversation storage, tools/RAG/MCP, or UI work.
