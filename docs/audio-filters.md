# Inbound audio filters

Package `core/audio/filter`. Off by default: with `audio.input_filters` empty no processor is inserted and the pipeline is identical to before.

```yaml
audio:
  input_filters: [denoise]   # known: denoise
  denoise_strength: 0.5      # 0..1, 0 = default (0.5)
```

A flow can turn denoise on for its own calls (`audio.denoise` on the start step, optional `strength` 0..1, default 0.5); the server's `input_filters` still apply to every call. See "Per-flow audio" below.

## Pipeline order

`transport -> DTMF detector -> filter chain -> VAD -> smart-turn tap -> STT -> ...`

Everything after the chain (VAD, the smart-turn model, STT) hears filtered audio, as in Pipecat, where the filter runs in the transport input before all of them.

DTMF sits before the chain because suppression attenuates keypad tones (tested with a destructive filter). **Recording** taps inbound audio at the transport, so it stores the raw audio, not the filtered audio. Recording filtered audio would need a tap after the chain; not built.

## Contract

`Filter.Process(pcm []int16, rate) ([]int16, error)` returns a new slice of the same length and never mutates its input. `Reset()` drops adaptive state; `Close()` releases resources. A filter instance is per session (stateful, not concurrency safe); the server builds a fresh chain per call. `filter.Processor` is copy-on-write: it pushes a new frame with a new buffer, so other holders of the original frame are unaffected. It filters only PCM16 mono at 16 kHz; other rates, stereo, odd byte counts and empty frames pass through untouched, and a filter error fails open (original frame forwarded, error counted).

Filters are frame-size agnostic: the denoiser buffers internally, and any chunking of a stream yields bit-identical output (tested).

## Denoiser

Pure Go, no cgo, no network. STFT with 256-sample sqrt-Hann windows and a 64-sample hop, minimum-statistics noise tracking (1.5 s memory, 5 sub-windows), decision-directed Wiener gain with over-subtraction, a spectral floor, and time/frequency gain smoothing. `strength` maps to over-subtraction 1..3 and a gain floor of -4..-24 dB.

**Algorithmic latency: 255 samples = 15.9 ms**, constant (measured by cross-correlation in tests). This is below the 20 ms budget. A 512-sample window (the usual choice) would cost 32 ms, so the window is 256 instead.

Cost: about 21 us per 20 ms frame on an M4 Pro (`go test -bench Denoise20ms ./audio/filter`), roughly 0.1% of one core per call, one 640 B allocation per frame.

Measured on synthetic speech (harmonics, formants, syllabic modulation) at default strength 0.5, after a 2 s convergence period:

| noise (input SNR 5 dB) | SNR gain |
|---|---|
| white | +7.1 dB |
| 50 Hz hum + hiss | +7.6 dB |
| 60 Hz hum + hiss | +6.6 dB |
| pink | +5.0 dB (low-frequency noise overlaps the strongest speech harmonics) |

At 15 dB input SNR the output SDR against clean speech is 20.1 dB (white) and 18.5 dB (pink); on already-clean speech it stays above 20 dB. Babble (five synthetic talkers, 10 dB) is left alone (no gain, no loss): non-stationary noise is out of scope for this algorithm.

Silero VAD (`vad/denoise_eval_test.go`, skipped when onnxruntime is missing): a 90 Hz amplitude-modulated buzz that Silero fires on in 311 of 312 chunks fires on 0 after denoising; real speech in 10 dB hiss keeps its speech chunks (92 -> 92). Limits found: the same buzz under heavy hiss is not suppressed (299 -> 299), and Silero does not false-trigger on plain white noise at all, so no gain shows there. Quiet speech (all 18 clips at -35 dBFS, fresh denoiser per clip so speech arrives before convergence): speech chunks 1642 -> 1639 in -70 dBFS hiss and 1651 -> 1633 at 20 dB SNR; first detection later than raw + one chunk on 1 of 18 clips (two chunks, 64 ms). Treat this as a demonstration, not a corpus result; a real noisy corpus is still needed for the "false barge-in rate drops" acceptance.

