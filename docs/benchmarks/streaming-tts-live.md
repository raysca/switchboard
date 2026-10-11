# Streaming TTS, live S9 (2026-10-11)

Scenario S9 (`tools/livecheck`, one long reply to "Tell me about the planet Mars.", turn manager off) against the real providers, server commit `4f1d69b`, one run per profile (n=1: an existence check, not a rate). Command:
`GOVOICE_LIVE=1 go run ./tools/livecheck -profiles google,xai -modes off -scenarios S9 -max-calls 60 -yes -trace`. 15 provider requests measured (estimate 36, cap 60).

Streaming was on by default: Google used `en-US-Chirp3-HD-Aoede` (StreamingSynthesize, one stream per sentence); xAI used the WebSocket (one connection per reply).

| profile | TTS requests | reply chars | sentences | first audio | TTS first byte | gaps >100 ms | audio |
|---|---|---|---|---|---|---|---|
| xai (streaming) | 1 | 1255 | 7 | 2.58 s | 0.66 s | 0 | 61.0 s |
| google (streaming) | 7 | 818 | 6 | 3.02 s | 0.22 s | 0 | 52.4 s |

Against the stored unary run (`tts-unit-sizing.md`, S9, mode off):

| profile | TTS requests | first audio | gaps >100 ms |
|---|---|---|---|
| xai (unary) | 12 | 4.07 s | 0 |
| google (unary) | 6 | 6.35 s | 5 (5.4 s total) |

- **xAI:** one request for the reply instead of 12, first audio 1.5 s sooner, still no gaps.
- **Google:** first audio 3.3 s sooner and the 5 gaps (5.4 s) are gone; TTS first byte 0.22 s against a 3.5 s mean unary. Requests are one stream per sentence (7 for 6 estimated sentences).
- Both replies played in full (audio span matches audio length), so Google's PCM stream was accepted as raw PCM with no audible header problem; the defensive WAV strip was not needed to explain anything here.
- Remaining first-audio time is the fixed cost before TTS (VAD, STT final, LLM first token), not synthesis.

Not covered by this run: a mid-stream failure and the unary fallback on live providers (covered offline), xAI idle timeout and in-band `error` behaviour, the Google 100-stream quota, and more than one run. A Google look-ahead is not needed on this evidence (no gaps).
