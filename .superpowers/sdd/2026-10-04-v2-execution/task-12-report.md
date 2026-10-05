# Task 12 — Google/xAI tool adapters

## Scope and commits

- Google implementation: `3cffe88`; corrections: `78e74df`, `a9c060b`,
  `ed3b930`.
- xAI implementation: `de186c2`; corrections: `bb03691`, `a042dff`,
  `c6f5679`.
- Shared immutable provider-fence composition: `5e17548`.
- Provider-specific RED/GREEN evidence and assumptions remain in
  `task-12-google-report.md` and `task-12-xai-report.md`.

The adapters consume the shared core definitions, ToolLoop, context ledger,
and safe result policy. They do not implement usage metrics, readiness, MCP,
or framework lifecycle ownership.

## Review corrections

Independent cross-provider review used native Google GenAI HTTP fixtures and
xAI Responses event fixtures in addition to neutral mocks. Corrections covered:

- Google thought signatures, complete response occurrences, mixed native
  parts, stream termination before side effects, stream-wide fallback IDs,
  replay occurrences, arbitrary JSON/large integers, definition copying, and
  composed application/provider fences.
- xAI text-vs-argument/reasoning event separation, added/delta/done identity
  assembly, per-call generation fencing, native later-turn history, explicit
  failure/cancel status, arbitrary JSON/large integers, and configured result
  bounds.
- Both adapters claim through ToolLoop before context projection, retain
  ordered multiple calls, and avoid re-execution on exact replay.

## Independent closeout

- Final review: **APPROVE** for `a042dff..ed3b930`. The reviewer replayed all
  prior overlays under the race detector and verified Google response/result
  ordering plus xAI configured-bound result preservation.
- Final test: **PASS** at `ed3b930`. Full Google and xAI tagged/untagged
  race/vet, full core and root tagged checks, ToolLoop race ×50, provider
  regressions ×20, and mocked end-to-end continuations passed using the
  documented macOS 26 SDK override.
- Live paid-provider requests were not run. Task 12 is complete; Gate C waits
  for the Phase 3 independent gate after Tasks 13–14.
