# Turn Latency (Pipecat Parity) Design

**Date:** 2026-10-11
**Status:** approved in conversation; awaiting written-spec review.

## Intent

Replies should start sooner after the caller stops talking, without cutting callers off mid-sentence. Success:
- voice-to-voice latency on the nightly live S1 test goes down;
- the push-to-talk and mute scenarios (S11, S12) do not regress;
- hesitations shorter than the resume window (S10, 400 ms) stay one turn. A pause longer than about 0.8 s inside one utterance (S4's default 1 s) splits it into two turns, as in Pipecat. Accepted by the user on 2026-10-11 after the final review; the window is tuned from live S4/S10 numbers if needed;
- no turn ends before its transcript (or before a bounded fallback).

The user made three decisions on 2026-10-11:
1. **Turn manager on by default,** Pipecat-style: a transcript-gated speech timeout and a lower VAD stop, both tunable.
2. **STT p99 latency from a built-in per-provider table,** seeded from Pipecat's benchmark and overridable.
3. **Silero: compare first.** Run our model file and Pipecat's offline on the committed clips, and switch only if Pipecat's is at least as good at the new, lower VAD stop.

## Current behaviour (main at e5c2259)

- **VAD stop.** VAD ends a speech segment after `vad.stop_ms` of silence: default 800 ms, range 200 to 2000; the per-agent override is `vad_stop_ms`. Pipecat's `VAD_STOP_SECS` is 0.2 s.
- **Manager off (the default).** `turn.manager` is off. A user turn then ends on the raw VAD stop, so the 800 ms VAD stop is the only guard against mid-sentence cuts.
- **Manager on.** The stop chain must include `speech-timeout`, which is `SilenceDetector` with `SilenceAfter` (700 ms default). It ends the turn on silence alone, with no transcript gate. `core/turn/stop.go` notes that gap with a ponytail comment.
- **Where silence is measured from.** `DetectionInput.SilenceFor` counts from silence onset, the VAD stop backdated by its hangover (`manager.go`: `silenceSince = stopSince`). So the manager's 120 ms `StopDebounce` does not add on top of the timeout. `DetectionInput` already carries `Text` (joined across the turn's segments) and `TextFinal`.
- **No-transcript fallback.** There is none, other than `MaxTurnDuration` (60 s).

## Pipecat's reference behaviour

`pipecat-impl/src/pipecat/turns/user_stop/speech_timeout_user_turn_stop_strategy.py`, with `VAD_STOP_SECS` = 0.2. After a VAD stop, two timers run, and the turn stops only when the user is not speaking, both timers are done, and at least one transcript has been received (`wait_for_transcript=True`, the default):
- **`user_speech_timeout`** (0.6 s). This is the window in which the user may resume. It always runs to completion.
- **`stt_timeout`.** This is the provider's TTFS p99 minus `stop_secs`. It is cut short when the STT sends a finalized transcript.

The aggregator's `user_turn_stop_timeout` ends a turn that never received a transcript. The p99 table is in `pipecat-impl/src/pipecat/services/stt_latency.py`.

## Design

### 1. The transcript-gated `speech-timeout` strategy (core/turn)

`speech-timeout` becomes a stateless function of `DetectionInput`, configured once per call:

```go
type SpeechTimeoutConfig struct {
    VADStop             time.Duration // the call's VAD stop (the onset-to-VAD-stop offset)
    UserSpeechTimeout   time.Duration // resume window after the VAD stop; default 600 ms
    STTP99              time.Duration // the primary STT's p99, speech end to final transcript
    NoTranscriptTimeout time.Duration // end an untranscribed turn after this much silence; default 5 s
}
func SpeechTimeout(cfg SpeechTimeoutConfig) UserTurnStopStrategy
```

