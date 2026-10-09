# M4: test health, build/packaging, operations (round 6 independent audit)

Auditor scope: testability, build, packaging, ops. Read-only; no provider calls, no GOVOICE_LIVE. Branch `main` at `40dcf5f`, 2026-10-09, Apple M4 Pro (12c), macOS, Docker 29.4 (linux/arm64).

## What was run

| Command | Result |
|---|---|
| `go vet ./...` + `go test -race -count=3 -json ./...` in all 10 go.work modules | all pass 3/3, vet clean, ~8 min wall. No flakes on an idle-ish host (load avg 2.5 to 4.5) |
| `go vet -tags {xaispeech, fakeproviders, xaispeech+fakeproviders, integration, live, integration+xaispeech}` across modules | all clean |
| `go test -race -count=2 -tags xaispeech ./...` (providers/xai), `-tags xaispeech` server pkg | pass (these never run under `make test`) |
| gofmt + 3 node checks from `make verify` | pass (playback-clear probe passes only under the recorded product decision) |
| Postgres store tests, throwaway `postgres:16-alpine`, `-race -count=2` | pass (skipped by `make test`) |
| `docker build -f deploy/Dockerfile .` | builds (arm64, 314 MB, context 95 MB) |
| Image run: `--read-only --cap-drop ALL`, throwaway Postgres, generated test secrets | starts, 8 vadworkers, `/ready` 200 inside; SIGTERM exits 0 in about 10 ms |
| loadtest smoke (4 sessions, 30 s) and `scripts/perf-gate.sh gate` | PASS (CPU 1.54 vs 1.85 baseline) |
| 3 concurrent runs of `server` lifecycle tests | FAIL: `bind: address already in use` |
| `GOOS=linux` cross-build | not possible from macOS (cgo plus onnxruntime_go); Linux binaries exist only through Docker |

## Findings

### M4-01: high. No CI runs the test suite; the one workflow never gates
- Where: `.github/workflows/perf-gate.yml` (the only workflow); `.github/workflows/perf-gate.yml:2-4,16`.
- Evidence: `git ls-files .github` lists only perf-gate.yml. Nothing runs `make test`, vet, gofmt, `-tags xaispeech`, Postgres or loadtest-smoke on a PR. The perf-gate job sets `PERF_ALLOW_MISSING: '1'` and runs on `macos-14` (3 cores, so label `darwin-arm64-3c`), and only `darwin-arm64-12c` is committed (`docs/benchmarks/baselines/`). So it always exits 0 ("generated candidate baseline"). The repo also has no git remote, so the workflow has never run. README:17 advertises "a CI perf gate".
- Impact: regressions reach main unless someone runs the 8-minute suite locally. All the "verified" claims depend on local discipline.
- Fix direction: add a `test.yml` that runs `make verify` (macOS arm64 plus a Linux job in the golang image), `-tags xaispeech` and `make test-postgres`; commit a baseline for the runner label, or drop the claim.
- Confidence: high (confirmed). Launch impact: no automated safety net for hotfixes after launch.

### M4-02: high. Readiness, metrics and the management API are unreachable outside the container; the runbook's Kubernetes probes cannot work
- Where: `config/production.yaml:6` (`admin_listen_addr: "127.0.0.1:9090"`); `deploy/docker-compose.yaml:38` (`127.0.0.1:9090:9090`); `deploy/nginx.conf:128-134` (proxies `govoice:9090`); `docs/operations_runbook.md:160,169`.
- Evidence: in the built image, `curl host:19090/ready` gives "Empty reply from server". From a peer container, `http://<container-ip>:9090/ready` gives "Could not connect". From inside, `docker exec … curl 127.0.0.1:9090/ready` returns 200. `deploy/smoke.sh` works around this with `docker exec`.
- Impact:
  - A kubelet `readinessProbe {port: 9090}`, as the runbook sketches it, never passes, so the pod is never Ready.
  - Prometheus cannot scrape `/metrics`, so the drain, admission and circuit metrics are invisible.
  - The nginx admin vhost (9091) proxies to a closed port.
  - To fix probes, operators must bind admin to 0.0.0.0. That also exposes `/api/v1` (bearer auth) and the unauthenticated `/metrics` and `/debug/timeline` to the pod network.
