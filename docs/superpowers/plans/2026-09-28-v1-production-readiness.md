# V1 Production Readiness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce an evidence-backed V1 release that runs end-to-end voice calls over the pinned Pipecat WebSocket transport and has no unresolved Critical or Important production-readiness findings.

**Architecture:** Preserve the existing core/provider/server boundaries. Make socket readiness, session ownership, interruption barriers, provider startup, admission, and release evidence explicit and single-owner; keep slow network work outside the session run loop. Treat release configuration, deployment assets, compatibility data, and evidence as executable contracts rather than prose.

**Tech Stack:** Go 1.27.1, `github.com/coder/websocket` 1.8.15, JWT v5, Pipecat client 1.13.1, Pipecat WebSocket transport 1.7.2, Protobuf, Google provider SDKs, xAI Responses API, Docker, Nginx, systemd, GitHub Actions, CycloneDX, Trivy, Node.js.

**Spec:** `docs/superpowers/specs/2026-09-28-v1-production-readiness-design.md`

## Global Constraints

- V1 supports WebSocket transport only. WebRTC and all other transports remain V2.
- V1 supports Google STT/LLM/TTS and xAI LLM interchangeability; xAI STT/TTS remains build-tagged, experimental, and non-release-gating.
- The server-side interruption SLO is p99 at most 200 ms from `UserStartedSpeakingFrame` to server flush acknowledgement.
- No old-generation audio may be accepted after the interruption barrier.
- The pinned unmodified WebSocket client may finish already-buffered local playback; V1 must state this limitation and must not claim client-buffer clearing.
- A WebSocket owns at most one voice session, and `bot-ready` means that session is running.
- Production authentication requires HS256, `iss`, `aud`, `sub`, `iat`, `exp`, `jti`, and `kid`, with atomic single-use replay enforcement.
- Production Origin policy fails closed; missing Origin requires an explicit non-browser override.
- All queues, maps, messages, decoded fields, deadlines, sessions, tasks, provider operations, and diagnostic buffers remain bounded.
- Release tooling uses pinned versions or digests and emits no developer-local paths.
- Canonical Go module root is `github.com/raysca/go-voice`.
- No task may add a second voice execution model, unbounded queue, dynamic plugin system, WebRTC shim, or proprietary client-clear message.
- Every task uses red-green-refactor, a focused test command, a full affected-module race test, one commit, and an independent review gate.

## Review Focus

- Duplicate or concurrent `client-ready` input must never create more than one session; Task 2 adds the real-socket regression.
- A stalled control write must not delay server audio flush beyond 200 ms; Task 3 adds the contention regression.
- Startup failure at every processor index must terminate cleanly without leaking admission or waiting on an unstarted run loop; Task 1 adds the table test.
- A muted or half-open socket must follow ping/idle/close bounds without provider allocation or goroutine leaks; Task 5 adds lifecycle tests.
- Clean-clone release gates must detect contract drift, local paths, missing submodules, and mutable tooling; Tasks 8, 9, and 12 add isolated-checkout tests.

## Execution Order

Execute Tasks 1–7, then Task 12 (canonical module migration), Task 8 (executable contract), Task 10 (final deployment artifacts), Task 9 (SBOM/CI over those final artifacts), Task 11 (quickstart), and finally Tasks 13–14. Task 12 intentionally precedes every artifact that consumes command/import paths, while Task 9 intentionally follows Task 10 so release evidence is generated only from the finalized Dockerfile and deployment layout.

---

### Task 1: Transactional Core Session Startup

**Files:**
- Modify: `core/session.go:199-215,346-390`
- Test: `core/core_test.go`

**Interfaces:**
- Consumes: existing `Attacher`, `Closer`, `SessionState`, `Session.Start() error`, and `Session.Close() error`.
- Produces: `Session.Start()` that reaches `StateRunning` or a terminal failed state; `Close()` is safe before, during, or after `Start`.

- [ ] **Step 1: Write the failing startup-cleanup tests**

Add a test processor with configurable attachment failure and close counting:

```go
type attachFailureProcessor struct {
	fail  bool
	close atomic.Int32
}

func (p *attachFailureProcessor) ProcessFrame(context.Context, Frame, Direction, Emitter) error { return nil }
func (p *attachFailureProcessor) Attach(SessionController, int) error {
	if p.fail { return errors.New("attach failed") }
	return nil
}
func (p *attachFailureProcessor) Close() error { p.close.Add(1); return nil }
```

Add `TestSession_StartFailureIsTerminalAndCloseDoesNotBlock`, iterating failure indexes 0, 1, and 2. For each case, assert `Start` fails, `State()==StateFailed`, previously attached processors close exactly once, `Done()` closes, and a goroutine calling `Close()` returns within 100 ms. Add `TestSession_CloseBeforeStartDoesNotBlock` and `TestSession_StartOnlyOnce`.

- [ ] **Step 2: Run the focused tests and confirm red**

Run: `cd core && go test -count=1 -run 'TestSession_(StartFailureIsTerminalAndCloseDoesNotBlock|CloseBeforeStartDoesNotBlock|StartOnlyOnce)' ./...`

Expected: failure or timeout because attachment failure never closes `runLoopDone` and pre-start close waits forever.

- [ ] **Step 3: Implement transactional startup**

Add an explicit lifecycle mutex, `StateNew`, and single teardown owner:

```go
lifecycleMu    sync.Mutex
runLoopStarted bool
attached       []Processor
```

