# AGENTS.md — Agent Guidelines & Architecture Directives

## Mission & Identity

**go-voice** is a high-throughput, low-latency, and resource-efficient Go implementation of the [Pipecat](https://github.com/pipecat-ai/pipecat) voice AI pipeline framework. It provides full compatibility with the official, unmodified Pipecat web clients (`@pipecat-ai/client-js`, `@pipecat-ai/websocket-transport`) using binary Protobuf frames and the RTVI 2.1.0 protocol over WebSockets.

While Python Pipecat is hindered by interpreter GIL contention, heavy CPU overhead, and high per-session memory footprints, **go-voice** is engineered for industrial concurrency:
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
│  - Agent DB Storage & Credential Keyring (SQLite / Postgres)│
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
- **Role:** The orchestration daemon that manages persistent agents, provider credentials, and live calls.
- **Invariants:**
  - Consumes `core/` exactly as an external 3rd-party client would.
  - Dynamically builds and configures `core.Pipeline` instances for incoming calls according to the caller's pinned agent version (`server/call.go`, `server/registry.go`).
  - Encapsulates database storage (`server/storage`), credentials (`server/credential`), MCP servers (`server/mcp`), and management endpoints (`server/api`).

---

## Development Principles

### 1. Favor Established Frameworks Over Handrolling
- Always favor established industry frameworks, standard libraries, and battle-tested packages over handrolled implementations.
- Do not invent custom serialization formats, cryptography schemes, state machines, or wire protocols when standard, well-maintained solutions exist.
- Keep wire protocols strictly compatible with the standard Pipecat RTVI protocol.

### 2. Strict YAGNI (You Aren't Gonna Need It) & Zero Bloat
- **Server Consumption Requirement:** Every feature, processor, or configuration added to `core/` must ultimately be consumed and utilized by the `server/` runtime.
- Do not write speculative, dead, or "just-in-case" library abstractions. If the server does not expose or consume it on a call path, it should not exist in the tree.
- Eliminate dead code, unused helper methods, and redundant configuration knobs aggressively.

### 3. Resource Bounds & Thread Safety
- **Every queue must be bounded.** Never create unbounded Go channels or unbounded slices that grow with incoming audio or tokens.
- **Generation fencing:** Every asynchronous task and frame stream must respect `CurrentGeneration()` to avoid race conditions or late-arriving audio from interrupted turns.
- **Zero Race Conditions:** All code must run and pass cleanly under `go test -race ./...`.

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
   - `make verify` (or `go test -race ./core/...` and `go test -race ./server/...`).
   - Clean git status with descriptive conventional commit messages.