- Fix direction: serve `/ready` and `/metrics` on a separate probe/metrics listener (or the public one, with `/ready` only), and keep the management API on loopback.
- Confidence: high (reproduced). Launch impact: the deployment as documented has no readiness gating and no metrics.

### M4-03: high (if xAI speech is in launch scope). The production image has no xAI STT/TTS, and `make test` never compiles or runs the xAI speech code
- Where: `deploy/Dockerfile:25-27` (no `-tags xaispeech`); `Makefile:28` (`go test -race -count=1 ./...` with no tags); `server/registry_noxaispeech.go`.
- Evidence: `GET /api/v1/providers` on the built image returns `{"id":"xai","capabilities":["llm"]}`. 19 files are behind `//go:build xaispeech`, including `providers/xai/stt.go` and `tts.go` and 16 test files. They pass when run by hand (`-tags xaispeech`, `-race -count=2`, 36 s), but `make test` and `make verify` skip them. The live xAI agent-matrix run used a server built with the tag.
- Impact: an agent configured with `xai` STT/TTS fails readiness in production. The tested and the shipped build differ.
- Fix direction: decide the launch scope. If xAI speech ships, add `-tags xaispeech` to the Dockerfile and to `make test` (or run the matrix both ways). If not, state it in the launch notes.
- Confidence: high (confirmed). Launch impact: blocker for any "xAI end-to-end voice" launch claim; none for xAI LLM only.

### M4-04: high. The alerting docs name metrics that do not exist, and no alert rules ship
- Where: `docs/observability.md:57-73,89,96`; `docs/operations_runbook.md:176,182,184,187`.
- Evidence: comparing against the 66 `# TYPE voice_*` series the server emits:

  | Documented | Real series |
  |---|---|
  | `voice_session_tasks_leaked_total` | `voice_leaked_work_total`, `voice_leaked_sessions_total` |
  | `vad_worker_restarts_total` | `voice_vad_worker_restarts_total` |
  | `vad_worker_errors_total` | none |
  | `provider_circuit_state` (documented with 2 = open) | `voice_{stt,llm,tts}_circuit_state` (1 = open) |
  | `voice_sessions_active` | `voice_active_sessions` |
  | `voice_sessions_total` | none |
  | `voice_sessions_rejected_total` | none |
  | `provider_request_duration_seconds` | none |

  - The timeline path is documented as `/timeline?session_id=` but is actually `/debug/timeline`.
  - The runbook says `/ready` returns "checks: true"; it actually returns `{"status":"ready","circuits":…}`.
  - The repo has no Prometheus rules or dashboard file.
- Impact: alerts copied from the docs never fire. Leaked work, VAD crashes and open circuits go unnoticed.
- Fix direction: regenerate the metrics table from `server/metrics.go`, ship a tested `deploy/alerts.yaml`, and add a doc-vs-`/metrics` contract test.
- Confidence: high (confirmed). Launch impact: on-call is blind to the documented failure modes.

### M4-05: high. Nvidia (a launch provider) breaker state is not observable
- Where: `server/registry.go:112,675-690`; `server/nvidia.go:30-32,85-90`; `server/server.go:692-699`; `server/metrics.go:262-307`.
- Evidence: Nvidia breakers live in `nvidiaBreakers sync.Map` with no `SetObserver`. The circuit gauges and the `/ready` `circuits` map list only google and xai. `voice_{llm,stt,tts}_errors_total` have no provider label. Only `voice_provider_requests_total{provider,stage}` mentions nvidia.
- Impact: an NVCF outage or bad function id that opens the breaker raises no metric and leaves `/ready` unchanged. Operators cannot tell an Nvidia outage from a client problem.
- Fix direction: add an observer for each Nvidia breaker key, emit `voice_*_circuit_state{provider="nvidia",endpoint=…}`, and add a provider label to the error counters.
- Confidence: high (confirmed). Launch impact: blind spot for one of the three launch providers.

### M4-06: medium. Server lifecycle tests use fixed ports and fail when run concurrently
- Where: `server/lifecycle_test.go:25-26,94-95,124-125,148-149,197-198` (127.0.0.1:18081-18085 and 19091-19095).
- Evidence: three concurrent runs of the compiled test binary gave `listen tcp 127.0.0.1:18081: bind: address already in use` in 3 of 3 runs. This repo routinely has parallel worktrees and agents (`.worktrees/`).
- Impact: spurious red runs in parallel CI or worktrees, and a habit of "rerun until green".
- Fix direction: use `127.0.0.1:0` and read the bound address back.
- Confidence: high (reproduced). Launch impact: low directly; it erodes trust in the gate.

