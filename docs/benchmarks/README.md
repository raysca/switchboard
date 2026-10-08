# Performance gates (P5-5)

`make perf-gate` fails when latency, capacity cost or leaks regress against a committed baseline **for the same machine class**. Nothing is ever compared across machines: a baseline is a file named after a label, `docs/benchmarks/baselines/<label>.json` (loadtest) and `<label>.bench.json` (framework bench). The label is `PERF_BASELINE` if set, else `<os>-<arch>-<cpus>c` (`darwin-arm64-12c`). The loadtest comparison also refuses a baseline whose OS, arch, CPU count, CPU model (unknown on either side counts as different) or run shape differs from the current run; the bench compares OS/arch/CPU count and run shape only, so an Apple M1 vs M4 with the same core count is caught by the loadtest half of the gate, not the bench half.

## The two gates

| gate | what runs | proves | does not prove |
|---|---|---|---|
| framework overhead (`tools/eval/cmd/govoice-bench`, 4 sessions x 200 turns, zero delay) | in-process `core.Session` with fake LLM/TTS; overhead = stop-to-first-audio minus configured delays | the frame pipeline did not get slower (p50/p95/p99 within +1/+3/+6 ms), and no goroutine leak per run | VAD, STT, transport, real providers |
| loadtest smoke (`tools/loadtest`, 4 calls, 30 s) | real server child (`-tags fakeproviders`), real WebSocket protocol, Silero in `vadworker`, a real speech clip | client voice-to-voice, per-stage server latency, CPU per session-minute, MB per session, goroutine/fd/RSS leaks, errors | provider latency, network, TLS, codecs, other audio, more than 4 concurrent calls |

Both use fake providers on localhost with the load generator on the same host. They catch framework regressions only; they say nothing about what a user hears with real providers.

## Tolerances

One data-driven table, `Tolerances` in `tools/loadtest/compare.go` (limit = baseline x (1 + rel%) + abs); the bench's allowances are `tol` in `tools/eval/cmd/govoice-bench/compare.go`. Unit tests: `go test ./tools/loadtest ./tools/eval/cmd/govoice-bench`.

| metric | allowance | why |
|---|---|---|
| client v2v p50 / p95 | +25 ms absolute | ~700 ms of the ~940 ms is constant VAD hangover; a 25% tolerance would hide ~235 ms. 11 runs on the baseline machine (load average 5-6 from other work): p50 940.8 to 942.3, p95 960.6 to 962.5 ms. +25 ms is below one 32 ms VAD window |
| client v2v p99 | +40 ms | max of ~30 samples; spread 961.3 to 963.3 ms |
| server stage p95 | +100 ms | histogram buckets are 50/100 ms wide (one bucket is the resolution); with zero fake delay llm_ttft/tts_ttfb sit inside the first bucket (47.5 is interpolation), so these rows only catch a stage that jumps a bucket. Sub-ms framework regressions are the bench's job, which is the VAD-hangover-free signal |
| CPU per session-minute | +30% +0.05 | measured 1.37 to 1.83 over 5 runs (baseline 1.83); shared with the generator |
| MB per session | +25% +2 | measured 17.7 to 20.3 (baseline 20.3) |
| error % | +1 point | |
| goroutines / fds after drain | +5 / +10 | leak caps |
| RSS / goroutine growth windows | +10 points / +10 | a 30 s smoke has 7 s windows dominated by warmup (RSS growth reads 12 to 14%); the real growth check is the soak |
| bench overhead p50/p95/p99 | +1 / +3 / +6 ms | measured 0.008-0.009 / 0.04-0.08 / 0.20-0.24 ms; scheduler noise on a loaded host |

Also failing, never warnings: a metric missing from the current run (stage gone, no CPU sample, no growth windows, no completed turns), the run's own verdict failing, a machine or run-shape mismatch. `PERF_SCALE=2` doubles every allowance for a noisier host.

Validation: the gate passed 5 of 5 consecutive runs on the baseline machine, and again after the v2v allowance was tightened to +25 ms. `make perf-gate-selftest` (about 2 min) shows both gates PASS clean and FAIL with an injected delay: `GOVOICE_FAKE_LLM_DELAY=100ms` (read only by the `fakeproviders` server build, `server/registry_fake.go`; every fake LLM chunk waits, which added about 200 ms to v2v) and `govoice-bench -inject-delay 100ms`; each injected run must exit exactly 1 and fail a latency row (v2v_p50 / overhead_p50), not some other row. A regression under roughly 25 ms of v2v or a few ms of framework overhead is not detected.

