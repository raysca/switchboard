# Decision: Whisker compatibility stays semantic

**Status:** accepted · **Task:** P6-2 (pipecat parity plan)

## Context
Whisker is Pipecat's live pipeline debugger: a UI that attaches to a running
pipeline over a WebSocket. It is not a test framework and cannot run in CI.
No official Whisker package or protocol version is pinned in this repo.

## Decision
- Keep the semantic adapter in `tools/compat/whisker` and its 11 conformance
  scenarios. They are regression tests for `core.DebugEvent` ordering and
  correlation (see `docs/pipecat-compatibility.md`).
- Do **not** pursue wire compatibility until a Whisker release and protocol
  version are pinned and a team actually needs live debugging of a Go server.
- Live debugging today uses the per-session timeline, the observer API
  (`Session.Subscribe`, P0-1), Prometheus latency histograms (P0-2) and optional
  OpenTelemetry export (P5-1).
- End-to-end black-box checks use `pipecat.evals` through `make eval-pipecat`
  (see `docs/decisions/pipecat-evals.md`), not Whisker.

## Revisit when
A Whisker version is pinned, or someone asks to attach Whisker to a running
`go-voice` instance. The observer API is the integration seam: a WebSocket
bridge subscribing to `DebugEvent` would be the whole implementation.