### M4-07: medium. A fresh clone cannot pass `make test`, because the macOS ONNX Runtime library is git-ignored and undocumented
- Where: `.gitignore` (`*.dylib`); `vad/silero.go:86-91`; `smartturn/model_test.go:52-55`; `vad/vad_test.go:22-26`.
- Evidence: `third_party/onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.dylib` is untracked (the headers are tracked). The `vad` and `smartturn` tests `t.Fatal` when ORT is missing; they do not skip. Only `perf-gate.yml` downloads ORT. README and quickstart never mention it. Linux has a candidate only for `onnxruntime-linux-x64` (no aarch64 path).
- Impact: new contributors and CI see failures unrelated to their change. A Linux test run needs manual setup.
- Fix direction: add a `make ort` target (pinned URL plus sha256, as the Dockerfile already does), make the test prerequisite explicit, and add a linux-aarch64 candidate.
- Confidence: high (static; the failure paths were read). Launch impact: slows CI bring-up.

### M4-08: medium. A wall-clock latency assertion in a unit test is close to its limit on a fast host
- Where: `smartturn/model_test.go:165-187` (`TestInferenceLatencyP95`, p95 < 50 ms).
- Evidence: the 3 runs measured p95 = 40.5, 24.7 and 24.3 ms. The 40.5 ms run had other packages running at the same time, which puts it at 81% of the budget on an M4 Pro. The runbook says x86 cores can be 2 to 3 times slower.
- Impact: on a GitHub Linux runner or a loaded host this test will flake.
- Fix direction: move it to a benchmark or the perf gate, or skip under `-race` or when `CI` is set.
- Confidence: medium (risk extrapolated from measured values). Launch impact: CI flakiness.

### M4-09: medium. The production store (Postgres) is not exercised by `make test`
- Where: `Makefile:69-79` (`test-postgres` is a separate, docker-only target); skipped tests `TestRepositoryPostgresIntegration` and `TestMigrationPostgresUpgradeRollback`.
- Evidence: in the full run they show as SKIP. Run against a throwaway container (`-race -count=2`) they pass.
- Impact: production uses `storage.dialect: postgres`, but the default suite tests SQLite only. Migration regressions would ship unnoticed.
- Fix direction: run `make test-postgres` in CI (a service container).
- Confidence: high. Launch impact: migration risk on the first schema change after launch.

### M4-10: medium. All performance, capacity and soak evidence is macOS with fake providers; the 24 h soak has never run; no fault injection under load
- Where: `docs/benchmarks/capacity.md:28,86`; `.govoice-loadtest-soak.md` (the longest soak: 16 sessions, 180 s); `tools/loadtest/main.go` flags.
- Evidence:
  - The only fault knobs are `GOVOICE_FAKE_LLM_DELAY` and `GOVOICE_FAKE_STT`.
  - No loadtest mode covers:
    - provider errors or outages (breaker open under load, failover under load);
    - slow STT or TTS;
    - client abuse (slow reader, half-open TCP, malformed or oversized frames, handshake floods);
    - drain or SIGTERM while calls are active;
    - VAD worker kill under load.
  - Nothing has run on Linux or x86.
  - `production.yaml` sets `max_sessions: 250` and `vad.workers: 8` on the strength of a 4-to-16-session macOS test.
- Impact: capacity limits and runbook sizing are unvalidated for the production OS and CPU class. Slow leaks would surface only in production.
- Fix direction: run the existing loadtest inside the built image on a Linux/x86 host; add `-fault` modes to the fake providers; run the 24 h soak once before launch.
- Confidence: high. Launch impact: unknown real capacity; risk of overload or leaks at launch traffic.