## Commands

```
make perf-gate              # ~1 min; exit 1 on regression, 2 on a harness error (a bug, never retried). Reports in .perf-out/ (git-ignored)
make perf-baseline          # write this machine's baseline; refuses to overwrite without FORCE=1
make perf-gate-selftest     # prove the gate can fail
PERF_BASELINE=<label> make perf-gate
```

Missing baseline for this label: locally it prints instructions and exits 0; with `CI=true` it exits 1, unless `PERF_ALLOW_MISSING=1`, in which case it runs, writes candidate baselines to `.perf-out/baselines/` and exits 0 (not a pass/fail result).

## Updating a baseline

Do it on an otherwise idle machine of the class you want to gate, from a clean tree at the revision you want to bless: `FORCE=1 make perf-baseline`, run `make perf-gate` twice, then commit the two JSON files with a message naming the reason. The JSON does not record the git revision, so the commit that adds it is the record. A baseline must never be regenerated to make a failing gate pass without understanding the regression. The committed `darwin-arm64-12c` baseline was regenerated 2026-10-07 23:38 BST (Apple M4 Pro, load average 1.6 at start) on the settled tree, which was uncommitted on top of base commit `52282ac`; `make perf-gate` passed 3 of 3 against it (and 5 of 5 against the previous baseline on the same tree). CPU per session-minute on that quiet machine ranged 1.84 to 1.89 over 8 runs (run-to-run spread about 1.5%, far inside the +30% +0.05 allowance); the 2.42 seen once earlier was a loaded-machine reading (load average 3+ from concurrent builds/tests), not a regression. It must be regenerated with `FORCE=1 make perf-baseline` once the tree is committed.

### CPU baseline history

`cpu_s_per_session_min` is the only host-sensitive row (30% + 0.05 tolerance). After round 2 the gate read 2.43-2.47 against 1.85, which looked like +32%. Bisected on 2026-10-08 (load average 2-4 from the desktop, so not quiet) with the exact smoke flags, one worktree build per commit, interleaved 5 rounds: medians 52282ac 2.28, 3cf7752 2.33, 380599a 2.22, 264ecfb 2.29, 5a9f54a (HEAD) 2.32; each commit spans about 1.9-2.5 between runs. The baseline commit itself reads 2.28 on that host, so the rise is the host (background load, efficiency-core scheduling), not code: the commits differ by less than the spread (no per-connection ping cost shows up, and the loadtest YAML pins `vad.workers/batch/queue_cap` explicitly, so the G1 VAD default change cannot affect it). The 1.85 baseline stays valid for a quiet host (load < 1.5); when the gate fails only on CPU, check `uptime` and re-run on an idle machine before suspecting code.

To start gating CI: run the `perf-gate` workflow on a PR once (first runs upload the candidate baseline under `baselines/` in the `perf-gate-reports` artifact), copy the files into `docs/benchmarks/baselines/`, commit. GitHub-hosted runners vary (CPU model and core count differ from run to run), so the runner label is the machine class here; if the CPU model changes, the gate fails on mismatch and a new baseline is needed. A self-hosted runner gives stable numbers.

## Caveats

- Baselines are machine-specific; other processes on the host move CPU and tail numbers. macOS timers overshoot ~2 ms.
- Fixed: the bench used to fail with `core: session closed` in ~3-5% of runs. Cause: a processor (fake LLM/TTS) still running when the bench called `Session.Close` got `ErrSessionClosed` from `Session.Go`, returned it, and the run loop recorded it as the session's failure, so a clean Close returned an error. `core.Session` now ignores `ErrSessionClosed` from a processor once teardown has begun (`TestCloseRacingProcessorIsClean`). 0 failures in 150 runs after the fix. (The old retry never fired anyway: `go run` turns exit 2 into 1, so the script now builds the binaries.)
- CI: macos-14 runners have 3 cores, so the first run produces a `darwin-arm64-3c` candidate; the onnxruntime dylib is git-ignored and is downloaded by the workflow.
- Client v2v includes the VAD hangover and a 100 ms clip margin, so it is not comparable with `baseline.md`.
- One speech clip, 4 calls: the smoke is a regression tripwire, not a capacity measurement (see `capacity.md`).

Live provider checks (opt-in, spends API credit, not part of the gates): `make live-check`, results in `turn-manager-live.md`.
