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

### CI policy

`.github/workflows/ci.yml` (blocking) runs `make verify` on Linux and macOS, the Postgres tests and the image smoke; none of it compares timings. `perf-gate.yml` is advisory (`continue-on-error: true`): the only committed baseline is `darwin-arm64-12c` (a developer Mac), the hosted runner label is `darwin-arm64-3c` (or a Linux label), and a baseline is never compared across labels, so on CI the gate can only produce a candidate baseline. It becomes a required check when a baseline for the exact runner class is committed (best on a self-hosted runner, whose numbers are stable): run the workflow once, commit the uploaded candidate files from `perf-gate-reports`, remove `PERF_ALLOW_MISSING` and `continue-on-error`. Until then the perf gate is a local pre-release check (`make perf-gate`). `TestInferenceLatencyP95` (smartturn) enforces its strict 50 ms bound only with `GOVOICE_STRICT_LATENCY=1` for the same reason: wall-clock bounds are host-class specific.

To start gating CI: run the `perf-gate` workflow on a PR once (first runs upload the candidate baseline under `baselines/` in the `perf-gate-reports` artifact), copy the files into `docs/benchmarks/baselines/`, commit. GitHub-hosted runners vary (CPU model and core count differ from run to run), so the runner label is the machine class here; if the CPU model changes, the gate fails on mismatch and a new baseline is needed. A self-hosted runner gives stable numbers.

## Caveats

- Baselines are machine-specific; other processes on the host move CPU and tail numbers. macOS timers overshoot ~2 ms.
- Fixed: the bench used to fail with `core: session closed` in ~3-5% of runs. Cause: a processor (fake LLM/TTS) still running when the bench called `Session.Close` got `ErrSessionClosed` from `Session.Go`, returned it, and the run loop recorded it as the session's failure, so a clean Close returned an error. `core.Session` now ignores `ErrSessionClosed` from a processor once teardown has begun (`TestCloseRacingProcessorIsClean`). 0 failures in 150 runs after the fix. (The old retry never fired anyway: `go run` turns exit 2 into 1, so the script now builds the binaries.)
- CI: macos-14 runners have 3 cores, so the first run produces a `darwin-arm64-3c` candidate; the onnxruntime dylib is git-ignored and is downloaded by the workflow.
- Client v2v includes the VAD hangover and a 100 ms clip margin, so it is not comparable with `baseline.md`.
- One speech clip, 4 calls: the smoke is a regression tripwire, not a capacity measurement (see `capacity.md`).

## Live provider matrix (opt-in, spends API credit, not part of the gates)

`tools/livecheck` runs ONE standard agent (same system prompt, same scenarios) on a real server child against many provider **profiles**; adding a provider combination means adding a profile, not code. Results: [agent-matrix.md](agent-matrix.md). Earlier one-off runs: [turn-manager-live.md](turn-manager-live.md) (Google), [nvidia-live.md](nvidia-live.md).

```
go run ./tools/livecheck -list                       # profiles and whether this environment can run them (free)
GOVOICE_LIVE=1 make agent-matrix PROFILES=google,nvidia SCENARIOS=S1 MODES=off,on
go run ./tools/livecheck -dry-run -profiles all      # the provider-request estimate only (free)
make live-check-selftest                             # same engine, fake providers, no network (about 2 min)
GOVOICE_LIVE=1 make live-check                       # old entry point: google profile, all modes (LIVE_ARGS for flags)
```

Run control (all flags: `go run ./tools/livecheck -h`): `-profiles a,b|all`, `-scenarios S1,S2`, `-modes off,on,on+smart` (`on` = turn.manager + min_words 3, `on+smart` adds Smart-turn), `-repeat N`, `-max-calls N` (a hard cap; see "Cost control" below), `-yes` or `GOVOICE_LIVE=1` to proceed, `-out dir` (results.json + agent-matrix.md, default `.agent-matrix-out/`, git-ignored), `-write-docs` (merges into `docs/benchmarks/agent-matrix.json` and re-renders `agent-matrix.md`: each profile x mode x scenario keeps its latest run, stamped with date and commit, so separate runs accumulate), `-clips dir`, `-profiles-file path` (use another YAML), `-stt-model` (override the STT model), `-mcp-url` (S6), `-trace`, `-keep-work`. Exit status 1 when any assertion fails or a scenario errors; N/A and SKIPPED do not count.

