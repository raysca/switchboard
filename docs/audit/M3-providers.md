# M3 audit: Google, xAI and Nvidia provider integration

Independent read-only audit, round 6, branch `main` at `40dcf5f`. Scope: `providers/google`, `providers/xai`
(LLM, plus STT/TTS under `-tags xaispeech`), `providers/nvidia`, `providers/openai` as used by NIM, `core/netpool`,
`server/registry*.go`, `server/buildstage.go`, `server/nvidia.go`, the failover classifiers they feed, and the live
docs in `docs/benchmarks/`.

Method: code reading, plus throwaway probe tests compiled with `go test -overlay` (the probe files live in the session
scratchpad, nothing in the tree was edited). No provider calls were made. `go vet` and `go test -race` pass for
`providers/{google,nvidia,openai}` and `core`, and `providers/xai` passes with `-tags xaispeech -count=3`.

Summary: 10 findings. 1 high, 6 medium, 3 low.

| ID | Sev | Title |
|---|---|---|
| M3-01 | high | Google TTS starts synthesizing the next unit only after the previous unit has played (the cause of the Google gaps) |
| M3-02 | medium | xAI STT writes `audio.done` before the audio it buffered before `transcript.created` (an STT failover replay into xAI loses the utterance) |
| M3-03 | medium | xAI TTS network failures are classed as client errors: no retry, no breaker count, no TTS failover |
| M3-04 | medium | Breakers are shared across the whole process and count 429: one tenant's exhausted quota opens the breaker for every tenant (for xAI, every stage too) |
| M3-05 | medium | Gemini HTTP 500 INTERNAL is not retried; Gemini errors are classified by substring |
| M3-06 | medium | The production image and `make build` leave out `-tags xaispeech`, so xAI STT/TTS are absent in production |
| M3-07 | medium | NIM reasoning models: reasoning deltas do not count as output, so a long think is timed out and retried 3x, and a reply with only reasoning is played as silence |
| M3-08 | low | Gemini blocked or empty candidates end the turn silently (the finish reason is read but never used) |
| M3-09 | low | xAI STT treats a TCP-level drop mid-utterance as a clean end: no error, no failover, and the text so far is lost |
| M3-10 | low | The xAI Responses SSE parser ignores `error` and `response.incomplete`, and drops the message of `response.failed` |

---

## M3-01: Google TTS starts synthesizing the next unit only after the previous unit has played

- **Severity:** high. **Confidence:** confirmed by code and probe. Its share of the live gaps is likely, not measured.
- **Where:** `providers/google/tts.go:335-347` (each unit task waits for `finishedSeq+1 == seq` before it calls
  `SynthesizeSpeech`), `providers/google/tts.go:318-333` (`finishedSeq` moves only in the task's defer, after every
  audio frame is re-entered), `core/session.go:438-450` (`enqueueOutputAudio` runs the output gate), and
  `transport/ws/transport.go:1010-1012` + `transport/ws/pacing.go:36` (the gate blocks the producer at playback rate
  once 2 s of audio is queued).
- **Evidence:** a probe ran two queued units, each with 300 ms of synthesis and 1 s of audio, under a playback-paced
  output gate. Unit 2's synthesis started at **1.35 s**, after unit 1 had played. Unit 1 started at 0 ms. The xAI TTS
  processor does it the other way round: it synthesizes first and waits for its turn only to emit
  (`providers/xai/tts.go:342-358`). That matches the live data: S9 had **Google 5 gaps (5.4 s total) and xAI 0 gaps**
  on the same scenario (`docs/benchmarks/tts-unit-sizing.md`, "Live confirmation").
- **Failure scenario:** a multi-sentence reply on Google TTS (the all-Google launch profile). Unit N's audio goes out at
  playback rate, so its task holds the slot until about 2 s of audio is left in the queue. Only then does unit N+1's
  request start. With Google's measured 1.1-3.5 s per request (3.5 s mean time to first byte in S9), every sentence
  boundary gets a silence of about max(0, synthesis time - 2 s). The 250-char pieces of one unit are also synthesized
  one after another before any audio plays (`tts.go:374-390`).
- **Fix direction:** run `Execute` before the in-order wait, as xAI does, with bounded prefetch (one or two units
  ahead), and keep emission in order.
