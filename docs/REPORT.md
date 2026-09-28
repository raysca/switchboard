# Go Voice Runtime POC — Findings & Verdict

Host: Apple M4 Pro (8 P + 4 E cores), 24 GB, macOS · Go 1.27.1 · ONNX Runtime 1.29.0 · Silero VAD v5 · Pipecat 1.12.0 (baseline).
Plan: `PLAN.md` (independently reviewed: *approve with changes* → revised → *approved*). Raw data: `results/*.csv`, graphs `results/g*.png`, logs `results/*_log.txt`.

**SLO (fixed before runs):** per-chunk VAD latency (chunk due → result applied in session) p99 ≤ 50 ms, dropped/stale ≤ 0.1 %, worst per-session p95 ≤ 100 ms, onset miss ≤ 1 %, false onsets ≤ 1 per session-minute. Ceiling claims only from ≥ 30 s windows.

## Verdict per hypothesis

| | Result | Key evidence |
|---|---|---|
| **H1 startup** | **PASS** | Warm session ready **p50 1.2–2.7 µs, p99 ≤ 12 µs**, flat from 0 → 5,000 existing sessions (`t1_startup.csv`). Over HTTP: p50 0.04–0.07 ms (`t1_http.csv`). Cold exec → first session 165 ms. Python: spawn→READY **p50 360 ms**, warm forkserver **55.6 ms**. Burst of 1,000 simultaneous calls: p99 4.7 ms, running calls' VAD p99 unchanged (`t2_burst.csv`). |
| **H2 resources** | **PASS** (note) | Marginal cost **19.5 KB phys_footprint / 16.8 KB heap per connected session** (0→10k) vs Python **88.8 MB** (spawn) / **59 MB** (forkserver) per call — ≈3,000–4,500× lower. 20,002 churn cycles: heap, footprint, goroutines, VAD registrations flat; 0 failed/leaked; teardown p99 < 0.5 ms (`t4_churn.csv`). Note: Go returns freed heap to its own allocator, not promptly to the OS (RSS after 10k→0 teardown stays ~360 MB, heap back to 13 MB). |
| **H3 containment** | **PARTIAL PASS** | At 4,000 streams (80 % of ceiling), 9/11 fault modes contained (targets failed/stopped in 17 ms–3.6 s, 0 healthy failures, healthy p99 inside control noise floor) (`t56_containment.csv`). **Fails by design:** `hang` / `badteardown` (code ignoring cancellation) leak — detected, counted exactly, never reclaimed. **Native crash** (cgo null deref) kills the whole process and all calls (`isolation.csv`). |
| **H4 VAD real-time** | **PASS** | Best config (sharded, 11 shards, micro-batch 8): **5,000 active streams** within SLO (30 s window: p99 17.8 ms, 0 drops, 100 % onsets); collapses at 5,500. Knee = VAD worker saturation (workers 98 % busy, CPU 96 %); inference p99 stays ~1 ms, queue wait explodes. Stub control (fixed 75 µs) saturates exactly where arithmetic predicts → knee is real, not harness. |
| **H5 capacity** | **PASS** | See capacity table below. |
| **H6 admission** | **PASS** | Offered 7,500 calls (1.5× ceiling). Off: all admitted, **p99 105 ms, 35.7 % audio dropped for everyone**. On (static cap + VAD queue-wait feedback): 5,000 admitted, 2,500 rejected, **p99 6.8 ms, 0 drops** (`t11_*`). |

## Capacity (this host)

| Number | Value |
|---|---|
| Technical max (process still functions) | > 10,000 connected; VAD degraded beyond ~5,500 continuous streams |
| **Active-VAD real-time ceiling** (every chunk inferred) | **5,000 streams** |
| Connected-session RT ceiling, energy gate on | 10 % speech duty: **≥ 32,000** (grid max, CPU 97 %) · 25 %: **18,000** · 50 %: **10,000** (`t9_*.csv`) |
| Combined load (fake STT/LLM/TTS on every turn) | still 5,000 (`t10_combined.csv`) |
| **Recommended production** | **3,500–4,000 continuous VAD streams / host** (70–80 %); ~13–14k connected calls if lines are quiet enough for the gate at ~25 % duty. Enforce with admission control. |

## Scheduler comparison (T8, `g7_scheduler.png`)

