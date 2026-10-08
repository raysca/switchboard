# xAI live run (2026-10-08)

Real xAI APIs, one machine, n = 1 per cell: a capability map and regression signal, not a benchmark. Keys come
from `examples/.env` through the harness `make` targets and never appear here. Reproduce:

```
GOVOICE_LIVE=1 make provider-matrix PROVIDERS=xai STAGES=llm,stt,tts TAGS=xaispeech
GOVOICE_LIVE=1 make agent-matrix PROFILES=xai SCENARIOS=S1,S2,S4,S5,S7 MODES=on LIVE_ARGS="-write-docs"
```

## What was used

| stage | model / voice | endpoint |
|---|---|---|
| LLM | `grok-4.20-non-reasoning` (`XAI_MODEL`) | `https://api.x.ai/v1/responses` (production default of `providers.xai_llm.endpoint`) |
| STT | `grok-voice-transcribe-2.0`, `en`, 16 kHz PCM, `interim_results=true` | `wss://api.x.ai/v1/stt` |
| TTS | voice `eve`, `en`, PCM 24 kHz | `POST https://api.x.ai/v1/tts` (unary) |

`XAI_ENDPOINT` in `examples/.env` points at `/v1/chat/completions`. Production never reads it, and chat/completions has
no tool calls, so the `xai` / `xai-llm` livecheck profiles no longer pass it on (`tools/livecheck/profiles.yaml`,
test `TestResolveExpandsEnvAndDefaults`); provtest already ignored it. **The older `xai-llm` / `off` row in
`agent-matrix.md` (S1, S7) was captured before this change on chat/completions and was not re-run.**

## Stage-level matrix (after fixes)

Provider calls: 17 + 17 + 6 + 6 (initial run, full rerun, STT-only reruns).

| stage | result | headline numbers |
|---|---|---|
| LLM | pass (11 checks) | stream ok, ttft 629 ms, total 0.6 s, usage reported, tool round trip ok (Responses API), cancel ok, bad key 164 ms, bad model 124 ms |
| STT | pass (12 checks) | WER 0.00 on all four clips, first interim 1457 ms after speech start, final 197 ms after the VAD stop, cumulative finals, resumed speech WER 0.00, rebind ok, bad key 554 ms |
| TTS | pass (15 checks) | ttfa 851 ms (unary: whole unit synthesized first), 8.3 s of audio, total 2.7 s, usage 134 chars, interrupt ok, bad key 113 ms |

First run (before the fixes) failed LLM (3 checks), STT (3) and TTS (1). Root causes, all provider-side except the last STT one:

1. **LLM: every Responses request ended in `xai sse: stream ended without terminal completion event`.** The Responses API
   ends the stream with `response.completed` and sends no `[DONE]` (confirmed with a raw request); the adapter required
   `[DONE]` (chat/completions style) so every turn errored after the text had streamed (and `retry prohibited after output
   was committed`). Fix: `response.completed` / `response.done` is a terminal event (`providers/xai/tools.go`),
   test `TestLLM_ResponsesStreamEndsWithCompletedAndNoDone`.
2. **STT: candidates were per segment, not cumulative.** xAI sends interims and chunk finals (`is_final`, about every 3 s of
   speech) with only that chunk's text and `speech_final` with the stitched utterance; the adapter forwarded them raw, so a
   pause made finals shrink ("What is" then "the capital of France?") and the turn manager would join them wrongly. Fix:
   candidates are cumulative per turn like Deepgram's (`base` + locked `chunks` + current chunk; `speech_final` settles the
   utterance, speech resumed later extends it). Test `TestXAISTT_CandidatesAreCumulativeAcrossChunksAndUtterances`.