- **Launch impact:** audible gaps between sentences on every longer Google reply.

## M3-02: xAI STT writes `audio.done` before its buffered audio

- **Severity:** medium. **Confidence:** confirmed by probe. This is the known-open item "audio.done before
  transcript.created", root-caused.
- **Where:** `providers/xai/stt.go:840-852` (`beginDrain` calls `CloseSend`, which sends `{"type":"audio.done"}`,
  without checking `st.isReady`), against `stt.go:473-484` (audio is buffered in `st.audioBuf` until ready) and
  `stt.go:665-688` (the buffer is flushed only when `transcript.created` arrives).
- **Evidence:** the probe sent `UserStartedSpeaking`, then 50 audio frames, then `UserStoppedSpeaking`, then
  `transcript.created`. The order on the wire was `audio.done` first, then all 50 chunks. The existing
  `TestXAISTTKeepsReplayBurstBeforeReady` never sends a stop, so it does not catch this.
- **Failure scenario:** xAI configured as an STT fallback. `core.STTFailover` journals `UserStoppedSpeakingFrame`
  (`core/stt_failover.go:228-229`) and replays started, audio and stop synchronously in `jump`
  (`core/stt_failover.go:438-442`), microseconds after the dial returns and well before `transcript.created`.
  Any failover after the VAD stop therefore sends the whole utterance after `audio.done`, and the user's turn is lost.
  On the primary path the same thing happens for a short utterance whenever the stream is dialed at speech start and
  the VAD stop comes before the handshake. That is rare, because a stream is normally pre-opened on the silence.
- **Fix direction:** in `beginDrain`, when the stream is not ready, set a pending-done flag. The `transcript.created`
  handler then sends `audio.done` after flushing the buffer, still under `sendMu`.
- **Launch impact:** STT failover into xAI loses exactly the utterance it exists to save. This only applies if xAI
  speech ships at all (see M3-06).

## M3-03: xAI TTS network failures are classed as client errors

- **Severity:** medium. **Confidence:** confirmed by probe.
- **Where:** `providers/xai/tts.go:130-133` and `141-144` (`MapError("tts", err)` for a `Do` or body-read error),
  `providers/xai/errors.go:186-200` (`MapError` defaults to `ClassificationNonRetryable`), `providers/xai/classify.go:14-17`,
  and `server/tts_failover.go:24-34` (`ttsClassify` uses `ClientError()`).
- **Evidence:** a connection refused on the xAI TTS endpoint produced
  `verdict={Retryable:false ClientError:true}`, with `ProviderError.ClientError()` returning true.
  The STT path was already patched for this: `sttClassify` reads the xAI `StatusCode` instead of the class
  (`server/stt_failover.go:28-35`). The LLM path is safe because it uses `MapHTTPError(nil, ...)`, which is retryable.
  TTS was missed.
- **Failure scenario:** a connection reset, DNS failure, TLS error or truncated body on a TTS request. The unit is not
  retried and is not counted on the breaker. Its failover class is `client_error`, which is not in the default failover
  classes (`server/agent/fallback.go:52`), so the configured TTS fallback is never used. The sentence is skipped.
- **Fix direction:** map transport errors with `MapHTTPError("tts", nil, "", err)`, as the LLM does, or classify
  `StatusCode == 0` as retryable.
- **Launch impact:** sentences dropped on transient network faults, and TTS failover dead for xAI. Applies only with
  `xaispeech`.

## M3-04: Process-wide breakers count 429, so one tenant's quota opens them for every tenant

- **Severity:** medium. **Confidence:** the mechanism is confirmed by code. How much it hurts depends on traffic.
- **Where:** `server/registry.go:137-140` and `242` (one `xaiBreaker` for xAI LLM, STT and TTS across all tenants), the
  registry also has one Google breaker per stage across all tenants, `server/nvidia.go:85-91` (Riva keyed only by
  endpoint, so every tenant on hosted NVCF shares it), `providers/xai/classify.go:16` (429 is `ClassificationRateLimit`,
  not a client error, so it counts), `providers/google/errors.go:215-222` (ResourceExhausted counts), and
  `providers/xai/breaker_scope_test.go:22` (pins "429 counts").
