# Pipecat parity: final report

**Scope:** server-side gaps vs Pipecat for a robust, fast, scalable voice server (no extra transports, no video, no Flows/bus/S2S).
**Plan:** [pipecat-parity.md](plans/pipecat-parity.md) · **Truth table:** [pipecat-parity-matrix.md](pipecat-parity-matrix.md) · **Tracking:** goalpulse goal `pipecat-parity-for-a-robust-fast-scalabl`.
**State:** committed in 23 slices on branch `feat/pipecat-parity` (base `194c741`); every commit passes `go vet` on all modules. `make verify` was green on the final tree (46 packages, 1m43s).

## What was done

48 tasks: the 36 planned (P0–P6) plus 12 added while working (T1–T12). Every task was built by one agent and then reviewed by an independent Opus agent that fixed what it found.

| Phase | Delivered |
|---|---|
| 0 Measure | Observer API, latency observers (STT/TTFT/TTFB/voice-to-voice, Prometheus + timeline), `pipecat.evals` black-box run over RTVI with fake providers, fake-provider bench |
| 1 Turn-taking | Start/stop strategy chains, Smart-turn ONNX worker (p95 25 ms), LLM turn-completion markers, user mute, user/bot idle controller, turn-quality gate, turn manager wired live (`turn.manager`) |
| 2 Providers | Fallback chains in agent config, LLM/TTS/STT failover (STT with audio replay), Deepgram STT, Cartesia + ElevenLabs TTS and STT, OpenAI-compatible LLM (SSRF-guarded), shared pools + optional pre-dial, shared TTS unit runner |
| 3 Conversation | Safe sentence aggregation, heard-text tracker + interrupted-reply reconciliation, parallel + async tools end to end, tool-round history ordering, live summarization |
| 4 Telephony audio | DTMF in/out + `send_dtmf`, input filter chain + pure-Go denoiser, output gain + ambience, call recording with retention, sink redaction |
| 5 Scale/ops | Per-agent/per-principal limits, rolling drain, OpenTelemetry (off by default), load/soak harness, perf CI gate with per-machine baselines |
| 6 Verification | Parity matrix with enforced Default column, 56 paired Pipecat-vs-Go fixtures, Whisker decision, this report |

## What independent review caught

Reviews changed the outcome on almost every task. The ones that mattered most:

- **Turn manager was not live** in real calls (found in P1-1). Wired in T1; the reviewer then found that after a barge-in the user's phrase never reached the LLM, fixed by rebinding STT streams to the new generation.
- **Smart-turn verdicts were discarded** on most ticks on the real clock (monotonic vs wall-clock cache key). Unit tests could not see it.
- **Core bug behind two flakes:** a processor returning `ErrSessionClosed` during a clean `Close` marked the session Failed. Fixed centrally.
- **Scale:** the 384-session failure was CPU contention exposing two server bugs (VAD watchdog killing healthy workers at 100 ms; hang-up counted as a slow-client failure), plus head-of-line blocking of caller audio behind faster-than-real-time bot audio (stale VAD chunks 200/s → 0).
- **Failover:** breaker-stranding resolver, late output from abandoned candidates, Gemini call/result ordering, STT replay losing audio on xAI, TTS wedges (watchdog added).
- **Security/privacy:** SSRF on user-supplied LLM base URLs, worker child inheriting server secrets, decrypted secrets left in memory on error paths, recording symlink/janitor/crash-recovery issues, redaction evasion via Unicode digits and non-idempotence.
- **Honesty fixes:** the "5 ms framework overhead" was macOS timer slop (real: 0.01 ms p50); the parity matrix initially over-claimed (now has an enforced Default column).

## Measured (this machine, fake providers, localhost)

- Framework overhead: p50 0.01 ms, p95 0.03 ms.
- Smart-turn inference through the worker: p95 25.4 ms.
- Load and capacity: see `docs/benchmarks/capacity.md` (latest 384-session sweep, CPU per session-minute, stale/dropped VAD chunks). Latency baseline: see `docs/benchmarks/README.md`; it is regenerated in G6.

These numbers are framework cost only. No real STT/LLM/TTS was called anywhere (cost rule).

## Not done / known risks

