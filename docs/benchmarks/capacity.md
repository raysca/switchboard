# Load, soak and capacity (P5-4): fake providers, real transport and VAD

Captured 2026-10-07 on an Apple M4 Pro (12 cores), darwin/arm64, go1.27.1. **Machine-specific**: other processes were running on this host, and the load generator shares the CPUs with the server. Re-run on the target hardware before using any number for sizing.

## What this measures, and what it does not

`tools/loadtest` starts a real server child (`go build -tags fakeproviders ./cmd/server`, plus `vadworker`), provisions a fake agent through the management API with a generated key, and drives N concurrent calls over the real protocol: single-use voice token, WebSocket upgrade, binary protobuf frames (`transport/ws` encoders), RTVI `client-ready`/`bot-ready`, then a real-time 20 ms microphone stream. Each caller plays a real speech clip ("What is two plus two?", `e2e/fixtures/voice-debug.wav`, 1.6 s after silence trim), waits for the bot reply, thinks, and repeats.

Exercised: transport and pacing, input handling, Silero VAD in the `vadworker` child (onnxruntime), the VAD stop hangover, STT/LLM/TTS stages (fakes), output pacer, admission, metrics.

Not exercised, so numbers are a lower bound on cost and latency in production:

- **Provider I/O.** STT, LLM and TTS are fakes that answer instantly (`GOVOICE_FAKE_STT=audio` makes the fake STT turn any detected speech into a transcript; this env var only exists in the `fakeproviders` build). No network, TLS, codecs or provider rate limits.
- **Real audio diversity.** One clip, identical for every caller, so VAD work is uniform. A deterministic synthetic clip (`-speech-clip synthetic`, harmonic source through moving formants) also triggers Silero in this setup, but it is a proxy and was only checked in short runs.
- **Network.** Everything is on 127.0.0.1 from one source IP. The server limits upgrades to ~20/s per source IP (not configurable), so the harness spreads call starts at >= 100 ms apart.
- **Smart-turn, tools, MCP, barge-in.** Off by default. `-turn-manager` turns the turn manager on.
- **The load generator is on the same machine.** Its CPU competes with the server.

Client-side voice-to-voice is measured (Go monotonic clock, one process) from the last chunk of the clip sent to the first bot audio frame received. The clip keeps 100 ms of trimmed margin after the speech, so the figure starts up to 100 ms after the real end of speech. A turn that gets no reply within `-turn-timeout` (10 s) is counted as a timeout and also enters the percentiles at 10 s (censored, not dropped). It therefore contains the server's VAD stop hangover (about 0.7 s) and is not comparable with `baseline.md`, which starts at the VAD-stop event. The server's own `voice_latency_seconds{stage}` histogram is reported next to it, but its first bucket is 50 ms and the next ones are 100 ms wide, so those percentiles are interpolated bucket positions and not precise.

Load model: every caller streams real-time audio whether or not the server keeps up (the audio load is open-loop), but turns and calls are closed-loop: a caller starts its next turn only after the reply (or the timeout) and a failed call backs off 1 s. Under saturation the harness therefore issues fewer turns and calls instead of queueing them, so latency at overload is understated (coordinated omission). The capacity table is meant for the region below saturation, where that does not apply; an open-loop turn mode was not added.

## Commands

```
make loadtest-smoke                 # ~1 min, 4 sessions, fails on verdict; skips if third_party/silero_vad.onnx is missing
make loadtest-soak                  # 16 sessions, 5 min default, sampling every 30 s
make loadtest-soak SOAK_DURATION=24h SOAK_SESSIONS=32      # the P5-4 acceptance run (not run yet)
go run ./tools/loadtest -find-capacity -cap-steps 8,32,128,256,384 -step-duration 60s -md-out capacity-run.md
```

