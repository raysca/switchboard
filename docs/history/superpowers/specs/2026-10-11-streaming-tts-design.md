> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# Streaming TTS for Google and xAI Design

**Date:** 2026-10-11
**Status:** approved in conversation; awaiting written-spec review.

## Intent

First audio of each spoken sentence should start sooner, and sentences should join without audible gaps. Today Google and xAI synthesize a whole sentence before sending any audio. Success:
- the first audio frame of a unit is emitted before the provider has finished synthesizing it (provable offline);
- the live long-reply scenario (S9) shows lower time to first audio and fewer or shorter gaps than the stored unary numbers;
- a call never loses its speech because a streaming path is unavailable (fallback, kill switch);
- barge-in stops audio at once on the streaming paths.

The user made three decisions on 2026-10-11:
1. **Google voices.** Route by voice. Chirp 3 HD voices stream; other voices keep the current unary processor unchanged. The shipped default voice moves to a Chirp 3 HD voice, so default calls stream. Saved agents keep their voice.
2. **Rollout.** Streaming is on by default. A stream that cannot open, or fails before any audio is sent, retries as the existing unary request. A config switch turns streaming off.
3. **Granularity.** Streaming stays at the sentence-sized units the assistant stage already produces. Token-level input streaming is out of scope.

## Current behaviour (main at a69500b)