`NewSession` begins in `StateNew`. `Start` holds the lifecycle transition through attachment, moves to `StateStarting`, records successfully attached processors, then starts the run loop and moves to `StateRunning`. On failure it records the error, closes the recorded processors exactly once in reverse order, and completes teardown through the same internal teardown function used by `Close`. `Close` serializes with `Start`, checks `runLoopStarted` before waiting for `runLoopDone`, and owns closure of `stopped`. Add concurrent tests that block each attachment index while another goroutine calls `Close`; neither goroutine may race, double-close, or remain blocked.

- [ ] **Step 4: Verify core lifecycle**

Run: `cd core && go test -race -count=1 ./...`

Expected: PASS with no race reports and no teardown timeout.

- [ ] **Step 5: Commit and review**

```bash
git add core/session.go core/core_test.go
git commit -m "fix(core): make session startup transactional"
```

Independent review focus: terminal-channel ownership, double-close prevention, reverse cleanup order, and compatibility with normal `Start`/`Close`.

### Task 2: One-Shot RTVI Readiness and Truthful `bot-ready`

**Files:**
- Modify: `transport/rtvi/rtvi.go:94-122`
- Modify: `transport/ws/transport.go:92-484`
- Modify: `server/server.go:215-387`
- Test: `transport/rtvi/rtvi_test.go`
- Test: `transport/ws/rtvi_integration_test.go`
- Test: `server/scenarios_test.go`
- Create: `tools/compat/verify_rtvi_lifecycle.js`
- Create: `tools/compat/rtvi_lifecycle_test.go`

**Interfaces:**
- Consumes: Task 1 transactional `Session.Start`/`Close`.
- Produces: explicit `unseen → allocating → running/failed` readiness, one allocation per socket, and `bot-ready` only after the session is running.

- [ ] **Step 1: Write duplicate-ready and failed-allocation tests**

In RTVI tests, invoke two compatible messages concurrently and assert the callback count is one and the loser returns `ErrAlreadyReady`. Repeat while allocation is deliberately blocked and while connection close begins.

In WebSocket integration tests, count `OnSessionReady` invocations and send two encoded `client-ready` frames. Assert one session and either a policy close or an ignored duplicate, but never another allocation.

Add a callback returning `errors.New("quota exhausted")`; assert the client receives no `bot-ready`, receives an RTVI fatal error, and the socket closes with a policy/service status.

Create `tools/compat/verify_rtvi_lifecycle.js` using only the pinned, unmodified packages from `tools/compat/package-lock.json`. Add `TestPinnedRTVILifecycle` in Go to start the real WebSocket server on an ephemeral port with stubbed providers, mint a valid token, invoke the Node script with endpoint/token environment variables, enforce a bounded startup/test/cleanup deadline, and report child output on failure. Assert exactly one `bot-ready`, compatible decode, speaking/audio ordering, interruption notification, and terminal error/close behavior. This Go-owned harness becomes the cross-task compatibility regression rerun after Tasks 3, 5, and 8.

- [ ] **Step 2: Run tests and confirm red**

Run: `go test -count=1 -run 'TestRTVI_(DuplicateReady|ReadyAllocationFailure)|TestWS_DuplicateClientReady' ./transport/...`

Expected: duplicate callback count is two and allocation failure occurs after `bot-ready`.

- [ ] **Step 3: Implement the one-shot transition**

In `rtvi.Handler`, replace the readiness boolean with a terminal state machine:

```go
if !h.state.CompareAndSwap(rtviStateUnseen, rtviStateAllocating) {
	return nil, ErrAlreadyReady
}
if h.OnClientReady != nil {
	if err := h.OnClientReady(readyData.Version); err != nil {
		h.state.Store(rtviStateFailed)
		return nil, err
	}
}
h.state.Store(rtviStateRunning)
```

Never roll a failed socket back to `unseen`. In the transport callback, allocate and start first, publish `conn.session`, then send `bot-ready` and close `readyCh`. If sending `bot-ready` fails, close the newly running session. Map admission/provider failures to a safe RTVI error and appropriate close reason.

- [ ] **Step 4: Verify transport and server integration**

Run: `go test -race -count=1 ./transport/... && (cd server && go test -race -count=1 ./...) && go test -count=1 -run TestPinnedRTVILifecycle ./tools/compat/...`

Expected: PASS; the allocation counter remains one.

- [ ] **Step 5: Commit and review**

```bash
git add transport/rtvi/rtvi.go transport/rtvi/rtvi_test.go transport/ws/transport.go transport/ws/rtvi_integration_test.go server/server.go server/scenarios_test.go tools/compat/verify_rtvi_lifecycle.js tools/compat/rtvi_lifecycle_test.go
git commit -m "fix(ws): make client readiness one-shot"
```

Independent review focus: failure rollback, session publication races, admission release, and Pipecat’s expected single `bot-ready` lifecycle.

### Task 3: Non-Blocking Interruption Barrier and Bounded Control Writer

**Files:**
- Create: `transport/ws/control_writer.go`
- Create: `transport/ws/control_writer_test.go`
- Modify: `transport/ws/transport.go:92-810`
- Modify: `transport/ws/pacing.go:124-223`
- Modify: `core/assistant_context.go:386-484`
- Test: `transport/ws/rtvi_integration_test.go`
- Test: `core/context_test.go`

