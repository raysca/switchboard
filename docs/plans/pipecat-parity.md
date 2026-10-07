# Pipecat Parity Plan (server-only, no extra transports)

**Scope:** everything that makes a voice AI *server* robust, fast and scalable. Out of scope: video, new transports, Flows DSL, bus/multi-worker agents, speech-to-speech realtime models, Tail UI.
**Tracked in:** goalpulse goal `pipecat-parity` (task ids below match).
**Reference:** `pipecat-impl/` @ `2967e1c`. Every task that ports a Pipecat behavior adds a paired fixture to the parity matrix (P6-1).

## Rules for all tasks
- Core stays provider-neutral and never imports `server/` (see `docs/v2-plan.md` §1).
- Every new processor/strategy ships with a deterministic test using fake clocks (`core/fakes.go`) and passes `-race`.
- Every new knob is config-driven, off or conservative by default, and has a metric.
- Done = tests green (`make verify`), metric/timeline visible, one-paragraph doc update.

---

## Phase 0 — Measure first (observability foundation)
Nothing else can be tuned without numbers.

**P0-1 Observer subscription API** (`core/`). General observer delivery for `core.DebugEvent` (the "Task 35" noted in `docs/pipecat-compatibility.md`): non-blocking, bounded, drop-counted, per-session subscribe/unsubscribe. *Accept:* slow observer never stalls the pipeline (test with blocked observer); drops counted.

**P0-2 Latency observers** (`core/observe/`, deps P0-1). Port Pipecat `user_bot_latency_observer`, `startup_timing_observer`, `service_metrics_observer`, `turn_tracking_observer`. Emit: STT final latency, LLM TTFT, TTS TTFB, user-stop→bot-first-audio (voice-to-voice), per turn. *Accept:* Prometheus histograms (no high-cardinality labels), values on the session timeline, one e2e test asserting ordering.

**P0-3 Pipecat evals black-box spike** (`tools/eval/`). Point `pipecat.evals` (scenario, persona, judge) at the Go server over RTVI/WebSocket. *Accept:* 3 scripted scenarios run green in CI nightly; a short note on what pipecat evals cannot assert (internal state) so Go tests keep that. No new Go harness unless the spike fails.

**P0-4 Baseline benchmark capture** (deps P0-2). Record current p50/p95 voice-to-voice and per-stage latency with Google providers on a fixed audio corpus; commit as `docs/benchmarks/baseline.md`. *Accept:* reproducible by one make target.

## Phase 1 — Turn-taking quality (largest UX gap)

**P1-1 UserTurnStartStrategy interface** (`core/turn/`). Strategies: VAD, transcription-based, min-words, wake-phrase, external. Replace the hard-wired start path in the manager. *Accept:* min-words suppresses a cough/"uh-huh" barge-in in a test; existing manager ordering tests unchanged.

**P1-2 UserTurnStopStrategy interface** (deps P1-1 shape). Strategies: speech-timeout, turn-analyzer, eager, deferred, external. Existing silence fallback becomes `speech-timeout`. *Accept:* manager still has a single bounded detector call in flight (preserves current reentrancy guarantee).

**P1-3 Smart-turn ONNX worker** (`vad/`-style supervised worker, deps P1-2). Audio-based end-of-turn model run in a child process like Silero, with the same crash isolation, capacity and timeout rules; implement as a turn-analyzer. *Accept:* worker crash falls back to speech-timeout with a metric; inference p95 < 50 ms on target CPU; model hash pinned in the compat lock.

**P1-4 LLM turn-completion strategy** (deps P1-2). Port `llm_turn_completion` / `user_turn_completion_mixin` (complete/incomplete markers in LLM output, hold-and-wait on incomplete). *Accept:* incomplete utterance holds the turn, no TTS emitted, timeout releases it.

**P1-5 User mute strategies** (deps P1-1). Mute user input during tool calls, first bot speech, or flagged critical output; maps to the interruptible/uninterruptible output classes already in V2. *Accept:* muted audio is not transcribed or does not barge in; unmute restores.

**P1-6 User and bot idle controller** (deps P0-1). Configurable prompt after N s of silence ("are you still there?"), escalate, then end session; bot-idle (stuck LLM/TTS) watchdog. *Accept:* fake-clock test walks prompt → prompt → hang-up; counters exported.

**P1-7 Turn-quality eval gate** (deps P1-3, P1-4). Run Pipecat's `evals/turn-completion` dataset against the Go strategies; track false-cut and late-cut rates. *Accept:* thresholds committed; CI fails on regression.

## Phase 2 — Provider robustness and breadth

**P2-1 Fallback chain model.** Agent config gets an ordered provider list per stage (STT/LLM/TTS) with explicit failover policy. Integrates with `core/resilience` breaker state (open breaker ⇒ skip). Server migration + API + stored-agent version pinning. *Accept:* config validated; chains pinned per call.

**P2-2 LLM failover** (deps P2-1). Switch only before first token has been emitted; after that, fail the turn cleanly. *Accept:* primary 5xx/timeout → fallback answers within one extra attempt budget; no duplicated assistant text.

**P2-3 TTS failover** (deps P2-1). Switch at sentence boundary; never replay already-played audio. *Accept:* mid-turn provider death continues with next sentence on fallback voice.