Pass extra flags with `LOADTEST_ARGS=...`. Key flags: `-sessions`, `-duration`, `-ramp`, `-turns-per-session`, `-think-time`, `-speech-clip`, `-vad-mode process|inprocess`, `-turn-manager`, `-soak`, `-find-capacity` (`-cap-steps` at most 8, `-step-duration`, `-cap-budget-ms`), and the verdict thresholds `-max-p95-v2v`, `-max-rss-growth-pct`, `-max-goroutine-growth`, `-max-error-pct`. Reports go to `-md-out` / `-json-out`. The harness kills the whole server process group on exit, on a panic in its main goroutine, and on SIGINT/SIGTERM, uses ephemeral ports and a temp dir (config file 0600), and never prints the generated management key or token secret (both go to the child through the environment, not argv). macOS has no parent-death signal, so a SIGKILLed harness leaves the server running; clean up with `pkill -f govoice-loadtest-` (the server's `-config` path) and its `vadworker` children with `pkill -f 'vadworker 8'`.

Sampling: server goroutines and heap come from the existing `/metrics` endpoint on the admin listener (`voice_process_goroutines`, `voice_process_heap_bytes`), so no new debug endpoint was added. RSS, CPU time and open file descriptors come from `ps` and `lsof` over the server's process tree (server plus `vadworker`).

Growth is the median of the first versus the last window of the post-warmup period (window = a quarter of it, at most 5 min). Goroutines are also compared with the idle count after all calls ended. A run too short for two windows reports growth as invalid, and a growth check then fails instead of passing silently.

## Capacity sweep (`-find-capacity`, 50 s per step)

Re-run 2026-10-07 after the two server fixes below (`-cap-steps 8,128,256,384 -step-duration 50s`), with other agents' builds and tests loading the host (load average 8 to 11 on 12 cores). p95 voice-to-voice budget: first step's p95 + 150 ms = 1111 ms. A step fails on p95 over budget, more than 1% errors, any admission reject, any server-side session failure or VAD worker restart, or load-generator saturation (pacing lag p99 over 100 ms).

| sessions | client v2v p95 ms | error % | cores at full load | harness lag p99 ms | result |
|---|---|---|---|---|---|
| 8 | 962 | 0 | 0.18 | 5 | pass |
| 128 | 962 | 0 | 1.02 | 5 | pass |
| 256 | 962 | 0 | 1.83 | 5 | pass |
| 384 | 963 | 0 | 2.50 | 5 | pass |

**Max sessions measured clean on this machine: 384** (the largest step tried; 512 was not run). CPU is 0.39 CPU-seconds per session-minute at 384 (2.5 cores), which extrapolates to about **154 sessions per core** (`sessions / cores at full load`). That is framework CPU only, with fake providers. Real deployments add TLS, provider clients and audio codecs; the runbook's planning figure (250 calls on 8 vCPUs) is not contradicted by it. CPU per session-minute is about 0.4 at 128 to 384 sessions but 1.3 to 1.9 at 4 to 8 sessions, because fixed server cost and call setup/teardown (a new call every 3 to 5 turns) are spread over more sessions.

At the 384 step: RSS 274 MB idle, 793 MB steady (about 1.4 MB per session above idle; a fresh process tree with four Silero workers already costs 274 MB), goroutines 18 idle, 4630 peak (about 12 per session), 18 after drain, open fds 97 idle, 481 peak, 97 after drain. At 4 to 16 sessions RSS above idle looks like 6 to 18 MB per session because the idle process has not grown its heap yet; use the large-step figure for sizing.

### The earlier 384-session failure: root cause

Before the fixes, 384 calls failed in most attempts (and in this review 2 of 2 unconstrained runs): `vadworker` restarts (all four replaced within 2 s), a few `vad_unavailable` admission rejects while every worker was down, and 1 to 2 "session failed: slow client (output audio queue budget exceeded)" per run, with latency flat. Evidence:

- **Not an undersized VAD pool.** Silero costs 76 µs per 32 ms window alone and 0.42 ms per batch of 8 (52 µs per window) on this CPU, single-threaded. 384 calls are 12,000 windows/s, under 1 core even unbatched, against a pool of 4 workers. The default `vad.workers: 4` was under 25% busy.
- **Not the per-IP limit or a slow load generator.** No rate-limit rejects, harness pacing lag p99 at most 2 ms, and `voice_transport_slow_client_terminations_total` stayed 0 in every run: the pacer never found the client too slow.
- **Host CPU stalls plus a hair-trigger watchdog (server bug, fixed).** The parent kills a worker that has not answered a batch in 100 ms of wall time (`ParentDeadline` 50 ms + `ParentGrace` 50 ms). The host was oversubscribed (server, 4 workers, a load generator using about 3 cores, and other agents at load average 7 to 13), so both processes were descheduled for 50 to 130 ms at times: instrumented runs showed the server's 10 ms ticker stalling 71 to 74 ms and `vadworker` batches that normally take 0.1 to 0.4 ms taking 55 to 126 ms. Two of three logged "hangs" had the parent's timer fire 101 to 104 ms late with the reply already in the pipe; the third reply came 19 ms after the deadline. Each kill and respawn costs a model load and, with all four down at once, readiness. `vad/worker_executor.go` now waits one more grace period after the deadline fires before declaring a hang, measured once the parent is running again. In a later instrumented run, worker batches of 122 and 126 ms no longer killed anything.
- **Hang-up counted as a failed session (server bug, fixed).** The 1 to 2 "session failed" lines landed exactly at the harness teardown. A client that hangs up while bot audio is being backpressured made `transport/ws` return the queue-full (slow client) error after the session was already ending, and the session recorded it as its failure. It now returns nil when the session context is done.
- Limiting the load generator (`GOMAXPROCS=2 nice -n 10`) before the fix also gave a clean run (0 restarts), which supports host contention as the trigger.

After the fixes: 4 of 5 unconstrained 384-session runs and the sweep above were clean. One run still lost all four workers at once at the end of the ramp, with a burst of about 10,000 stale VAD chunks in 2 s, which looks like a host-wide stall over a second long. The cause of that stall is not known; it was not instrumented. Treat 384 as "usually clean on a shared, busy laptop", not as a guarantee.

Fixed (T11): `voice_vad_stale_chunks_total`. A chunk is stale when it reaches a worker more than 100 ms after its audio arrived. Instrumented runs (4 calls) showed worker queue time of 2 to 30 µs but frame age at the VAD processor of 60 to 160 ms at p99: the delay was in the session, not the worker. Cause: caller audio and bot audio shared one FIFO inbox (`core.Session.data`), and once the pacer's 2 s output queue is full (TTS runs faster than real time, so it fills early in a reply) the transport blocks the single run-loop goroutine on every bot frame (`enqueueOutput`, 20 ms polls) to pace output. Up to 64 bot frames therefore queued ahead of each caller frame and VAD results (which re-enter through the same inbox), and VAD saw caller audio late during bot speech, which is exactly when barge-in matters. It is real head-of-line blocking, not harness burstiness. Fix: at most 2 bot audio frames may sit in the inbox (`maxQueuedOutputAudio`); the TTS producer waits for a slot. The slot is returned when the run loop dequeues the frame, so frames fenced by an interruption return theirs too. A producer that gets no slot within `MaxFrameProcessing` still ends the session with `ErrSlowOutput`, but that now happens after 2 queued frames rather than a full inbox. Ordering is unchanged. A separate caller-audio lane was tried first and rejected: it let audio overtake `UserStartedSpeaking` and broke the STT failover replay test. Result: smoke 38 to 44 stale per run down to 0 to 8; 384 sessions about 200/s down to about 18/s (908 over a 50 s step plus a short warm-up step). With only the cap, a caller frame could still wait behind one in-flight and two queued paced bot frames (about 20 ms each). Review follow-up: the session's output gate (`Session.SetOutputGate`, set by the ws transport to `PacedWriter.WaitRoom`) makes the TTS producer wait until the pacer has room for the frame plus the two the session may hold, so `enqueueOutput` normally no longer blocks the run loop. The gate is advisory: it gives up after `WriteTimeout`, and `enqueueOutput` still enforces the queue budget and slow-client detection. Smoke after the gate: 0 stale chunks in 4 of 4 runs, with client voice-to-voice unchanged (p50 941 ms, p95 962 ms, the same as the baseline). Re-run 2026-10-07 17:30 BST after the gate (full doc command, uncommitted tree on `194c741`, host load average about 3): all five steps 8/32/128/256/384 PASS, p95 voice-to-voice 962 ms at 384 (budget 1113 ms), 0 errors, 0 admission rejects, 0 session failures, 0 VAD worker restarts, 0 dropped and 0 stale VAD chunks, 0.37 CPU-seconds per session-minute (2.37 cores at 384), RSS 270 MB idle to 792 MB steady, 974 calls started at the 384 step, 0 failed. One clean run is not a guarantee; see the earlier caveat. Not measured: a barge-in latency scenario (not added).

## 2-minute smoke soak (`make loadtest-soak SOAK_DURATION=2m`, 16 sessions)

Verdict PASS with `-max-rss-growth-pct 10 -max-goroutine-growth 50 -max-p95-v2v 1500`.

| | value |
|---|---|
| calls / turns | 97 started, 461 turns completed, 0 timeouts, 0 errors, 0 rejects |
| client v2v p50 / p95 / p99 | 941 / 961 / 968 ms |
| RSS growth, first vs last 26 s window | +2.9% (idle 273 MB, steady 366 MB) |
| goroutines | 17 idle, 211 peak, 17 after drain, 0 growth between windows |
| open fds | 56 idle, 113 peak, 96 after drain, -1 growth between windows |
| CPU | 0.93 CPU-s per session-minute, 0.24 cores |

Open fds stay about 40 above the pre-load count after the first calls but do not grow between windows, so it reads as lazily opened server resources and not a leak. Only the 24 h run can confirm that.

The 24 h soak is **not run**. Command: `make loadtest-soak SOAK_DURATION=24h`. It writes `.govoice-loadtest-soak.json` and `.md` (git-ignored); acceptance is no growth in goroutines or RSS (the default thresholds above are 10% and 50) and zero errors.

## Baseline and regression gate (for P5-5)

The gate is `make perf-gate` (P5-5); policy, tolerances, baselines and caveats are in [README.md](README.md). The smoke baseline for this machine is `docs/benchmarks/baselines/darwin-arm64-12c.json` (machine-specific, `"machine_specific": true`). Direct use:

```
go run ./tools/loadtest -sessions 4 -duration 30s -ramp 3s -turns-per-session 3 -sample-interval 2s \
    -json-out current.json -compare docs/benchmarks/baselines/<label>.json [-tolerance-scale 1]
go run ./tools/loadtest -compare docs/benchmarks/baselines/<label>.json -current current.json   # compare only
```

Exit code 0 pass, 1 regression or failed verdict, 2 harness error. The per-metric tolerance table is `Tolerances` in `tools/loadtest/compare.go`. A baseline JSON with another schema version is refused.

## Findings

- Fixed (server): a normal client hang-up during bot playback was counted as `voice_sessions_failed_total` with a "slow client" error (`transport/ws` `enqueueOutput`). Test: `TestEnqueueOutputHangUpIsNotSlowClient`.
- Fixed (server): the VAD worker watchdog killed healthy workers when the host descheduled the server or the worker for about 100 ms (`vad/worker_executor.go` `awaitReply`). Test: `TestAwaitReplyToleratesParentStall`.
- Stale VAD chunks under load: mostly fixed, see above.
- Gate sensitivity (resolved in P5-5): client v2v is about 940 ms, of which about 700 ms is the constant VAD hangover, so a relative tolerance hides real regressions. The gate uses absolute allowances (+25 ms p50/p95) instead.