**Interfaces:**
- Produces: `Connection.Attach(core.SessionController, int) error`, storing the transport processor index; `ControlWriter.Enqueue(ctx context.Context, payload []byte, completion func(error)) error`, bounded by message count/bytes; flush acknowledgement payload generation remains old while envelope generation bypasses stale filtering.

- [ ] **Step 1: Add red tests for the real flush path**

Create a real `ws.Connection` integration test that interrupts generation 1 after generation becomes 2. Assert:

```go
if got := asst.FlushAcks.Load(); got != 1 { t.Fatalf("flush acks=%d", got) }
if got := asst.FlushTimeouts.Load(); got != 0 { t.Fatalf("flush timeouts=%d", got) }
```

Add a transport observer assertion that `flush_ack` increments only after core accepts `FlushProcessedFrame`. Add a write function that blocks longer than 200 ms and assert the pacer flush callback completes or closes the connection without the session run loop blocking on any RTVI control message. Add ordering assertions: `bot-started-speaking` precedes first audio, `bot-stopped-speaking` follows the last successful final audio write, fatal RTVI error is attempted before close, and transcript/bot-text writes preserve their frame order.

- [ ] **Step 2: Run tests and confirm red**

Run: `go test -count=1 -run 'TestRTVI_RealFlushAckCrossesGenerationFence|TestWS_InterruptionDoesNotWaitForControlWrite' ./transport/ws ./core`

Expected: flush acknowledgement is stale-discarded and interruption waits on the socket writer.

- [ ] **Step 3: Implement the barrier ordering**

Make `Connection` an `Attacher` and record its pipeline index. For `FrameInterruption`, update speaking state and forward immediately; do not synchronously write. For `FrameFlush`, call `BeginFlush` first. On completion:

```go
ack := &core.FlushProcessedFrame{Gen: ff.Gen}
if err := c.session.Reenter(ack, core.Upstream, c.index-1, 0); err != nil {
	c.Close()
	return
}
c.observe("flush_ack", 1)
_ = c.control.Enqueue(c.ctx, stoppedPayload, c.failControlWrite)
```

Implement one bounded control queue with default maximum 32 messages and 256 KiB. Migrate every control write in `ProcessFrame` and the pacer completion callback to it: user speech, interim/final transcript, bot text, bot speaking state, interruption notification, and error. It serializes control/audio writes under a single ordering policy, applies the configured write deadline, and reports every failure. Queue overflow and write failure close only the affected connection. Audio enqueuing may remain paced, but the ordering tests above are binding.

- [ ] **Step 4: Verify race, latency, and failure behavior**

Run: `go test -race -count=1 ./transport/ws ./core && go test -count=1 -run TestPinnedRTVILifecycle ./tools/compat/...`

Expected: PASS; measured test barrier stays below 200 ms and no goroutine remains after close.

- [ ] **Step 5: Commit and review**

```bash
git add transport/ws/control_writer.go transport/ws/control_writer_test.go transport/ws/transport.go transport/ws/pacing.go transport/ws/rtvi_integration_test.go core/assistant_context.go core/context_test.go
git commit -m "fix(ws): enforce asynchronous interruption barrier"
```

Independent review focus: ack ordering, queue bounds, session-run-loop blocking, socket-write serialization, and false-positive metrics.

### Task 4: JWT Leeway and Strict Production Origin Policy

**Files:**
- Modify: `transport/token/token.go:101-208`
- Modify: `transport/token/token_test.go`
- Modify: `server/config.go:151-355`
- Modify: `server/config_test.go`
- Modify: `server/server.go:174-195`
- Modify: `config/development.yaml`
- Modify: `config/production.yaml`
- Test: `transport/ws/ws_test.go`

**Interfaces:**
- Produces: exact 5-second JWT leeway and explicit `insecure_overrides.allow_missing_origin_in_production`.

- [ ] **Step 1: Add token-boundary and Origin validation tests**

Create tokens expired by 4 seconds and 6 seconds; assert the former is accepted and the latter rejected. Add `nbf` and future-`iat` boundary cases.

Table-test production configuration with origin lists `nil`, empty, `{"*"}`, and `{"https://app.example.com"}`. Wildcard always fails in production. An empty list succeeds only with the new missing-Origin override and then accepts only requests with no Origin; any present Origin still requires an explicit allowlist match.

- [ ] **Step 2: Run tests and confirm red**

Run: `go test -count=1 ./transport/token ./transport/ws && (cd server && go test -count=1 -run 'Test.*Origin|Test.*Production' ./...)`

Expected: the 4-second-expired token is rejected and wildcard production defaults validate.

- [ ] **Step 3: Apply one lock value everywhere**

Use `jwt.WithLeeway(v.cfg.ClockSkew)` in `ParseWithClaims`. Set the compatibility/config value to `5s`. Remove `"*"` from production defaults. Add:

```go
AllowMissingOriginInProduction bool `yaml:"allow_missing_origin_in_production"`
```

Production validation always rejects wildcard origins. It rejects an empty allowlist unless the missing-Origin override is true. The override affects only absent Origin headers; it never bypasses validation of a present browser Origin. Development retains permissive behavior.

- [ ] **Step 4: Verify auth and server configuration**

Run: `go test -race -count=1 ./transport/... && (cd server && go test -race -count=1 ./...)`

- [ ] **Step 5: Commit and review**

```bash
git add transport/token transport/ws/ws_test.go server/config.go server/config_test.go server/server.go config/development.yaml config/production.yaml
git commit -m "fix(security): align token leeway and origin policy"
```