All durations are measured as `SilenceFor`, from silence onset. `Detect` ends the turn (reason `silence`) when either of these holds:
- **The normal path,** when all three are true:
  - **Resume window:** `SilenceFor >= VADStop + UserSpeechTimeout`, Pipecat's timer started at the VAD stop event.
  - **Transcript gate:** `strings.TrimSpace(Text) != ""`.
  - **STT safety net:** `TextFinal || SilenceFor >= STTP99`. Pipecat's `stt_timeout` is p99 minus `stop_secs`, counted from the VAD stop, which is p99 counted from onset.
- **The fallback:** `SilenceFor >= NoTranscriptTimeout`, whether or not a transcript arrived. An untranscribed turn is dropped downstream, as today.

Zero fields take these defaults:
- `UserSpeechTimeout` 600 ms;
- `NoTranscriptTimeout` 5 s;
- `STTP99` 1 s (Pipecat's `DEFAULT_TTFS_P99`);
- `VADStop` the call's VAD stop. The registry always sets it.

`SpeechTimeout` replaces the current `speechTimeout{after}` everywhere. The old `SpeechTimeout(after time.Duration)` constructor and the ponytail comment go away. `SilenceDetector` stays, as the manager's no-detector default.

**Interaction with other chains:**
- **`eager`** and **`llm-completion`** sit in the same any-stop chain and are unchanged.
- **`turn-analyzer`.** The smart-turn strategy's internal fallback, which today uses the speech-timeout base duration, keeps its semantics. Its base becomes `VADStop + UserSpeechTimeout`. The analyzer is not otherwise gated: its model verdict is audio-based by design.
- **Push-to-talk** calls do not use `speech-timeout` (ExternalStop only) and are unaffected.

### 2. STT p99 table (server)

A `sttP99` table in `server/` maps provider id to duration, seeded from Pipecat's `stt_latency.py` for the providers go-voice ships:

| Provider | p99 |
|---|---|
| google | 1.57 s |
| xai | 2.14 s |
| deepgram | 0.35 s |
| cartesia | 0.81 s |
| elevenlabs (Scribe realtime) | 0.41 s |
| nvidia | 1.0 s |

Unknown providers get 1.0 s.

**Overrides,** from highest to lowest:
1. the agent config `stt_p99_ms` (new; 0 means unset; range 100 to 10000);
2. the server `turn.stt_p99` map (provider to duration);
3. the table.

A call with STT fallbacks uses its primary provider's value.

**Reporting.** The nightly live job's S1 result already records `final_tr_ms` (clip end to the final transcription). The job report adds a line comparing the measured median `final_tr_ms` to the table value per profile, so stale defaults are visible. No new metric.

### 3. Defaults (server)

| Setting | Old | New | Pipecat equivalent |
|---|---|---|---|
| `turn.manager` | false | true | — |
| `vad.stop_ms` | 800 | 200 | `VAD_STOP_SECS` 0.2 |
| `turn.speech_timeout` (now the resume window, `UserSpeechTimeout`) | 700 ms | 600 ms | `user_speech_timeout` 0.6 |
| `turn.no_transcript_timeout` (new) | — | 5 s | `user_turn_stop_timeout` |

- **Configurability.** Every value stays configurable, and the per-agent `vad_stop_ms` override is unchanged.
- **Meaning of `turn.speech_timeout`.** The YAML key keeps its name. Its meaning, documented in `server/config.go`, becomes "the resume window after the VAD stop", not "silence from onset".
- **Config validation.** `server/config_validate.go` currently rejects turn keys when the manager is off. That stays, with the new default. A startup **warning** fires when `turn.manager` is false and `vad.stop_ms` is below 500: turns would end on a short raw VAD stop and cut callers off.
- **Shipped configs.** `config/development.yaml` and `config/production.yaml` and their golden effective-config tests (`server/config_effective_test.go`) are updated to the new defaults.

### 4. Silero comparison (tooling, before any model switch)

A free, offline tool, `tools/vadcompare` (a Go `main`), runs the VAD processor's detector over the 18 clips in `assets/speech/`, at `stop_ms` 200 and 800, with each model file:
- ours: `third_party/silero_vad.onnx`, sha256 `1a153a22…`;
- Pipecat's: `pipecat-impl/src/pipecat/audio/vad/data/silero_vad.onnx`, sha256 `597d30b3…`.

It reports, per clip and file:
- speech start and stop times;
- the number of segments, where more than 1 on a single utterance means a mid-utterance split;
- the agreement between the two files.

A Python script run with `pipecat-impl/.venv` runs Pipecat's own `SileroVADAnalyzer` (stop 0.2) on the same clips. That checks that our Go inference with Pipecat's file reproduces Pipecat's decisions within one 32 ms chunk.

The results go to `docs/benchmarks/silero-compare.md`.

**The decision rule.** Switch `third_party/silero_vad.onnx` to Pipecat's file if, at 200 ms, it causes no more mid-utterance splits than ours and its stop times are no later on average. Otherwise keep ours and record why. The matrix VAD row is updated either way.

### 5. Evidence and testing

- **Unit (core/turn).** A table test of `SpeechTimeout.Detect`:
  - the resume window not reached;
  - no text yet;
  - text but not final, before p99;
  - text not final, after p99;
  - final text after the resume window;
  - the no-transcript fallback;
  - zero-value defaults.
- **Processor timing (core/turn, fake clock).**
  - A final transcript that arrives before the resume window ends ends the turn exactly at `VADStop + UserSpeechTimeout`.
  - A turn whose final arrives late, after the window but before p99, ends when it arrives.
  - A turn whose final never arrives but has interim text ends at p99.
  - A turn with no transcript at all ends at `NoTranscriptTimeout`.
  - Speech resuming inside the window keeps one turn.
  - Each runs through the existing harness, plus the real transcript finalizer where commits matter (`core/turn/commit_test.go`'s harness).
- **Server.**
  - Effective-config golden tests for the new defaults.
  - The p99 resolution order (agent, then server map, then table, then default).
  - The startup warning.
  - The existing turn-stop tests updated to the new constructor.
- **Live.**
  - The nightly job's S1 (`v2v_ms`, `vad_stop_ms`, `final_tr_ms`) gives after-numbers. The before-numbers are the existing `docs/benchmarks/agent-matrix.md` S1 rows for google and xai.
  - One optional paid before/after run (`make live-critical PROFILES=google SCENARIOS=S1,S4,S11 MAX_CALLS=20`), only with explicit user approval.
- **Docs.**
  - Matrix rows: VAD, Turn manager in live calls, Stop: speech timeout (currently "partial", with the transcript-gate difference). The speech-timeout row should then reach parity, apart from the `finalized` versus final distinction below.
  - Remaining-work item 2 marked done or updated.
  - `docs/quickstart.md` turn section.

## Out of scope

- Streaming TTS (priority 3).
- Lowering the manager's start debounce or minimum speech duration.
- A runtime-measured p99.
- Changes to the smart-turn model or eager stop.
- Pipecat's distinction between "finalized" (STT says nothing more is coming) and "final" (a final result for the current utterance). go-voice treats `TextFinal` as finalized: our providers send a final per utterance, and the turn joins utterances. Recorded as a difference in the matrix.

## Risks

- **More mid-sentence cuts from a 200 ms VAD stop.** The transcript gate and the 600 ms resume window are the mitigation, as in Pipecat. The S4 (mid-utterance pause) and S10 (hesitation) live scenarios measure it.
- **A wrong p99 for a provider.** Too low and turns end before slow finals: the finalizer's 2 s timeout still catches a late final, but the reply may start on a partial transcript. Too high and replies wait. The nightly report line makes drift visible.
- **Default-on manager for existing deployments.** Deployments that relied on manager-off behaviour change on upgrade. The changelog and quickstart call this out. `turn.manager: false` restores it, but needs `vad.stop_ms: 800` too, and the startup warning says so.