- **Google** (`providers/google/tts.go`, bespoke processor). Each `SynthesisUnitFrame` is split into pieces of at most 250 runes. Each piece is one unary `SynthesizeSpeech` call (LINEAR16, WAV header stripped). Nothing is emitted until the whole unit is synthesized. Up to 2 units synthesize ahead of the one being emitted (`ttsLookAhead`), and their audio is held until their turn. The default voice is `en-US-Journey-F`.
- **xAI** (`providers/xai/tts.go`, build tag `xaispeech`, bespoke processor). Each unit is one `POST /v1/tts` returning the whole PCM body. No look-ahead.
- **Shared machinery.** `core/ttsunit.Runner` already provides in-order unit playback, cancel on interruption and close, the trace span, 20 ms framing with a held-back final frame, retries before the first emitted frame, error-then-close ordering for failover, and the usage metric. Cartesia and ElevenLabs build on it. `ttsunit.ReplySlot` holds one provider connection per reply, reused across its units (Cartesia's WebSocket).
- **Matrix and audit.** The matrix lists both as unary (known gap, remaining-work item 3). The audit deferred Google `StreamingSynthesize`.

## Vendor facts (checked 2026-10-11)

- **xAI WebSocket TTS** (`wss://api.x.ai/v1/tts`; Pipecat's `XAITTSService` is the reference implementation):
  - Query parameters: `voice` (default `eve`), `language` (required), `codec` (default `mp3`), `sample_rate` (default 24000), `optimize_streaming_latency` (0 to 1), `text_normalization`, `with_timestamps`, `speed`.
  - Client messages: `{"type":"text.delta","delta":"..."}` (at most 60,000 characters each) and `{"type":"text.done"}`.
  - Server messages: `{"type":"audio.delta","delta":"<base64 audio>"}`, `{"type":"audio.done"}`, `{"type":"error","message":"..."}`. On `error` the connection may close.
  - One connection carries many utterances in sequence: after `audio.done` the client sends the next `text.delta`.
  - `text.clear` and `audio.clear` exist in Pipecat but are not documented. The docs give no idle timeout and no connection limit.
- **Google `StreamingSynthesize`** (bidirectional gRPC, Go client `cloud.google.com/go/texttospeech` v1.23.0, already a dependency):
  - Only Chirp 3: HD voices (`<locale>-Chirp3-HD-<Name>`, 30 voices, for example `en-US-Chirp3-HD-Aoede`, female).
  - First message `StreamingSynthesizeConfig` (voice, `StreamingAudioConfig`: PCM, ALAW, MULAW or OGG_OPUS; `SampleRateHertz`; `SpeakingRate`). Later messages carry text chunks. Responses carry `AudioContent`.
  - No SSML in streaming. There is no per-utterance boundary on a stream; the response ends when the client half-closes and synthesis completes.
  - Quotas: 100 concurrent streaming sessions per project, 200 Chirp 3 requests per minute per project, 5,000 bytes per request.

## Design

### 1. xAI streaming processor (`providers/xai`)

`StreamingTTSProcessor` embeds `*ttsunit.Runner` (as Cartesia's processor does) with:
- **Config.** The existing `TTSConfig`. The WebSocket URL is the configured endpoint with `https` replaced by `wss` and `ws` kept as is (the endpoint config therefore stays one value, used by both processors), plus the query parameters above (`codec=pcm`, `sample_rate`, `language`, `voice`, `optimize_streaming_latency=1`).
- **Connection.** A `ttsunit.ReplySlot[*wsSession]` holds one dialed connection per reply `(Turn, Gen)`. Dial through the same HTTP/1.1 `netpool` pool as the other WebSockets, with `Authorization: Bearer <key>`.
- **Attempt.** For the unit: `Get` or dial the session; send `text.delta` with the unit text and `text.done`; read messages until `audio.done`. Each `audio.delta` is base64-decoded and passed to `Unit.Write`. `audio.done` calls `Unit.Finish`. An `error` message maps through the existing xAI error classification and drops the session. A read or write failure drops the session.
- **Barge-in and close.** `EndReply` is `slot.Drop`, which closes the connection without blocking the runner. The next reply dials again. `text.clear` is not used (undocumented).
- **Retries.** Before the first audio the Runner retries. Attempt 0 uses the streaming path; any retry (attempt greater than 0) uses the existing unary `TTSClient.Synthesize` for that unit and writes the PCM through `Unit.Write`.
- **Unchanged.** The existing unary `TTSProcessor`, its client, tests and metrics stay as they are; they are the kill-switch path and the source of the fallback call.

### 2. Google streaming processor (`providers/google`)

`StreamingTTSProcessor` embeds `*ttsunit.Runner`:
- **Client.** `TTSClient` gains an optional second interface `StreamingTTSClient` with `StreamSynthesize(ctx, pieces []string, voice string, sampleRate int, onAudio func(pcm []byte) error) error`. The real client opens `StreamingSynthesize`, sends the config message (voice name, language from the voice, `StreamingAudioConfig{AudioEncoding: PCM, SampleRateHertz}`), sends each piece as its own input message, calls `CloseSend`, and loops on `Recv` calling `onAudio` until `io.EOF`. gRPC errors map through the existing `MapError`.
- **One stream per unit** on the already-warm gRPC connection. Pieces come from the existing `splitUnit` at `MaxSynthesisUnitChars` (250), sent as separate messages on the one stream.
- **Attempt.** Attempt 0 streams; the audio goes to `Unit.Write` as it arrives and the attempt finishes with `Unit.Finish`. A retry uses the unary `SynthesizeSpeech` for the pieces (the Chirp 3 HD voice works unary) and writes the PCM through `Unit.Write`.
- **Defensive PCM.** If the first bytes of the first response begin with `RIFF`, strip the WAV header before writing. A live run confirms whether it ever happens.
- **No look-ahead on this path.** The Runner emits units strictly in order; the next unit's stream opens when the previous unit's audio has been fully received, which is faster than playback. The existing processor keeps its look-ahead for non-Chirp voices.
- **Routing.** `NewTTSProcessor` keeps its signature for the unary processor. The server registry (`google.tts`) builds the streaming processor when the voice is Chirp 3 HD (voice name contains `Chirp3-HD`, case-insensitive) and streaming is not disabled; otherwise the unary processor.

### 3. Defaults, config and docs

- **Default voice.** `en-US-Chirp3-HD-Aoede` in `providers/google/tts.go` (`DefaultTTSConfig`, `NewTTSProcessor`), `ui/src/routes/onboarding/OnboardingView.tsx` and the placeholder in `AgentsView.tsx`, `scripts/dev-seed.mjs`, `deploy/smoke-google.sh`, `tools/livecheck/profiles.yaml` (the three Google profiles), and the docs examples (`README.md`, `docs/quickstart.md`, `docs/api/v1.md`, `docs/plans/stored-agents-drive-calls.md`). UI tests that save Journey-F as test data keep it. Saved agents are untouched.
- **Kill switches.** `providers.google_tts.disable_streaming` and `providers.xai_tts.disable_streaming` (bool, default false meaning streaming on). The effective-config goldens gain the two zero-valued keys.
- **Operations doc.** Google's 100 concurrent streaming sessions per project (one stream per speaking call), and that a quota error falls back to unary for that unit.
- **Matrix.** Remove the "xAI TTS is unary" gap and the "Google TTS StreamingSynthesize" audit item; update the providers row; mark remaining-work item 3 done or reduced to what the live run shows.

### 4. What does not change

- `core/ttsunit` (Runner, Unit, ReplySlot) is used as is. If implementation shows a missing hook, that is a spec change.
- Cross-provider TTS failover, breakers, the usage metric, trace spans and `TTSCounts` stay as they are, because they live in the Runner.
- Provider-request accounting stays per unit (`RequestsTotal`), so the live spend-cap estimates do not change.

### 5. Testing and evidence

- **Core property (offline).** A fake provider that withholds the end of its response: the first `OutputAudioFrame` of the unit is emitted before the fake finishes. One test per provider.
- **xAI** (fake WebSocket server in-process):
  - the wire messages and query string are exactly those above;
  - one dial serves all units of a reply, and a second reply dials again;
  - units stay in order;
  - a barge-in mid-stream stops frames and closes the connection;
  - an `error` message becomes a classified error and a dropped session;
  - a dial failure and an `error` before audio fall back to unary on the retry;
  - an error after audio has started ends the unit with an error and the closing frames;
  - `disable_streaming` builds the unary processor.
- **Google** (fake streaming client):
  - the config message and ordered piece messages;
  - the odd-length and header-prefixed first chunk;
  - a unit over 250 characters is one stream with several messages;
  - routing by voice (Chirp 3 HD streams, Journey does not);
  - a barge-in cancels the stream context;
  - a stream open failure falls back to unary;
  - `disable_streaming` builds the unary processor.
- **Registry and config.** Routing in `BuildCallPipeline`, the defaults, and the golden snapshots.
- **Live (needs user approval; paid).** S9 long reply for `google` (with a Chirp 3 HD voice) and `xai`, about 24 provider calls: time to first audio and gaps, against the stored S9 unary figures in `docs/benchmarks/agent-matrix.md`. The nightly critical-path job gains no new scenario (S9 stays manual, over its cap).

## Out of scope

- Token-level input streaming (sending LLM tokens to TTS before a sentence completes).
- xAI word timestamps (`with_timestamps`) and `text.clear`.
- A Google look-ahead for streamed units, unless the live run shows gaps.
- Moving non-Chirp Google voices (Journey, Neural2, ...) onto the Runner.
- A new nightly scenario for streaming.

## Risks

- **Google PCM framing.** If streaming PCM carries a header or odd-length chunks, `Unit.Write` already realigns; the header strip covers the first case. A live run confirms both.
- **Gaps between units on Google.** One stream per unit adds a first-byte wait per unit. Audio arrives faster than playback, so this is hidden unless a unit is very short. The live S9 run measures it; a look-ahead is the follow-up if needed.
- **xAI behaviour not in the docs.** Idle timeout, `error` semantics and what happens when `text.delta` arrives before the previous `audio.done` are undocumented. The design never sends the next unit before `audio.done`, and treats any error or close as a dropped session.
- **Quota.** Google allows 100 concurrent streaming sessions per project. Beyond that, streams fail and the fallback serves the unit unary (200 Chirp 3 requests per minute applies there).
- **Default voice change.** Chirp 3 HD sounds different from Journey-F and is priced differently. Existing agents are not changed; new ones get the new default.
