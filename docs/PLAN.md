# Go Voice POC — Implementation & Verification Plan

Source spec: `poc.md`. Host: Apple M4 Pro, 12 cores (8P+4E), 24 GB, macOS, Go 1.27.1.
This is a hypothesis-testing POC: code is disposable, evidence is the product.

## 0. Pre-flight facts (already verified)

| Item | Result |
|---|---|
| ONNX Runtime | 1.29.0 osx-arm64 official tarball in `third_party/` (matches `yalue/onnxruntime_go` v1.36.0 headers) |
| Silero model | `silero_vad.onnx` v5 from snakers4/silero-vad (sha256 1a153a22…). IO: `input[B,576]` (512 new + 64 context), `state[2,B,128]`, `sr` int64 → `output[B,1]`, `stateN[2,B,128]`. **Batch dim is dynamic.** |
| Go inference smoke | 74 µs/inference, batch 1, intra-op=1 → one core ≈ 430 real-time streams theoretical |
| cgo link | needs `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk` (CLT 27 SDK tbd files break the linker). Put in Makefile. |
| Speech samples | macOS `say -o x.wav --data-format=LEI16@16000` (multiple voices/sentences) → realistic speech with known onsets |
| Python | `.venv` (3.12) with onnxruntime 1.29.0 (same ORT version as Go) |

## 1. Key design decisions (architect's calls where the spec is open)

1. **Execution model: one run-loop goroutine per session, processors called synchronously.**
   Session has a bounded inbox of `envelope{frame, dir, fromIndex}`. The run loop pops an envelope and calls
   `procs[i].ProcessFrame(ctx, f, dir, emitter)`; `emitter.Push` calls the next processor *inline* (downstream i+1 / upstream i-1).
   No goroutine/channel per processor. Async boundaries (VAD results, fake STT/LLM/TTS provider calls) re-enter the pipeline by
   posting an envelope back into the session inbox at the correct processor index → processor code is never concurrent with itself
   (Pipecat-like single-threaded-per-session semantics without locks in processors).
2. **Queue policy by frame class.** Inbox = two bounded channels: `control` (lifecycle/system/error; never dropped — overflow ⇒ fail session)
   and `data`. Data overflow: `AudioFrame` ⇒ drop + count (stale audio is worthless); other data frames ⇒ fail session (budget breach).
   Sustained audio drops beyond `MaxDroppedAudio` per window ⇒ fail session ("stopped consuming").
3. **Budget (`ResourceBudget`)**: `MaxQueuedFrames`, `MaxAsyncTasks`, `MaxPendingVAD` (default 1), `MaxBufferedAudio` (default 256 ms),
   `MaxEmitsPerFrame` (runaway emit guard, default 64), `MaxFrameProcessing` (watchdog, default 2 s), `TeardownTimeout` (default 2 s).
4. **Supervisor**: `session.Go(fn)` is the only way to spawn session work: recover panics → fail session; errors → fail session; counts
   against `MaxAsyncTasks`; `Stop()` cancels ctx and waits ≤ TeardownTimeout. Goroutines still alive after that are **leaked** and counted
   (host metric `leaked_goroutines`, session marked `leaked`). Go cannot kill a goroutine — the POC must surface, not hide, this.
   A watchdog fails a session whose run loop is stuck in one ProcessFrame longer than `MaxFrameProcessing`.
5. **VAD processor** is an ordinary `Processor`. Streaming Silero is sequential (chunk n+1 needs state n), so per-session outstanding
   inference is naturally 1; extra chunks buffer up to `MaxBufferedAudio`, then oldest are dropped as stale (counted).
   State (`[256]float32` + 64-sample context) lives in the session; jobs carry it; workers are stateless → any worker (or process) can run any job.
6. **VAD executors** (one `Executor` interface, three implementations = legitimate):
   - **A global**: one bounded MPMC queue → N workers, each owning one ORT session + pre-allocated tensors.
   - **B sharded**: `hash(sessionID) % S` → per-shard bounded queue → 1 worker per shard.
   - **C process pool**: M `cmd/vadworker` child processes over stdin/stdout binary protocol; crash ⇒ in-flight jobs fail as dropped, worker respawned.
   - **Micro-batching** (dimension for all three): worker drains up to `BatchMax` ready jobs and runs one batched inference (tensors pre-allocated per batch size).
   - Every executor enforces: bounded queue (reject when full), per-session outstanding limit (reject abuse), deadline check at dequeue (drop stale), queue-wait/inference timestamps.
   - ORT: intra-op=1, inter-op=1, sequential execution; Go owns concurrency. One extra config (fewer workers × intra-op 3) as explicit dimension.