| scenario | asserts | needs |
|---|---|---|
| S1 baseline turn | question heard (WER vs the known text), answer is "four", bot audio within `-v2v-max`; reports v2v, VAD stop, STT final, LLM first token, TTS first byte | |
| S2 barge-in | bot audio stops within `-stop-max`, second utterance heard and answered | a bot reply that keeps talking (the Mars question) |
| S3 backchannel | with min_words, "uh-huh yeah" starts no reply and does not interrupt | an STT with interim results (`interims: yes`, else detected; N/A otherwise) |
| S4 mid-utterance pause | under the turn manager one user turn, one reply, whole sentence heard | |
| S5 idle | an idle prompt is spoken, then the server hangs up | |
| S6 tool call | the answer carries the tool result (`-mcp-expect`) | `-mcp-url`: a public HTTPS MCP server with `get_time` (the server refuses loopback and plain-http MCP endpoints, so the harness cannot host one) |
| S7 text turn | RTVI `send-text` gets a spoken, correct answer | |
| S8 muted client | a client that sends nothing outlives `idle_timeout` (3 s) for 9 s because the 1 s pings are answered | |

Each profile and turn mode gets a fresh server child on ephemeral ports (S5 and S8 their own, with short idle/ping timers), a fresh credential store (one stored credential per provider, referenced per stage, deleted when the server stops), and the server binary is built once with the union of the build tags the selected profiles need (xAI STT/TTS: `xaispeech`). A profile whose required env is missing is SKIPPED with the reason. Secrets are read from `examples/.env` into the harness environment only; the server child inherits an allowlist (PATH, HOME, locale, ONNX runtime paths), never provider keys or `GOVOICE_DATABASE_DSN`; all output and report files are redacted (patterns plus the literal values of every `*KEY*/*SECRET*/*TOKEN*` variable).

### Adding a provider profile

1. Vendor already known to `tools/livecheck/creds.go` (google, nvidia, xai, deepgram, cartesia, elevenlabs, openai)? Go to 2. Otherwise add a `credSpecs` entry (env var, how the stored secret is built); stage capabilities come from the server's own catalog (`server/provider/registry.go`), which must already know the provider.
2. Add an entry to `tools/livecheck/profiles.yaml`: `name`, `stages` (provider, model, language, voice; `${VAR:-default}` allowed), `requires` (every env var the credentials read), `defaults` (public ids only), `build_tags` if the stages need any, `interims` if you know whether the STT sends interim results, `notes`.
3. `go test ./tools/livecheck` (validates the file: capabilities, required env, credentials, and runs every profile's agent payload through the server's own config and credential validation offline), then `go run ./tools/livecheck -list` (does your environment satisfy it?) and `-dry-run -profiles <name>` (the estimate).
4. Smoke it: `GOVOICE_LIVE=1 make agent-matrix PROFILES=<name> SCENARIOS=S1 MODES=off` (about 7 provider requests measured; the plan shows an upper bound of about 15 plus any missing clips).
5. Full run, then `-write-docs` and commit `agent-matrix.md` with the reason. Findings go in the matrix `notes:` of the profile and in the capabilities/parity docs, not in this README.

Limits: timings are small-n samples on one machine and network; S1's WER and "four" checks depend on the STT; S6 needs an external MCP server; `-max-calls` is enforced on measured provider requests (clip synthesis for missing clips is counted), but a provider may bill more than one unit per request. `-clips` also reads the shared provtest set (`clips.json`, matched by spoken text) when present.

## Nightly critical-path job

`.github/workflows/live.yml` runs every night at 03:17 UTC and on manual dispatch. It runs
`make live-critical` for two profiles in mode `off`, with the call cap split per step (60 per run):

| Profile | Scenarios | Cap |
|---|---|---|
| google | S1 baseline, S11 push-to-talk, S12 server-side mute, S13 LLM failover | 31 |
| xai | S1, S11, S12, S13 | 29 |

The caps come from the harness's upper-bound estimates (27 calls per profile, plus up to 3 clip
syntheses on a cold cache). The expected spend is lower (about 18 per profile).

Manual-only (`make agent-matrix`):
- S2 barge-in: 16 more calls for google would exceed the 60 cap;
- S6 tool call: needs a public MCP server;
- S9 long reply.

Secrets live in the `live-providers` GitHub environment:
- `GOOGLE_API_KEY`;
- `GCP_SA_JSON`: the service-account JSON, which the job writes to a temp file for `GOOGLE_APPLICATION_CREDENTIALS`;
- `XAI_API_KEY`.

Synthesized clips are cached between runs. Results (results.json and agent-matrix.md per profile) are
uploaded as the `live-critical-results` artifact. A failed check fails the job.

