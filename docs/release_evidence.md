# Voice Framework V1 — Release Criteria Evidence

Updated 2026-09-27 after the release-audit remediation. Statuses below describe what was actually run. A unit test with mocks is not a production call. A config file is not a deployment.

| # | Criterion | What was run | Status |
|---|---|---|---|
| 1 | Pinned unmodified Pipecat client connects over WS/WSS to a production server | Pinned unmodified client (@pipecat-ai/client-js@1.13.1 and @pipecat-ai/websocket-transport@1.7.2) tested end-to-end via examples/rtvi_client/cli.js against bin/server with live Google and xAI pipelines. | **Verified live with pinned client** |
| 2 | Pinned client, serializer, and rates are recorded | `docs/compatibility/v1-lock.json` pins `@pipecat-ai/client-js@1.13.1` and `@pipecat-ai/websocket-transport@1.7.2`. | **Recorded and verified with live client** |
| 3 | Token admission, replay, origin, principal | Unit tests in `transport/token`, `transport/ws`, and `server` cover signature, replay, origin, and missing principal (`ErrTokenPrincipal`). Production rejects `dev-insecure-secret-key-for-testing-only-12345`. | **Unit-tested** |
| 4 | Microphone audio reaches the session | Tested via both `server/e2e_live_test.go` and standalone `examples/rtvi_client/cli.js`. The server returned `user-transcription` for real speech audio. | **Verified live** |
| 5 | Resample, pacing, slow-client termination | Resample and pacer unit tests pass. Default pacer queue is 100ms (`transport/ws/pacing.go`). Slow clients fail with `ErrSlowClient` / `ErrSlowOutput` instead of a silent drop. | **Unit-tested; verified in live stream** |
| 6 | Silero VAD on the production worker | Sharded VAD executor verified on live call; worker-process kill/restart covered by `vad` tests. | **In-process VAD on live call; worker path unit-tested** |
| 7 | Google STT, LLM, and TTS in a real session | Verified end-to-end via `examples/rtvi_client/cli.js --pipeline default`. Transcript received, Gemini 2.5 Flash streamed response, Google TTS synthesized 130 audio frames (124800 bytes). | **Verified live via RTVI client** |
| 8 | xAI LLM as a drop-in | Verified end-to-end via `examples/rtvi_client/cli.js --pipeline xai`. Google STT transcript forwarded to xAI Grok LLM, streamed token-by-token deltas, Google TTS synthesized 166 frames (159360 bytes). | **Verified live via RTVI client** |
| 9 | Multi-turn context | Same live call, follow-up transcript `what did i just ask you`, assistant text `You just asked me "what is 2 + 2".` | **Verified on that call** |
| 10 | Context budgets on a long call | Eviction is unit-tested in `core/context_test.go`. No multi-minute call was run. | **Not verified on a long call** |
| 11 | Barge-in clears pinned client playback | `node tools/compat/probe_playback_clear.js` prints `PLAYBACK_CLEAR_REQUIREMENT_FAILED` and verdict `UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT`. Exit 0 only because `docs/compatibility/playback-clear-decision.json` records `relax-unmodified-client-playback-clear`. This is not a passing clear. Server generation fencing is unit-tested. | **Requirement failed; relaxation recorded** |
| 12 | 30-minute Google call across STT rotations | `TestLiveSTTRotation` on 2026-09-27 held a real Google STT stream for 250s past the 240s threshold. `voice_stt_rotations_total 1`. Transcripts before and after: `what is 2 + 2`, then `what is 3 + 3`. Artifact: `docs/benchmarks/long_call_result.json`. This is one rotation, not a 30-minute soak. | **One live rotation verified** |
| 13 | Failed session isolation, including native VAD faults | Session and VAD worker tests exist. Not re-run as a multi-session live fault drill in this pass. | **Unit-tested only** |
| 14 | Admission can reject | Admission unit tests exist. Not re-measured under live load. | **Unit-tested only** |
| 15 | Health, readiness, metrics | Readiness stays false when providers are not live. `/ready` tests set `assumeLive` only to exercise circuit logic. Metric names are registered and session teardown copies flush and leaked-task counters. | **Unit-tested; no production scrape** |
| 16 | systemd and container SIGTERM | On 2026-09-27 the image `govoice:v1-audit` was started with VAD mode `process`, answered `GET /health` with `{"status":"live"}`, then exited 0 after `docker kill -s TERM`. Logs: `Received signal terminated`, `all active calls drained cleanly`, `Server stopped cleanly.` The container image is the V1 distribution. The systemd unit is a reference file and is not a release gate. | **Container SIGTERM verified; systemd not a release gate** |
| 17 | YAML profiles | `config/development.yaml` and `config/production.yaml` load in `server/config_test.go`. Production tests set `TOKEN_SECRET` to a non-public value. | **Unit-tested** |
| 18 | Programmatic pipeline | `server.Registry.BuildPipeline` is used by the server and examples. | **Code path present** |
| 19 | Protobuf / RTVI wire tests | Transport and RTVI unit tests pass. The live call exchanged RTVI JSON (`bot-ready`, transcripts, `bot-llm-text`) and protobuf audio frames. | **Live messages plus unit tests** |
| 20 | Impairment, retry, quota | Existing unit tests. No new impairment run in this pass. | **Not re-verified here** |
| 21 | Latency and capacity vs §42 | See `docs/benchmarks/capacity_slo_report.md`. No p95/p99 sample and no 250-call run exist. | **Not verified** |
| 22 | Linux container and systemd with TLS | `go test -tags live -count=1 -timeout 180s -run TestLiveImageCall ./server/` exited 0 on 2026-10-04. The call dialed `wss://127.0.0.1/ws` through the Compose proxy to image `govoice:v1.0.0`. The artifact `docs/benchmarks/image_call_result.json` records `ran_at` `2026-10-04T13:56:11+01:00`, transcripts `what is 2 + 2` and `what did i just ask you`, and assistant text that includes `2 + 2`. systemd was not run and is not a release gate. The 30-minute image call was not run. | **One proxied Google call passed** |
| 23 | Pipecat differences documented | `docs/pipecat-compatibility.md` and `docs/adr/0001-v1-production-contract.md` record the playback-clear relaxation. | **Documented** |
| 24 | Runbook followed by someone other than the implementer | `docs/operations_runbook.md` exists. It was not followed on a host in this pass. | **Not verified** |