7. **Energy gate** (configurable, default off for capacity tests): chunks below an RMS floor skip inference. This is what makes
   "connected" ≠ "active VAD stream" (T9). Reported both ways, because a noisy line defeats it.
8. **Detector**: Silero probability → start when p ≥ 0.5 for 2 consecutive chunks (64 ms), stop when p < 0.35 for 800 ms. Emits `SpeechStartedFrame`/`SpeechStoppedFrame`.
9. **Latency SLO (agreed now, before runs)**: per-chunk VAD latency = *chunk due time → VAD result applied in session*.
   **p99 ≤ 50 ms, stale/dropped chunks ≤ 0.1 %, worst per-session p99 ≤ 100 ms.** Chunk = 512 samples = 32 ms at 16 kHz.
   Speech-onset detection latency (known onset → SpeechStarted emitted) is reported separately and decomposed into algorithmic + system delay.
10. **Load generator runs in-process** (bench driver) with a few feeder goroutines (not one per session), each session phase-staggered.
    "Due time" is the scheduled wall-clock time of a chunk, so feeder lag counts against us (honest). Audio frames reference shared
    read-only clip buffers (no per-session copies). Timeline per session is deterministic from a seed: silence (low noise) + speech clips at duty cycle d.
11. **Metrics**: a lock-free log-bucket histogram (≈3 % resolution) used for all latencies incl. per-session; Prometheus-text `/metrics`
    (no deps). Resource sampler: RSS & threads via `ps`, FDs via `/dev/fd`, `runtime/metrics` for heap/objects/GC pauses/goroutines, CPU via getrusage (+ child processes via `ps` for C).
12. **Python baseline**: `pipecat-ai[silero]` (+ `openai` import) bot per process: import, build a Pipeline with `SileroVADAnalyzer`, print READY.
    This is the production-relevant process-per-call baseline (not a bare interpreter). VAD baseline: same model/ORT version, intra-op 1, one process per core.

## 2. Layout

```
Makefile                 build/test/bench targets, SDKROOT, ORT lib path
voice/                   frame.go pipeline.go session.go manager.go (registry+admission) fakes.go metrics.go audio.go(loadgen timeline)
vad/                     silero.go (ORT wrapper, batched runner) executor.go (A,B) procpool.go (C) processor.go (VADProcessor+detector)
cmd/server/              HTTP control surface: POST/DELETE/GET /sessions, POST /sessions/:id/fault, GET /metrics
cmd/vadworker/           Strategy C worker process
```

The T1–T11 harness (`cmd/bench`, `scripts/run_t8.sh`, `scripts/run_phase2.sh`, `scripts/plot.py`) is not in this tree. Do not treat those commands as a way to reproduce capacity numbers. `REPORT.md` is a historical verdict; its reproduce commands are not in this tree.

## 3. Implementation milestones (maps to spec §15)

| M | Deliverable | Done when |
|---|---|---|
| M0 | `vad/silero.go` batched runner + `executor.go` A/B + microbench | streaming state test passes (speech clip → p>0.5 in speech, <0.2 in silence; batched == unbatched results within 1e-4) |
| M1 | frames, Processor/Emitter, Pipeline routing up/down, fakes | unit tests: ordering, upstream routing, consume, multi-emit, async re-entry index |
| M2 | Session/Supervisor/Budget/Manager | unit tests: panic recovery, child panic, runaway emit, queue overflow policy, teardown timeout → leaked counted |
| M3 | VADProcessor + detector + loadgen | onset detected on synthetic timeline with known onsets; decomposition timestamps populated |
| M4 | Fault injection (HTTP + bench) | each fault mode reproducible |
| M5 | T1/T2/T3 + Python startup/memory baseline | CSVs |
| M6 | T7/T8/T9 | CSVs |
| M7 | T4 churn + T10 combined | CSVs |
| M8 | T11 admission | CSVs |
| M9 | Strategy C + worker-crash demo + in-process native-crash demo | CSVs |
| M10 | plots + REPORT.md | report answers all 12 decision questions |

`go vet ./...` and `go test -race ./voice/...` must pass (race detector on the non-cgo runtime).

## 4. Verification plan (tests, pass bars fixed before the run)

Durations are sized to finish in ≈45 min total on this host; each plateau: 3 s warmup + ≥10 s measurement.

