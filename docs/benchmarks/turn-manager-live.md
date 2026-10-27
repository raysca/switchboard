# Turn manager with real Google providers

Measured 2026-10-08 with `tools/livecheck` (`GOVOICE_LIVE=1 make live-check`) on one Apple Silicon
Mac (macOS, home network, other work running: load average about 3), a real server child
(no fake providers), real Silero VAD / Smart Turn workers, and one client over the real WebSocket/RTVI
protocol that streams prerecorded speech in real time. Not a benchmark: **n is 1 to 3 per cell** (22 for the
S1 manager rows, pooled from several runs), one machine, one network, one afternoon. Treat differences
under about 300 ms as noise.

Providers: STT Google Speech v2 streaming `chirp_2` (us-central1; also `latest_long` where marked),
LLM `gemini-2.5-flash`, TTS `en-US-Journey-F`. Prompts: one or two short sentences, six or more for
"tell me about a topic". Speech: `e2e/fixtures/voice-debug.wav` ("What is two plus two?", 1.6 s after trimming) and
four clips synthesized once with Google TTS (`en-US-Neural2-D`: "Tell me about the planet Mars.", "Uh-huh, yeah.",
"I would like to know" + pause + "what the capital of France is."); the clips are cached outside the repo.

Variants (server configs): `off` = default VAD-driven turns; `plain` = `turn.manager: true`; `on` = manager +
`min_words: 3`; `fast` = `on` + `speech_timeout: 150ms`; `smart` = `on` + `stop: [turn-analyzer, speech-timeout]`; `smart0` = `smart` without `min_words`.

## Verdict first

* The manager path works end to end with real Google providers after two fixes made during this run (below).
  No transcript was lost after barge-in (S2: the second utterance was transcribed and answered in every variant).
* **Do not make `turn.manager` default-on yet.** With the default stop it costs about **+0.6 s voice-to-voice** on this
  stack (S1 p50 4.12 s vs 3.48 s), because the manager's 700 ms `speech_timeout` starts after the VAD's own ~0.74 s
  hangover. With `speech_timeout: 150ms` the cost is +0.14 s. Make the VAD stop configurable (Pipecat uses 0.2 s) or
  start the manager's clock at the audio end, then re-measure.
* `min_words` only works with an STT that sends interim results. `chirp_2` sends none (only a final about 1.5 s after
  speech end), so with `min_words: 3` there is no barge-in at all until the final arrives (S2). `latest_long` sends interims.
* Smart Turn is not worth enabling on this stack yet. The audio-window bug that disabled it under `min_words` is fixed
  (round 2, below), but the model itself scores the real recorded question "What is two plus two?" incomplete
  (p < 0.1 at the call point, Pipecat's Python gives the same), so that turn is held for the full `stop_secs` (+2.3 s).

## Results (chirp_2)

S1, normal turn, end of speech to first bot audio (client clock). Final transcript = time from end of speech to the
final `user-transcription`. Transcript heard: "what is 2 + 2" in every turn of every variant.

| variant | n | v2v p50 | v2v min..max | final transcript p50 |
|---|---|---|---|---|
| off | 16 | 3.48 s | 3.20..4.02 | 1.75 s |
| plain | 3 | 4.02 s | 3.56..4.18 | 1.66 s |
| on (min_words 3) | 22 | 4.12 s | 3.79..4.87 | 2.41 s |
| fast (speech_timeout 150ms) | 22 | 3.62 s | 3.33..4.87 | 1.88 s |
| smart (min_words 3) | 22 | 3.71 s | 3.26..4.44 | 1.89 s (model disabled by bug, see below) |
| smart0 (no min_words), 3 turns x 2 runs | 6 | 5.4..5.9 s | | 3.94 s (held 3 s) |

Where the time goes (server stage means, `off`): STT final 0.93..1.03 s after VAD stop, LLM first token 0.8..1.2 s,
TTS first byte 1.0..1.8 s, plus ~0.74 s VAD hangover. Google's own `chirp_2` finalization (about 1.5 s after the end of
speech, measured with `-stt-probe`) is the largest single item; `latest_long` finalizes 0.85 s after speech end.

