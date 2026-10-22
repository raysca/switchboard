# AGENTS.md — Agent Guidelines & Architecture Directives

## Mission & Identity

**Switchboard** is a high-throughput, low-latency, and resource-efficient Go implementation of the [Pipecat](https://github.com/pipecat-ai/pipecat) voice AI pipeline framework. It provides full compatibility with the official, unmodified Pipecat web clients (`@pipecat-ai/client-js`, `@pipecat-ai/websocket-transport`) using binary Protobuf frames and the RTVI 2.1.0 protocol over WebSockets.

While Python Pipecat is hindered by interpreter GIL contention, heavy CPU overhead, and high per-session memory footprints, **Switchboard** is engineered for industrial concurrency:
- **Low Memory & CPU Consumption:** Bounded memory structures, ring-buffered history, zero-copy audio frames, and media-clock paced writers.
- **Sub-200ms Barge-In Interruption:** Generation fencing that drops late model frames instantly upon speech detection.
- **Crash Isolation:** Native C/C++ ONNX Runtime dependencies (Silero VAD, Smart Turn) run in supervised worker child processes, shielding the Go server process from segmentation faults.

---

## Architecture: Two-Tier Separation

The codebase is strictly bifurcated into an independent **Core Library** and an application-level **Voice AI Server**.

```
┌─────────────────────────────────────────────────────────────┐
│                       server/                               │
│  (Configuration-based Agentic Voice AI Server)              │
│  - REST Management API (/api/v1)                            │
│  - Flow Storage (SQLite / Postgres) & Env-Only Secrets      │
│  - MCP Tools & Runtime Session Pinning                      │
│  - WebSocket Transport (/ws) & RTVI Framing                 │
└──────────────────────────────┬──────────────────────────────┘
                               │ Consumes core as an
                               │ independent 3rd-party library
┌──────────────────────────────▼──────────────────────────────┐
│                        core/                                │
│  (Independent Pipeline Framework & Engine)                  │
│  - Bidirectional Pipeline & Frame Streaming Engine          │
│  - Generation Fencing & Interruption Barrier Coordination   │
│  - Assistant Context, Sentence Chunking & TTS Text Clean    │
│  - VAD, Turn Taking, Audio Resampling & Ambience Underlay   │
└─────────────────────────────────────────────────────────────┘
```

### 1. `core/` (The Pipeline Library)
- **Role:** Pure, reusable voice pipeline framework.
- **Invariants:** 
  - Must remain completely agnostic of server concerns.
  - Never import `server/`, never access database tables, never parse server YAML configs, and never make HTTP management calls.
  - Operates purely on `Frame` abstractions (`ProcessFrame`, `Emitter`, `Direction`), generation counters, audio buffers, and provider interfaces.

### 2. `server/` (The Agentic Voice AI Server)
- **Role:** The orchestration daemon that manages stored flows, provider configuration, and live calls.
- **Invariants:**
  - Consumes `core/` exactly as an external 3rd-party client would.
  - Dynamically builds and configures `core.Pipeline` instances for incoming calls according to the caller's pinned flow version (`server/call.go`, `server/registry.go`).
  - Encapsulates database storage (`server/storage`), provider catalog (`server/catalog`; secrets are read from the environment only, see `docs/security/credentials.md`), MCP servers (`server/mcp`), and management endpoints (`server/api`).

---

## Development Principles

### 1. Favor Established Frameworks Over Handrolling
- Always favor established industry frameworks, standard libraries, and battle-tested packages over handrolled implementations.
- Do not invent custom serialization formats, cryptography schemes, state machines, or wire protocols when standard, well-maintained solutions exist.
- Keep wire protocols strictly compatible with the standard Pipecat RTVI protocol.

### 2. Strict YAGNI (You Aren't Gonna Need It) & Zero Bloat
- **Server Consumption Requirement:** Every feature, processor, or configuration added to `core/` must ultimately be consumed and utilized by the `server/` runtime.
- **Planned or parked library code:** a `core/` (or `transport/`, `vad/`, `smartturn/`) capability that no server call path reaches may stay only while its row in `docs/pipecat-parity-matrix.md` has the status `planned (hook exists)` or `parked` with the reason. Otherwise it is deleted. `make deadcode` reports functions unreachable from the roots `./cmd/... ./examples/... ./tools/... ./offline/cmd/...` (tests are not roots, so a function only tests call is reported; it is a pinned `deadcode` run, not a proof, and the parked or planned rule rests on the parity-matrix rows, not on the tool); each one it keeps is named with a reason in `scripts/deadcode-allow.txt`, and a helper only tests use lives in a `_test.go` file (`export_test.go` for an external test package).
- Do not write speculative, dead, or "just-in-case" library abstractions. If the server does not expose or consume it on a call path, it should not exist in the tree.
- Eliminate dead code, unused helper methods, and redundant configuration knobs aggressively.

### 3. Resource Bounds & Thread Safety
- **Every queue must be bounded.** Never create unbounded Go channels or unbounded slices that grow with incoming audio or tokens.
- **Generation fencing:** Every asynchronous task and frame stream must respect `CurrentGeneration()` to avoid race conditions or late-arriving audio from interrupted turns.
- **Zero Race Conditions:** All code must run and pass cleanly under `go test -race ./...`.
- **No goroutine leaks:** the listed packages that own goroutines (`core/sttunit`, `core/ttsunit`, `transport/ws`, `vad`, `server`) end their tests with a `go.uber.org/goleak` check; a new package that starts goroutines adds one. (Not yet covered, recorded as a gap: `core` itself, whose tests leave one deliberately hung test processor, `core.hungProcessor`; `smartturn`; and the other packages that start goroutines, such as `core/turn`, `core/record`, `core/tools`, `providers/xai`, `server/otelexport`, `server/runtime` and `offline/adapter`.)

---

## Go Idioms & High-Throughput Engineering Standards

To sustain hundreds of concurrent voice calls with sub-200ms interruption latency, all Go code must strictly conform to these engineering standards:

### 1. Goroutine Lifecycle & Context Propagation
- **Explicit Lifecycle:** Never spawn a goroutine without an unambiguous termination path. Every goroutine must select on `ctx.Done()` or a dedicated close/done channel.
- **Supervisor Integration:** In `core/`, all session-scoped background tasks must be spawned via `session.Go(...)` so unhandled panics and lifecycle teardowns are coordinated by the session supervisor.
- **No Context in Structs:** Never store a `context.Context` inside a struct field. Pass context as the first argument (`ctx context.Context`) to methods and functions. Two exceptions, each tagged with a comment at the field:
  - *Trace parentage* (`// trace parentage`): a context kept for span parentage, so spans started later, off the caller's stack (failover replays, the async tracer, session trace roots), have the right parent. Any cancellation it carries is its owner's, never a per-request deadline.
  - *Owner lifetime* (`// owner lifetime`): the context of a long-lived object that owns goroutines, created together with its `cancel` in the constructor (or the stream open) and cancelled by the object's own `Close`/teardown (session, WebSocket connection, provider streams, export queue). Never a caller's request context.
- **Immediate Defers:** Always invoke `defer cancel()` immediately after creating derived contexts (`context.WithTimeout`, `context.WithCancel`).

### 2. Concurrency & Synchronization Hygiene
- **Never Hold Locks Across I/O:** Never perform network I/O, database queries, disk writes, or blocking channel operations while holding a `sync.Mutex` or `sync.RWMutex`.
- **Fast-Path Atomics:** On the hot audio streaming path (50 frames/sec per call), prefer `sync/atomic` primitives (`atomic.Uint64`, `atomic.Bool`, `atomic.Pointer`) over mutexes for generation checks, counters, and status flags.
- **Channel Directionality:** Explicitly declare channel directions (`<-chan T` for receive-only, `chan<- T` for send-only) in function parameters and interface signatures.
- **Producer-Only Closes:** Only the producer goroutine may close a channel. Never close a channel from the receiver side, and coordinate concurrent teardown with `sync.Once`.

### 3. Allocation & Memory Efficiency on Hot Paths
- **Zero-Allocation Audio Framing:** A single call processes 50 audio frames per second (20ms PCM chunks). At 300+ calls, that is >15,000 frames/sec. Avoid heap allocations on this path: reuse fixed buffers, slice existing arrays, and utilize `sync.Pool` where appropriate.
- **Preallocate Capacities:** Always provide capacity hints when creating slices whose size or upper bound is known: `make([]T, 0, capacity)`.
- **String Building:** Use `strings.Builder` with `b.Grow(...)` or `bytes.Buffer` for multi-fragment string assembly; never concatenate strings with `+` in loops.

### 4. Monotonic Timing vs. Wall-Clock
- **Always Use Monotonic Time:** Use monotonic timers (`time.Since`, `time.Until`, `time.Duration`) for calculating elapsed time, silence hangovers, and speech timeouts.
- **Never Rely on Wall Clocks for Durations:** Avoid `time.Now().UnixNano()` for interval comparisons. Wall clocks jump during NTP adjustments, which corrupts turn-taking detectors and timeout loops.

### 5. Error Handling & Panic Resilience
- **Contextual Wrapping:** Wrap errors with actionable context using `fmt.Errorf("action description: %w", err)`.
- **Idiomatic Inspection:** Use `errors.Is` and `errors.As` instead of direct equality (`err == ...`) or raw type assertions.
- **Exported Sentinels:** Define package-level exported errors as `var Err... = errors.New(...)`.
- **Crash Protection on Transports:** WebSocket frame writers and provider callbacks must protect against panics (`defer func() { if r := recover(); r != nil { ... } }()`) so an individual client anomaly cannot terminate the server daemon.

### 6. Interface Philosophy
- **Accept Interfaces, Return Concrete Structs:** Define small, focused interfaces at the consumer call site rather than declaring large producer god-interfaces.

---

## Mandatory Code Review Workflow

All engineering tasks, features, and refactors must conclude with an **independent agent review**:

1. **Adversarial Reviewer:** An independent agent (running in a dedicated review context) must evaluate the diff before work is considered complete.
2. **YAGNI & Bloat Audit:** The reviewer must explicitly evaluate:
   - Is this code over-engineered or unnecessarily complex?
   - Does this add unneeded abstractions or dead paths?
   - Will every new library capability genuinely be consumed by the server?
3. **Correctness & Concurrency Audit:**
   - Race conditions, unsynchronized mutations, deadlock potential, or goroutine leaks.
   - Monotonic vs. wall-clock time comparisons.
   - Error handling, recover blocks, and resource cleanup (`Close()`, `defer cancel()`).
4. **Verification Gate:**
   - `gofmt -w .`
   - `make verify`: vet and `go test -race` in every module of the Makefile's `MODULES`, the Go end-to-end tests, gofmt, the protobuf fixture and manifest checks, and the docs link checker (`scripts/check-links.mjs`).
   - `make deadcode`: no unreachable function outside `scripts/deadcode-allow.txt` (advisory in CI).
   - `make e2e-echo` (Go and browser end-to-end on the free echo provider) and, when the offline stack is running, `make offline-test`. Paid/live targets only when asked.
   - Clean git status with descriptive conventional commit messages.
