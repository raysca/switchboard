# Round 6 audit: consolidated launch-blocker list

Launch scope: Google, xAI, Nvidia. Four independent read-only audits (M1 core, M2 transport/server, M3 providers, M4 tests/ops). 44 findings: 0 blocker, 9 high, 16 medium, 19 low (counts by auditor severity). Details and evidence in `M1-core.md`, `M2-transport-server.md`, `M3-providers.md`, `M4-ops-tests.md`.

## Fix first (high)

| # | ID | Problem | Why it matters |
|---|---|---|---|
| 1 | M1-01 | VAD result discarded (generation bump or full inbox) is never replaced: no barge-in/end-of-speech for the rest of the call | Calls go deaf; all providers |
| 2 | M1-02 | Idle controller unit ids start at 2^40; ws pacer needs non-decreasing ids, so the reply after an idle phrase fails the session | Call dies after idle prompt (`idle.prompt_mode: say`, `bot_fallback`) |
| 3 | M1-03 | Each TTS sentence holds one of 16 task slots while queued; long replies drop sentences (4 of 20 in probe) | Truncated replies; Nvidia first |
| 4 | M3-01 | Google TTS starts sentence N+1 only near the end of N: any request slower than ~2 s is a gap | The 5 gaps seen live on Google |
| 5 | M4-01 | No CI runs tests; perf gate never fails, baseline is a 12-core Mac | No regression safety net |
| 6 | M4-02 | Admin listener binds 127.0.0.1 in the container: `/ready` and `/metrics` unreachable | K8s probe never passes; no scraping |
| 7 | M4-03 / M3-06 | Production image and `make build` omit `-tags xaispeech`: xAI STT/TTS unavailable; tests not run | Decision: is xAI speech in launch scope? |
| 8 | M4-04 | Alert docs name metrics that do not exist; no alert rules shipped | Alerts would never fire |
| 9 | M4-05 | Nvidia breakers have no metrics, not in `/ready`; error counters lack provider label | Nvidia outages invisible |

## Medium (next)

M3-02 xAI STT `audio.done` sent before buffered audio (failover to xAI loses the utterance); M3-03 xAI TTS network errors not retried or failed over; M3-04 breakers shared across tenants, 429 opens them for everyone; M3-05 Gemini error classification by message text; M3-07 NIM reasoning models (retries 3x, billed, silent); M1-04 failed no-audio TTS unit leaves reply active; M1-05 large tool result can end the call; M2-01 IPv6 handshake limiter lockout; M2-02 failed call setups never logged; M4-06 fixed ports in server tests; M4-07 fresh clone `make test` fails (ORT lib); M4-08 smart-turn p95 test flaky; M4-09 Postgres tests not in `make test`; M4-10 no Linux, soak, fault-injection evidence; M4-11 runbook wrong; M4-12 `/ready` ignores DB; M4-13 `idle.user_timeout` off in production.yaml.

## Low

M1-06..M1-10, M2-03..M2-06, M3-08..M3-10, M4-14..M4-18 (see reports).

## Suggested fix batches

1. **Calls that die or go deaf (M1-01, M1-02, M1-03, M1-04, M1-05).** Each has a failing probe test in the auditor scratchpad: port the probes into the repo as regression tests first.
2. **Google/xAI/Nvidia correctness (M3-01..M3-05, M3-07..M3-10, M2-02).**
3. **Operability (M4-02, M4-04, M4-05, M4-12, M4-13, M2-04..M2-06, M4-11):** admin listener reachability, real metric names + shipped alert rules + a test that every documented metric exists, Nvidia breaker metrics, runbook fixes.
4. **Safety net (M4-01, M4-03/M3-06, M4-06..M4-09, M4-16, M4-17):** CI workflow (`make verify`, xaispeech tests, Postgres), smoke script probing `/ready` from outside the container, port-free tests.
5. **Evidence (M4-10):** Linux container run, real-provider soak, fault injection. Do after 1-4.

## Decisions needed

- Is xAI STT/TTS in launch scope (drives M3-06/M4-03 and M3-02/03/09)?
- Remaining batch order and whether batch 5 (soak against real providers) has a spend budget.

---

# Status after round 7 (fix-audit-findings)

Fixed = merged on `main`. Commit hashes are the fixing commits; "open" means no fix yet.