| scenario | off | plain | on / fast / smart (min_words 3) |
|---|---|---|---|
| S2 barge-in: user onset to bot audio stop | 170..181 ms | 333..356 ms | **no barge-in**: the reply played to its end over the user (final arrives after speech end) |
| S2 second utterance transcribed and answered | yes, answered 3.3..3.9 s after its end | yes, 3.3 s | yes (reply started right after the first reply ended) |
| S3 backchannel ("uh-huh, yeah") while bot speaks | interrupts and the model replies to "yeah" | interrupts (no min_words) | **ignored**: no flush, no new reply, bot finished its answer |
| S4 1.0 s mid-utterance pause | 2 user turns, 2 replies, first reply a wrong "please tell me what you would like to know" | 1 turn, 1 correct reply ("Paris"), first audio 3.6 s after end | same as plain (3.1..3.7 s) |
| S4 2.2 s pause | 1 reply | 2 replies (split) | smart: 2 replies (split, see Smart Turn issues) |

S5 idle prompt and hang-up (`idle.user_timeout: 4s`, `max_prompts: 1`, `prompt_mode: say`, goodbye set, no speech):
off and on identical. Before the TTS fix the prompt and goodbye were never heard and the call ended at 18.0 s with
`user_idle`; after the fix the prompt is audible at 4.6..4.8 s and the call hangs up at 10.5..10.9 s.

## Results (latest_long, interim results available; n = 3 per row)

| variant | S1 v2v p50 | final transcript p50 | S2 audio stop after onset | S3 backchannel |
|---|---|---|---|---|
| off | 3.00 s | 1.06 s | 181 ms | interrupts |
| plain | 3.17 s | 1.66 s | 356 ms | interrupts |
| on (min_words 3) | 3.16 s | 1.66 s | 1.46 s (3 words needed) | ignored |
| fast | 2.90 s | 1.11 s | 1.56 s | ignored |
| smart (min_words 3) | 5.62 s | 3.96 s | 1.46 s | ignored |

`latest_long` is about 0.5 s faster than `chirp_2` end to end and is what makes `min_words` usable; even so a 3-word
gate costs about 1.3 s of extra barge-in delay relative to VAD-driven barge-in (the model needs time to hear 3 words).

## Round 2: after the fixes (same machine and providers, same day, n = 1 or 2 per cell)

`-only S1,S4 -variants off,smart0,smart -s1-turns 2`, then S4 again for `smart0,smart` after fix 5 below.
Before = the tables above.

| cell | before | after |
|---|---|---|
| S1 off, v2v | p50 3.48 s (n=16) | 3.56, 3.47 s |
| S1 smart (min_words 3), v2v | p50 3.71 s (n=22): every turn ended at once because the model was scoring silence | 3.53, 6.13 s: the model now judges the speech; one turn "complete", one held to `stop_secs` |
| S1 smart0, v2v | 5.4..5.9 s (n=6) | 6.09, 5.38 s |
| S4 1.0 s pause, off | 2 turns, 2 replies, first reply wrong | same (n=1) |
| S4 1.0 s pause, smart | 1 reply | 1 reply, first audio 3.16 / 3.69 s after the end, full sentence heard |
| S4 1.0 s pause, smart0 | not measured | before fix 5: 1 reply but the first half ("I would like to know") was lost, twice; after: full sentence heard, 3.50 s |
| min_words warning (`voice_turn_min_words_no_interims_total`) | none | logged once per `smart` call with `chirp_2` |
| ERROR lines at teardown | 2 (first runs) | none in these runs (both root-caused, see 6) |

Smart Turn verdicts (server counters, all smart runs): 7 "complete", 3 "incomplete", 0 fallbacks; inference 39 ms mean (n=10).

### Is it the model or our window? (offline, `third_party/smart_turn_v3.2_cpu.onnx`, same file as Pipecat's)