### M4-11: medium. Runbook deployment steps are wrong for the systemd path and the docker stop timeout
- Where: `docs/operations_runbook.md:85-97,150`.
- Evidence (compared against `deploy/govoice.service` and the server code):
  - Binaries are installed as `/usr/local/bin/govoice-server` and `govoice-vadworker`. The unit runs `/opt/govoice/bin/server`, and the server looks for `vadworker` next to its own executable (`cmd/server/main.go:60-64`). VAD would fail closed.
  - The model is copied from a non-existent `vad/models/silero_vad.onnx` to `/opt/govoice/vad/models/`. The unit expects `/opt/govoice/models/`.
  - No ORT library step is given, although the unit expects `/opt/govoice/lib/libonnxruntime.so`.
  - `turnworker` is not installed.
  - The config is copied to `config.yaml`, but the unit reads `production.yaml`.
  - `docker stop -t 45 govoice`: the container is named `govoice-server`, and 45 s is below the server's own budget of `drain_timeout + 5s` = 50 s, so the process is SIGKILLed mid-drain.
- Impact: following the runbook gives a server that fails to start (systemd path) or drops calls on stop.
- Fix direction: rewrite path B from the Dockerfile layout and use `docker stop -t 60 govoice-server`.
- Confidence: high. Launch impact: medium if anyone deploys without containers or follows the stop command.

### M4-12: medium. `/ready` does not reflect store health and reports circuits that are not built
- Where: `server/server.go:666-706`.
- Evidence:
  - The readiness check is `s.agents == nil`; there is no DB ping. A Postgres outage leaves `/ready` 200 while every call fails at agent resolution.
  - In the default build `xai_stt` and `xai_tts` are reported "closed" even though they are not compiled in.
  - Nvidia, Deepgram, Cartesia, ElevenLabs and OpenAI are absent.
- Impact: the LB keeps routing calls to an instance whose store is down, and the circuit report misleads.
- Fix direction: add a bounded DB ping (cached for about 1 s), and build the circuits map from registered breakers.
- Confidence: high (static). Launch impact: a store outage looks like random call failures, not unreadiness.

### M4-13: medium. The production config template leaves idle hang-up off
- Where: `config/production.yaml:57-61,128-129`.
- Evidence: `idle.user_timeout` is commented out. The file's own comment and the runbook (lines 166 and 197) say to set it in production. With `max_call_duration: 60m` and `max_sessions_per_principal: 5`, an abandoned tab that still answers pings holds a slot, and provider streams, for up to an hour.
- Impact: slots and provider spend leak from abandoned browser tabs.
- Fix direction: enable `idle: {user_timeout: 30s, max_prompts: 2}` in `production.yaml`.
- Confidence: high. Launch impact: capacity and cost drain under real user behaviour.