| ID | Status | Where / reason |
|---|---|---|
| M1-01 | fixed | 9661f31 |
| M1-02 | fixed | d078572 (shared allocator), 356768f (id taken at delivery) |
| M1-03 | fixed | 7f3b920 (serial TTS queue); add3ee8 keeps the Google look-ahead under it |
| M1-04 | fixed | 02dd6d6, fe539ab |
| M1-05 | fixed | fe10898, 91f7140, d9b9b80 |
| M1-06 | open | span pile-up with OTel on; low |
| M1-07 | open | idle "say" speech cannot be barged in; low |
| M1-08 | open | AsyncTrigger/turn-start TOCTOU; speculative |
| M1-09 | open | late first-token cancel to the old candidate; speculative |
| M1-10 | fixed | 1651dfb |
| M2-01 | fixed, residual | 60596fd (IPv6 /64 keys, shared overflow bucket), c56efc0 (throttled sweep). Residual: a /48 owner can fill the table and starve unrelated sources into the shared overflow bucket |
| M2-02 | fixed | 6abbca5, 31778e2 (phase and stage only, never error text) |
| M2-03 | fixed | forced-cancel shutdown closes sessions concurrently outside `s.mu` (round 7, N4) |
| M2-04 | fixed | 4f35ba0 |
| M2-05 | fixed | 5b49ad7 (runbook affinity and drain text) |
| M2-06 | fixed | 9c66b1d (probe listener; admin stays on loopback) |
| M3-01 | fixed, partial | d057147 (bounded look-ahead), add3ee8. Deferred: true `StreamingSynthesize` for Google TTS |
| M3-02 | fixed | 57f4cf6 |
| M3-03 | fixed | 62ff1de |
| M3-04 | fixed, partial | 26e81b8 (quota 429 no longer opens shared breakers; per-stage xAI breakers). Deferred: per-tenant breaker keys |
| M3-05 | fixed | 26e81b8 |
| M3-06 | fixed | 6cb493f (`-tags xaispeech` default) |
| M3-07 | fixed | b099e16 |
| M3-08 | fixed | b1d7dcd |
| M3-09 | fixed | 57f4cf6 (drop while a turn is bound), plus round 7 N4: a drop after `audio.done` with an open utterance also raises the failover error; core test `TestSTTFailoverInvalidatingDropAfterSpeechFinalCommitsOnce` |
| M3-10 | fixed | 2f35d63 |
| M4-01 | fixed | c1b2a26 (CI), 5c3ac77 (submodule and npm ci) |
| M4-02 | fixed | 9c66b1d, d8ca2a0 |
| M4-03 | fixed | 6cb493f |
| M4-04 | fixed | 5b49ad7 (real metric names, alert rules, contract test) |
| M4-05 | fixed | 9c66b1d |
| M4-06 | fixed | 90e5bb4 |
| M4-07 | fixed, partial | 6cb493f (`make ort`). Open: macOS ONNX Runtime sha256 is not pinned yet |
| M4-08 | fixed | c1b2a26 |
| M4-09 | fixed | c1b2a26 (CI Postgres job) |
| M4-10 | partly done | Linux container capacity, churn and fault-injection runs with fake providers (`docs/benchmarks/container-soak-churn-chaos.md`); 24 h soak dropped (calls last 2 to 3 minutes). Open: longer churn at real call lengths and real-provider runs |
| M4-11 | fixed | 5b49ad7 |
| M4-12 | fixed | 9c66b1d, d8ca2a0. Residual: `/ready` can flap under DB pool exhaustion |
| M4-13 | fixed | 9c66b1d |
| M4-14, M4-15, M4-16, M4-17 | fixed | 34a4747 (M4-16 also 6cb493f, 9c66b1d) |
| M4-18 | open | nginx reference config has no 503 retry (`proxy_next_upstream`) |

Found and fixed during round 7 verification (not in the original audit): idle speech re-entered 1 to 2 stages early when `dtmf.detect` or `audio.input_filters` was on (the TTS index was computed before those stages were inserted at the front of the pipeline); the `otelexport` queue-overflow test was timing dependent.

## Remaining before launch

- M4-10: longer churn at real call lengths (2 to 3 minutes) and real-provider runs; the Linux container capacity, churn and fault-injection runs are done (fake providers).
- M1-06, M1-07, M1-08, M1-09 (low).
- M4-18: nginx 503 retry for multi-instance edges.
- Pin the macOS ONNX Runtime sha256 in `scripts/fetch-ort.sh`.
- Google TTS `StreamingSynthesize` (look-ahead removes most gaps, not the unary first-byte cost).
- Per-tenant breaker keys (breakers are still process-wide per stage).
- Rolling upgrades: startup now fails when a stored agent uses a provider with no factory (runbook "Rolling upgrades").
- IPv6 /48 overflow-bucket starvation (M2-01 residual).
- `/ready` DB flap under pool exhaustion (M4-12 residual).
- Tool-result sizing against the context budget (M1-05 drops the reply from history instead of failing, but oversized results still waste budget).
- `provider=failover` metric label: failover-wrapped stages report under the wrapper, not the active provider.