Each clip was trimmed to its speech, given 500 ms of leading silence (pre-speech) and the trailing silence shown,
and scored by our Go backend and by Pipecat's `LocalSmartTurnAnalyzerV3._predict_endpoint` (pipecat-impl venv,
onnxruntime 1.24.4) on the identical samples. Pipecat calls the model at the VAD stop, i.e. with ~0.2 s of trailing
silence (its VAD `stop_secs` default); ours is called at our VAD stop, ~0.74 s after the speech. p(complete), > 0.5 = complete:

| clip | kind | Go 0.2 s | Py 0.2 s | Go 0.8 s | Py 0.8 s | Go 1.0 s | Py 1.0 s |
|---|---|---|---|---|---|---|---|
| `voice-debug.wav` "What is two plus two?" | recorded, complete | 0.08 | 0.06 | 0.08 | 0.09 | 0.56 | 0.37 |
| "What time is it in Tokyo?" | TTS, complete | 0.98 | 0.98 | 0.99 | 0.99 | 0.99 | 0.99 |
| "Tell me about the planet Mars." | TTS, complete | 0.97 | 0.97 | 0.99 | 0.99 | 0.99 | 0.99 |
| "I would like to know" | TTS, fragment | 0.91 | 0.93 | 0.64 | 0.64 | 0.81 | 0.83 |
| "Can you tell me about the" | TTS, fragment | 0.42 | 0.62 | 0.91 | 0.85 | 0.96 | 0.95 |

* The log-mel features are identical (max difference 3.5e-6 over all 30 windows), so the gaps are onnxruntime
  (Go links 1.29.0, Python 1.24.4) on this int8 model: Python alone moves the same input by up to 0.39 between
  graph-optimization levels. The earlier "up to 0.06" held only for synthetic fixtures. One of 30 windows flips verdict.
* It is the model, not our window: at Pipecat's own 0.2 s call point the recorded question is still "incomplete"
  in both runtimes, and both TTS fragments (falling, sentence-final TTS prosody) are "complete". Trimming our window
  to 0.2 s of trailing silence would change no verdict here, so it was not done; the window rules now match Pipecat
  (start at the VAD speech start minus 500 ms, span pauses while incomplete, cleared by a complete verdict, by the
  turn ending, or by `stop_secs` of silence; last 8 s).
* Hold: Pipecat holds an "incomplete" turn `stop_secs` (3 s) after its VAD stop (~3.2 s after speech); ours counts
  from our VAD stop (~3.7 s after speech). The default stays 3 s for parity; `turn.analyzer.stop_secs` bounds the cost.
* n = 5 clips, 4 of them synthetic: this is a sanity check of the pipeline, not a model-quality evaluation.

## Bugs found and fixed (with tests)

1. **Idle prompt and goodbye were never spoken with Google TTS** (`providers/google/tts.go`). Idle speech is
   synthesized as generation-0 units; the post-synthesis fence compared `gen != session generation` (the session starts
   at generation 1), so the audio was dropped silently. Fixed by exempting generation 0, like the earlier staleness
   checks already did. Test `TestTTS_GenerationZeroUnitIsPlayed` (fails without the fix).
2. **Lost user turns with `min_words` and an STT without interims** (`core/turn/processor.go`). A turn that has not
   started is dropped 1 s after the VAD stop; `chirp_2`'s final arrives 1.0..1.9 s after it, so about 7% of manager turns
   (5 of about 70 across `on`/`smart`/`fast`) produced no transcript, no reply, and the server timeline showed
   `speech_stop` with no `stt_result`. An unstarted turn whose newest segment has no final yet now waits up to 2.5 s.
   Test `TestProcessorLateFinalAdmitsMinWordsTurn` (fails without the fix); after the fix 0 of 48 manager turns were lost.
   The cap is fixed: an STT slower than 2.5 s after VAD stop would still lose the turn.

Round 2 (review):