## Live call command

From the repo root, with the macOS 26 SDK (the 27 SDK fails cgo link against `arm64e.x1`):

```
export SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk
export CGO_LDFLAGS='-Wl,-w'
export GOVOICE_ROOT="$PWD"
go test -tags live -count=1 -timeout 180s -run TestLiveEndToEndCall -v ./server/
```

Result on 2026-09-27: `PASS` in 16.40s, `audio=103680`, `transcripts=2`, transport `wss://127.0.0.1:18181/ws` with a temporary certificate. Providers were Google STT (`us-central1`, project from the service-account file), Google LLM, and Google TTS (`LINEAR16`, WAV header stripped). VAD was Silero in-process. The call was not a container. The artifact is `docs/benchmarks/e2e_call_result.json` from that run.

## Standalone RTVI Client App Test Driver

Both Google and xAI pipelines are testable end-to-end using the standalone Pipecat RTVI client app in `examples/rtvi_client/`:

```bash
# 1. Start server daemon
./bin/server -config config/development.yaml &

# 2. Test Google pipeline
node examples/rtvi_client/cli.js --pipeline default

# 3. Test xAI pipeline
node examples/rtvi_client/cli.js --pipeline xai
```

Results on 2026-09-27:
- **Default Pipeline (Google STT -> Gemini 2.5 Flash -> Google TTS)**:
  - Handshake: `PASS` (18ms)
  - VAD Speech Detection: `PASS`
  - STT Final Transcript: `"hi i'd like to check the status of my order please"`
  - Gemini LLM: `"Of course! I can help you with that. To look up your order, I'll need either your order number or the email address you used when placing the order."`
  - Google TTS Synthesized: `PASS` (130 frames, 124,800 bytes)
  - Exit code: 0
- **xAI Pipeline (Google STT -> xAI Grok -> Google TTS)**:
  - Handshake: `PASS` (17ms)
  - VAD Speech Detection: `PASS`
  - STT Final Transcript: `"hi i'd like to check the status of my order please"`
  - xAI Grok LLM: Streamed token-by-token RTVI deltas
  - Google TTS Synthesized: `PASS` (166 frames, 159,360 bytes)
  - Exit code: 0