### Task 5: Complete WebSocket Timeout, Ping, and Size Contract

**Files:**
- Modify: `server/config.go:59-71`
- Modify: `server/server.go:174-201`
- Modify: `transport/ws/transport.go:26-61,437-484`
- Modify: `transport/ws/wire.go`
- Modify: `config/development.yaml`
- Modify: `config/production.yaml`
- Test: `server/config_test.go`
- Test: `transport/ws/ws_test.go`

**Interfaces:**
- Produces YAML fields `read_timeout`, `idle_timeout`, `ping_interval`, `write_timeout`, `close_timeout`, `max_encoded_message_bytes`, and `max_decoded_audio_bytes` wired into transport configuration.

- [ ] **Step 1: Add locked-value and lifecycle tests**

First extend the lock with exact ping interval, close timeout, active/per-principal/per-source limits, handshake rate, control-queue budget, and encoded/decoded limits. Add a bidirectional production-config key-set test so a missing or unexpected locked field fails. Add a muted-client test that waits beyond both the nominal message-read bound and two ping intervals while Pong traffic keeps it alive, a half-open peer test that terminates within idle bounds, and encoded/decoded oversize rejection tests.

- [ ] **Step 2: Run tests and confirm red**

Run: `go test -count=1 -run 'TestWS_(PingKeepsMutedClientAlive|HalfOpenTimesOut|LockedSizes)|TestProductionConfigMatchesLock' ./transport/ws ./server`

Expected: no ping is sent and several YAML fields do not exist.

- [ ] **Step 3: Implement lifecycle configuration and ping task**

Add the exact fields to `Limits`, validate every positive duration/size, and wire them to `ws.ServerConfig`. Define semantics explicitly: `read_timeout` bounds assembly of one WebSocket message, `idle_timeout` bounds time since any successful application message or Ping/Pong activity, and successful Ping/Pong refreshes idle activity. Start a supervised connection goroutine:

```go
ticker := time.NewTicker(c.cfg.PingInterval)
defer ticker.Stop()
for {
	select {
	case <-ticker.C:
		ctx, cancel := context.WithTimeout(c.ctx, c.cfg.Pacer.WriteTimeout)
		err := c.conn.Ping(ctx)
		cancel()
		if err != nil { c.cancel(); return }
	case <-c.ctx.Done(): return
	}
}
```

Apply encoded limits before decode and decoded-audio limits before allocation.

- [ ] **Step 4: Verify transport lifecycle**

Run: `go test -race -count=1 ./transport/... && (cd server && go test -race -count=1 ./...) && go test -count=1 -run TestPinnedRTVILifecycle ./tools/compat/...`

- [ ] **Step 5: Commit and review**

```bash
git add server/config.go server/config_test.go server/server.go transport/ws config/development.yaml config/production.yaml
git commit -m "feat(ws): enforce locked connection lifecycle"
```

### Task 6: Single-Owner Admission, Provider Startup, and Effective Capacity

**Files:**
- Modify: `server/admission.go`
- Modify: `server/admission_test.go`
- Modify: `server/registry.go:266-355`
- Modify: `server/registry_test.go`
- Modify: `server/server.go`
- Modify: `transport/ws/transport.go`
- Modify: `server/metrics.go`
- Test: `server/scenarios_test.go`

**Interfaces:**
- Produces: provider initialization succeeds only when every configured provider is locally usable; transport emits pending/source rejection observations; effective Google-STT session ceiling is `floor(max_streams / rotation_headroom_multiplier)`.

- [ ] **Step 1: Add startup and saturation tests**

Test production configuration with valid Google factories and missing xAI credentials; `Prepare` must return an xAI-specific error. Add per-reason saturation tests for socket, source rate, principal, VAD, STT quota, and global admission, asserting reservation counts return to zero.

- [ ] **Step 2: Run tests and confirm red**

Run: `(cd server && go test -count=1 -run 'TestRegistry_PrepareRequiresAllConfiguredProviders|TestAdmission_.*Observable' ./...)`

Expected: `Prepare` returns nil when one pipeline is live and pending metrics remain zero.

- [ ] **Step 3: Consolidate ownership and fail startup correctly**

Remove unused `AcquireHandshake`/`ReleaseHandshake` state from `AdmissionController`. Add transport observations `pending_handshakes`, `admission_reject_pending`, and `admission_reject_source_rate`. Make `Prepare` return joined initialization errors before considering circuit readiness:

```go
if len(errs) > 0 { return errors.Join(errs...) }
if !r.allConfiguredProvidersLiveLocked(cfg) {
	return errors.New("configured provider initialization incomplete")
}
return nil
```

Expose and document `EffectiveSTTSessionLimit()` and make admission/metrics use it consistently.

- [ ] **Step 4: Verify server admission and readiness**

Run: `(cd server && go test -race -count=1 ./...) && go test -race -count=1 ./transport/ws`

- [ ] **Step 5: Commit and review**

```bash
git add server/admission.go server/admission_test.go server/registry.go server/registry_test.go server/server.go server/metrics.go server/scenarios_test.go transport/ws/transport.go
git commit -m "fix(server): make admission and provider readiness truthful"
```

### Task 7: Complete Required Metrics and Bounded Timelines

