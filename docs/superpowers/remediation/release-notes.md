# Review remediation: release notes

What changed between the review baseline (`1993240`, 2026-10-15) and the head of `feat/phase-9-closeout` (`87f906c`, the final-review fixes up to `442ad79`, and this document): 580 files, +38,643 / -41,213. The plans are in [plans](plans/2026-10-15-review-remediation.md); this page is the summary an operator, a library user and a reviewer need. Every number in "Measured" comes from a run recorded under `.superpowers/sdd/2026-10-15-review-remediation/p9/logs/9.7-*` (local, git-ignored); nothing there is estimated.

Phase 1 (correctness fixes) is merged as `946f646`, with `2e55347` restoring the free end-to-end gate. Phases 2 to 9 follow below.

## 1. Breaking for operators

- `critical_output` mute strategy removed (7b): a server YAML that lists it under `turn.mute` fails at start-up with its own message (`core/turn/mute.go`, `removedMuteCriticalOutput`). It never muted anything.
- Traces REST: `arguments_summary` and `result_summary` are gone (Phase 5). The columns stay in the database; nothing writes them.
- Metric `voice_stt_deduplicated_results_total` removed (Phase 5, it was always zero).
- Voice tokens: `token.max_lifetime` defaults to 1 minute; a value above 5 minutes is refused at start-up, below 2 seconds is refused when set (4.6b; `server/config_validate.go`).
- Flow create and duplicate validate the MCP selections and answer 400 (Phase 1).
- The xAI LLM adapter requires an explicit key (Phase 1); there is no environment fallback inside the adapter.
- WebSocket read limit (Phase 1/4): an over-limit message closes with 1009 and counts `protocol_error`.
- A client that stalls an RTVI write is closed as `slow_client` (9.5a, see section 4).
- `voice_smartturn_fallbacks_total{reason="timeout"}` no longer counts a superseded or closed analysis (9.3). Expect the counter to drop; the old value included every silence window that was replaced by a newer one.
- The automatic trace-content purge is deleted (5.4b). Databases written by builds between `5be193e` and `12a7884` with trace content mode configured must be purged by hand; the SQL is in `docs/operations/retention.md`. Retention results `ToolCallsPurged` and `TracesSanitized` are gone.
- New flow publish refusals (400, 7b): `turn` settings while `turn.manager` is off; `tts.clean_text` or `failover` on a non-start step; unknown route cue or hold cue; `max_mute_ms` above the server's `turn.max_mute`; `max_call_duration_seconds` above `limits.max_call_duration`; `smart` end of turn without the Smart Turn model. Stored versions keep decoding. A flow saved with a 7b field becomes `invalid_config` on an older binary.
- `deploy/sbom.json` is no longer tracked; `make release-security` writes it (8.1). The `offline/contract` module is gone; its tests are `server/offlinecontract` and `make offline-test` runs them from `server/` (8.1).
- The effective-config goldens key `StopSecs` became `Hold` (4.6f); the YAML tag is unchanged.