- **Failure scenario:** each tenant stores its own key or project, so each has its own quota. Tenant A runs out of
  quota. Five failures in a row (two turns at 3 attempts each) open the shared breaker for 30 s. Every other tenant's
  xAI LLM, STT and TTS (or Google stage) then fails with `circuit_open`. If the half-open probe is tenant A's request
  again, the breaker reopens. For xAI, an STT outage also blocks the LLM, because the stages share one breaker. The
  registry comments call out exactly this for self-hosted NIM/Riva ("one tenant's broken ... must not open it for the
  others") but do not apply it to xAI or Google.
- **Fix direction:** key breakers per credential (or tenant) and stage, or stop counting 429 and quota errors as
  breaker failures. Keep them retryable.
- **Launch impact:** one noisy tenant degrades every tenant on the same provider.

## M3-05: Gemini HTTP 500 is not retried; Gemini errors are classified by substring

- **Severity:** medium. **Confidence:** confirmed by probe.
- **Where:** `providers/google/errors.go:116-173`. `genai.APIError` is not a gRPC status, so it falls through to
  substring matching on `"Error %d, Message: ..., Status: ..."`. The default case is `ErrProviderInternal` with
  `Retryable: false`.
- **Evidence:** the probe gave `{Code:500, Status:INTERNAL}` → `retryable=false client=false`. It also showed the order
  of the checks matters: any "401" or "403" substring (a number inside a quota message, for example) is read as an auth
  error, a 400 whose message says "unavailable" is read as an outage and retried, and any "404" anywhere is read as a
  client error (`classify.go:32`).
- **Failure scenario:** Gemini's transient "500 An internal error has occurred. Please retry" fails the turn at once,
  with no retry, while still counting against the breaker. With an LLM fallback it fails over as `server_error`;
  without one the turn is lost.
- **Fix direction:** `errors.As(err, &genai.APIError{})` and classify by `Code`/`Status` (5xx and 429 retryable,
  4xx client) before any substring matching.
- **Launch impact:** avoidable lost turns on Gemini (Google is a launch default).

## M3-06: The production build leaves out `-tags xaispeech`

- **Severity:** medium. This is a launch-scope decision. **Confidence:** confirmed.
- **Where:** `deploy/Dockerfile:23-25` and the `Makefile` `build` target (no `-tags`), against
  `server/registry_noxaispeech.go:8` and `server/provider/xaispeech.go:5`.
- **Evidence:** every xAI speech result in `docs/benchmarks/xai-live.md`, `agent-matrix.md` and `tts-unit-sizing.md` came
  from tagged builds (`TAGS=xaispeech`, and livecheck adds the tag itself, `tools/livecheck/profiles.go:253`). The
  shipped image registers no `xai.stt` or `xai.tts`, and its provider catalog advertises xAI as LLM-only.
- **Failure scenario:** if xAI STT/TTS is a launch target, agents that use it fail at call start with
  `stt provider "xai" is not available on this server` (`server/call.go:161-163`). As a fallback it is skipped, with
  only a log line and a counter (`server/call.go:206-208`), so failover chains quietly lose a candidate.
- **Fix direction:** decide the scope. Either add `-tags xaispeech` to the Dockerfile and `make build` (and CI vet/test),
  or state in the launch notes that xAI is LLM-only.
- **Launch impact:** xAI speech is unavailable in production despite passing live checks.

## M3-07: NIM reasoning models are timed out and retried, and reasoning-only replies are silent

- **Severity:** medium. **Confidence:** likely. The code path is confirmed; the trigger depends on the model.
- **Where:** `providers/openai/llm.go:227-249` (the chunk struct has no `reasoning_content`), `llm.go:584-596`
  (`committed` is set only on content or tool calls), `llm.go:509` (the retry policy, with a 30 s attempt timeout
  covering the whole stream), and `server/nvidia.go:24` (the default `openai/gpt-oss-20b` is itself a reasoning model).
  There is no way to pass `max_tokens`, `reasoning_effort` or `chat_template_kwargs`.
- **Evidence:** `docs/benchmarks/nvidia-live.md:18-23`: nemotron-3.5-lightning took 80 s and 662 tokens for "Hello", and
  nemotron-3-super's first token ranged from 0.5 to 6 s.
- **Failure scenario:** a slow-reasoning model hits the 30 s `AttemptTimeout` before any content arrives. Nothing is
  committed, so `Execute` retries, up to 3 x 30 s of silence and 3x the billing, before `ErrTimeout`. If the
  model ends with reasoning only (empty `content`), the turn ends as `LLMTextFrame{Text:""}` with no error, and the
  caller hears nothing. The first-token deadline in `core.LLMFailover` applies only when fallbacks are configured.
- **Fix direction:** parse `reasoning_content` and count it as liveness and committed output, pass a reasoning-effort
  knob per provider, and turn an empty final text into an error frame.
- **Launch impact:** only when an operator picks such a model, but the NIM catalog defaults lean that way.

## M3-08: Gemini blocked or empty candidates end the turn silently

- **Severity:** low. **Confidence:** likely.
- **Where:** `providers/google/llm.go:188-194` (`FinishReason` and `PromptFeedback.BlockReason` are captured),
  `llm.go:446-500` (never read in the live path), and `llm.go:594-599` (an empty `LLMTextFrame` is emitted). Only
  `summarize.go:45-48` acts on it.
- **Failure scenario:** a SAFETY, PROHIBITED_CONTENT or RECITATION finish, or a blocked prompt, produces no text and
  no `ErrorFrame`. There is no failover and nothing in the metrics. The caller hears silence after speaking.
- **Fix direction:** when the turn's text is empty and the finish reason is not STOP, emit a non-fatal `ErrorFrame`.
- **Launch impact:** rare silent turns that are hard to diagnose.

## M3-09: xAI STT treats a TCP-level drop mid-utterance as a clean end

- **Severity:** low. **Confidence:** likely. coder/websocket wraps `io.EOF` as "failed to read frame header: EOF".
- **Where:** `providers/xai/stt.go:637-646`. `errors.Is(err, io.EOF)` returns silently, as does any error string that
  contains "closed", "1006" or "1000". The numeric checks are dead code: `CloseError` prints status names, not numbers.
- **Failure scenario:** a load balancer or server drops the TCP connection mid-utterance. The receiver exits with no
  `ErrorFrame` and no breaker report, so `STTFailover` never fires. The next audio frame opens a new stream bound to
  the same turn with an empty `base` and `chunks`, so the words already spoken are missing from the transcript.
- **Fix direction:** an EOF while the stream is not `closing` and has not seen `transcript.done` should raise a
  retryable `ErrorFrame` (`TranscriptInvalidationFrame` plus an error, as the buffered-send failure path does).
- **Launch impact:** occasional truncated user turns on network blips (xAI speech builds only).

## M3-10: The xAI Responses SSE parser ignores `error` and `response.incomplete`

- **Severity:** low. **Confidence:** likely. These event names are from the Responses API spec; xAI's use of them was
  not observed live.
- **Where:** `providers/xai/tools.go:489-508`. `response.failed` becomes a bare `errors.New`, which is retryable, counts
  on the shared breaker, and drops the server's reason. A `{"type":"error"}` event is ignored. `response.incomplete`
  (max tokens, content filter) is not terminal, so the stream ends in `ErrMissingTerminalEvent`, which after streamed
  text surfaces as `ErrRetryCommitted`.
- **Failure scenario:** a request-caused failure (context too long, content policy) is retried 3x and counts toward
  opening the cross-tenant xAI breaker (M3-04). A truncated reply is reported as a provider failure.
- **Fix direction:** handle `error`, `response.failed` and `response.incomplete` explicitly, carry their `code` and
  `message` into a `ProviderError`, and treat incomplete as terminal.
- **Launch impact:** misleading errors, plus some extra breaker pressure.

---

## Known-open live items: root cause status

- **xAI S2 barge-in / S4 pause with the turn manager:** fixed in round 4. The offline harness
  (`stt_turn_agent_test.go`) passes, and a probe of the barge-in path showed one stream carrying the whole sentence
  (60 frames, nothing late). `agent-matrix.md` still shows `xai on S4: FAIL 2 turn`. That is stale: it was counted
  with the old VAD-stop rule, as `xai-live.md` explains. It is a doc issue, not a code defect.
- **xAI `audio.done` before `transcript.created`:** root cause and the failover replay trigger in M3-02.
- **Google gaps:** M3-01. **Google slow TTS first byte:** unary `SynthesizeSpeech` on Journey voices, where the whole
  unit must be synthesized before the first byte (`tts.go:101`), plus sequential 250-char pieces. `StreamingSynthesize`
  is not used. Each call also builds its own gRPC client and channel (`registry.go:176`, `225`); the channel is warmed
  by `warm.go`, and the per-RPC auth is not.
- **chirp_2 sends no interims:** the code does request them (`stt.go:180-182`, `InterimResults: true`), so this is the
  model's behaviour, not an adapter bug. The consequence is that with `turn.min_words`, turns and barge-in start only
  at the final; the registry warns and counts this (`registry.go:572-580`). Use `latest_long` where interims matter.
- **NIM reasoning-only models:** M3-07. **404/410 catalog models:** classified `client_error` with an explicit
  "set the model explicitly" message (`providers/openai/errors.go:54-58`). That is correct, but `client_error` is not
  a default failover class, so a retired model fails every turn non-fatally unless the agent opts into
  `failover.on: [client_error]`. This is by design and should be documented for launch.
- **Google gRPC burst replay:** the replay order is correct. Google STT sends audio synchronously and `CloseSend` comes
  after it (`stt.go:434-445`), so the M3-02 ordering bug does not apply. There is no pacing. Whether Speech v2 rejects
  audio faster than real time is still unverified live. If it does (OutOfRange), `mapGRPCStatus` returns a non-client
  `ErrProviderInternal`, the class is `server_error`, and the chain moves on to the next candidate, so the effect is
  contained.

## Areas checked, found clean

- xAI LLM: cancellation on barge-in, `Close` and session end (`streamCtx` is derived per turn); generation fencing of
  deltas and final frames; commit-before-retry; Retry-After parsing, capped by `MaxBackoff`; error bodies limited to
  1 KiB, sanitized and truncated; `store:false` on every request; tool-call assembly and conflict detection;
  `chat/completions` with tools refused at construction.
- xAI STT/TTS: breaker gating on dial (`WouldRefuse` plus one report per open); a cancelled open is not counted;
  bounded drain (2 s) and the draining-stream cap (8); stream ownership across turns; TTS in-order emission and slot
  release on refused tasks; odd-byte trimming; usage frames.
- Google LLM: Vertex vs API-key client construction (Vertex location defaults to `us-central1`, with no config override
  for the LLM; noted, not a bug); producer goroutine exits on cancel (buffered `errCh`, `select` on `ctx.Done`);
  native tool history and the retry reset per tool round.
- Google STT: `OpenBounded` open with stream-lived ctx; rotation overlap and offset dedup; send `io.EOF` handled as a
  stream end; detach at the VAD stop; `Close` releases the gRPC client.
- Google TTS: WAV stripping; rune-safe 250-char split at whitespace; per-unit cancellation on interruption; client
  closed on `Close`.
- Nvidia: SSRF guard (link-local and metadata always blocked; plaintext only to private addresses; bearer never sent on
  plaintext); hosted endpoint requires both `function_id` and `api_key`; `tts_function_id` fallback; breakers keyed per
  endpoint, and the failover `Open` check uses the same key; send timeout cancels a hung RPC; breaker success is
  recorded on the first server response, not on open; TTS carry of split samples and per-attempt resampler.
- NIM via the openai processor: base URL from the credential, default model; secret redaction (`nvapi-`) in errors;
  mid-stream `error` objects retryable; `stream_options` fallback.
- Registry and buildstage: built processors are closed when a later stage fails; secrets are zeroed on resolve
  failure; Google `$ENV` keys refused in stored secrets; `apiKey` rejects multi-token secrets; `failoverPolicy` only
  shortens.
- `core/netpool`: pool key independent of user input; guarded pools never use a proxy; request counting covers HTTP,
  WebSocket upgrades and gRPC opens (`CountRequest`).
- Secret leakage: the xAI, Nvidia and OpenAI sanitizers cover bearer tokens, `api_key=`, `xai-` and `nvapi-`. The
  Google sanitizer redacts only the first `key=` and `Bearer` occurrence, but genai sends the key in a header, so this
  is not exploitable today.