**P2-4 STT failover with audio replay** (deps P2-1). Ring buffer of recent audio (bounded) replayed to the fallback so the in-progress utterance is not lost. *Accept:* kill primary mid-utterance, the final transcript still contains the whole utterance.

**P2-5 Deepgram STT provider** (deps none, uses existing provider contract). Streaming, interim/final, endpointing off (we own turns). *Accept:* provider contract conformance suite + resilience tests parity with google.

**P2-6 Cartesia TTS provider.** Streaming with word timestamps (feeds P3-1). Same acceptance.

**P2-7 ElevenLabs TTS provider.** Same acceptance.

**P2-8 OpenAI-compatible LLM provider.** Covers OpenAI, Groq, Together, vLLM via base URL; tools + streaming + usage. Reuse xai SSE code where possible. *Accept:* contract + tool-loop tests.

**P2-9 Connection warm-up and keepalive.** Pre-open STT/TTS websockets and LLM HTTP/2 during session setup; keepalive and reconnect on idle drop. *Accept:* first-turn TTFB improves by measured amount vs baseline (P0-4); no leaked sockets after teardown.

## Phase 3 — Conversation intelligence

**P3-1 Word timestamps and sentence aggregation.** Plumb word timings from TTS providers through frames; sentence/clause aggregator tuned for first-audio latency. *Accept:* first-audio latency not worse than baseline; timings present on output frames.

**P3-2 Interrupted-assistant reconciliation** (deps P3-1). On barge-in, context keeps only what was actually played (by timestamp/paced-output position), not what was generated. *Accept:* test interrupts mid-sentence; context contains the heard prefix only.

**P3-3 Async and parallel tool calls** (`core/tools`, see `docs/decisions/tool-loop.md`). Parallel execution, per-call timeout, result ordering, and in-flight user messages while a long tool runs (Pipecat `test_async_tool_messages`). *Accept:* two tools run concurrently; user speaks during a tool and conversation stays coherent; cancellation on interruption.

**P3-4 Context summarization** (deps none). Trigger on token budget; summarize older turns with a configurable LLM stage; keep system prompt, tools and last N turns verbatim. *Accept:* budget never exceeded in a long-conversation test; summary call failure degrades to truncation.

## Phase 4 — Telephony-grade audio

**P4-1 DTMF in/out.** Detect inbound tones (frame kind + RTVI/tool event), send outbound digits. Expose as a tool primitive for IVR navigation. *Accept:* golden tone fixtures detected; outbound tones are accurate.

**P4-2 Audio filter abstraction** (`core/audio/filter`). Input filter chain placed before VAD; one real denoiser behind it (decision needed: RNNoise vs vendor, see Open Questions). *Accept:* filter in a supervised worker if native; false-barge-in rate drops on noisy corpus.

**P4-3 Output mixer and volume** (deps P4-2 abstraction). Optional background ambience and gain control. *Accept:* mixing is paced correctly and does not break flush/barge-in bounds.

**P4-4 Call recording** (deps P0-1). Stereo (user/bot) audio buffer and transcript, written to pluggable storage, honoring `server/retention` and `docs/privacy.md`. *Accept:* recording on/off per agent; PII/retention policy tested; barge-in does not corrupt the bot track alignment.

## Phase 5 — Scale and operations (the differentiator)

**P5-1 OpenTelemetry tracing** (deps P0-2). Conversation → turn → service spans; use the existing `otel` config. *Accept:* spans exported in a test collector; no content in attributes by default.

**P5-2 Per-tenant concurrency and rate limits.** Extend `server/admission.go`: tenant/agent quotas, fair queueing vs reject, metrics. *Accept:* noisy tenant cannot starve others in a test.

**P5-3 Rolling drain and affinity guidance.** SIGTERM drain with deadline, `/ready` flips immediately, sessions finish or are told to reconnect; document LB affinity and deploy recipe. *Accept:* rolling-restart test with no dropped active calls inside the deadline.

**P5-4 Load and soak harness** (deps P0-3, P0-4). N concurrent simulated calls using fake providers (isolates framework cost) and recorded real providers; reports p50/p95/p99, CPU, memory, goroutines. *Accept:* 24 h soak with no growth in goroutines/RSS; max-sessions-per-core number published.

**P5-5 Latency and capacity CI gate** (deps P5-4). Fail on regression beyond a tolerance vs the committed baseline. *Accept:* gate runs on PRs touching `core/` and `vad/`.

## Phase 6 — Parity verification

**P6-1 Parity matrix and paired fixtures.** Update `docs/pipecat-compatibility.md`: per feature, Pipecat behavior, Go behavior, intentional differences. Add paired fixtures for turn strategies, idle, context aggregation, tool loop.

**P6-2 Whisker decision.** Keep the semantic adapter and its 11 scenarios; do not pursue wire compatibility until a package and protocol version are pinned; only revisit if a team needs live pipeline debugging. Record in `docs/decisions/`.

**P6-3 Final parity report.** Render goalpulse snapshot, publish gaps closed and consciously skipped.

## Open questions (assumptions logged in goalpulse)
1. Denoiser choice (RNNoise/open source vs vendor) — assumed open source first.
2. Smart-turn model licensing and distribution (pin hash like Silero).
3. Recording storage backend — assumed local/S3-compatible interface only.
4. Which two extra TTS/STT vendors the team actually wants first.