3. **STT: `transcript.done` re-emitted the last text as a second end-of-utterance.** The live `done` has `text: ""` and comes
   right after `speech_final`, so it was a pure late duplicate (it landed in the next turn's window). Now only an open
   utterance is finalized from it. Test `TestXAISTT_DoneFinalizesOnlyAnOpenUtterance`.
4. **STT breaker (review item).** A cancelled open no longer counts as a provider failure and an open breaker now stops the
   dial (`WouldRefuse` gate, one `Report` / `RecordSuccess` per open, never `Allow`); a send failure on a stream torn
   down concurrently is not a failure either. Test `TestXAISTT_BreakerGatesDialAndCancelIsNotAFailure`.
5. **TTS: no usage frame** (provtest `usage_chars` = 0). Now reports characters, audio duration and request like
   `core/ttsunit`, and drops a trailing odd byte so frames stay sample aligned. Test `TestXAITTS_UsageFrameAndOddByte`.
6. **Harness assumption (provtest `stt.go`).** The wait for a clip's final ended on the first post-stop final, or on any
   utterance end even one before the stop. xAI sends a chunk final and the utterance final up to 0.6 s apart, and a clip with
   a pause gets an utterance end mid-clip, so late candidates leaked into the next clip's window (false `turn_binding`,
   `cumulative`, `resumed_speech` failures). The wait now ends on an utterance end after the VAD stop, else on the quiet rule.

## Agent-level matrix (profile `xai`: xAI STT + Grok + xAI TTS, mode `on` = turn.manager + min_words 3)

Provider calls: about 41 (estimates 32 + 9; includes 5 Google TTS calls for the missing livecheck clips).
Mode `off` and the `xai-llm` profile were not run in this pass (call budget).

| scenario | result | numbers |
|---|---|---|
| S1 baseline turn (2 turns) | pass | v2v 3381 ms (STT final 365 ms, LLM ttft 840 ms, TTS first byte 1011 ms), WER 0.00, answer "Two plus two equals four." |
| S2 barge-in | first run **fail** (`bot audio flushed`); rerun after the fixes below **pass** | rerun: bot audio stopped 1438 ms after the user's onset (limit 1500 ms), user-started event 201 ms, second reply 2813 ms after the clip, heard "What is two plus two?" (WER 0.00), answered "Four." |
| S4 mid-utterance pause (1.0 s) | first run **fail** (2 turns, WER 0.55); rerun: whole sentence in **one** committed user turn, one reply; recorded as FAIL only by the old turn count (see below) | rerun: one user transcription "I would like to know. What the capital of France is.", one bot reply ("The capital of France is Paris, ..."), 2 VAD stops |
| S5 idle prompt and hang-up | pass | prompt at 4.5 s, hang-up at 11.0 s, status 1000 |
| S7 text-only turn | pass | text to first audio 1320 ms |
| S6 tool call | not run | needs an external HTTPS MCP server (`-mcp-url`); tools are covered at stage level by the provtest tool round trip (pass) |

Comparison with the numbers already in `provider-matrix.md` / `agent-matrix.md` (not re-run): Google S1 v2v 3.3 s (off) and
4.1 s (on); Nvidia stage rows pass. xAI S1 on is 3.4 s, the same band, with the fastest STT final of the three (365 ms vs
949 / 1607 ms for Chirp 2) and the slowest TTS first byte besides Google (1011 ms; the unary endpoint must finish the whole
unit before the first byte, ttfa 851 ms at stage level vs Nvidia 475 ms). Stage rows: Google TTS ttfa 1687 ms, Nvidia 475 ms;
Google STT final 794 ms, Nvidia 83 ms, xAI 197 ms; LLM ttft Google 915 ms, Nvidia 933 ms, xAI 629 ms.

### S4 and S2: root cause and fixes (round 4)

Reproduced offline first (`providers/xai/stt_turn_agent_test.go`: the real xAI STT adapter, turn processor with
`min_words 3`, transcript finalizer, user and assistant context, driven in real time by a fake xAI stream with the live
cadence: interims about every 0.5 s, `speech_final` 0.3 s after `audio.done`, empty `transcript.done`). Three defects:

1. **Core, all STT providers (the S4 loss).** The turn processor forwarded the STT's `EndOfUtterance` for a managed
   turn, and the transcript finalizer commits any EOU candidate 200 ms later. So "I would like to know" was committed at
   the VAD stop while the manager still held the turn open (700 ms speech timeout); the resumed half was then joined to
   that same turn, but the turn was already retired, so the second half was silently dropped (no transcript, no reply).
   Deepgram (EOU on `Finalize`) has the same exposure; Chirp 2 only escaped because its final came after the resume.
   Fix: `core/turn/processor.go` strips EOU from re-keyed candidates (keeps `Final`); `TurnEventCompleted` commits.
   Test `TestProcessorResumedSpeechAfterEOUCommitsWholeTurn` (fails before the fix).
