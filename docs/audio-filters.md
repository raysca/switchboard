# Inbound audio filters

Package `core/audio/filter`. Off by default: with `audio.input_filters` empty no processor is inserted and the pipeline is identical to before.

```yaml
audio:
  input_filters: [denoise]   # known: denoise
  denoise_strength: 0.5      # 0..1, 0 = default (0.5)
```

Server-wide only. Per-flow filter settings are left for later.

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

RNNoise, Krisp and AIC are not implemented. Each would be another `Filter` registered in `knownInputFilters` (`server/audiofilter.go`). Native ones should run in a supervised worker process like the VAD worker, with the `Filter` talking to it over a pipe and failing open on worker loss. Chain them with `denoise` or instead of it. Their latency must report via `Delayer`.

## Observability

`voice_audio_filter_frames_total{filter}` and `voice_audio_filter_latency_seconds{filter}` (histogram). Labels come from a fixed set (`denoise`, `other`). `make bench-latency` does not send audio and is unaffected.

# Output gain and background ambience

Package `core/audio/mixer`; mixing hook in `transport/ws/pacing.go`. Both are off by default (no stage, no extra traffic).

```yaml
audio:
  output_gain_db: -3        # -30..+6, 0 = off (no processor inserted)
  background:
    file: /etc/govoice/office.wav   # PCM16 mono WAV, 8..192 kHz, <= 10 MB
    gain_db: -30            # -60..0; unset = -30, 0 = full level
    loop: true              # default true
```

Server-wide only. Both are validated at config load; the file is read when the server starts (a bad file stops startup).

## Gain

`mixer.Gain` is the last pipeline stage before the transport, so it scales speech and DTMF tones. Q12 fixed-point int32 math with rounding and saturation (+6 dB on a loud sample clips, never wraps). Copy-on-write frames (a struct copy: every field, e.g. `NonSpeech`, `WordTimings`, is kept). Gain runs before the paced writer, so pacing durations are unchanged. Recording taps the paced writer's frames after this stage, so a recording carries the gain.

## Background ambience

The WAV is parsed (PCM16 mono only, including `WAVE_FORMAT_EXTENSIBLE`; 8/24-bit, float and stereo are rejected with a conversion hint; the 10 MB cap applies before parsing), low-passed (63-tap windowed sinc) when the file rate is above 24 kHz, resampled once to the 24 kHz player rate and gain-scaled once at startup. The last 5 ms are crossfaded into the first 5 ms so the loop point has no click. The samples are shared read-only; each session has a `Cursor` (just a position).

Mixing happens inside `PacedWriter` (an `Underlay`), on the pacer's clock:

- speech frame released: `speech + ambience` (saturating) is what goes on the wire;
- queue empty: the writer emits 20 ms ambience-only frames at real time, so the ambience never stops during silences;
- ambience frames are `NonSpeech` + `Uninterruptible`: `BeginFlush` clears speech and leaves them alone; they never call `OnWriteComplete` and never change speaking state. So the recording tap, the heard-text tracker (`Media` durations), the idle controller and the "bot first audio" metric see speech only. **Recordings capture the bot's speech without ambience** (the tap gets the unmixed frame);
- queue bound: ambience is never queued. An ambience frame still waiting for its slot when speech arrives gives the slot to speech, so speech waits at most the one 20 ms frame already on the wire; the 100 ms queue cap and flush behavior are unchanged (tests use the real `PacedWriter`);
- requires `EnablePacing` (the production default); without pacing the underlay is ignored.

Cost and limits:

- **Bandwidth**: continuous audio on the WebSocket, 48 kB/s per call at 24 kHz PCM16 (about 170 MB/h), plus protobuf framing; calls that were silent now send traffic.
- **CPU**: one timer wakeup, encode and socket write per 20 ms per call: about 10 us per frame, so 1000 silent calls cost about half a core (`BenchmarkUnderlayIdleWriters`, M4 Pro, no-op socket). A shared ticker would not remove the writes, so there is none.
- Ambience frames are sent just-in-time (no lead), like speech, so network jitter can underrun the client's player. After a stalled write, ambience restarts at real time and skips the stalled span instead of bursting it.
- Mixing saturates (hard clip): at the default -30 dB the ambience adds at most ~3% of full scale, so clipping only touches speech that is already at full scale.
- If a flush or arriving speech races an ambience frame between mixing and write, the ambience position skips one frame (a possible tick, once).
- A one-shot also starts with that 5 ms blend. A one-shot (`loop: false`) ends in silence and the writer stops emitting.
- An ambience write failure ends the connection like any slow-client error.

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

The flow picks one with `route_cue: {name: chime, after_ms: 300}` in its config. The cue starts only if the gap after the old step's last audio outlasts `after_ms` (default 300), so quick routes stay silent. It fades out (30 ms) on the new step's first audio, when the caller speaks, if the route fails, or after 10 s. Cue frames are non-speech, uninterruptible output like the background: recordings and heard-text ignore them. The timeline records `cue_start` and `cue_stop` (with the reason). An unknown cue name logs a warning and the call runs without a cue. At most 8 cues; names match `[a-z0-9_-]{1,32}`.