**Files:**
- Modify: `server/metrics.go`
- Modify: `server/timeline.go`
- Modify: `server/server.go`
- Modify: `providers/google/stt.go`
- Modify: `providers/google/llm.go`
- Modify: `providers/google/tts.go`
- Modify: `transport/ws/transport.go`
- Test: `server/metrics_test.go`
- Test: `server/timeline_test.go`
- Test: `server/scenarios_test.go`

**Interfaces:**
- Produces bounded histogram buckets/counters for required V1 operational signals and content-free timeline events.

- [ ] **Step 1: Add focused signal tests plus one ordering integration**

Add focused tests for admission rejection, provider retry/circuit transition, STT rotation, interruption/slow control write, context eviction, VAD restart, and teardown. Each focused test owns only its signal and content-redaction assertions. Add one smaller integration test covering the ordering that matters across components: admission → provider start → interruption/flush → teardown. Assert exported metric names and ordered timeline event types, and assert output does not contain transcript strings or PCM bytes.

- [ ] **Step 2: Run tests and confirm red**

Run: `(cd server && go test -count=1 -run 'TestMetrics_V1RequiredSignals|TestTimeline_V1FaultSequence' ./...)`

- [ ] **Step 3: Add only the missing signals**

Add fixed-bucket histograms for handshake, VAD delay, first LLM text, first client audio, flush, write, and teardown. Add gauges for queued audio milliseconds/bytes and provider capacity. Add counters for arrival discontinuity, control-write failure, diagnostic shedding, retry, rotation, circuit transition, stale output, and native worker restart. Record content-free timeline metadata only: provider name, operation, generation, duration bucket, and safe reason code.

- [ ] **Step 4: Verify metrics and server race tests**

Run: `(cd server && go test -race -count=1 ./...) && go test -race -count=1 ./transport/ws`

- [ ] **Step 5: Commit and review**

```bash
git add server/metrics.go server/metrics_test.go server/timeline.go server/timeline_test.go server/server.go server/scenarios_test.go providers/google transport/ws/transport.go
git commit -m "feat(observability): cover v1 release signals"
```

### Task 8: Make the Compatibility Lock Executable

**Files:**
- Modify: `docs/compatibility/v1-lock.json`
- Modify: `docs/Voice Framework V1 Specification (Revised).md`
- Modify: `docs/Voice Framework V1 — Implementation Plan.md`
- Modify: `docs/adr/0001-v1-production-contract.md`
- Modify: `docs/pipecat-compatibility.md`
- Modify: `providers/google/tts.go`
- Test: `providers/google/tts_test.go`
- Modify: `tools/compat/validate_manifest.js`
- Modify: `tools/compat/verify_rtvi_lifecycle.js`
- Modify: `tools/compat/rtvi_lifecycle_test.go`
- Create: `tools/compat/contract_values.go`
- Test: `tools/compat/client_compat_test.go`
- Modify: `Makefile`

**Interfaces:**
- Produces: `go run ./tools/compat/contract_values.go` JSON describing shipped Go/runtime values; validator performs field-by-field comparison with the lock.

- [ ] **Step 1: Add failing drift fixtures**

Refactor the validator to accept `--manifest` and `--runtime-values`. Create a temporary copy changing clock skew, Pipecat commit, TTS encoding, and glibc; assert each invocation exits nonzero with the changed JSON path.

- [ ] **Step 2: Run tests and confirm red**

Run: `go test -count=1 ./tools/compat/... && node tools/compat/validate_manifest.js`

Expected: existing validator does not detect runtime/config drift.

- [ ] **Step 3: Emit and compare authoritative values**

`contract_values.go` loads production YAML and emits toolchain, module versions, transport defaults, token requirements, timeouts, sizes, audio rates, Google TTS request encoding, model/library hashes, and gitlink commit. Node validator compares exact keys and npm lock integrity.

Revise every contradictory playback-clear requirement in specification sections 18, 20, 40, Scenario D, M0, M4, release criteria, and success demo, plus the older implementation plan. Rename lock field `clear_event` to `interruption_notification_event`; record server queue/in-flight cancellation separately and set `client_cleared_buffered_playback:false`. Do not claim a client residual-playout duration without browser/device evidence.

Change Google TTS to request stable `texttospeechpb.AudioEncoding_PCM` and assert the response is headerless PCM16LE. Derive the native-systemd glibc floor in the pinned Linux builder with `readelf --version-info` against the exact binary digest; local validation consumes that Linux evidence rather than inspecting a macOS binary. The container contract remains the pinned Debian image digest.

Extend the lifecycle regression created in Task 2 to assert the now-final locked values. It must continue using the pinned, unmodified Pipecat packages and real transport with stubbed providers, covering duplicate `client-ready`, exactly one `bot-ready`, Pipecat-compatible message decoding, speaking/audio ordering, interruption notification, and terminal error/close behavior. No fork or patch of the client package is permitted.

- [ ] **Step 4: Verify manifest failure and success paths**

Run: `go test -race -count=1 ./tools/compat/... && make verify`

- [ ] **Step 5: Commit and review**

```bash
git add docs/compatibility/v1-lock.json docs/Voice\ Framework\ V1\ Specification\ \(Revised\).md docs/Voice\ Framework\ V1\ —\ Implementation\ Plan.md docs/adr/0001-v1-production-contract.md docs/pipecat-compatibility.md providers/google/tts.go providers/google/tts_test.go tools/compat Makefile
git commit -m "fix(contract): validate shipped v1 coordinates"
```

### Task 9: Standard SBOM and Reproducible CI Release Gate