### M4-14: low. The production server binary auto-loads `.env` files
- Where: `cmd/server/main.go:22`, `examples/envload`.
- Evidence: `envload.Load()` runs unconditionally. It reads `$ENV_FILE`, `./.env`, `examples/.env`, `../examples/.env` and `../../examples/.env`, and sets any variables not already set. This includes `GOOGLE_APPLICATION_CREDENTIALS` path rewriting.
- Impact: a stray dev `.env` in the working directory silently supplies a DSN, keys or a token secret in production.
- Fix direction: skip `envload` when `profile: production`, or move it behind a dev flag.
- Confidence: high (static). Launch impact: low (the image's working directory has no .env), but a footgun for host installs.

### M4-15: low. Mixed log formats in production
- Where: `cmd/server/main.go:105-140` (`fmt.Printf`, `log.Fatalf`); `server/registry.go:575` (`slog.Warn` on the default text logger, which writes to stderr); `server/server.go:111` builds a JSON logger but never calls `slog.SetDefault`.
- Evidence: the container log interleaves JSON lines with "Voice Framework V1 Server running" and "Received signal terminated…".
- Impact: JSON log pipelines drop or mis-parse startup, fatal and min_words warnings.
- Fix direction: `slog.SetDefault(logger)`, and log startup and shutdown through it.
- Confidence: high (observed). Launch impact: low.

### M4-16: low. No version stamping; supply-chain pinning is partial
- Where: `deploy/Dockerfile:25-27`, `.dockerignore` (excludes `.git`), `deploy/docker-compose.yaml:3,74,90`, `Makefile:57`.
- Evidence:
  - No `-X main.version`, no build-info metric or endpoint, and no `-trimpath`. With `.git` excluded, Go embeds no VCS revision, so a running pod cannot say what commit it is.
  - `postgres:16-alpine` and `nginx:1.25-alpine` are unpinned (the digest check covers only the Dockerfile).
  - `govulncheck@latest` is unpinned.
  - The obsolete `version: "3.8"` key is present.
  - No git tags exist.
- Impact: incident triage and rollback cannot identify builds, and builds are not reproducible.
- Fix direction: pass `ARG GIT_SHA`, use `-ldflags -X` with `-trimpath`, expose `voice_build_info`, and pin the compose images by digest.
- Confidence: high. Launch impact: low to medium (ops friction).

### M4-17: low. The Docker build context sends local agent output and worktrees
- Where: `.dockerignore`.
- Evidence: the context is 95 MB. It includes `.worktrees/` (133 MB on disk), `.govoice-eval-pipecat/` (`server.log` plus `data/`), `.agent-matrix-out/` (live-run results), `.goalpulse/`, `.superpowers/` and `.perf-out/`. These are copied into the builder stage by `COPY . .`, though not into the final image.
- Impact: local artifacts end up in builder layers and build caches, and the build is slower.
- Fix direction: switch to an allowlist `.dockerignore` (`*`, then `!go.*`, `!core`, …).
- Confidence: high. Launch impact: low.

### M4-18: low. The nginx reference config lacks the 503 retry the runbook depends on
- Where: `deploy/nginx.conf:94-110` versus `docs/operations_runbook.md:162`.
- Evidence: the runbook says to retry 503 on another target (`proxy_next_upstream error http_503`). nginx.conf has a single upstream variable and no `proxy_next_upstream`.
- Impact: during a rolling deploy, clients routed to a draining instance get 503 instead of being retried.
- Fix direction: add an upstream block with `proxy_next_upstream error http_503` (multi-instance only).
- Confidence: medium. Launch impact: low (single instance).

## Cheapest tests that buy the most confidence (priority order)

1. CI workflow running `make verify` plus `-tags xaispeech` plus `make test-postgres` (about 10 min; covers M4-01, 03, 09).
2. `docker build`, then run the image with Postgres and probe `/ready` from outside the container. This is `deploy/smoke.sh` minus the Google key, so it costs nothing; it would have caught M4-02 and M4-03.
3. A contract test that scrapes `/metrics` and checks every metric named in `docs/observability.md` and the runbook (M4-04).
4. A unit test that an Nvidia breaker opening moves a gauge (M4-05).
5. Replace fixed ports with `:0` (M4-06), and skip or relax `TestInferenceLatencyP95` under CI (M4-08).
6. Run loadtest-smoke inside the Linux image on an x86 host, then a 1 h soak, before the 24 h one (M4-10).
7. Fake-provider fault modes (error burst, 10 s stall) during loadtest to show breaker plus failover under load; a SIGTERM-under-load test asserting `server_draining` and 1001 (M4-10).
8. A `/ready` test with the DB closed (M4-12).

## Checked and clean

- All 10 modules pass vet and `go test -race -count=3` (no flake in about 1,300 tests × 3). The xaispeech-tagged tests and the Postgres tests pass when run.
- Every build-tag combination (`xaispeech`, `fakeproviders`, `integration`, `live` and their pairings) vets cleanly. gofmt, manifest and fixture checks pass.
- `deploy/Dockerfile`:
  - builds on arm64 with pinned base digests and sha256-verified ORT 1.29.0 for amd64 and arm64;
  - pins the Silero and Smart-turn model hashes, which are also verified at runtime;
  - sets `GOVOICE_ORT_LIB`, `GOVOICE_ROOT` and `GOVOICE_MODEL_PATH` correctly;
  - runs as non-root uid 10001 under tini;
  - copes with a read-only root and tmpfs `/tmp`.
- Graceful shutdown in the container: SIGTERM marks the instance unready, drains and exits 0. The `drain_timeout + 5s` budget fits compose `stop_grace_period: 60s` and systemd `TimeoutStopSec=60s`.
- Production config validation: refuses an empty, public-dev or under-32-character token secret, in-process VAD, and plain WS without TLS or a trusted proxy. It also rejects unknown keys. Recording defaults to off.
- Secrets: provider keys are not passed through env or compose. Worker children get an allowlisted environment. No logging of DSN, keys or tokens was found. nginx redacts `token=` in access logs.
- loadtest-smoke and `perf-gate` pass on this machine against the committed baseline (CPU 1.54 vs 1.85, inside +30%).
- Drain, admission, failover, recording (`core/record`), idle and lifecycle all have dedicated tests (`server/drain_test.go`, `admission*_test.go`, `*_failover_test.go`, `core/record/record_test.go`).
