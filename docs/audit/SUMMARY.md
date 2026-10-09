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