**Files:**
- Create: `.github/workflows/verify.yml`
- Create: `.github/workflows/release.yml`
- Create: `deploy/sbom/README.md`
- Create: `scripts/verify_release.sh`
- Modify: `Makefile`
- Modify: `deploy/security_scans.md`
- Delete: `deploy/sbom.json`

**Interfaces:**
- Produces: `make release-security` with pinned `govulncheck`, Syft/CycloneDX, and Trivy versions; per-platform SBOM/provenance artifacts bound to immutable image digests; CI clean-checkout gate.

- [ ] **Step 1: Add a release-artifact validation script and red test**

The script rejects either platform SBOM unless `bomFormat=="CycloneDX"`, `specVersion` is fixed, metadata binds it to the matching immutable platform image digest, components include Go modules, Debian packages, ONNX Runtime and Silero, and serialized content contains neither `/Users/` nor the checkout path. It also validates a release index binding both platform digests and SBOM checksums to the multi-architecture manifest digest.

Run: `bash scripts/verify_release.sh --artifacts-only`

Expected: FAIL because the current raw module list is not CycloneDX.

- [ ] **Step 2: Pin tools and generate the SBOM**

Pin exact versions in Make variables and scanner image digests. Generate each platform image, then generate `sbom-amd64.cdx.json` and `sbom-arm64.cdx.json` from those image digests so OS and application components share one artifact per platform. Generate a release index containing the manifest digest, platform digests, SBOM checksums, tool versions, and commit. These are generated release artifacts, not a checked-in SBOM that becomes stale. Run `govulncheck` for every workspace module and Trivy against each built digest.

- [ ] **Step 3: Add CI workflows**

`verify.yml` runs `make verify` on Linux for every pull request. `release.yml` runs tagged release tests, initializes submodules, builds and executes runtime smoke tests for both amd64 and arm64 (native or emulated), pushes the immutable platform images and manifest when authorized, generates/validates both SBOMs and their index, scans, and uploads evidence. It must not claim publish or attestation success unless credentials and an actual registry push/attestation occur.

- [ ] **Step 4: Verify locally**

Run: `make release-security && bash scripts/verify_release.sh --artifacts-only`

Expected: PASS with zero High/Critical fixed vulnerabilities under the documented gate.

- [ ] **Step 5: Commit and review**

```bash
git add .github Makefile scripts/verify_release.sh deploy/sbom deploy/security_scans.md
git rm deploy/sbom.json
git commit -m "ci(release): add reproducible security gate"
```

### Task 10: Repair Container, Nginx, systemd, and Shutdown Paths

**Files:**
- Modify: `deploy/Dockerfile`
- Modify: `deploy/docker-compose.yaml`
- Modify: `deploy/nginx.conf`
- Modify: `deploy/govoice.service`
- Modify: `docs/operations_runbook.md`
- Create: `scripts/smoke_deploy.sh`
- Test: `server/lifecycle_test.go`

**Interfaces:**
- Produces: one consistent `/opt/govoice` filesystem layout and supervisor grace period greater than application drain plus five seconds.

- [ ] **Step 1: Write deployment static/smoke checks**

The script renders Compose config, runs `nginx -t` with generated temporary certificates, asserts every referenced host/container path exists, verifies systemd `ExecStart`, model and ORT library paths, and asserts both supervisor grace periods exceed `drain_timeout + 5s`. It builds and runs the real image with a read-only root filesystem plus only the declared tmpfs/writable mounts, confirms an unprivileged user, exercises health/readiness, and checks child-process reaping and SIGTERM exit.

- [ ] **Step 2: Run and confirm red**

Run: `bash scripts/smoke_deploy.sh`

Expected: missing private-key mount, wrong native model/config paths, and insufficient grace periods.

- [ ] **Step 3: Align deployment assets**

Mount cert and key directories exactly where Nginx reads them. Install server, worker, model, ORT library, production YAML, and environment file to paths used by the unit. Make the container run unprivileged with a read-only root and explicit writable mounts. Set Compose/systemd grace to at least 55 seconds for the 45-second drain. Pin Compose image references by digest or require `GOVOICE_IMAGE_DIGEST`.

- [ ] **Step 4: Verify container and shutdown smoke**

Run: `bash scripts/smoke_deploy.sh`

The script must run the built container, not merely build it. Then run the lifecycle test that holds a session through the drain deadline and confirms application exit precedes supervisor hard-kill; repeat the SIGTERM/drain assertion for the systemd unit in the target-Linux certification environment.

- [ ] **Step 5: Commit and review**

```bash
git add deploy docs/operations_runbook.md scripts/smoke_deploy.sh server/lifecycle_test.go
git commit -m "fix(deploy): align production runtime paths"
```

### Task 11: Make Quickstart Executable and Claims Honest

**Files:**
- Modify: `docs/quickstart.md`
- Modify: `README.md`
- Modify: `docs/observability.md`
- Modify: `docs/benchmarks/capacity_slo_report.md`
- Modify: `docs/release_evidence.md`
- Modify: `docs/Voice Framework V1 — Phase Checklist.md`
- Modify: `examples/rtvi_client/README.md`
- Modify: `examples/rtvi_client/cli.js`
- Modify: `examples/rtvi_client/server.js`
- Modify: `examples/rtvi_client/package.json`
- Modify: `examples/rtvi_client/package-lock.json`
- Create: `scripts/verify_quickstart.sh`