Known limits: needs a pause per frequency bin within about 1.5 s to find the noise floor (long continuous speech makes it suppress more, not less); first ~1 s after call start converges (speech in the first ~200 ms of a call, before any pause, comes out about 3 dB quieter); no help for speech-over-speech.

## Pluggable replacements

RNNoise, Krisp and AIC are not implemented. Each would be another `Filter` registered in `knownInputFilters` (`server/audiofilter.go`). Native ones should run in a supervised worker process like the VAD worker, with the `Filter` talking to it over a pipe and failing open on worker loss. Chain them with `denoise` or instead of it. Any algorithmic delay must be constant and fixed.

## Observability

`voice_audio_filter_frames_total{filter}` and `voice_audio_filter_latency_seconds{filter}` (histogram). Labels come from a fixed set (`denoise`, `other`). `make bench-latency` does not send audio and is unaffected.

# Output gain and background ambience

Package `core/audio/mixer`; mixing hook in `transport/ws/pacing.go`. Both are off by default (no stage, no extra traffic).

```yaml
audio:
  output_gain_db: -3        # -30..+6, 0 = off (no processor inserted); a flow can override it
  backgrounds:              # named sounds (at most 8); a call plays one, never a path a flow gives
    office:
      file: /etc/govoice/office.wav   # PCM16 mono WAV, 8..192 kHz, <= 10 MB
      gain_db: -30          # -60..0; unset = -30, 0 = full level
      loop: true            # default true
  background_default: office   # every call plays this unless its flow picks another; empty (default) = none
  allow_agent_background: false # a flow may choose a sound from `backgrounds` (default false; true in the development profile)
```

Validated at config load; every file is read when the server starts (a bad file stops startup). The single `audio.background` key of earlier builds is gone: an old file that still has it fails to load (unknown field).

## Gain

`mixer.Gain` is the last pipeline stage before the transport, so it scales speech and DTMF tones. Q12 fixed-point int32 math with rounding and saturation (+6 dB on a loud sample clips, never wraps). Copy-on-write frames (a struct copy: every field, e.g. `NonSpeech`, `WordTimings`, is kept). Gain runs before the paced writer, so pacing durations are unchanged. Recording taps the paced writer's frames after this stage, so a recording carries the gain.

## Background ambience