Run the same thing locally: `GOVOICE_LIVE=1 make live-critical PROFILES=google SCENARIOS=S11 MAX_CALLS=10`.

## Running things, cheapest first

`make help` lists every target by cost tier. In short:

| Tier | Targets | Cost |
|---|---|---|
| Free | `make verify`, `eval-pipecat`, `eval-turn`, `bench-latency`, `loadtest-smoke`, `perf-gate`, `live-check-selftest`, `provider-matrix-dry` | none (fake providers, no keys) |
| Preview | `make live-list`, `provider-env`, `agent-matrix-plan PROFILES=... SCENARIOS=... MODES=...` | none (shows runnable profiles, env names, request estimate) |
| Paid | `make provider-matrix`, `agent-matrix`, `live-check`, `e2e` | real provider requests; each needs `GOVOICE_LIVE=1` and `examples/.env` |

Keep paid runs small: preview first (`make agent-matrix-plan` shows the per-profile upper-bound estimate and the cap), then one
profile, `SCENARIOS=S1`, `MODES=off`.

### Cost control: estimate versus actual

- **Actual** requests are measured at the provider boundary, always on, no switch: `core/netpool` counts every request a
  provider client sends (HTTP calls, WebSocket dials, retries included) and Google/Nvidia gRPC stream opens and RPCs are
  counted by the provider (`voice_provider_requests_total{provider,stage}` on the server's `/metrics`). `tools/livecheck`
  reads it before and after each scenario (`actual_requests`, `actual_requests_by_stage` in `results.json`; est vs actual
  table in `agent-matrix.md`) and counts the harness's own clip synthesis. `tools/provtest` reads the same counters
  in-process (`actual_requests` per row).
- **Estimate** is a deliberately pessimistic *upper bound* (`tools/livecheck/estimate.go`): per scenario, 1 STT stream at
  call start + 2 per user turn (adopt + reopen) + 1 for a barge-in or resumed speech, 1 LLM request per turn (+1 for a tool
  round), and TTS requests per reply of 5 (xAI: unary, one request per sentence), 4 (Google: per piece), 2 otherwise. It
  came from a measured miss: an xAI S2/S4 run estimated at 15 made 30 to 35 requests. Recalibrate it from the
  `est vs actual` table after real runs.
- **Cap**: `make agent-matrix`, `agent-matrix-plan` and `provider-matrix` default to `MAX_CALLS=40` (`DEFAULT_MAX_CALLS` in
  the Makefile; `live-check` and a bare `go run ./tools/livecheck` use the tool default of 60, so the old full
  Google off/on/smart shape (about 187) is refused until narrowed or `-max-calls` is raised). A plan whose estimate exceeds the cap is refused before anything
  is spent (`agent-matrix-plan` shows the verdict and still exits 0). During the run the measured total is polled every 250ms:
  past the cap the current call is closed (scenario `ERR`), later scenarios are skipped, and the stage suite refuses further
  units. The poll fails closed: if the server's `/metrics` cannot be read the call is ended and later scenarios are skipped.
  The stage suite (`provider-matrix`) has no up-front refusal: it gates each unit on planned + measured requests, so it stops
  at the cap (rows `incomplete`) and can overshoot by at most one unit's requests (e.g. an STT reopen inside a unit). Raise it deliberately: `MAX_CALLS=120`.
- **TTS timing for the offline simulator**: each TTS row in `provider-matrix`'s `results.json` carries `tts_timing`, taken from the
  3 synthesis requests the suite already makes (no extra calls): `ttfa_ms` (cold first unit), `audio_ms`, `chars`, `wall_ms`
  (sum of post to TTSStopped), `chars_per_sec`, `realtime_factor` (audio s per wall s) and
  `units[]` (per request `chars`, `ttfa_ms`, `wall_ms`, `audio_ms`; fit any overhead model from these). Repeats keep the median-wall run.
  Absent in older files; `-merge` accepts those.
- Every live run ends with `actual provider requests: N (upper-bound estimate M, -max-calls C)`.
- Not counted: provider-side billing units (characters, tokens, audio seconds), OAuth token fetches (not billed), pre-dial
  warm-ups (bare TCP/TLS or gRPC channel connects, no request), and transport-internal replays of a request the server never
  processed. Google Vertex-mode LLM calls are counted per stream by the provider (genai's own client is not on the pool);
  Nvidia LLM (OpenAI-compatible) is labelled `provider="nvidia"`.
Stage-level `STAGES=llm` is the cheapest paid check; `e2e` (Playwright, real Google) the most expensive.