3. **Smart Turn scored silence when the turn started late** (`smartturn/analyzer.go`). The window started at the
   manager's turn start, which with `min_words` is when the transcript arrives (after the speech). The audio tap now
   tracks the VAD speech start itself (Pipecat's `_speech_start_time`). Tests `TestAnalyzerWindowFollowsSpeechStart`,
   `server` `TestTurnAnalyzerWindowWithMinWords` (both fail without the fix).
4. **A barge-in did not stop an idle prompt mid-chunk** after fix 1 (`providers/google/tts.go`, `providers/xai/tts.go`):
   gen-0 units are exempt from the generation fence, so the chunk loop now also stops on the cancelled unit context.
   `core/ttsunit` (Cartesia, ElevenLabs, NVIDIA) already exempted gen 0 and stops through the stream context.
   Test `TestTTS_GenerationZeroUnitStopsOnInterruption`.
5. **A pause lost the first half of the sentence** (`core/turn/processor.go`). Smart Turn ended the turn at the VAD stop
   before `chirp_2`'s final arrived; the user resumed, the new turn retired the old segment, and its late final was
   dropped. A turn that ended with no transcript at all now hands its still-due segment to the next turn (within the
   2.5 s cap). Test `TestProcessorPendingFinalJoinsNextTurn`; live S4 `smart0` above.
6. **ERROR `session failed` at hang-up.** `ws: paced writer closed`: the connection closes the pacer before the session,
   so bot audio still in flight failed the session; now dropped as a hang-up (`TestOutputAfterPacerClosedIsHangUp`).
   `stt: EOF`: gRPC `Send` returns `io.EOF` once the stream has ended (hang-up cancelled it, or the server closed it;
   the status is on `Recv`); Google STT now drops that frame and opens a new stream on the next one instead of failing
   the session (`TestGoogleSTTSendEOFIsNotSessionFailure`; NVIDIA already did this). Root-caused from code and unit
   repros; not reproduced live (it needs a hang-up mid-frame).
7. **`min_words` with an STT without interims** is now visible: a WARN log and `voice_turn_min_words_no_interims_total`
   once per call when the first transcript is already final. Behaviour unchanged.

## Open issues (not fixed here; repros)

* **Smart Turn v3.2 misjudges these clips** (round 2 table): the recorded question is "incomplete" and TTS fragments
  are "complete", in Python as well. Needs an evaluation on real conversational audio before it is recommended.
* **Stacked silence.** `speech_timeout` counts from the VAD stop (fixed 25 chunks, about 0.74 s, not configurable), so
  the default manager waits about 1.5 s of silence; see Verdict.
* The min_words WARN is printed through the default `slog` logger (plain text), not the server's configured logger.

## Spend

About 25 calls (WebSocket sessions) in the final matrices and roughly 100 in total across development runs: about
250 STT streams (`chirp_2`, ~2..30 s of audio each), about 230 Gemini requests (tiny prompts), about 250 TTS requests
(Journey, <60 characters each), 4 clip syntheses, 8 STT probe streams. Estimated under 2 USD; exact billing not read.
Round 2: 3 live runs (about 18 STT streams, 13 Gemini requests, 17 TTS requests) and 2 clip syntheses, about 50 calls.

## Reproduce

```sh
cp examples/.env.example examples/.env     # GOOGLE_API_KEY and GOOGLE_APPLICATION_CREDENTIALS (service account)
GOVOICE_LIVE=1 make live-check             # all scenarios, variants off,plain,on,fast,smart, idle S5
GOVOICE_LIVE=1 make live-check LIVE_ARGS="-only S2 -variants off,plain -stt-model latest_long -trace"
GOVOICE_LIVE=1 make live-check LIVE_ARGS="-stt-probe -stt-model latest_long"   # raw STT result timing, 2 streams
```

`-trace` prints the client event timeline and the server session timeline; `-keep-work` keeps the temp dir (server
logs); `-pause 2200ms` changes the S4 gap. The tool refuses without `GOVOICE_LIVE=1`, hands the server child only
the provider variables its profile needs, exactly like `./dev up` (never printed or stored), and
redacts keys/tokens in the server log lines it echoes. Recorded audio is cached only in a directory you give with
`-clips` (default: a removed temp dir).
