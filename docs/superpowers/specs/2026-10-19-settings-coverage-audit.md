# Settings coverage audit: library capabilities vs server, flow and UI exposure

Date: 2026-10-19. Tree: `main` @ `dbcb21f` (Phases 1-6 merged). Read-only audit; nothing was changed or run beyond reading code.
Question answered: *"flow and agent options like bed music, silence, barge-in etc: there is considerably more features supported by the library than the flow/agent settings expose."*

Short answer: true for **turn-taking and interruption** (every knob is server-YAML-only; the per-flow behaviour spec deliberately deferred it as "B2"), partly true for **bed music** (a flow can choose a server sound, its volume and loop, but nothing changes it during the call and there is no ducking, fade, hold sound, upload or preview), and mostly false for **silence prompts** (fully per-flow with UI since the per-flow call-behaviour work). Several things the user might expect are **not in the library at all** (LLM temperature/max tokens, STT keywords, TTS speed outside ElevenLabs, voicemail detection, ducking). Three per-step settings are **silently ignored** (see "Dead or misleading").

## How to read the matrix

Layers:

- **A. Core knob**: the struct field or function in the library (`core/`, `vad/`, `smartturn/`, `transport/ws`, `providers/`).
- **B. Server YAML**: `server/config.go` (+ defaults in `server/config_defaults.go`, `server/idle.go`, `server/turn_stop.go`, `server/warmup.go`; samples in `config/*.yaml`). There is no `docs/configuration*.md`; YAML is documented in struct comments, `config/development.yaml` (commented reference) and `docs/audio-filters.md`, `docs/operations_runbook.md`.
- **C. Per-flow**: `server/flow/config.go` `Config` (+ `call_behavior.go`, `definition.go`). "start step" = call-level (rejected on other steps by `validateCallLevel`); "per step" = honoured on every step.
- **D. UI**: `ui/src/routes/flows/...` (flow editor). `ServerView.tsx` is read-only operations status; **no UI edits server YAML**, so a YAML-only setting never has a UI control.
- **E. Pipecat**: `pipecat-impl/src/pipecat/...` at the pinned commit. Note: the pinned Pipecat has **no** `allow_interruptions`, `MinWordsInterruptionStrategy` or `VolumeInterruptionStrategy` (grep finds none); interruption control there is the `turns/user_start` + `turns/user_mute` strategies, which is what Switchboard mirrors.

Gap types (the last column):

| Code | Meaning |
|---|---|
| `OK` | Exposed at every layer that makes sense (per-flow + UI, or server-only by design with a reason) |
| `SRV` | Server-only by design (infrastructure, cost or safety ceiling; not a conversation choice) |
| `MPF` | Missing per-flow: a server YAML key exists, a flow cannot set it, and it is a conversation-design choice |
| `MUI` | Missing UI: the server/flow accepts it but the editor has no (or only a weak) control |
| `LIB` | Library-only: a core knob no config reaches (fixed default) |
| `NI` | Not implemented in core (Pipecat has it, or the user asked for it) |
| `DEAD` | Dead or misleading: accepted/shown but has no effect, or has no producer |

---

## 1. Bed / background music, cues and output audio

| # | Capability | A. Core knob | B. Server YAML | C. Per-flow | D. UI | E. Pipecat | Gap |
|---|---|---|---|---|---|---|---|
| 1.1 | Looping background bed under the voice (keeps playing in silences) | `core/audio/mixer/background.go` `Asset`, `Cursor.Mix`; mixed in `transport/ws/pacing.go` (`ambientLocked`, `Underlay`); holder `transport/ws/cue.go` `CueUnderlay` | `audio.backgrounds{name:{file,gain_db,loop}}` (max 8), `audio.background_default`, `audio.allow_flow_background` (default false; true in development) | start step `audio.background{name,gain_db,loop}`; `{"name":""}` = none; name only, never a path (`server/background.go` `backgroundFor`) | `CallBehaviorFields.tsx` `AudioFields`: select (server default / None / catalogue names from `GET /server-config`), disabled with reason when not allowed | `audio/mixers/soundfile_mixer.py` `SoundfileMixer(sound_files, default_sound, volume=0.4, loop)` | `OK` |
| 1.2 | Bed volume | `mixer.NewCursorGain` (per-session gain over the shared asset) | `backgrounds.<n>.gain_db` -60..0 (default -30) | `audio.background.gain_db` -60..0 | number input "Background volume (dB)" | `volume` 0..1 | `OK` |
| 1.3 | Loop vs one-shot | `Cursor` `loop`; 5 ms loop-point crossfade in `newAsset` | `backgrounds.<n>.loop` (default true) | `audio.background.loop` | "Repeat" checkbox | `loop` | `OK` |
| 1.4 | Adding your own sound file | `mixer.LoadAsset`: PCM16 mono WAV, 8..192 kHz, <= 10 MB, low-passed and resampled once to 24 kHz | operator copies a WAV onto the server filesystem, adds it under `audio.backgrounds` and **restarts** (read once at start-up; a bad file stops start-up; no reload/SIGHUP) | none, by design (a flow names a catalogue key so it cannot make the server open a file) | **none**: no upload, no preview/listen, only the name list | code-level `sound_files` dict | `SRV` (path) + `MUI` (no preview; no upload) |
| 1.5 | Change or stop the bed mid-call / per step | `CueUnderlay.SetBackground(u)` is atomic and safe while mixing (nil = off) | none | call-level only (`audio` rejected on non-start steps) | none | `MixerUpdateSettingsFrame{sound, volume}`, `MixerEnableFrame` | `MPF` (library capable; server calls `SetBackground` once, when the flow resolves) |
| 1.6 | Pause the bed during tool calls / while the user speaks | none (bed is NonSpeech + uninterruptible, never paused) | none | none | none | only by pushing `MixerEnableFrame` from app code | `NI` |
| 1.7 | Ducking (lower bed under speech) | none: `Cursor.Mix` adds at a constant level, saturating | none | none | none | none (constant `volume`, `np.clip`) | `NI` (neither has it) |
| 1.8 | Bed fade in/out | none (only the 5 ms loop crossfade) | none | none | none | none | `NI` |
| 1.9 | Hold music / "working on it" sound while a slow (synchronous) tool runs | none; route cue covers route gaps only; async tools give an immediate spoken ack instead | none | `async_tools` (start step) is the only lever | ToolPicker "Background" checkbox | none built in | `NI` |
| 1.10 | Route cue (sound in the gap between steps) | `server/cue.go` `cueController` (starts after `after_ms` of quiet, 30 ms fade, 10 s cap, 90 s max wait; stops on first new-step audio, caller speech, failure) over `CueUnderlay.Start/Stop(fade)` | `audio.cues{name:{file,gain_db(-20),loop}}` (max 8) | definition-level `route_cue{name, after_ms 0..10000}` | `FlowSettingsPanel.tsx` "Route sound": **free-text** name + after ms | none (pipecat-flows has none) | `MUI` + `DEAD`-risk: cue names are not in `GET /server-config`, publish does not check the name (`flow/policy.go` checks backgrounds only), an unknown name just logs a warning per call |
| 1.11 | Cue fade/cap timings | `server/cue.go` consts `cueFade` 30 ms, `cueHardCap` 10 s, `cueMaxWait` 90 s | none | none | none | n/a | `LIB` (fine) |
| 1.12 | Thinking / filler cues (sound or phrase while the LLM thinks) | none (grep `filler|thinking|duck` finds only completion-marker docs) | none | none | none | none built in | `NI` |
| 1.13 | Voice output gain | `mixer.Gain` (last stage; Q12, saturating) | `audio.output_gain_db` -30..+6 | start step `audio.output_gain_db` (explicit 0 overrides server gain) | number input "Voice volume (dB)" | `audio/volume.py` helpers | `OK` |
| 1.14 | Inbound noise reduction | `core/audio/filter` denoiser | `audio.input_filters: [denoise]`, `audio.denoise_strength` | start step `audio.denoise{strength 0<..1}` (can turn on, cannot turn off a server-wide denoise) | checkbox + strength % | `audio/filters/rnnoise_filter.py`, krisp, koala, aic | `OK` (only the "flow cannot disable" asymmetry) |
| 1.15 | Recording with/without bed | recording tap gets the unmixed speech frame (bed and cues never recorded) | n/a | n/a | n/a | `AudioBufferProcessor` records what is sent | `OK` (documented) |