| Test | Procedure | Measured | Pass bar |
|---|---|---|---|
| T1 warm startup sweep | hold 0,1,10,50,100,200,500,1000,2000 sessions; create 200 samples serially at each plateau | stage timings (allocated→pipeline→registered→ready) p50/p95/p99 | p99 ≤ 5 ms at every plateau; p50@2000 ≤ 2× p50@1 |
| T1 cold | exec `cmd/server` → first session ready (incl. ORT init + model load) | wall ms | reported (no bar) |
| T1 Python | spawn pipecat bot at 0,5,10,25 existing bots | spawn→READY | Go warm p50 ≥ 50× faster |
| T2 burst | bursts 10/50/100/250/1000 concurrently while 200 sessions stream audio | ready p50/p95/p99, running sessions' VAD p99 during burst | burst-250 p99 ≤ 50 ms; running p99 stays within SLO |
| T3 memory | ramp idle-connected sessions (VAD registered, pipeline built) 0→5000; also streaming sessions 0→500 | RSS, heap, goroutines, threads, FDs, marginal/session; teardown return | marginal ≤ 1 MB/session (≥50× below Python bot RSS); goroutines back to baseline ±5 after teardown |
| T3 Python | 1,5,10,25 bot processes | total RSS, marginal | reference |
| T4 churn | 200 streaming sessions, continuously replace ≥ 20 000 cycles | RSS/heap/goroutines/threads/active VAD states vs time, teardown latency | slope of heap & goroutines over 2nd half ≈ 0 (heap growth < 10 %, goroutines ±5); VAD state count == live sessions |
| T5 panic | 200 healthy streaming sessions; inject `panic`, `child-panic` into 5 targets | target state, healthy VAD p99 & errors before/after, goroutines | target failed ≤ 1 s; healthy p99 delta ≤ max(20 %, 5 ms); healthy errors 0; host alive; goroutines back to baseline |
| T6 abuse | same, faults: `flood` (runaway emit), `overflow` (inbox flood), `slow` (200 ms/frame), `stall` (async blocks until ctx), `hang` (ignores ctx), `error`, `vad-abuse`, `cancel-during-work`, `bad-teardown` | as T5 + queue depths, drops, leaked count | same as T5; `hang`/`bad-teardown` ⇒ session reported `leaked`, leaked goroutines exactly accounted & bounded; no host queue/heap growth |
| T7 VAD sweep | active streams 50,100,200,400,800,1200,1600,2000,2500,3000… until clearly broken; 100 % speech-activity (gate off) | queue wait/inference/total p50/p95/p99, per-session p99 distribution, CPU, queue depth, drops | find SLO ceiling (H4 passes if a ceiling exists and is reported with the knee explanation) |
| T8 scheduler compare | A{4,8,11 workers}×{batch 1,8}, B{8,11 shards}×{batch 1,8}, C{4,8 procs}×{batch 8}, A-intraop3 | ceiling per config under SLO + fairness | pick highest SLO ceiling that also keeps fairness bar |
| T9 duty cycle | best config; connected 1000@10 %, 1000@25 %, 500@50 %, N@100 % with energy gate on & off | SLO met?, CPU | reported matrix |
| T10 combined | T7 near-ceiling + fake STT/LLM/TTS (async, 50–300 ms latencies, TTS audio out) | VAD p99 delta vs T7 same load | VAD p99 still ≤ SLO |
| T11 admission | ramp +20 sessions/s to 1.5× ceiling, admission off vs on (static cap from T7 + feedback on VAD p99/queue-wait/CPU) | admitted p99, rejects, CPU | off: SLO violated; on: rejects > 0 and admitted sessions within SLO |
| Isolation limits | (a) in-process server with 50 sessions, fault `native-crash` (cgo null deref) ⇒ whole process dies; (b) Strategy C, `kill -9` a VAD worker under load | sessions surviving, drops, respawn time | (a) documents the limit; (b) all sessions survive, only in-flight jobs dropped |
| Python VAD | 1..12 processes each running K streams real-time | per-inference µs, streams/core | reference vs Go |

Every claim in REPORT.md must cite a file in `results/`.

## 5. Known risks / honest limitations to report
- Load generator shares the process/CPUs with the runtime (a real transport would too; noted).
- macOS, not Linux production hardware; P/E core asymmetry affects tail latency.
- Energy gate numbers depend on synthetic noise floor.
- Goroutine leaks from ctx-ignoring code are detectable but not reclaimable.

---

## 6. Revision 2 — changes from independent review (all accepted)