**Interfaces:**
- Produces offline-verifiable quickstart plus credential-gated real-call command; removes unsupported 250-call and completed-certification claims.

- [ ] **Step 1: Add a quickstart verifier**

Extract and validate the documented YAML with `server.LoadConfig`, mint and validate the documented token, install pinned npm dependencies with `npm ci`, type-check/run the client constructor path, start a stubbed local server, connect, receive `bot-ready`, and complete a synthetic two-turn protocol flow.

- [ ] **Step 2: Run and confirm red**

Run: `bash scripts/verify_quickstart.sh`

Expected: obsolete YAML, wrong environment name, missing npm installation, incorrect client API, and payload mismatches.

- [ ] **Step 3: Rewrite commands and evidence language**

Use `GOOGLE_API_KEY`, the actual config schema, mandatory token claims, and tested `WebSocketTransport`/`PipecatClient` construction. Label the stub path as protocol-only and provide a separate credential-gated real-call command. Replace 250-call statements with the effective configured ceiling and “not certified” until Task 13 evidence exists. Uncheck every Phase Checklist item contradicted by `release_evidence.md`.

- [ ] **Step 4: Verify docs and examples**

Run: `bash scripts/verify_quickstart.sh && make verify`

- [ ] **Step 5: Commit and review**

```bash
git add docs README.md scripts/verify_quickstart.sh examples/rtvi_client
git commit -m "docs(v1): make release paths executable"
```

### Task 12: Publishable Canonical Multi-Module Graph

**Files:**
- Modify: `go.mod`, `go.work`
- Modify: `core/go.mod`
- Modify: `providers/google/go.mod`
- Modify: `providers/xai/go.mod`
- Modify: `server/go.mod`
- Move: `transport/` to `core/transport/`
- Move: `vad/` to `core/vad/`
- Move: `examples/envload/` to `core/envload/`
- Move: `cmd/server/` to `server/cmd/server/`
- Move: `cmd/vadworker/` to `server/cmd/vadworker/`
- Modify: all Go imports beginning `govoice`
- Modify: `Makefile`
- Modify: `docs/compatibility/v1-lock.json`
- Create: `scripts/verify_modules.sh`

**Interfaces:**
- Produces module paths rooted at `github.com/raysca/go-voice`; no committed module file contains a local `replace`; `go.work` supplies local workspace resolution; release metadata resolves coordinated tagged sibling modules.

- [ ] **Step 1: Add external-consumer verification**

Create a temporary directory outside the repository, set `GOWORK=off`, and build minimal programs importing:

```go
import (
	"github.com/raysca/go-voice/core"
	googlevoice "github.com/raysca/go-voice/providers/google"
)
```

Also build the server command without `go.work`. Assert no committed module contains a `replace` directive. Before publication, construct a temporary file-backed GOPROXY containing release-form module archives for the exact coordinated versions and resolve/build only through that proxy. After tags are published, repeat against the public proxy and verify tag-to-module-path/version correspondence.

- [ ] **Step 2: Run and confirm red**

Run: `bash scripts/verify_modules.sh`

Expected: noncanonical `govoice/...` imports and local replacements prevent external resolution.

- [ ] **Step 3: Rewrite module/import paths**

Use these exact module names:

```text
github.com/raysca/go-voice
github.com/raysca/go-voice/core
github.com/raysca/go-voice/providers/google
github.com/raysca/go-voice/providers/xai
github.com/raysca/go-voice/server
```

Use exact sibling requirements at `v0.1.0`. The coordinated repository tags are `v0.1.0`, `core/v0.1.0`, `providers/google/v0.1.0`, `providers/xai/v0.1.0`, and `server/v0.1.0`; tag creation/publication remains a release action, not part of this source-edit task.

Eliminate the existing root↔server cycle deterministically: move transport, VAD, and the generic environment loader into the dependency-safe `core` module; move both production commands into the `server` module; and update imports to `github.com/raysca/go-voice/core/transport/...`, `github.com/raysca/go-voice/core/vad`, and `github.com/raysca/go-voice/core/envload`. This removes the `examples/envload` root import from both the server command and live server test.

Remove `core/vad`'s source-tree fallback that runs `go build ./cmd/vadworker`. Core accepts a configured/preinstalled worker binary and fails with an actionable error when none is available; the server packaging/build owns production of `server/cmd/vadworker`, while tests inject a fixture path. The root module remains an umbrella/examples/tools consumer and may depend on core/providers/server; server must not depend on the root module. Providers depend only on core. Keep `go.work` for local development and remove every local `replace` from all committed `go.mod` files.

Update every Make target affected by the moves, including server/worker build paths, test and format package roots, release image inputs, and the canonical Protobuf `go_opt` import. `make verify` must run solely against the new layout.

- [ ] **Step 4: Verify workspace-off and workspace-on builds**

Run: `bash scripts/verify_modules.sh && make verify`

- [ ] **Step 5: Commit and review**

```bash
git add go.mod go.sum go.work core providers server cmd examples Makefile docs/compatibility/v1-lock.json scripts/verify_modules.sh
git commit -m "refactor(modules): publish canonical module graph"
```

Independent review focus: dependency direction is core ← providers ← server with no reverse edge to the umbrella module, external consumers need no local replace, and both production commands remain buildable.

### Task 13: External Scenario Harness and Release Evidence