## 2. Silence handling: idle prompts, VAD and end of turn

| # | Capability | A. Core knob | B. Server YAML | C. Per-flow | D. UI | E. Pipecat | Gap |
|---|---|---|---|---|---|---|---|
| 2.1 | Quiet-caller wait | `core/idle.go` `IdleConfig.UserTimeout` | `idle.user_timeout` (default 15 s; 0 = off) | start step `idle.user_timeout_ms` 0 or 1000..300000 (must exceed `dtmf.aggregate_timeout` when aggregation is on: `flow/policy.go`) | `QuietCallerBasics` checkbox + seconds | `turns/user_idle_controller.py` `user_idle_timeout` | `OK` |
| 2.2 | Prompts before hang-up | `IdleConfig.MaxPrompts` | `idle.max_prompts` (2) | `idle.max_prompts` 1..10 | number | none (app callback) | `OK` |
| 2.3 | Prompt text and mode (model-worded vs verbatim) | `IdleConfig.UserPrompt`, `Say` | `idle.prompt`, `idle.prompt_mode` | `idle.prompt` (<= 200 B), `idle.prompt_mode` llm/say | textarea + radio | app code | `OK` |
| 2.4 | Goodbye before idle hang-up | `IdleConfig.Goodbye`, `GoodbyeWait` (10 s, fixed) | `idle.goodbye` | `idle.goodbye` (<= 200 B) | textarea | app code | `OK` (`GoodbyeWait` `LIB`) |
| 2.5 | Idle pauses while a tool runs / caller muted / reply active | `IdleConfig.ToolsBusy`, `Muted`, `Answering` | automatic | automatic | n/a | n/a | `OK` |
| 2.6 | Stuck-bot watchdog (no bot speech after a user turn) | `IdleConfig.BotTimeout`, `BotMaxStuck`, `BotFallback` | `idle.bot_timeout` (0 = off), `idle.bot_max_stuck` (3), `idle.bot_fallback` | none (Ruling 4 of the per-flow spec: operational safety net) | none | pipeline `idle_timeout_secs` only | `SRV` (but `bot_fallback` is caller-facing wording: candidate `MPF`, low) |
| 2.7 | VAD stop hangover | `vad.ProcessorConfig.StopChunks` (32 ms chunks) | `vad.stop_ms` (200; 200..2000) | start step `vad_stop_ms` | number "Wait after the caller stops" | `VADParams.stop_secs` 0.2 | `OK` |
| 2.8 | VAD start confidence / stop threshold | `vad.ProcessorConfig.StartThresh` 0.5, `StopThresh` 0.35 | none | none | none | `VADParams.confidence` 0.7 | `LIB` |
| 2.9 | VAD start duration | `ProcessorConfig.StartChunks` 2 (64 ms) | none | none | none | `VADParams.start_secs` 0.2 | `LIB` |
| 2.10 | VAD minimum volume | `ProcessorConfig.EnergyGate` (0 = off; nothing in `server/` sets it) | none | none | none | `VADParams.min_volume` 0.6 | `LIB` (zero-value-only knob) |
| 2.11 | Speech-timeout resume window | `core/turn/stop.go` `SpeechTimeoutConfig.UserSpeechTimeout` (0 = 600 ms) | `turn.speech_timeout` | none | none | `speech_timeout_user_turn_stop_strategy.py` `user_speech_timeout` 0.6 | `MPF` |
| 2.12 | Late-transcript wait (STT p99) | `SpeechTimeoutConfig.STTP99`; `turn.DefaultSTTP99(provider)` | `turn.stt_p99{provider: dur}` | start step `stt_p99_ms` 100..10000 | number "Wait for a late transcript" | implicit in the strategy | `OK` |
| 2.13 | No-transcript fallback | `SpeechTimeoutConfig.NoTranscriptTimeout` (5 s) | `turn.no_transcript_timeout` | none | none | aggregator `user_turn_stop_timeout` | `SRV` (safety) |
| 2.14 | End-of-turn strategy: eager | `turn.EagerStop(grace)` | `turn.stop: [eager, speech-timeout]`, `turn.eager_grace` (200 ms) | none | none | `eager_user_turn_stop_strategy.py` (speculative, different mechanism) | `MPF` (depends on the flow's STT punctuating, and STT is per-flow) |
| 2.15 | End-of-turn strategy: Smart Turn model | `smartturn.Stop`, `smartturn.Config{Budget, Hold, PreSpeech}` | `turn.stop: [turn-analyzer, speech-timeout]`, `turn.analyzer.{mode,workers,model_path,stop_secs,budget}` | none | none | `turn_analyzer_user_turn_stop_strategy.py`, `local_smart_turn_v3.py` (`stop_secs`, `pre_speech_ms`) | `MPF` for on/off and `stop_secs`; workers/model `SRV`; `PreSpeech` `LIB` |
| 2.16 | End-of-turn strategy: LLM completion markers | `core/turn_completion.go` `TurnCompletionConfig{ShortWait, LongWait, Nudge}` | `turn.stop: [llm-completion, speech-timeout]`, `turn.completion.{short_wait,long_wait,prompt,nudge}` | none | none | `llm_turn_completion_user_turn_stop_strategy.py` | `MPF` (prompt-level conversation design; costs tokens per turn) |
| 2.17 | Turn manager on/off | `core/turn.Processor` | `turn.manager` (true) | none | none | `user_turn_processor.py` | `SRV` |
| 2.18 | Turn debounce, minimum speech, max turn length, silence timeout | `core/turn/manager.go` `Config{StartDebounce 80ms, StopDebounce 120ms, MinimumSpeechDuration 150ms, MaxTurnDuration 60s, SilenceTimeout 700ms}` (server uses `DefaultConfig()`; `SilenceTimeout` only matters with no detector, which the server never builds) | none | none | none | VAD analyzer | `LIB` |
| 2.19 | Transcript finalizer (interim stability, grace, finalization timeout) | `core/transcript/finalizer.go` `Config` (`DefaultConfig()` only) | none | none | none | n/a | `LIB` |
| 2.20 | Push-to-talk | `turn.ExternalStart/ExternalStop` | none (YAML `external` rejected) | none: client opt-in (`client-ready push_to_talk`) | `FlowVoiceTest.tsx` "Push to talk" checkbox | external strategies | `OK` (client choice by design) |
| 2.21 | Silence trimming (strip leading/trailing silence from caller or TTS audio) | none | none | none | none | none | `NI` (neither) |
| 2.22 | Empty user turn recovery ("sorry, say again?") | none (turn without transcript dropped) | none | none | none | `turns/empty_user_turn.py` | `NI` |

## 3. Barge-in and interruption

| # | Capability | A. Core knob | B. Server YAML | C. Per-flow | D. UI | E. Pipecat | Gap |
|---|---|---|---|---|---|---|---|
| 3.1 | Barge-in itself (cancel reply, drop late frames, `bot-interrupted`) | generation fence (`core/session.go`), `AssistantContextConfig.BargeInOnTurnEvent`, `core/heard_text.go` | always on (moves to turn events with `turn.manager`) | none | none (test panel counts barge-ins) | always on | `OK` (no off switch; see 3.4) |
| 3.2 | Minimum words to interrupt (ignore "uh-huh" backchannels while the bot speaks) | `core/turn/start.go` `MinWordsStart(min, useInterim)` | `turn.min_words` (0 = off) | none | none | `min_words_user_turn_start_strategy.py` `min_words`, `use_interim` | `MPF` (high value) |
| 3.3 | `use_interim` for min words | `MinWordsStart` 2nd arg, hardcoded `true` in `server/registry.go` | none | none | none | `use_interim` | `LIB` |
| 3.4 | Interruptions off (bot cannot be interrupted) | `turn.mute` strategy `always` (`core/turn/mute.go` `MuteAlways`), capped by `MaxMute` | `turn.mute: [always]`, `turn.max_mute` (30 s) | none | none | `AlwaysUserMuteStrategy` (the pinned Pipecat has no `allow_interruptions`) | `MPF` |
| 3.5 | Protect the greeting / opening disclaimer from interruption | `turn.mute` `first_speech` (mute during the bot's first turn) or `until_first_bot_complete` | `turn.mute: [first_speech]` | none; the step `greeting` is ordinary interruptible output (`AssistantContextProcessor.Say` emits normal frames) | none | `FirstSpeechUserMuteStrategy`, `MuteUntilFirstBotCompleteUserMuteStrategy`; frame-level `Frame.interruptible=False` (`frames/frames.py`) | `MPF` |
| 3.6 | Mute the caller while a tool runs | `turn.mute` `function_call` + `turn.ToolActivity` | `turn.mute: [function_call]` | none | none | `FunctionCallUserMuteStrategy` | `MPF` |
| 3.7 | `critical_output` mute (mute while uninterruptible output plays) | `turn.MuteCriticalOutput`, keyed on `SpeakingStateFrame.Uninterruptible` | `turn.mute: [critical_output]` | none | none | n/a (Switchboard addition) | `DEAD`: no speech producer sets `core.OutputUninterruptible`; only bed/cue frames do, and those are `NonSpeech` and never change speaking state (`transport/ws/pacing.go` ~l.832), so this strategy never mutes |
| 3.8 | Uninterruptible bot speech (per line) | `core.OutputUninterruptible` disposition is honoured by the session fence and the paced writer | none | none | none | `Frame.interruptible` | `NI` at the producer (plumbing exists, nothing emits it for speech) |
| 3.9 | Volume-based interruption (only loud speech interrupts) | only `vad.ProcessorConfig.EnergyGate` (2.10), unwired | none | none | none | `VADParams.min_volume`; `VolumeInterruptionStrategy` is not in the pinned Pipecat | `LIB`/`NI` |
| 3.10 | Interrupted-reply reconciliation (history keeps what was heard) | `core/heard_text.go` | always | n/a | n/a | aggregator TTSTextFrame words | `OK` |
| 3.11 | `limits.immediate_cancel` | not a barge-in setting: `server/server.go` shutdown skips the drain | `limits.immediate_cancel` | n/a | n/a | n/a | `SRV` (name is easy to misread) |
| 3.12 | STT mute (stop sending audio to STT while muted) | none: while muted, audio still flows to VAD/STT (billed) and recording | none | none | none | `STTMuteFrame` (`services/stt_service.py`) | `NI` |

## 4. Speech output: text, voice, pacing

| # | Capability | A. Core knob | B. Server YAML | C. Per-flow | D. UI | E. Pipecat | Gap |
|---|---|---|---|---|---|---|---|
| 4.1 | TTS text cleaning (markdown, links, emoji, tags) | `AssistantContextConfig.DisableTextCleaning`, `core/clean_text.go` | none | `tts.clean_text` accepted on every step but **only the start step's is used** (`server/call.go` `callSpec`; `stepSpec` ignores it; not in `validateCallLevel`) | `StepAdvanced.tsx` checkbox on every step | `markdown_text_filter.py`, `skip_tags_aggregator.py`, `pattern_pair_aggregator.py` | `DEAD` on non-start steps |
| 4.2 | First-clause early cut | `AssistantContextConfig.FirstClauseMinLength` | `first_clause_chars` (0) | none | none | `simple_text_aggregator.py` | `SRV` (latency/prosody trade; candidate `MPF` low) |
| 4.3 | Slack-aware unit merging (unary TTS) | `core/tts_slack.go` `SlackMergeConfig` | `tts_merge.{enabled,margin,cap}` | none | none | n/a | `SRV` |
| 4.4 | Unit length, flush timeout | `AssistantContextConfig.MinUnitLength 10/MaxUnitLength 150/FlushTimeout 200ms` | none | none | none | aggregator | `LIB` |
| 4.5 | TTS look-ahead (unary providers) | `core/ttsunit` `Config.LookAhead` | none (provider-set) | none | none | n/a | `LIB` |
| 4.6 | Output pacing | `transport/ws/pacing.go` `PacerConfig` (`ws.DefaultPacerConfig()` in `server/server.go`) | none | none | none | transport | `LIB` (fine) |
| 4.7 | Voice per step | `core.StageSwap` (TTS) | none | `tts.provider/voice` per step | StepPanel voice picker | Pipecat Flows / `TTSUpdateSettingsFrame` | `OK` |
| 4.8 | ElevenLabs stability/similarity/style/speed | `providers/elevenlabs/tts.go` `TTSConfig` | none | `tts.voice_settings` per step (elevenlabs only; fallbacks use defaults) | StepAdvanced "Voice settings" | ElevenLabs `InputParams` | `OK` |
| 4.9 | Speed / pitch / style / emotion for Google, Cartesia, xAI, Nvidia | none: their `TTSConfig`s have no such fields | none | none | none | Google `speaking_rate`, `pitch`, `volume`, `google_style`; Cartesia `speed`, `emotion` (`GenerationConfig`) | `NI` |
| 4.10 | TTS language | `cartesia/xai/nvidia TTSConfig.Language`, Google `LanguageCode` (from voice prefix) | none | none: flow `TTSConfig` has no `language`; the TTS `StageSpec.Language` is never filled from the flow, so these use the adapter default (`en` / `en-US`) | none | `language` in TTS settings | `MPF` (for non-English flows on Cartesia/xAI/Nvidia) |
| 4.11 | Language switching mid-call | none (STT language is call-level; only the TTS voice changes per step) | none | per-step voice only | none | `STTUpdateSettingsFrame`/`TTSUpdateSettingsFrame` | `NI` |
| 4.12 | Pronunciation dictionary | none | none | none | none | Cartesia `pronunciation_dict_id` | `NI` (low) |
| 4.13 | Greeting (verbatim opening line) | `AssistantContextProcessor.Say` | none | `greeting` per step (<= 500 B) | StepPanel textarea | `TTSSpeakFrame` | `OK` (interruptible, see 3.5) |
| 4.14 | Response pacing / speaking-rate-aware delays | none beyond media-clock pacing | none | none | none | none | `NI` (neither) |

## 5. LLM, tools, context, routing

| # | Capability | A. Core knob | B. Server YAML | C. Per-flow | D. UI | E. Pipecat | Gap |
|---|---|---|---|---|---|---|---|
| 5.1 | Temperature, max output tokens, top_p, seed | none: `providers/google/llm.go` `LLMConfig`, `providers/openai/llm.go` `Config`, `providers/xai/llm.go` `Config` have no sampling fields | none | none | none | `services/openai/base_llm.py` `InputParams` (temperature, top_p, max_completion_tokens, seed, ...); Google settings | `NI` (high user value) |
| 5.2 | Reasoning effort / chat template kwargs | none | none | none | none | `extra` | `NI` (known gap for NIM reasoning models) |
| 5.3 | Tool choice (auto / none / required) | none | none | none | none | `tool_choice` on the context | `NI` |
| 5.4 | Parallel tool calls | `tools.ToolLoopConfig.MaxParallel` | `tools.max_parallel` | `llm.max_parallel` per step (1..16) | StepAdvanced number | `run_in_parallel` | `OK` |
| 5.5 | Tool loop bounds (rounds 4, calls 16, arg 32 KiB, result 64 KiB, depth 16, call 30 s, round 2 min) | `tools.DefaultToolLoopConfig()` | `mcp.timeout` (15 s) bounds MCP calls server-wide | none | none | per-function `timeout_secs` | `LIB`; per-tool timeout `MPF` (low) |
| 5.6 | Async tools (ack now, result later) | `core.ToolPolicy.Async`, `core/async_trigger.go` | none | start step `async_tools` | ToolPicker "Background" checkbox | `async_tool_messages.py` | `OK` |
| 5.7 | Async limits (in-flight 4, timeout 5 min, settle 300 ms, queue 8, max wait 60 s) | `ToolLoopConfig.MaxAsyncInFlight/AsyncTimeout`, `AsyncTriggerConfig` | none | none | none | n/a | `LIB` |
| 5.8 | Cancel sync tool on interruption | generation fence, always | none | none | none | `cancel_on_interruption` | `LIB` (fixed true) |
| 5.9 | Function-call event detail to the client | fixed report level `none` | none | none | none | `RTVIObserverParams.function_call_report_level` | `SRV` (privacy) |
| 5.10 | Hang-up tool (`end_call`) | `server/call.go` `endCallDefinition` -> idle `RequestEnd` | none | `end_call` per step, **only in flows with >= 2 steps** (`definition.go`: "end_call needs a flow with at least two steps") | StepPanel checkbox (shown checked only with >1 step) | `EndTaskFrame`; pipecat-flows end node | `MPF` (one-step agents cannot hang up) |
| 5.11 | Transfer between steps | `core/router.go`, transfer tools | none | routes | Canvas | pipecat-flows transitions | `OK` |
| 5.12 | Transfers per call (10), step build/retire timeouts | `RouterConfig.Max`, `BuildTimeout`, `RetireTimeout` | none | none | none | n/a | `LIB` |
| 5.13 | Call transfer to a human / phone number | none (WebSocket transport only) | none | none | none | telephony transports | `NI` (out of scope) |
| 5.14 | Context size | `core.ContextConfig` | none | start step `context.max_messages/max_tokens` | number inputs | aggregator | `OK` |
| 5.15 | Context summarization | `core/context_summary.go` `SummaryConfig` (`Timeout` 10 s, `RetryAfter` 30 s fixed) | none | start step `context.summarization.{enabled,trigger_tokens,keep_turns,prompt}` | checkbox + fields | `llm_context_summarizer.py` | `OK` |
| 5.16 | LLM failover chain | `core/llm_failover.go` | none | `llm.fallbacks` per step | StepAdvanced lists | `ServiceSwitcherStrategyFailover` | `OK` |
| 5.17 | Failover error classes | `LLM/TTS/STTFailoverConfig.Allow` | none | `failover.on` accepted on every step but **only the start step's is used** (`stepSpec` keeps `call.Failover`) | StepAdvanced checkboxes on every step | n/a | `DEAD` on non-start steps |
| 5.18 | LLM first-token watchdog / budget | `LLMFailoverConfig.FirstTokenTimeout`, `Budget` | `failover.llm_first_token` (8 s), `failover.llm_budget` (60 s) | none | none | n/a | `MPF` (low: a reasoning-model flow needs a higher first-token wait) |
| 5.19 | TTS unit stall watchdog | `TTSFailoverConfig.UnitTimeout` (30 s) | none | none | none | n/a | `LIB` |
| 5.20 | STT replay buffer | `STTFailoverConfig.BufferBytes` | `stt.replay_buffer` (10 s) | none | none | n/a | `SRV` |
| 5.21 | Operator notes / context injection mid-call | `core/async_trigger.go` operator items | none | n/a | none yet (remediation plan Task 7.1) | context frames | `MUI` (already planned) |

## 6. STT

| # | Capability | A. Core knob | B. Server YAML | C. Per-flow | D. UI | E. Pipecat | Gap |
|---|---|---|---|---|---|---|---|
| 6.1 | STT provider, model, language | provider `STTConfig.Model/Language` | none | start step `stt.provider/model/language` | "Listening" StageFields | service settings | `OK` |
| 6.2 | Keywords / key terms / phrase hints | none in any adapter | none | none | none | Deepgram `keyterm`, `keywords`; Google adaptation | `NI` |
| 6.3 | Provider endpointing, utterance end, smart format, interim on/off | none (adapters fix their own) | none | none | none | Deepgram `endpointing`, `utterance_end_ms`, `smart_format` | `NI` |
| 6.4 | Google STT region, stream rotation | `providers/google/stt.go` | `providers.google_stt.{project_id,location,rotation_threshold}` | none | none | n/a | `SRV` |

## 7. DTMF, telephony behaviours, call limits

| # | Capability | A. Core knob | B. Server YAML | C. Per-flow | D. UI | E. Pipecat | Gap |
|---|---|---|---|---|---|---|---|
| 7.1 | In-audio DTMF detection | `core/audio/dtmf` `DetectorConfig` (defaults only) | `dtmf.detect` | none | none | transport DTMF events | `SRV` (cost) / `LIB` thresholds |
| 7.2 | Key-press aggregation into user messages | `dtmf.AggregatorConfig{Timeout, Terminator '#', Prefix "DTMF: "}` | `dtmf.aggregate`, `dtmf.aggregate_timeout` (2 s); terminator/prefix not configurable | none | none | `dtmf_aggregator.py` (`timeout`, `termination_digit`, `prefix`) | `MPF`; terminator/prefix `LIB` |
| 7.3 | `send_dtmf` tool (model presses keys) | `dtmf.ToolDefinition`, `OutputConfig` | `dtmf.send_tool` | none | none | `extensions/ivr/ivr_navigator.py` | `MPF` + `DEAD`-ish: `server/dtmf.go` `buildDTMF` adds the tool to the start step's LLM and its fallbacks only; steps built later (`buildStep` -> `stepSpec`) do not get it, so it disappears after the first route |
| 7.4 | Voicemail / answering-machine detection | none | none | none | none | `extensions/voicemail/voicemail_detector.py` | `NI` (out of scope: no telephony transport) |
| 7.5 | Max call duration | `server/server.go` timer -> `conn.Hangup("max_call_duration")` | `limits.max_call_duration` (30 min) | none | read-only in `GET /server-config` | n/a | `MPF` (a flow could tighten it) |
| 7.6 | Concurrent calls per flow | admission | `limits.max_sessions_per_flow` (one number for every flow) | none | none | n/a | `SRV` (candidate per-flow ceiling, low) |

## 8. Recording, privacy, transport, observability

| # | Capability | A. Core knob | B. Server YAML | C. Per-flow | D. UI | E. Pipecat | Gap |
|---|---|---|---|---|---|---|---|
| 8.1 | Call recording | `core/record` `Config{MaxDuration, MaxBytes, QueueLen, FlushEvery}` | `recording.{enabled,dir,retention_days,max_duration,max_bytes}` | start step `recording.{enabled,disclosure_confirmed}` | two checkboxes | `audio_buffer_processor.py` | `OK` (caps `SRV`) |
| 8.2 | Played recording disclosure | none (operator can write it into `greeting`) | none | via `greeting` | via greeting textarea | none | `NI` (low) |
| 8.3 | Sink redaction (card, SSN, IBAN, PIN, digits, email, phone; DTMF masking) | `core/redact` `Config{Enabled, Rules, DTMF}` | `privacy.redaction.{enabled,rules,dtmf}` | none | none | none | `SRV` (candidate per-flow opt-in for payment flows, low) |
| 8.4 | RTVI message toggles (bot TTS text, LLM text, audio levels, metrics) | none | none | none | none | `RTVIObserverParams` (`bot_tts_enabled`, `bot_llm_enabled`, `metrics_enabled`, ...) | `NI` |
| 8.5 | Ready timeout, pings, handshake limits | `ws.ServerConfig` (`PingMaxFailures` fixed 2, `FixedPlayerRate` 24000) | `limits.ready_timeout`, `ping_interval`, `ping_timeout`, ... | none | none | n/a | `SRV` |
| 8.6 | Pipeline audio rates | in 16 kHz fixed; out from `providers.google_tts.sample_rate` | that key | none | none | `PipelineParams.audio_in/out_sample_rate` | `SRV` |
| 8.7 | Retention, OTel, warm-up, VAD workers, provider endpoints | various | `retention.*`, `otel.*`, `warmup.*`, `vad.{mode,workers,batch,...}`, `providers.*` | none | none | n/a | `SRV` |

---

## Detailed answers to the user's three examples

### Bed / background music

What exists:

- **Catalogue on the server.** The operator lists up to 8 named sounds under `audio.backgrounds` (`file` = a PCM16 mono WAV of 8..192 kHz, at most 10 MB; `gain_db` -60..0, default -30; `loop`, default true). `audio.background_default` names the sound every call gets; `audio.allow_flow_background` (false in production, true in development) lets flows choose. Files are read once at start-up (`server/audiofilter.go` `loadCues`), resampled to the fixed 24 kHz player rate, crossfaded at the loop point, and shared read-only by every call. There is no reload: a new file means editing YAML and restarting.
- **How a user adds their own file:** only the operator can, by copying the WAV onto the server's disk and adding a YAML entry (`docs/audio-filters.md`). The flow can only name a catalogue key (`server/background.go`): never a path, which is a deliberate security rule (Ruling 2 of `2026-10-15-per-flow-call-behavior-design.md`). The repo ships no sound files (`assets/` holds speech test clips only).
- **Per flow (start step):** `audio.background{name, gain_db, loop}`; `{"name":""}` means none. Publish refuses an unknown name or a flow background when not allowed (`flow/policy.go`); if the catalogue changes after publish, the call falls back to the server default with a log warning.
- **UI:** a select of the server's sound names (from `GET /server-config`, names only), a volume input and a Repeat checkbox, under Flow settings > Advanced > Audio. **No preview/listen, no upload.**
- **Mixing:** in the paced writer, at a constant level under speech and alone in silences (about 48 kB/s of WebSocket traffic per call). Bed frames are non-speech and uninterruptible, never recorded, never affect idle or speaking state.

What does not exist:

- **No ducking** under speech (the bed is added at a constant level and hard-clipped); Pipecat's `SoundfileMixer` has none either.
- **No fade in/out** of the bed (only the 5 ms loop crossfade); route cues fade out over 30 ms.
- **No start/stop or change mid-call**, and **no per-step bed**: `audio` is call-level. The library can do it (`CueUnderlay.SetBackground` is atomic and safe while mixing; Pipecat does it with `MixerUpdateSettingsFrame`/`MixerEnableFrame`), the server just calls it once when the flow resolves.
- **No pause during tool calls** and **no hold music while a slow synchronous tool runs.** The only gap-filling sound is the **route cue** (`audio.cues` + `route_cue{name, after_ms}`), which plays only while the call moves between steps (starts after `after_ms` of quiet, stops on the new step's audio, caller speech, failure, or 10 s). The only lever for slow tools is `async_tools` (spoken acknowledgement, result later).
- **No thinking/filler cues** of any kind.
- Route cue UI is a **free-text name**: cue names are not in `GET /server-config`, and publish does not check the name, so a typo silently plays nothing (one log warning per call).

### Silence handling

- **Quiet caller (idle prompts): fully per flow with UI.** `idle.user_timeout_ms` (0 = off, 1..300 s), `max_prompts` 1..10, `prompt` (<= 200 B), `prompt_mode` llm/say, `goodbye` (<= 200 B), on the start step; the editor shows the server default as placeholders. Built-in default is on (15 s, 2 prompts, then a spoken goodbye and hang-up). Prompts pause while a tool runs, the caller is muted, or a reply is active. The stuck-bot watchdog (`idle.bot_*`) is server-only by design.
- **Silence trimming:** does not exist (neither in Pipecat).
- **VAD:** only the stop hangover is exposed (server `vad.stop_ms`, per flow `vad_stop_ms` 200..2000 with UI). Start confidence (0.5), stop threshold (0.35), start duration (64 ms) and the energy gate (min volume, off) are library-only (`vad/processor.go` `DefaultProcessorConfig`), unlike Pipecat's `VADParams{confidence, start_secs, stop_secs, min_volume}`.
- **End of turn:** the late-transcript wait is per flow (`stt_p99_ms` with UI). Everything else is server YAML only: `turn.speech_timeout` (resume window, Pipecat `user_speech_timeout`), `turn.no_transcript_timeout`, `turn.eager_grace`, and the strategy list `turn.stop` (speech-timeout / eager / turn-analyzer (Smart Turn) / llm-completion with `turn.completion.*` and `turn.analyzer.*`). A flow cannot choose smart-turn or eager, nor its hold (`stop_secs`).

### Barge-in / interruption

- **Barge-in is always on**; there is no per-flow or per-step "allow interruptions" switch. (The pinned Pipecat no longer has `allow_interruptions` or the interruption-strategy classes either; it uses start and mute strategies, which Switchboard mirrors.)
- **Every interruption control is server-wide YAML:** `turn.min_words` (words needed to interrupt while the bot speaks: the backchannel filter, Pipecat `MinWordsUserTurnStartStrategy`), `turn.mute` (`always` = effectively no interruptions; `first_speech` / `until_first_bot_complete` = protect the greeting or disclaimer; `function_call` = no interruptions during tools) and `turn.max_mute` (cap, default 30 s). None is in the flow config or UI.
- **Protecting a greeting/disclaimer:** today only server-wide `turn.mute: [first_speech]`. The step `greeting` is ordinary interruptible output. Core has an `OutputUninterruptible` disposition honoured by the fence and the paced writer, but nothing produces it for speech.
- **`critical_output` mute is dead:** it waits for uninterruptible speech that nothing produces.
- **Volume-based interruption:** not available (the VAD `EnergyGate` exists but nothing sets it).
- **`limits.immediate_cancel` is not a barge-in setting:** it skips the shutdown drain.
- **Backchannels:** `min_words` is the only handling, and it needs an STT with interim results (with `chirp_2` barge-in waits for the final; a WARN and a metric flag it).

---

## Dead or misleading surfaces found (fix regardless of Phase 7 scope)

1. `tts.clean_text` on a non-start step: accepted, shown in StepAdvanced on every step, ignored (`server/call.go` `stepSpec`). Either honour it per step (it is a per-assistant-context setting, so it would need a per-step toggle on the assistant stage) or reject it in `validateCallLevel` and show it only on the start step. The second is the S-sized fix.
2. `failover.on` on a non-start step: same pattern (`stepSpec` keeps `call.Failover`). Same fix.
3. `send_dtmf` vanishes after the first route (`server/dtmf.go` `buildDTMF` only touches `call.LLM` and its fallbacks). Fix by applying it in `prepLLM` so every step gets it.
4. `turn.mute: critical_output` never mutes (no producer of uninterruptible speech). Delete it (YAGNI) or give it a producer (recommendation R4).
5. `route_cue.name` is free text with no publish check and no list in `GET /server-config`.
6. VAD `EnergyGate` is a zero-value-only knob (nothing sets it): expose it (R9) or delete it.

## Gap counts (rows in the matrix above; a row with two codes counts under both)

| Gap type | Count | Rows |
|---|---|---|
| `OK` | 27 | 1.1-1.3, 1.13-1.15, 2.1-2.5, 2.7, 2.12, 2.20, 3.1, 3.10, 4.7, 4.8, 4.13, 5.4, 5.6, 5.11, 5.14-5.16, 6.1, 8.1 |
| `SRV` (server-only by design) | 17 | 1.4, 2.6, 2.13, 2.15 (workers), 2.17, 3.11, 4.2, 4.3, 5.9, 5.20, 6.4, 7.1, 7.6, 8.3, 8.5, 8.6, 8.7 |
| `MPF` (missing per-flow) | 18 | 1.5, 2.11, 2.14, 2.15, 2.16, 3.2, 3.4, 3.5, 3.6, 4.10, 5.5 (per-tool timeout), 5.10, 5.18, 7.2, 7.3, 7.5, 2.6 (bot_fallback, low), 4.2 (low) |
| `MUI` (missing UI) | 3 | 1.4 (preview/upload), 1.10 (route cue select), 5.21 |
| `LIB` (library-only) | 19 | 1.11, 2.4 (GoodbyeWait), 2.8, 2.9, 2.10, 2.15 (PreSpeech), 2.18, 2.19, 3.3, 3.9, 4.4, 4.5, 4.6, 5.5, 5.7, 5.8, 5.12, 5.19, 7.2 (terminator/prefix) |
| `NI` (not implemented in core) | 23 | 1.6, 1.7, 1.8, 1.9, 1.12, 2.21, 2.22, 3.8, 3.9, 3.12, 4.9, 4.11, 4.12, 4.14, 5.1, 5.2, 5.3, 5.13, 6.2, 6.3, 7.4, 8.2, 8.4 |
| `DEAD` | 5 | 1.10 (unchecked cue name), 3.7, 4.1, 5.17, 7.3 |

101 rows; a row with two codes (for example 1.4, 2.15, 5.5, 7.2) is counted under each. Of the `NI` rows, Pipecat parity gaps are 1.6 (runtime mixer enable), 2.22, 3.8 (frame `interruptible`), 3.12, 4.9, 4.11, 4.12, 5.1, 5.2, 5.3, 6.2, 6.3, 7.4, 8.4; the rest (1.7, 1.8, 1.9, 1.12, 2.21, 4.14) are missing in Pipecat too.

---

## Recommendations for Phase 7 (or a "Phase 7b: per-flow conversation behaviour")

Principle used: expose per flow only what is a **conversation-design choice** that legitimately differs between agents (a receptionist vs an IVR bot vs a disclaimer-reading agent); keep **cost, capacity and safety ceilings** server-side, with the flow allowed to tighten but not loosen them. Everything below reuses existing core code unless marked otherwise. All go on the **start step** (call-level) unless stated, under the existing Flow settings > Advanced > "Turn-taking" fieldset, with `settingCopy.ts` entries (the existing test enforces copy for every control).

### R1. Fix the dead/misleading surfaces (bugs, do first)

- What: reject `tts.clean_text` and `failover.on` on non-start steps in `validateCallLevel` and move both controls from StepAdvanced to Flow settings (or honour them per step; rejecting is smaller); apply `send_dtmf` in `prepLLM` so every step keeps it; publish-check `route_cue.name` against `audio.cues` and add cue names to `GET /server-config` so the UI becomes a select; delete `critical_output` unless R4 lands.
- Why per-flow: n/a (correctness).
- Validation: existing.
- UI: route cue becomes a `<select>` like the background one; the two moved controls appear once.
- Privacy/security: cue names only, never paths (same as backgrounds).
- Size: S. Risk: a stored draft with `clean_text`/`failover` on a non-start step now fails validation; migration note (no deployed data per the earlier rulings).

### R2. Per-flow interruption control (`turn` object): min words + mute strategies

```json
"turn": { "min_words": 3, "mute": ["first_speech", "function_call"], "max_mute_ms": 15000 }
```

- Why per-flow: whether callers may interrupt, and whether the opening disclaimer is protected, is the single most agent-specific behaviour (a legal disclaimer agent vs a chatty receptionist). Today one YAML line changes every agent on the server.
- Validation: `min_words` 0 (server) or 1..10; `mute` unique subset of `always`, `first_speech`, `until_first_bot_complete`, `function_call` (not `critical_output`); `until_first_bot_complete` only when the start step has a `greeting` (mirrors the YAML comment); `max_mute_ms` 1000..60000. Present `turn.mute` replaces the server list (one unit, like `idle`).
- Requires `turn.manager` (default on): readiness reason `turn_manager_off` when the server has it off.
- UI sketch: "Interruptions" group: radio *Caller can interrupt any time* / *Only after N words* (number, hint: needs an STT with interim results, warn when the chosen STT model is known to send none) / *Never while the agent speaks* (= `always`); checkboxes *Protect the greeting* (= `first_speech`), *Don't interrupt while a tool runs* (= `function_call`); "Longest mute" seconds.
- Privacy/security: none (audio still reaches STT and recording while muted; say so in the copy: muted speech is still billed).
- Size: M (flow config + validation + `CallSpec` plumbing into the existing `coreturn.ParseMuteStrategies`/`MinWordsStart` call in `server/registry.go`, UI, e2e on echo). Risk: no change unless set; a flow with `always` can trap a caller until `max_mute`.

### R3. Per-flow end-of-turn timing and strategy

```json
"turn": { "speech_timeout_ms": 800, "end_of_turn": "smart", "smart_hold_ms": 2000 }
```

- Why per-flow: the resume window and smart-turn choice depend on the audience (slow or elderly speakers vs fast transactional calls) and on the flow's STT (eager needs punctuation), which is already per flow.
- Validation: `speech_timeout_ms` 0 or 200..3000; `end_of_turn` `default` | `smart` | `eager` (`eager` only with an STT the catalogue marks punctuating; `smart` only when the server loaded the Smart Turn model: readiness reason `smart_turn_unavailable`); `smart_hold_ms` 500..5000 (Pipecat `stop_secs`). LLM completion stays server-only (prompt-protocol cost and model-dependence).
- UI: under the existing "Turn-taking" fieldset next to `vad_stop_ms` and `stt_p99_ms`: "Wait for the caller to resume (ms)", a select "End-of-turn detection: Standard / Smart (model) / Fast (punctuation)".
- Privacy/security: none.
- Size: M. Risk: none unless set; `eager` cuts multi-sentence turns (copy must say so).

### R4. Uninterruptible greeting / disclaimer (only if R2's `first_speech` is not enough)

- What: `greeting_interruptible: false` per step; `Say` emits `OutputUninterruptible` for that line (core plumbing exists in the session fence and paced writer), which also gives `critical_output` a producer.
- Why per-flow: a compliance line must be heard in full even if the caller talks over it.
- Validation: boolean; only with a non-empty `greeting`.
- UI: checkbox under the greeting: "Caller cannot interrupt this line".
- Size: M (core change to `AssistantContextProcessor.Say` + tests for fence/heard-text). Risk: medium; new core path. Prefer R2 `first_speech` first; build R4 only if users need line-level control on non-start steps.

### R5. Bed music beyond "pick one at start"

- (a) **Background per step** (allow `audio.background` on any step; `Router` route-active calls `CueUnderlay.SetBackground`): S/M, low risk; Pipecat parity (`MixerUpdateSettingsFrame`).
- (b) **Hold sound while a tool runs**: `audio.tool_cue{name, after_ms}` reusing `audio.cues` and the existing `cueController`, armed by the `turn.ToolActivity` the server already creates (idle uses it). Why per-flow: only flows with slow tools want it. Validation as `route_cue`. UI: select from cue names + "start after (ms)". Size: M. Risk: low (non-speech output; stops on first bot audio or caller speech, like the route cue).
- (c) **Preview a server sound** in the editor (`GET /api/v1/sounds/{name}` streaming the decoded WAV, admin listener, management key): S/M. Security: names only, admin-only, no path; size cap already 10 MB.
- Not recommended now: **ducking and fades** (new core DSP on the hot path; Pipecat has neither), **upload** (L: file storage, format validation, multi-instance distribution, storage quota; keeps operator-file model for now).

### R6. One-step hang-up and per-flow call cap

- `end_call` on one-step flows (drop the ">= 2 steps" rule; build the call-flow end hook for single-step calls): S/M. Why: the most common agent is one step and cannot hang up after "goodbye". Risk: low (idle `RequestEnd` path exists).
- `limits.max_call_duration_s` per flow: 60..server cap (can only tighten). UI number in Flow settings. Size: S. Risk: none unless set.

### R7. Per-flow DTMF opt-in (within a server ceiling)

- `dtmf: {aggregate: bool, send_tool: bool}` honoured only when the server enables the matching capability (as backgrounds). Why: IVR-navigating agents need `send_dtmf`; others should not have the tool in their prompt. Size: M. Privacy: digits are masked at sinks by `privacy.redaction.dtmf` (unchanged). Risk: low.

### R8. LLM sampling parameters (needs core/provider work: Phase 8, not 7)

- `llm.temperature` 0..2, `llm.max_output_tokens` 16..8192 per step (both optional; absent = provider default). Implement in `providers/google`, `providers/openai` (covers nvidia/offline), `providers/xai`; reasoning models that reject `temperature` need a catalogue flag. Why per-step: tone and verbosity are persona-level. Size: L (five adapters, catalogue, failover fallbacks inherit the step's values). Risk: provider 400s on unsupported params (classify as client errors; do not fail over).

### R9. Lower priority / later

- VAD sensitivity per flow (`vad_confidence` 0.3..0.9, `vad_min_volume`) for noisy-line agents: S plumbing, but needs measurement first (false barge-in rates); or delete `EnergyGate` per YAGNI.
- `llm_first_token_ms` per flow for reasoning-model flows: S.
- TTS language per flow (non-English Cartesia/xAI/Nvidia voices): S; STT keywords and TTS speed for other providers: `NI`, provider-by-provider (M each).
- Keep server-only: bot watchdog, no-transcript timeout, `tts_merge`, `first_clause_chars`, pacing, VAD workers, redaction (unless a payments flow asks), RTVI toggles.

### Suggested Phase 7 scope addition

R1 (S, bugs) + R2 (M, interruption control) + R3 (M, end-of-turn) + R5a/R5b (M, bed per step and tool hold sound) + R6 (S/M). Together these close every user-named gap that the library already supports, with no new core DSP. R4, R5c, R7 are good follow-ups; R8 needs provider work and belongs in its own phase.