1. **Containment under real load**: T5/T6 run at ~80 % of the chosen config's T7 SLO ceiling, and every fault run is paired with a **no-fault control run** (same seed, same duration) that gives the noise floor. Bar: healthy p99 delta vs control ≤ max(20 %, 5 ms).
2. **Load-generator contamination controls**: feeders use non-blocking sends only (a stalled session can never delay a feeder); **feeder lag** (actual send − due) recorded as its own histogram; **stub-VAD control run** (fixed 75 µs busy-wait instead of ORT) measures harness latency. A knee counts only if queue-wait/worker CPU explain it, not feeder lag.
3. **VAD accuracy**: every VAD run reports **missed onsets** and **false onsets** vs known onset timestamps (onset matched if SpeechStarted within [onset−100 ms, onset+600 ms]). Bar at any claimed ceiling: miss ≤ 1 %, gate on and off.
4. **T9 = capacity search per duty cycle** (gate on): largest connected-session count meeting SLO at 10/25/50/100 % duty. Yields the capacity-vs-duty graph and the connected-session ceiling.
5. **Windows**: 30 s measurement at plateaus near the knee (≥ 900 samples/session). Fairness bar = **worst per-session p95 ≤ 100 ms** plus global p99 ≤ 50 ms.
6. **Time budget**: coarse grid for all T8 configs, refine top 2–3 near the knee. Each config runs in a **fresh child process** (bench re-execs itself). T11 starts at 70 % of ceiling, ramps +100 sessions/s. Expected total ≈ 90 min.
7. **Fair Python baseline**: memory via `phys_footprint` (`footprint`), not summed RSS; for Go use the same tool. Startup adds a **forkserver baseline** (warm parent with pipecat pre-imported forks a child that builds the pipeline) next to cold spawn.
8. **Evidence map** (§7) added; **Q1 evidence**: per-frame pipeline overhead vs processor count (1/4/8/16 sync processors) microbenchmark, vs a goroutine+channel-per-processor variant.
9. **Leaks are a partial fail**: `hang` and `bad-teardown` fail H3.2 (resources not reclaimed); reported as a documented limitation with exact leaked-goroutine accounting, never as pass.
10. **Defined details**: job deadline = due + 100 ms; stale/rejected chunks are excluded from latency histograms and counted against the ≤ 0.1 % drop bar. T4: 200 sessions, lifetime uniform 1–3 s (≈100 cycles/s), ≥ 20 000 cycles (~3.5 min).

Also adopted: T1 also measured via HTTP `POST /sessions` (apples-to-apples with spawn→READY); queue policy keyed on direction+type (outbound TTS audio never shed like inbound); admission = static cap + queue-wait feedback only; memory sampled in-process via `runtime/metrics` with `ps`/`footprint` at ≤1 Hz only in resource tests; T8 matrix trimmed to A{8,11}×{b1,b8}, B{11}×{b1,b8}, C{4,8}×b8, A-intraop3. Report worker 8 vs 11 as P-core-only vs all-core. Prometheus `/metrics` kept (cheap, spec lists it).

## 7. Evidence map

| Question / number | Test | Output file | Bar |
|---|---|---|---|
| Q1 model preserved w/o per-component cost | pipeline microbench | results/q1_pipeline.csv | sync pipeline ≥ 5× cheaper/frame than goroutine-per-processor; API = Processor/Emitter/Direction |
| Q2 warm ready | T1 | results/t1_startup.csv | p99 ≤ 5 ms (in-process), HTTP reported |
| Q3 flat with N existing | T1, T2 | t1_startup.csv, t2_burst.csv | p50@2000 ≤ 2× p50@1; burst-250 p99 ≤ 50 ms |
| Q4 marginal cost | T3, Python | t3_memory.csv, python_startup.csv | ≤ 1 MB/session and ≥ 50× below Python (footprint) |
| Q5 containment | T5, T6, isolation demos | t56_containment.csv, isolation.csv | per §4 + rev 1/9 |
| Q6 connected ceiling | T3 (technical), T9 (real-time) | t9_duty.csv | reported |
| Q7 active-VAD RT ceiling | T7/T8 | t7_*.csv, t8_summary.csv | SLO + accuracy + fairness |
| Q8 why it degrades | T7 decomposition + stub control | t7_*.csv | knee explained by queue wait / CPU / inference |
| Q9 global vs sharded vs process | T8 + crash demo | t8_summary.csv, isolation.csv | highest SLO ceiling + isolation |
| Q10 admission | T11 | t11_admission.csv | off violates SLO, on holds |
| Q11 advertise per host | derived | REPORT.md | 70–80 % of RT ceiling |
| Q12 build it? | all | REPORT.md | verdict A/B/C |
| H5 technical max / RT max / recommended | T3, T7, T9 | as above | reported |

**Re-review: APPROVED.** Minor points adopted: onset-delay distribution reported next to miss rate; false-onset bar ≤ 1 per session-minute; Q1 reports the measured ratio (5× is indicative, not a fail line); no ceiling/fairness claim drawn from 10 s plateaus.