**Files:**
- Create: `cmd/releasecheck/main.go`
- Create: `scripts/run_release_scenarios.sh`
- Create: `deploy/netem/README.md`
- Create: `docs/benchmarks/release-results.schema.json`
- Modify: `examples/rtvi_client/cli.js`
- Modify: `examples/rtvi_client/package.json`
- Modify: `examples/rtvi_client/package-lock.json`
- Modify: `docs/release_evidence.md`
- Modify: `docs/benchmarks/capacity_slo_report.md`
- Test: `server/e2e_live_test.go`
- Test: `server/scenarios_test.go`

**Interfaces:**
- Produces timestamped, checksummed, provenance-bound JSON results for mandatory Scenarios A, C, D, E, F, G, H, I, and J, with environment identity and percentile calculations. Scenario B remains optional and non-blocking unless the frozen specification promotes it.

- [ ] **Step 1: Add schema and self-test mode**

`releasecheck` records commit, dirty status, manifest and platform image digests, SBOM checksum, architecture, libc, deployment mode, provider regions/models, browser/device identity, client versions, sample counts, p50/p95/p99, maximum memory, live metric/timeline artifact checksums, and pass/fail reason. The schema rejects unknown/missing mandatory fields. `--self-test` uses deterministic fixtures and validates every required field without provider credentials.

- [ ] **Step 2: Run self-test and confirm red before implementation**

Run: `go test -count=1 ./cmd/releasecheck/... && go run ./cmd/releasecheck --self-test`

Expected initially: package or schema does not exist.

- [ ] **Step 3: Implement scenario orchestration**

The shell script refuses a dirty tree, unpinned image, missing target metadata, missing required credentials, or a result not bound to the commit/image/SBOM under review. It runs real Google and mixed xAI-LLM calls using the pinned unmodified Pipecat package in a real supported browser and named audio device, including an acoustic loopback or documented hardware path. It executes 30 minutes with at least five test-threshold rotations, concurrent isolation, admission security, `tc netem` impairment, native-worker fault injection, and a separate hard-kill case. It captures live exported metrics and bounded timeline evidence, but no secrets, transcript text, or PCM.

Run both amd64 and arm64 images (native or documented emulation) and exercise both container and systemd deployments through SIGTERM/drain. Require a second operator or fresh independent agent to follow only `docs/operations_runbook.md` and record whether startup, rollback, evidence collection, and shutdown succeed without undocumented steps.

- [ ] **Step 4: Run available certification**

Run deterministic mode locally:

`go run ./cmd/releasecheck --self-test && bash scripts/run_release_scenarios.sh --preflight`

On target Linux with credentials and explicit cost authorization:

`bash scripts/run_release_scenarios.sh --execute`

The release remains not-ready if any mandatory result, architecture/runtime smoke, browser/device/acoustic run, deployment-mode shutdown run, live metric/timeline capture, or independent runbook-following result is absent or below the frozen SLO. Scenario B absence alone does not block V1.

- [ ] **Step 5: Commit harness and truthful evidence**

```bash
git add cmd/releasecheck scripts/run_release_scenarios.sh deploy/netem docs/benchmarks docs/release_evidence.md server
git commit -m "test(release): add v1 certification harness"
```

### Task 14: Whole-Release Verification and Independent Spec/Pipecat Review

**Files:**
- Modify only files required by final review findings.
- Update: `docs/release_evidence.md`
- Update: `docs/Voice Framework V1 — Phase Checklist.md`

**Interfaces:**
- Consumes every preceding task and external certification artifact.
- Produces the final V1 go/no-go verdict.

- [ ] **Step 1: Run every deterministic gate from a clean checkout**

```bash
make verify
make release-security
bash scripts/verify_modules.sh
bash scripts/verify_quickstart.sh
bash scripts/smoke_deploy.sh
go test -race -tags xaispeech -count=1 ./providers/xai/...
go run ./cmd/releasecheck --self-test
git diff --check
```

- [ ] **Step 2: Audit external evidence**

Validate every mandatory result against `release-results.schema.json`, the commit under review, pinned image digest, target Linux identities, required durations/sample counts, and section 42 SLOs. Missing evidence is a release blocker, not a documentation task.

- [ ] **Step 3: Dispatch independent whole-branch review**

Give a fresh reviewer the complete base-to-head diff plus:

- `docs/Voice Framework V1 Specification (Revised).md`
- `docs/Voice Framework V1 — Implementation Plan.md`
- `docs/superpowers/specs/2026-09-28-v1-production-readiness-design.md`
- this plan
- `docs/compatibility/v1-lock.json`
- pinned Pipecat package source and gitlink
- release evidence and certification JSON

Require findings ranked Critical/Important/Minor across spec compliance, Pipecat feature compatibility, concurrency, security, failure semantics, bloat/YAGNI, deployment, and evidence integrity.

- [ ] **Step 4: Fix and re-review all blockers**

Dispatch one fix agent with the complete Critical/Important list. Run focused regression tests, the full deterministic gate, and one scoped independent re-review. No Critical or Important issue may be waived silently.

- [ ] **Step 5: Publish verdict**

Mark V1 production-ready only when deterministic gates pass, mandatory external certification is present, and the final reviewer has no unresolved Critical or Important findings. Otherwise update the evidence matrix with the exact remaining blocker and keep the release status `NO-GO`.

## Execution Ledger

Append one line per task containing its task number, exact commit range, review result, deferred Minor findings, fix-round counts, and any ruling plus the consequence if that ruling is wrong.