| Config | RT ceiling | Note |
|---|---|---|
| sharded 11 × batch 8 | **5,000** | best tails (p99 5.5 ms @ 4,500) |
| global 11 × batch 8 | 5,000 | same ceiling, worse tails (26 ms @ 4,500) |
| global 8 × batch 8 (P-cores only) | 4,000 | tightest tails (4.5 ms @ 4,000): E-core workers inflate tails |
| **process pool 8 × batch 8 (Strategy C)** | **3,500** | ≈12 % below in-process equivalent; **survives SIGKILL of workers**: 2,800 calls, 3 kills → 0 calls lost, 24 in-flight chunks dropped |
| batch 1 (global/sharded) | 2,000–3,000 | |
| global 4 × intra-op 3 | 2,000 | ORT internal threading loses to Go-owned concurrency |
| Python, 12 procs, batch 1 (bare ORT loop, no pipeline) | 3,252–3,624 | `python_vad.csv` |

Honest reading: Go at batch 1 *with the full session runtime + load generator on the same cores* is below a bare Python ORT loop. The 5,000 comes from **cross-session micro-batching**, which a shared runtime enables and process-per-call cannot do.

## Answers to the decision questions

1. **Model preserved without per-component cost?** Yes. Processor/Emitter/Direction API, one run loop per session, processors called inline: +20 ns per processor vs +130 ns for goroutine+channel (3.2× cheaper at 16 processors; `q1_pipeline.csv`).
2. **Warm ready?** ~2 µs in-process, ~0.05 ms over HTTP.
3. **Flat as the process fills?** Yes, to 5,000 existing sessions; 1,000-call bursts p99 4.7 ms.
4. **Marginal cost?** ~20 KB/session + ~1 goroutine; OS threads stay ≤ 31.
5. **Containment?** Yes for all managed-code faults incl. stalls, floods, overflow, VAD abuse and cancellation; no for ctx-ignoring code (leaks, surfaced) and native crashes (process dies).
6. **Connected sessions?** ≥ 10k trivially (memory); real-time bound by VAD duty cycle (table above).
7. **Active VAD streams at p99 SLO?** 5,000.
8. **Why it degrades?** VAD worker CPU saturation → queue wait → stale drops; sharp cliff (5,000 fine, 5,500 collapsed). Not inference slowdown, not scheduler overhead, not load generator.
9. **Global vs sharded vs process?** Sharded in-process for capacity/tails; process pool costs ~12 % but contains native crashes.
10. **Admission control?** Yes — prevents collapse cleanly.
11. **Advertise per host?** 3,500–4,000 continuous VAD streams.
12. **Build it?** Yes.

## Recommendation: **Outcome B (hybrid)** — Go sessions + small local VAD worker-process pool

The session runtime is decisively validated (µs startup, KB per call, flat churn, managed-fault containment). The one architectural risk is native code: a single ORT/cgo fault in-process kills ~4,000 calls, whereas the process pool turns it into 24 dropped chunks and an automatic restart, for ~12 % capacity. Take Outcome A only if ORT is proven crash-free in production soak; the executor interface makes the switch a config flag.

## Findings the POC surfaced (and fixed during the run)

- **Budget calibration bug:** audio-shedding limit (50/s) exceeded the 31 frames/s arrival rate, so a stalled session could never trip it. Fixed (10/s), unit test now feeds at real-time rate; slowsync then contained in 3.6 s.
- **Harness artefacts caught and removed:** forced `runtime.GC()` before control windows inflated the noise floor; synchronized session cohorts produced fake bursts in T9 (kept as `t9_duty0.1_cohort_artifact.csv`, itself a warning about admitting large bursts). 8 s windows understated tails near the knee → all ceilings re-confirmed at 30 s (`t8refine_*.csv`).
- A permanently stalled but undetected session does **not** degrade neighbours (reproduced with detection disabled).

## Limitations
macOS laptop, not Linux server hardware (P/E asymmetry); load generator in-process on the same cores; speech from macOS TTS voices; STT/LLM/TTS are fakes; `Close()` goroutines of leaked sessions are not counted in the managed-goroutine gauge (sessions are); per-session drop stats in containment include targets' own drops (~0.1 %).

The benchmark commands cited by the original POC (`scripts/run_t8.sh`, `scripts/run_phase2.sh`, `scripts/plot.py`, `cmd/bench`) are not in this tree. Numbers in this report are not reproducible from the current repository.