2. **xAI adapter: closing stream kept active after the VAD stop.** After `audio.done` the stream stayed `p.active` until
   `transcript.done` (0.2-0.4 s live), so audio of a quick resume went to a half-closed stream and was lost.
3. **xAI adapter: stale turn binding between turns.** The turn id was never released at the stop, so the stream opened
   on the silence after a turn was bound to the old turn; at the next VAD start it was parked (never sent `audio.done`,
   left open) and a new stream was dialed at speech start, losing the onset before the VAD start and adding a dial to
   barge-in latency. Fix for 2 and 3 (Deepgram's pattern): on `UserStoppedSpeaking` the stream is half-closed and
   detached and the turn released, so the next stream is unbound and adopted; another turn's stream is now finalized
   (`audio.done`) instead of parked; unbound candidates are held (no turn identity) and the drain bound is 2 s (the
   stream is detached, so the wait costs nothing). Tests `TestXAIAgent_MidUtterancePauseKeepsSecondHalf`,
   `TestXAIAgent_BargeInWhileBotSpeaks` (fail before the fix: onset lost, audio after `audio.done`, streams left open).

Harness: S2 evaluated the flush the instant the clip finished streaming, so a barge-in landing just after a short clip
read as "not flushed"; it now looks until `stop-max` + 1 s after the onset (the `audio stops <= stop-max` check is
unchanged). S4 counted RTVI `user-stopped-speaking` (raw VAD) as user turns; a 1.0 s pause is longer than the VAD stop
(25 x 32 ms), so that is 2 for every provider. It now counts committed (final) user transcriptions; VAD stops are
reported as `vad_stops`.

Live rerun (one traced run, `-trace -keep-work`, after the fixes, before the S4 count change): S2 **pass**; S4 heard the
whole sentence in one committed turn with one reply, and failed only the old VAD-stop count (2). Under the corrected
check that run's data is one user turn; S4 was not re-run to record it (call budget), so `agent-matrix.md` still shows
the run as `FAIL 2 turn`. Calls: the harness estimate was 15 (no clip synthesis: cached clips), but the estimate counts
one TTS request per reply while the unary xAI TTS makes one request per sentence unit: the trace shows 25 units and 3
LLM turns, so the real request count was about 30-35 (TTS is billed per character, so the spend is the reply text).

## Divergences from the review patterns (Deepgram STT, Cartesia TTS, `core/ttsunit`)

- STT: open policy is `OpenBounded` (matches); breaker gate fixed (above); same-turn rebind and idle close follow the Deepgram
  pattern (stage checks `turn_binding`, `barge_in_rebind`, `reuse_after_idle`, `resumed_speech` pass live); release at
  the VAD stop and adoption between turns did not (fixed in round 4, above; the stage suite feeds one clip per stream,
  so it could not see it). Open: `audio.done` is written immediately even when `transcript.created` has not arrived,
  so an utterance shorter than the handshake would be cut; rare now that a stream is pre-opened on the silence. The adapter still exports no breaker counters.
- TTS: unary endpoint, not streaming: the whole unit is synthesized, then chunked, so ttfa includes the full synthesis. The
  documented `wss://api.x.ai/v1/tts` (`text.delta` / `audio.delta`, ~24 kHz PCM) would lower time to first audio; not built.
  Error-before-final, final flags and interruption cancel match `ttsunit` (stage checks pass).
- LLM: summarization and tool loop unchanged; only the terminal event handling changed.

## Caveats

n = 1 per cell, one machine and network, shared with other work; xAI's endpointing (400 ms default, interims about every
second) is not tuned (`interim_results=true` only). The xAI docs do not say whether chunk finals are cumulative; the
live capture shows they are not. `grok-voice-transcribe-2.0` is the only STT model.