The WAV is parsed (PCM16 mono only, including `WAVE_FORMAT_EXTENSIBLE`; 8/24-bit, float and stereo are rejected with a conversion hint; the 10 MB cap applies before parsing), low-passed (63-tap windowed sinc) when the file rate is above 24 kHz, resampled once to the 24 kHz player rate and kept at unity once at startup (each call's cursor applies its own level). The last 5 ms are crossfaded into the first 5 ms so the loop point has no click. The samples are shared read-only; each session has a `Cursor` (just a position).

Mixing happens inside `PacedWriter` (an `Underlay`), on the pacer's clock:

- speech frame released: `speech + ambience` (saturating) is what goes on the wire;
- queue empty: the writer emits 20 ms ambience-only frames at real time, so the ambience never stops during silences;
- ambience frames are `NonSpeech` + `Uninterruptible`: `BeginFlush` clears speech and leaves them alone; they never call `OnWriteComplete` and never change speaking state. So the recording tap, the heard-text tracker (`Media` durations), the idle controller and the "bot first audio" metric see speech only. **Recordings capture the bot's speech without ambience** (the tap gets the unmixed frame);
- queue bound: ambience is never queued. An ambience frame still waiting for its slot when speech arrives gives the slot to speech, so speech waits at most the one 20 ms frame already on the wire; the 2 s queue cap and flush behavior are unchanged (tests use the real `PacedWriter`);
- requires `EnablePacing` (the production default); without pacing the underlay is ignored.

Cost and limits:

- **Bandwidth**: continuous audio on the WebSocket, 48 kB/s per call at 24 kHz PCM16 (about 170 MB/h), plus protobuf framing; calls that were silent now send traffic.
- **CPU**: one timer wakeup, encode and socket write per 20 ms per call: about 10 us per frame, so 1000 silent calls cost about half a core (`BenchmarkUnderlayIdleWriters`, M4 Pro, no-op socket). A shared ticker would not remove the writes, so there is none.
- Ambience frames are sent just-in-time (no lead), like speech, so network jitter can underrun the client's player. After a stalled write, ambience restarts at real time and skips the stalled span instead of bursting it.
- Mixing saturates (hard clip): at the default -30 dB the ambience adds at most ~3% of full scale, so clipping only touches speech that is already at full scale.
- If a flush or arriving speech races an ambience frame between mixing and write, the ambience position skips one frame (a possible tick, once).
- A one-shot also starts with that 5 ms blend. A one-shot (`loop: false`) ends in silence and the writer stops emitting.
- An ambience write failure ends the connection like any slow-client error.

## Per-flow audio

The start step of a flow can carry an `audio` object; each member overrides the server's setting for that flow's calls only when present:

```json
"audio": { "denoise": { "strength": 0.5 }, "output_gain_db": -3, "background": { "name": "office", "gain_db": -30, "loop": true } }
```

- `denoise`: present turns noise reduction on (`strength` above 0 up to 1, default 0.5). A flow cannot turn off a server-wide `input_filters` denoise.
- `output_gain_db`: -30..+6; an explicit `0` removes a server-wide gain for this flow.
- `background`: `name` must be a key of `audio.backgrounds` (lowercase letters, digits, `-`, `_`, up to 32): never a path; `{"name": ""}` means no background for this flow even when `background_default` is set. `gain_db` -60..0 and `loop` override the catalogue entry. Publishing a flow that names an unknown sound fails with `400 invalid_request` listing the available names; while `audio.allow_agent_background` is false any `background` is refused. If the catalogue or the ceiling changes after publishing, the flow stays ready and its calls play the server default (or none) instead, with a warning in the log.
- `tool_cue`: `{name, after_ms}`, a name from `audio.cues`. Plays that cue while a slow synchronous MCP tool runs (see the hold sound in `docs/api/v1.md`). It shares the route sound's player: it ends when a route starts preparing and never starts while the route sound waits. `after_ms` 0..10000, 0 = 1000 ms (the route sound's own wait is 300 ms because a route gap is always silent; most tool calls return within a second).
- Any step may set `audio.background` (it is the only `audio` member a step other than the start step may set). The sound switches when a route makes the step active; a step that sets none keeps what is playing; a route back to the start step restores the bed the call began with (its own, else the server default). A switch to the sound already playing at the same level is skipped, because each switch restarts the sound from its beginning. The same publish rules apply to every step (the step is named in the refusal). If the server refuses a step's sound at call time (flow sounds turned off, or the sound dropped after publishing), the step keeps the current sound; only the start step falls back to the server default. A step's `gain_db` and `loop` can be set through the API; the editor sets the step's sound name only and keeps any gain and loop already stored. Known limitation: a switch is an instant swap (the route sound fades over 30 ms, the background does not).
- Because the transport accepts the connection before the call's flow is known, the call's background is set once the flow is resolved (`Connection.SetBackground`), without disturbing the paced writer.

## Route cues

A flow with steps can play a sound while the call is between steps, so the caller never hears dead air. Sounds are operator files named under `audio.cues` (PCM16 mono WAV, resampled once at startup):

```yaml
audio:
  cues:
    chime:
      file: /etc/govoice/chime.wav
      gain_db: -20   # -60..0, default -20
      loop: false    # default true; a one-shot chime vs a bed that loops until the new step speaks
```

The flow picks one with `handoff_cue: {name: chime, after_ms: 300}` in its config. The cue starts only if the gap after the old step's last audio outlasts `after_ms` (default 300), so quick routes stay silent. It fades out (30 ms) on the new step's first audio, when the caller speaks, if the route fails, or after 10 s. Cue frames are non-speech, uninterruptible output like the background: recordings and heard-text ignore them. The timeline records `cue_start` and `cue_stop` (with the reason). Publishing a flow whose `handoff_cue.name` is not one of these cues is refused (`400`, naming the available ones); a stored flow whose cue the server no longer has logs a warning and the call runs without a cue. At most 8 cues; names match `[a-z0-9_-]{1,32}`.