REST API (`/api/v1`)
- `PUT /api/v1/flows/{id}/mcp-tools` is removed. Send `mcp_tools` together with `config` on `PUT` or `PATCH /flows/{id}`; the selection is stored in the same version as the config (`mcp_tools` without `config` is refused; an empty list clears the selection).
- The `409 version_conflict` response and `storage.ErrVersionConflict` are gone.
- `GET /sessions/{id}/turns`: `tools` is now always `[{"name","status","duration_ms"}]` (an empty list for a turn without calls), built from the stored `tool_calls` rows.
- MCP detach: `DetachResult.server` is omitted when the server has been deleted (`deleted: true`).
- MCP routes answer `500 internal_error` when the store fails; a store failure was answered as 404 (lookups) or 400 (tool test) before. A missing server stays 404.
- Added: `GET /flows/{id}/versions/{number}` (one earlier publish, read-only); `GET /server-config` carries `audio.cues` (the sound names a flow's `route_cue` may use) and a `turn` block (effective manager, min words, mute list, `max_mute_ms`, speech timeout, end of turn, Smart Turn availability and hold).

## 2. Wire (RTVI) additions

- `bot-interrupted` is sent on barge-in (Phase 4). A protobuf interruption frame is deliberately not sent: Pipecat's own server makes the pinned client log a deserialisation error on every barge-in.
- `llm-function-call-started`, `-in-progress`, `-stopped` carry only `tool_call_id` and a cancelled flag; no tool name, arguments or result on the wire (Phase 3).
- `bot-tts-text` after the last audio frame of each synthesis unit (7.4). Same shape as the Pipecat golden; the text is the cleaned unit, and its order against `bot-tts-stopped` is not specified.
- The `PLAYBACK_CLEAR_REQUIREMENT_FAILED` line under `relax-unmodified-client-playback-clear` is a recorded, expected result.

## 3. Breaking for library users of `govoice/core`

- Phase 5 removals: frames `StartFrame`, `CancelFrame`, `TTSAudioFrame`, `SystemMessageFrame` (their frame numbers stay reserved); `ConversationContext.AddToolCall` and `AddToolResult`; `Session.OnUpstream`, `Session.DebugDropped`; the `DebugEvent` envelope fields (Whisker owns them); `TracePolicy.RedactToolContent`; `record.Config.Holdback`; `sttunit.Config.Now`; `STTFailoverConfig.PreRollBytes`; in `core/turn`, `TurnAnalyzer`, `AnalyzerStop`, `SemanticEvidence`, `Config.Semantic`, `Config.InitialSilenceTimeout`, `Manager.Interrupt`, `TurnEventNoSpeechTimeout`; `filter.Delayer`; `rtvi.EncodeDebugEvent`; `token.RedactToken`; `ws.Connection.Pacer` and `.RTVI`; `smartturn.Analyzer.Analyze`, `Fallbacks()`, `Crashes()`; unused provider knobs (cartesia, deepgram, elevenlabs, google, nvidia, xai; list in the Phase 5 plan). `ValidateToolPairing` moved to `core/coretest`.
- Phase 6: removed `Session.NextUnitID`, `IdleController.SetSpeakIndex` and `SpeakIndex`; the google and xai `TTSProcessor` counter fields and own methods (the struct embeds `*ttsunit.Runner`, same method names). Added `ttsunit.Config.LookAhead`, `netpool.AllowedIP`, `core.ErrSayBusy`, `IdleConfig.Speak`.
- Phase 6 R5: breaker-open and attempt-timeout errors returned by `resilience.Execute` are no longer `*ProviderError`. Use `errors.Is` and the failover class.
- Phase 7: `OutputAudioFrame.Text`, `rtvi.TypeBotTTSText`, `Handler.FormatBotTTSText`.
- 7b: `SpeakingStateFrame.Uninterruptible` and `turn.MuteCriticalOutput` removed.
- Phase 8: the `tools/eval/scenarios` API is deleted (`SevenScenarios`, `ExtendedParityScenarios`, `AllScenarios`, `GetScenario`, `Scenario`).
- Further API changes between `1993240` and `442ad79` (checked symbol by symbol against both trees):
  - Removed from `core`: `dtmf.OutputProcessor.Send`; `filter.Chain.Len` and `Chain.Delay`; `record.Config.SampleRate`; `tools.LoopConfig.OnAsyncResult` (use `AsyncSink`); `AsyncTracer.DroppedOperations`; `turn.TurnReasonInitialNoSpeech`; `turn.Observation.Semantic`; `turn.DetectionInput.Semantic` and `SemanticMaxAge`; `turn.Config.SemanticMaxAge` (the Phase 5 line above names `Config.Semantic`, which did not exist; the field was `SemanticMaxAge`).
  - `core.STTCounters.STTCounts` returns three values (`rotations, rotationFailures, errors`), not four. This breaks any third-party STT implementer of the interface. `google.STTProcessor.STTCounts` follows, and `google.STTProcessor.DeduplicatedResults` is gone.
  - Added: `core.ErrSTTUnavailable` (wrapped by the STT runner and the google and xAI adapters; `server/endreason.go` reads it), `resilience.Policy.OnRetry`, `transcript.Finalizer.Reset`, `tools.ToolLoop.WithFrames`, `OutputDTMFFrame.Result` (a buffered `chan<- error` that receives nil once the tones played or the reason they were not queued).
  - `ToolCallFrame` and `ToolResultFrame` survive the generation fence (they implement `SurvivesGenerationFence`), so a result settles a call the client already saw.
  - `Session.Start` always wraps the tracer in a new `AsyncTracer`; a tracer that already is one is no longer passed through.
  - The 48 kHz to 16 kHz resampler carries leftover samples between chunks. The transcript processor resets its pending turns when the finalizer reports `ErrRetiredTurnLimit`.
- Providers and transport (library users):
  - `ErrProviderUnavailable` is removed from cartesia, deepgram, elevenlabs, nvidia and xai (google keeps its own).
  - `ws.PacedWriter.AfterSelect` and `BeforeWriteAdmission` are removed; `rtvi.ErrUnsupportedDebugEvent` and `rtvi.ErrMissingDebugPayload` are removed; `ws.MaxEncodedFrameSize` went from 64 KB to 20 KB (the largest valid frame is a 16 KB message plus a few bytes).
  - Config fields removed: deepgram and xAI `InterimResults`, elevenlabs `IncludeTimestamps`, cartesia `MaxSilenceSecs`, `MinVolume` and `Version` (the version is a constant), and `Now` on the cartesia, deepgram, elevenlabs and nvidia STT configs (google and xAI keep theirs).
  - The `turn_label` span attribute is gone. `govoice-eval` lost its `rtvi_wire` and `no_speech_timeout` self-checks.
- Phase 9: `DefaultServerConfig`, `LoadConfig`, `PublicAddr`, `WithClock` and `BlackHole.Accepted` are test helpers now (moved to `_test.go` files), not importable. `deadcode` is a permanent check (`make deadcode`, pinned `v0.52.0`); `core/coretest` is its single allowlisted entry. `deadcode` is advisory in CI (`continue-on-error` in `.github/workflows/ci.yml`) and is not part of `make verify`; the docs link checker (`scripts/check-links.mjs`) is.

## 4. Behaviour changes

Phase 2 (STT runner, `03eb695`)
- The STT stream dial runs off the session run loop; a slow dial no longer ends the call on the watchdog. Audio waits in a bounded, ordered pre-roll; at most four streams are open per call.
- A stream lost mid-utterance invalidates the turn, so failover replays it. A failed STT open is a non-fatal per-turn error; the call ends after two failed opens at least `turn.no_transcript_timeout` apart with no transcript between. Failover STT candidates give up after 2 x 1.5 s. Start-up logs a WARN for `turn.no_transcript_timeout` below 3.2 s.
- Deepgram, Cartesia, ElevenLabs and Riva run on the shared runner (about 1,400 lines removed); Deepgram gains `STTCounts`.

Phase 3 (tool events, `318d1cd`)
- The tool loop announces every call and settles it exactly once; the transport emits the RTVI function-call events; the recorder stores `tool_calls` rows (name only for operator-defined tools, a model-invented name is stored as its category; arguments and results are never stored); the Call page shows them. Provider retries and circuit state land on the per-call timeline.
- Offline long caller speech: the xAI STT stream waits 6 s for a final after `audio.done` (`9f37b84`, root cause `4716792`). A reply superseded before any of it was heard keeps its turn row (`3a243fd`).

Phase 4 (failover and transport, `4cc6058`)
- LLM failover fires on synchronous errors; the output resampler is keyed by rate; a VAD worker is respawned with routing to a live one; the TTS watchdog runs on the run loop; the demo install flight has its own bounded context that shutdown cancels and waits for; transport and server hardening (read limit, token lifetime, MCP enable on a missing server answers 404).

Phase 5 to 7b
- `send_dtmf` goes through the frame path, one bounded waiter per send, fenced on barge-in; DTMF A to D are redacted in traces (5). It is available on every step, not only the start step (7b).
- Idle lines (idle prompt in `say` mode, bot-stuck fallback, goodbye) go through `Say`: they are stored as turns, count in `turn_count`, appear in the LLM history, the call record and the recording sidecar (redacted). Turn ids are `say-N` (6.6).
- Unbound interim STT text heard before adoption is seeded into the adopting turn (6.7). An unbound final is not.
- Google and xAI unary TTS drifts (6 R6): a unit longer than `MaxSynthesisUnitChars` is retried whole; `Requests` counts at admission; `Seq` and `Duration` are set from the bytes; a lone trailing byte is dropped; Google's attempt timeout bounds one synthesis request, not the unit.
- `tools/livecheck` reads WAV more strictly and now decodes IMA ADPCM clips it used to feed as PCM.
- Flow settings (7b): interruptions, end of turn (default, smart, eager), route and hold sounds, background per step, `end_call` on a one-step flow, call limit. `muteTools` is `(Manager || PushToTalk) && function_call`. New `turn.analyzer.for_flows`.
- UI (7): "Guide this call" panel, `missing_env` listing, paged flow lists (1000 flows at most), microphone asked before the token is minted, Sign out clears the management key.

Phase 9 (user-visible)
- A scripted line (`Say`) that is admitted just as a model turn starts is dropped silently, with a debug log only; before, it could reset the model turn. An idle prompt dropped this way counts as spent and is not asked again (9.5b). A `Say` refused earlier, while a model reply is already active, keeps the Phase 6 rule: the idle prompt is not spent and is asked again one wait later.
- A client that stalls an RTVI write on the run loop is closed as `slow_client` within about 1.5 s (the write bound is 1 s; worst case in the test 1.505 s) instead of ending the call on the 2 s watchdog (9.5a). `Connection.writeMu` is deleted; the websocket library serialises writes.
- `server.waitUntil` no longer leaves its polling goroutine running after the wait returns (9.2); `Server.mgmt` is an atomic pointer, which fixes a real data race between `closeManagement` and the route, end-call and demo handlers (`1213d2c`).
- The resampler general path allocates one buffer per chunk (7 to 1) (9.5c). A VAD worker that fails to spawn, start or handshake, or whose pipe fails mid-call, is logged at WARN (`vad: worker failed`) with the last 4 KB of its stderr (the buffer holds 64 KB) (9.5d, `324b5d7`).

Final-review fixes (`5bb357a..442ad79`)
- A panic in a WebSocket goroutine (reader, ping, ready watchdog, pacer) or in `onSessionReady` closes that connection with 1011 instead of crashing the process (`3be4def`). The same commit contains panics in the turn recorder writer (`server/call.go`), the cue and hold timers, the STT-outage hang-up goroutine, and the echo and google LLM stream goroutines (reported as a stream error).
- A TTS watchdog retry rejects frames from the abandoned candidate, so the same audio is no longer heard twice (`5bb357a`).
- The LLM first-token watchdog runs its failover on the run loop, like TTS (`18d3b24`).
- A stopped STT stream emits no transcript (`eac00d6`).
- `bot-tts-text` is not sent for a unit a barge-in cut (`1d556c8`).
- Store errors and panic values are redacted before they are logged (`3be4def`); the context-summary retry pause uses monotonic time (`8453cc8`); causes are wrapped with `%w` in the VAD, Smart Turn and provider errors so `errors.Is` sees both the sentinel and the cause (`60bdb0f`).

## 5. Privacy and security

- Trace spans record the error type only (1.2). Secrets are read from the environment only. Tool arguments and results are never stored or sent; idle lines are redacted in the recording sidecar; DTMF A to D are redacted in traces.
- One SSRF rule, `netpool.AllowedIP`, used by openai and nvidia (6.2).
- The vendored ONNX Runtime tree (`third_party/onnxruntime-osx-arm64-1.29.0`: headers plus LICENSE, ThirdPartyNotices, README and cmake files; 31 files, 30,186 lines) is no longer vendored; `make ort` fetches the runtime (see the macOS pin under Known gaps).

## 6. Measured

Baseline `1993240` against head `87f906c`, same machine (Apple M4 Pro, 12 cores), idle, base and head interleaved. `-tags xaispeech` throughout.

**Lines** (`git diff --numstat 1993240 HEAD`)

| Category | Files | + | - | Net |
|---|---|---|---|---|
| Non-test Go | 149 | 5,783 | 6,060 | -277 |
| Test Go | 215 | 14,521 | 1,575 | +12,946 |
| UI (non-test) | 31 | 1,128 | 304 | +824 |
| UI tests and e2e | 36 | 2,010 | 188 | +1,822 |
| docs/superpowers and docs/history (records) | 14 | 14,206 | 3 | +14,203 |
| Other docs | 56 | 253 | 251 | +2 |
| Other (scripts, Make, CI, config, deploy) | 47 | 742 | 767 | -25 |
| Vendored (`third_party` ORT tree) | 31 | 0 | 30,186 | -30,186 |
| Generated (`deploy/sbom.json`) | 1 | 0 | 1,879 | -1,879 |
| Total | 580 | 38,643 | 41,213 | -2,570 |

**Dead code** (`deadcode@v0.52.0`, `-test=false`, `-tags xaispeech`)

| Roots | Base | Head |
|---|---|---|
| `./cmd/... ./examples/...` | 32 | 2 |
| plus `./tools/... ./offline/cmd/...` (wide) | 39 | 16 (all `core/coretest`, allowlisted) |
| wide, tests as roots (`-test`) | 33 | 16 (all `core/coretest`) |

**Tests and static analysis**

| | Base | Head |
|---|---|---|
| Go `Test/Benchmark/Fuzz/Example` functions (all modules) | 2,041 | 2,394 |
| Vitest tests | 677 | 795 |
| Playwright (`flows` + `oneport` configs) | 40 | 53 |
| `go vet` findings | 0 | 0 |
| staticcheck 0.8.2 findings | 25 | 13 |

Go counts are `git grep -hE '^func (Test|Benchmark|Fuzz|Example)' <rev> -- '*_test.go' | wc -l` at `1993240` and at `442ad79` (a count of declarations, not of executed subtests). The 2,019 / 2,355 recorded earlier came from a run at `87f906c`.

staticcheck by id, base to head: U1000 9 to 0, ST1011 2 to 0, S1002 1 to 0; unchanged: SA1019 8, S1040 1, SA4000 1, SA4006 1, ST1013 1, ST1018 1.

**Artifacts**

| | Base | Head | Change |
|---|---|---|---|
| `cmd/server` (trimpath) | 50,054,146 B | 51,084,834 B | +2.1% |
| UI JS raw / gzip | 781,650 / 232,155 B | 809,322 / 243,558 B | +3.5% / +4.9% |
| UI CSS raw / gzip | 65,529 / 12,050 B | 66,810 / 12,705 B | +2.0% / +5.4% |

**Benchmarks** (benchstat, 10 runs each; the seven benchmarks that exist on both sides)

| Benchmark | Base | Head | Change |
|---|---|---|---|
| `Resampler_20msChunk` (16 k to 24 k) | 1.132 us, 3.56 KiB, 7 allocs | 1.142 us, 1.00 KiB, 1 alloc | time ~, bytes -71.9%, allocs -85.7% |
| `Denoise20ms` | 18.64 us, 640 B, 1 alloc | 19.29 us, 640 B, 1 alloc | +3.5% (p<0.001) |
| `Detector20msFrame` | 13.63 us, 0 allocs | 13.73 us, 0 allocs | ~ |
| `STTFailoverAudioFrame` | 265.6 ns, 0 allocs | 254.5 ns, 0 allocs | ~ |
| `EmitFrameDebugAudio` (no observer / observer) | 1.305 / 7.508 ns, 0 allocs | 1.310 / 7.553 ns, 0 allocs | ~ / +0.6% |
| `smartturn Features` | 2.196 ms, 10 allocs | 2.210 ms, 10 allocs | +0.6% |
| `UnderlayIdleWriters` | 2.003 s, 1.302 M allocs | 2.003 s, 1.302 M allocs | ~ |

`Denoise20ms` is the only measurable slowdown: 0.65 us per 20 ms frame, no allocation change; not tuned. Head-only benchmarks (median of 10): `Downsample48To16` 794 ns, 640 B, 1 alloc; `ttsunit UnitWrite` 5,903 ns, 62,552 B, 65 allocs; google `UnaryUnit` 43,971 ns, 66,610 B, 163 allocs; xAI `UnaryUnit` 41,668 ns, 65,899 B, 154 allocs (per 1 s unit, not per frame).

**Load** (`tools/loadtest -sessions 4 -duration 30s`, median of 3 interleaved runs; echo provider, so provider latency is simulated)

| | Base | Head |
|---|---|---|
| Client voice-to-voice p50 / p95 / p99 | 980.6 / 1,000.2 / 1,003.1 ms | 981.0 / 1,003.4 / 1,004.4 ms |
| Server `stt_final` p99, `voice_to_voice` p99 | 997.5 ms, 997.5 ms | 997.5 ms, 997.5 ms |
| CPU seconds per session-minute | 2.18 | 2.25 |
| MB per session | 12.59 | 11.33 |
| Goroutines after drain | 17 | 17 |
| Turns completed / sessions failed | 28 / 0 | 28 / 0 |

`govoice-bench` framework overhead (median of 3, 4 sessions x 200 turns): p50 14 us / 14 us, p95 0.100 / 0.107 ms, p99 0.184 / 0.192 ms (base / head). At head, `make loadtest-smoke` and `make perf-gate` both pass (perf-gate against this machine's committed baseline). No p99 regression beyond the gate's tolerances.

**Race stress (9.6):** all 12 modules, `go test -race -count=5 -shuffle=on -tags xaispeech`, passed in 2,197 s with zero failures.

## 7. Known gaps, flakes and carried items

Line target. The master plan asked for at least 5,000 lines removed net. It is not met and no code was deleted to reach it: total net is -2,570 over 580 files (the figure was -3,244 over 553 files after Phase 8; Phase 9 added tests, the check scripts and this document). Non-test Go is -277. Almost all of the large negative number is outside code: vendored ORT headers -30,186 and the SBOM -1,879. Test Go grew by 12,946 lines.

Behaviour
- When a call has exactly one STT provider, a `Send` failure on its stream ends the call (pre-existing; the four providers on the shared runner now share it).
- `ttsunit.Config.LookAhead` (Google's two-unit look-ahead) has no effect when TTS fallbacks are configured: the failover wrapper drives one unit at a time (`TTSFailover.cur`).
- A VAD worker found down when a batch arrives, or one that hung past its deadline, is restarted without a log line; the batch of a down worker is handed to a live worker. Spawn, start, handshake and mid-call pipe failures log the stderr tail.
- A client that stalls writes reaches `slow_client`, but under a real stall the close frame often cannot be sent, so the client sees close code 1006, not 1008.
- The audio path can still hit the 2 s run-loop watchdog. This predates the remediation; 9.5a bounded only the run-loop RTVI writes.
- Unbound interim text seeded into a turn (6.7) has a false-positive risk on Riva that needs a live provider to measure.
- Gemini can send two consecutive user turns after a refused LLM turn; needs a live provider.
- The background switch between route steps has no fade and can click.
- `turn_count` counts idle and greeting lines (documented in `docs/api/v1.md`).
- Ping-failure close stays counted as `disconnect_client` (deliberate; `ws_ping_failures` has its own counter). U1 database columns (`turns.tools_json`, the two summary columns) are kept to avoid a migration.

Review items carried, not fixed
- Core: the session round holds an `RLock` across blocking operations; an abandoned executor is not reclaimed; preroll can emit empty frames; a per-frame `context.WithTimeout` allocates; LLM failover has unfixed edge cases; DTMF reports `ToolSucceeded` when the context ends.
- Providers and transport: `cmd.Wait` runs under a lock in the VAD worker; xAI preroll drops the newest audio when full, ignores a 16 KB RTVI refusal, and rotates Google credentials on the run loop; the Google environment fallback is used by the examples only.
- Server: cue timers can fire after hang-up; shutdown does not wait for in-flight HTTP requests; nvidia circuit endpoints are mixed in one breaker; `turnWarned` is unbounded.
- Tooling: the `deadcode` run roots include `tools`, and `SetSystemInstruction` is not reported by it; the `silence_timeout` eval scenario is the same as `turn_completion`; one fixture is untracked; the macOS ORT sha256 is unpinned (see Build and tooling).

Tests
- Test-only seams remain in production code: the `AsyncSink` nil path, `Denoiser.Delay`, `Runner.Streams`, the `OutputUninterruptible` branches, `adjustCall`, `ttsUnitTimeout`, `gapHook` and the pacing hooks. Several tests use sleeps as synchronisation.
- Two cross-lane tests depend on timing and are recorded rather than fixed: `providers/deepgram/stt_test.go:556` and `transport/ws/rtvi_integration_test.go:356`.
- The smartturn timeout flake is tied to the 9.3 counter bug by reasoning and a deterministic probe; the original failure was never reproduced. The core flush flake is explained by the run loop choosing between the control and data lane at random when both are ready (9.4); the old test did not fail in 300 stress runs.
- `goleak` is not yet on `smartturn`, `core/turn`, `core/record`, `core/tools`, `providers/xai`, `providers/google`, `providers/echo`, `core` itself, `server/otelexport`, `server/runtime`, `offline/adapter`. It runs where goroutines are owned and checked in 9.2.
- Whole-package stress of `smartturn` takes about 45 s per iteration, so it was run on a filtered test set.
- No deterministic test for an injected write error on `OnWriteComplete`; the denied-microphone path is covered by Vitest only.
- The checked-in Playwright run on the echo stack cannot exercise a real Smart Turn model.

Call-setup panic handling (open)
- A panic between the active-calls insert and the done goroutine can still leave an `activeCalls` entry; near-unreachable.
- A panic after the call is registered is owned by `finishCall`; it is not covered by the setup recover.
- Three recovered panic values are still logged unredacted: `core/session.go`, `transport/ws/pacing.go` (line 808) and `server/demo_api.go` (line 57).

Build and tooling
- The macOS ONNX Runtime download has no pinned sha256 yet (`fetch-ort` warns); the hash comes from the next CI `make ort` run.
- `go work sync` is not a no-op in this workspace; module files are edited by hand.
- `PLAYBACK_CLEAR_REQUIREMENT_FAILED` under `relax-unmodified-client-playback-clear` stays recorded.

Deferred backlog (not scheduled)
- Settings audit R8: LLM sampling parameters (temperature, maximum output tokens).
- Settings audit R4 (uninterruptible greeting), R5c (sound preview and upload), R7 (per-flow DTMF).
- `bot-output` and word-level TTS text; flow version history UI beyond the editor's compare.
- Offline long-speech: closed by `4716792`; 104 later ledger logs pass, none fail. The rule to rerun a failing `TestOfflineStackLongCallerSpeechIsOneTurn` five times alone stays in the gate.