- **Perf baseline** was taken on an uncommitted tree and records no git commit; regenerated in G6 (`FORCE=1 make perf-baseline`).
- **`turn.manager` defaults off**; turn strategies, mute, completion, idle-after-turn behaviours need it. Flipping the default needs a real-STT e2e and a decision on the ~800 ms later end-of-turn.
- **No live-provider verification:** Deepgram, Cartesia, ElevenLabs and OpenAI-compatible defaults and protocols, and Smart-turn on real audio, were checked against docs and fake servers only; xAI STT/TTS need `-tags xaispeech`; the integration tests exist behind build tags and env vars. Google gRPC burst-replay for STT failover is unverified.
- **Not run:** 24 h soak (command documented), `docker build`, the CI workflow remotely, noisy real-audio denoiser evaluation, a real-LLM turn-quality evaluation (gate validates plumbing with fake oracles).
- **Open product decisions:** which extra vendors you want first (goalpulse Q1; I assumed Deepgram + Cartesia + ElevenLabs), per-agent vs server-wide scope for DTMF/filters/idle, whether the idle hang-up wording suits your clients.
- **Residuals listed in the matrix "Known gaps and risks":** recording is unencrypted and continues after an agent is disabled mid-call, DTMF digits remain in LLM context, failover timing is bounded (LLM first token 8 s, 60 s budget; Google/xAI STT open 2x5 s; shortened non-last retries; see the runbook table) but a started LLM stream is still cut at 30 s per round and reasoning models need a raised `llm_first_token`, admission is reject-only (distinct reason codes, close 1013), no provider emits tool-call frames (tool-first leniency is test-only), tool rounds run sequentially in production, usage metrics reach traces only, Deepgram/xAI STT export no counters, some raw error text still reaches three log/trace sites. The rare ONNX exit abort is attributed to ORT telemetry (now disabled; `vad.ShutdownORT` runs after ORT sessions close on the clean exit paths of the server, both workers and the vad/smartturn test mains, not on error exits or in the server test binary): plausible, unproven, Linux unverified. See the matrix for the ranked list of what Pipecat has that go-voice lacks.
- **Out of scope by design:** video, extra transports, Flows, bus/workers, speech-to-speech models, Whisker wire compatibility.

## Suggested next steps

1. Regenerate the perf baseline on the committed branch, then open the PR (the 23 commits are ordered core → transport/vad → providers → server → tools → docs).
2. Run the live integration tests per provider with real keys once; enable `turn.manager` on a staging agent with Smart-turn and compare turn quality.
3. Decide Q1 and the `turn.manager` default.
4. Run the 24 h soak (`make loadtest-soak SOAK_DURATION=24h`) on a quiet host.

## Round 1 and 2 follow-ups

Uncommitted on top of the 23 slices.

- **Changed:** Nvidia provider (NIM LLM through the OpenAI adapter, Riva STT/TTS over gRPC); Cartesia and ElevenLabs STT; server WebSocket ping liveness (`limits.ping_*`, replaces data-silence as the liveness signal); Smart Turn window now starts at the VAD speech start; idle prompt/goodbye now audible with Google TTS; no ERROR logs at hang-up (paced writer closed, STT `EOF`); late finals no longer lose `min_words` turns or the first half of a paused sentence; ORT telemetry disabled and ordered ORT shutdown; distinct admission reject codes; `tools/livecheck` live harness.
- **Live results (small n, one machine):** Google chirp_2/latest_long + Gemini + Journey with the turn manager, and Nvidia NIM/Riva, see `docs/benchmarks/turn-manager-live.md` and `docs/benchmarks/nvidia-live.md`. Manager default stop costs about +0.6 s voice-to-voice (not default-on); Nvidia full turn 2.1 to 3.6 s voice-to-voice (LLM-bound).
- **Open:** Smart Turn v3.2 misjudges the measured clips (model); `chirp_2` sends no interims so `min_words` gives no barge-in; NIM reasoning-only models and 404/410 catalog entries; xAI STT breaker pattern differs; `PingMaxFailures` has no YAML key; abandoned tabs hold a slot until `max_call_duration` unless `idle.user_timeout` is set; Cartesia, ElevenLabs, Deepgram and xAI never run live. Full list: matrix "Known gaps and risks".

