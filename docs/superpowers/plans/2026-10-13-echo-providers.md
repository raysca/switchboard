# Echo Providers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the `-tags fakeproviders` hack (fakes that silently take over `google.*`) and the `core.Fake*` doubles with a real, always-built **`echo`** provider in its own module, `providers/echo`, that behaves like a real provider to the pipeline, the server, the UI and the tests: same processors, same frames, same retries, breaker, usage, tracing, tool loop and settings, with the network replaced by an echo backend that imitates Google's measured timing, at no cost.

**Architecture:** The real Google processors already take injectable backends (`TTSConfig.Client`, `LLMConfig.Client`, `STTConfig.StreamOpener`); the registry today builds real clients and passes them in. `providers/echo` ships echo **backends** that implement those same interfaces (TTS unary and streaming client, LLM stream client with native tool calls, STT stream opener). The `echo.*` factories build the **real** Google processors around them, so everything between the pipeline and the wire is the real code. The processors get a `Provider` label so frames, traces and usage say `echo`, never `google`. A keyless catalog provider `echo` mirrors Google's models, voices and languages; a `providers.echo` config block picks the mimic profile and test knobs. The build tag, `registry_fake.go`, every `GOVOICE_FAKE_*` variable and the public `core.Fake{LLM,TTS,STT}` doubles go away; every harness uses `echo` on the normal binary.

**Tech Stack:** Go (new module `providers/echo`; core, providers/google, server, tools, transport), YAML catalog, Playwright e2e (TypeScript), shell harnesses.

**Base:** reconciled with `main` at c6e858a (one-port server, end_call tool, transfer notes, dentistry demo, Cloud Run deploy). Echo must carry the new `end_call` phrase, and the new one-port and dentistry e2e stacks move to echo with the rest.

**Spec:** Decisions taken with the user in this session (no separate spec file): echo is **always built and always detected** (no env gate, no tag); knobs live in **server config plus catalog presets**; the build tag is **replaced everywhere**; echo **mimics Google with all its settings, as realistically as possible**; it is **its own provider module that behaves identically to the real ones**, so tests and the UI cannot tell the difference; the core fake doubles are **retired in the same plan**. `tools/provtest` (fault-injection fakes for resilience tests) is out of scope and stays. Measured Google behaviour comes from `docs/benchmarks/first-audio-latency.md` and `docs/benchmarks/agent-matrix.md`. Project rules: `AGENTS.md` (core agnostic of server concerns, every feature consumed by the server, bounded queues, supervised goroutines, `go test -race`).

## What echo imitates (from the repo's own measurements)

- **STT `chirp_2`:** no interim results; the final arrives about 1.2 s after the caller stops (the processor half-closes the stream at `UserStopped`; echo answers after that delay). **STT `latest_long`:** interim results, final about 0.5 s after the stop.
- **LLM:** first word about 0.7 s after the user turn, then word-sized deltas; usage token counts on the closing chunk; tool calls through the real tool loop.
- **TTS:** `Chirp3-HD` voices stream (`StreamSynthesize`, first audio about 0.4 s); every other voice is unary (`SynthesizeSpeech`: nothing plays until the whole unit is synthesized, about 0.6 s plus 50 ms per character). The server picks streaming or unary exactly as for Google (`IsChirp3HD` and `providers.google_tts.disable_streaming`). Audio is as long as the text takes to say (15 characters per second) at the configured `providers.google_tts.sample_rate`.
- **Transcripts of real microphone audio:** echo cannot recognise speech. In `audio` mode it counts voiced frames and emits placeholder words ("hello") at a speech-like rate (one per 20 frames, 0.4 s); typed turns bypass STT as always. It is a stand-in, not recognition.
- **`instant` profile:** every delay zero, one placeholder word per audio frame: the old fake behaviour, so the framework-overhead baselines stay comparable.

## Global Constraints

- `core/` imports nothing from `server/` or any provider; `providers/echo` imports `core` and `providers/google` only. Nothing in `core` knows about echo.
- Echo implements the **same interfaces** as the real backends and never bypasses the real processors: no echo-specific processor, no special-casing in the pipeline, the server or the UI beyond the registry factories and the catalog entry.
- Every echo option is consumed by the server's echo factories (YAGNI). Bounded everything: backend channels are bounded and closed only by their producer goroutine; every goroutine selects on `ctx.Done()`; no audio retained.
- The provider id is `echo`, display name `Echo (simulated)`; it has no credential kinds, sends and reads no secret, and no frame, trace or metric ever labels it `google`.
- `go test -race ./...` clean for every touched module (`core`, `providers/google`, `providers/echo`, `server`, root); `gofmt`; conventional commits ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- After the migration no file outside `docs/superpowers/` mentions `fakeproviders` or `GOVOICE_FAKE_`; `make verify`, `make test-e2e-go`, `make e2e-fake`, `make live-check-selftest`, `make perf-gate-selftest` and `make eval-pipecat` pass.
- Defaults: `providers.echo.mimic: google`, `stt: text`, `audible: false`, `label_replies: false`. Harness configs say `mimic: instant` explicitly.

## Review Focus

1. **Google must be unchanged.** With real keys and no echo config, every google flow behaves exactly as before; the new `Provider` label defaults to `"google"`; the provider list shows both providers; no `google.*` factory is replaced or wrapped.
2. **No leaks, no unbounded growth.** Echo backends under interruption, hang-up and stream rotation: producer goroutines end, channels are closed once by the producer, audio is not buffered beyond a unit, `go test -race` clean; a long `audio`-mode call grows no memory.
3. **Identical behaviour.** Echo calls pass through the same retry, breaker, error classification, usage frames and tracing as Google (contract tests run the real processors against the echo backends); `chirp_2` has no interims and `latest_long` has; Chirp3-HD streams and other voices are unary; the first-byte delay grows with the unit's text for unary voices; an unknown or custom model or voice falls back to Google's figures; `brokenvoice` still fails to build.
4. **Config validation and defaults.** An unknown `mimic` or `stt` value is a startup error naming the field and the allowed values; an absent block means the documented defaults; `llm_extra_delay` must not be negative.
5. **Harness regressions.** The perf baselines (framework overhead) must not move beyond the gate's tolerances (if the benchmark's process changes because it now runs real processors, say so and re-baseline deliberately, never silently); the self-test's injected delay must still fail the gate; the cue test must still see silence for speech; the browser suite's "only the suite's provider is available" assertion now sees `echo`.

---

### Task 1: A `Provider` label on the Google processors

**Files:**
- Modify: `providers/google/stt.go` (`STTConfig`, lines using `"google"` at about 283 and 883), `providers/google/llm.go` (`LLMConfig`, about 410 and 640), `providers/google/tts.go` (`TTSConfig`, `normalizeTTSConfig`, about 358 and 539), `providers/google/tts_streaming.go` (about 46)
- Test: `providers/google/provider_label_test.go` (create)

**Interfaces:**
- Produces: `Provider string` on `STTConfig`, `LLMConfig` and `TTSConfig`; empty means `"google"`. Used for the transcript frame's `Provider`, the usage record's `Provider`, the `ttsunit.Config.Provider` and the `"provider"` trace attribute. The `netpool` counters in the real clients keep `"google"` (they only run for the real clients).

- [ ] **Step 1: Write the failing test** — `provider_label_test.go`. Reuse the harness of `tts_streaming_test.go` (streaming TTS usage), `llm_usage_test.go` and `stt_test.go`; for each stage build the processor with `Provider: "echo"` and a mock client and assert the label on what the stage emits:

```go
func TestProviderLabelDefaultsToGoogle(t *testing.T)   // no Provider set: TranscriptCandidateFrame.Provider == "google", LLM and TTS usage Provider == "google"
func TestProviderLabelFollowsConfigSTT(t *testing.T)    // Provider "echo": every TranscriptCandidateFrame.Provider == "echo"
func TestProviderLabelFollowsConfigLLM(t *testing.T)    // the usage record the LLM reports carries Provider "echo"
func TestProviderLabelFollowsConfigTTSUnary(t *testing.T)
func TestProviderLabelFollowsConfigTTSStreaming(t *testing.T) // ttsunit usage Provider "echo"
```

- [ ] **Step 2: Run to see them fail**

Run: `cd providers/google && go test -race -run 'TestProviderLabel' .`
Expected: FAIL (no field).

- [ ] **Step 3: Implement.** Add `Provider string // label on frames, usage and traces; default "google"` to the three configs. In each constructor (`NewSTTProcessor`, `NewLLMProcessor`, `normalizeTTSConfig`) set `if cfg.Provider == "" { cfg.Provider = "google" }`, and replace the seven literals with `p.cfg.Provider` / `cfg.Provider`.

- [ ] **Step 4: Run to see them pass, then the module**

Run: `cd providers/google && go test -race ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add providers/google
git commit -m "feat(google): a Provider label on the STT, LLM and TTS processors, so another provider can reuse them honestly

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: The `providers/echo` module — figures and the TTS backend

**Files:**
- Create: `providers/echo/go.mod`, `providers/echo/figures.go`, `providers/echo/tts.go`, `providers/echo/tts_test.go`
- Modify: `go.work` (add `./providers/echo`), root `go.mod` is unchanged until Task 5 wires the server

**Interfaces:**
- Produces:

```go
type Mimic string
const (MimicGoogle Mimic = "google"; MimicInstant Mimic = "instant")
func (m Mimic) Valid() bool
type STTFigures struct{ Interims bool; FinalDelay time.Duration; FramesPerWord, VoicedThreshold int }
type LLMFigures struct{ FirstToken, TokenDelay time.Duration }
type TTSFigures struct{ FirstByte, FirstBytePerChar time.Duration; CharsPerSecond int }
func (m Mimic) STT(model string) STTFigures
func (m Mimic) LLM() LLMFigures
func (m Mimic) TTS(voice string) TTSFigures

type TTSClient struct{ /* unexported */ }
func NewTTSClient(f TTSFigures, audible bool) *TTSClient
// implements google.StreamingTTSClient:
func (c *TTSClient) SynthesizeSpeech(ctx context.Context, text, voice string, sampleRate int) ([]byte, error)
func (c *TTSClient) StreamSynthesize(ctx context.Context, pieces []string, voice string, sampleRate int, onAudio func([]byte) error) error
```

- Figures (the numbers are the repo's measurements; tune only with a benchmark in hand). `MimicGoogle`: STT `chirp_2` (also the fallback for any other model): `FinalDelay 1200ms, FramesPerWord 20, VoicedThreshold 500`; `latest_long`: `Interims true, FinalDelay 500ms`, same rest; LLM `FirstToken 700ms, TokenDelay 30ms`; TTS: voice containing `chirp3-hd` (case-insensitive) or empty: `FirstByte 400ms`; any other voice `FirstByte 600ms, FirstBytePerChar 50ms`; `CharsPerSecond 15` for both. `MimicInstant`: STT `FramesPerWord 1, VoicedThreshold 0`, everything else zero, TTS `CharsPerSecond 0` (a unit is two 20 ms frames).
- PCM: `pcm(text, rate, cps, audible)` returns PCM16LE mono: `samples = ceil(runes / cps × rate)` (`2 × rate/50` when `cps` is 0), silent, or a quiet (amplitude 3000) 220 Hz tone when `audible`. The streaming client delivers it in 20 ms chunks (`rate/50` samples), the unary client in one slice.

- [ ] **Step 1: Module skeleton.** `providers/echo/go.mod`: `module govoice/providers/echo`, `go 1.27.1`, `require govoice/core v0.0.0`, `require govoice/providers/google v0.0.0-00010101000000-000000000000`, `replace govoice/core => ../../core`, `replace govoice/providers/google => ../google` (copy the exact version stamps and the indirect requirements from `providers/google/go.mod` by running `go mod tidy` inside the workspace); add `./providers/echo` to `go.work`.

- [ ] **Step 2: Write the failing tests** — `figures_test.go` and `tts_test.go`:

```go
func TestGoogleFiguresFollowTheMeasurements(t *testing.T)   // chirp_2/unknown: 1200ms, no interims; latest_long: interims, 500ms; Chirp3-HD voice: 400ms first byte, no per-char; Journey voice: 600ms + 50ms/char; instant: all zero
func TestMimicValid(t *testing.T)                           // google, instant true; "", "x" false
func TestPCMLengthFollowsText(t *testing.T)                 // 15 cps, 24000 Hz, 30 runes -> 2 s = 48000 samples; instant: 2 frames; empty text -> at least one frame
func TestPCMToneIsQuietAndAudible(t *testing.T)             // audible: some sample != 0, peak <= 3000; silent: all zero
func TestUnaryTTSWaitsThenReturnsAllAudio(t *testing.T)     // FirstByte 40ms + 1ms/char: returns after >=40ms+len; honours ctx cancel (returns ctx.Err())
func TestStreamingTTSDeliversTwentyMillisecondChunks(t *testing.T) // onAudio sees chunks of rate/50 samples; stops and returns onAudio's error unchanged; ctx cancel stops it
func TestRealStreamingProcessorSpeaksThroughEchoClient(t *testing.T)  // google.NewStreamingTTSProcessor(cfg{Client: echoClient, Voice: Chirp3-HD...}) in a core.Session: a SynthesisUnitFrame yields OutputAudioFrames ending in IsFinal, Provider "echo" in the usage; copy the session harness of providers/google/tts_streaming_test.go
func TestRealUnaryProcessorSpeaksThroughEchoClient(t *testing.T)      // same with google.NewTTSProcessor and a non-Chirp voice; units over 250 runes are split by the real processor into several client calls
```

- [ ] **Step 3: Run to see them fail**

Run: `cd providers/echo && go test -race ./... 2>&1 | tail`
Expected: FAIL (nothing implemented).

- [ ] **Step 4: Implement** `figures.go` (the types and methods above, plus `Valid`) and `tts.go`:

```go
package echo

import (
	"context"
	"math"
	"time"
)

// TTSClient is a simulated Google Text-to-Speech backend: the real google TTS processors run
// against it (google.StreamingTTSClient), so splitting, retries, pacing, usage and tracing are the real ones.
type TTSClient struct {
	f       TTSFigures
	audible bool
}

func NewTTSClient(f TTSFigures, audible bool) *TTSClient { return &TTSClient{f: f, audible: audible} }

func (c *TTSClient) delay(text string) time.Duration {
	return c.f.FirstByte + time.Duration(len([]rune(text)))*c.f.FirstBytePerChar
}

// wait sleeps d unless ctx ends first.
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *TTSClient) SynthesizeSpeech(ctx context.Context, text, _ string, sampleRate int) ([]byte, error) {
	if err := wait(ctx, c.delay(text)); err != nil {
		return nil, err
	}
	return pcm(text, sampleRate, c.f.CharsPerSecond, c.audible), nil
}

func (c *TTSClient) StreamSynthesize(ctx context.Context, pieces []string, _ string, sampleRate int, onAudio func([]byte) error) error {
	chunk := sampleRate / 50 * 2 // 20 ms of PCM16 mono
	for i, piece := range pieces {
		if i == 0 {
			if err := wait(ctx, c.delay(piece)); err != nil {
				return err
			}
		}
		audio := pcm(piece, sampleRate, c.f.CharsPerSecond, c.audible)
		for len(audio) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := min(chunk, len(audio))
			if err := onAudio(audio[:n]); err != nil {
				return err
			}
			audio = audio[n:]
		}
	}
	return nil
}

// pcm is PCM16LE mono as long as the text takes to say: silence, or a quiet 220 Hz tone.
func pcm(text string, rate, charsPerSecond int, audible bool) []byte {
	samples := rate / 50 * 2
	if charsPerSecond > 0 {
		samples = max((len([]rune(text))*rate+charsPerSecond-1)/charsPerSecond, rate/50)
	}
	out := make([]byte, samples*2)
	if audible {
		for i := 0; i < samples; i++ {
			v := int16(3000 * math.Sin(2*math.Pi*220*float64(i)/float64(rate)))
			out[2*i], out[2*i+1] = byte(v), byte(v>>8)
		}
	}
	return out
}
```

(Round the sample count up to a whole 20 ms frame if the real processor requires frame multiples: check `providers/google/tts.go` where it slices audio into `ChunkDuration` pieces and keep the helper consistent with it.)

- [ ] **Step 5: Run to see them pass**

Run: `cd providers/echo && go test -race ./... 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add providers/echo go.work
git commit -m "feat(echo): the providers/echo module — Google-shaped timing figures and a simulated TTS backend the real Google processors run against

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: The echo LLM backend

**Files:**
- Create: `providers/echo/llm.go`, `providers/echo/llm_test.go`

**Interfaces:**
- Consumes: `google.LLMStreamClient`, `google.LLMNativeToolStreamClient`, `google.LLMStreamChunk`, `core.Message`, `core.ToolCall`, `core.ToolDefinition`.
- Produces:

```go
type LLMOptions struct {
	Figures    LLMFigures
	ExtraDelay time.Duration     // added to every word (the perf-gate self-test)
	Label      string            // prefixes every reply, e.g. "[front desk] "
	Phrases    map[string]string // lower-case phrase -> tool name ("transfer to sales" -> transfer_to_sales)
	Spoken     map[string]string // tool name -> the name its reply uses; these tools take {json} args from the message and say what came back
}
func NewLLMClient(o LLMOptions) *LLMClient
func (c *LLMClient) GenerateStream(ctx, model, systemPrompt string, history []core.Message) (<-chan google.LLMStreamChunk, <-chan error, error)
func (c *LLMClient) GenerateStreamWithNativeTools(ctx, model, systemPrompt string, history []core.Message, native []google.GoogleNativeExchange, defs []core.ToolDefinition, identity core.ToolCall) (<-chan google.LLMStreamChunk, <-chan error, error)
```

Behaviour (the old fake's, now as a model behind the real processor): the last user message decides. If its lower-cased text contains a phrase of `Phrases` (first in sorted order) and `defs` is non-empty, the response is one chunk with `ToolCalls: [{ID: "echo-call-<n>", Name: tool, Arguments: args}]` where `args` is the first balanced JSON object in the message for `Spoken` tools and `{}` otherwise; the processor's tool loop then runs the tool and calls the client again. When the last history message is a tool result (`Kind == core.MessageToolResult`), the reply is `"Connecting you."` (or `"Tool <name> returned <content>"` / `"Tool <name> failed: <status or category>"` for tools in `Spoken`). Otherwise the reply is `Label + "You said: " + user text`, one chunk per word (spaces kept), `FirstToken` before the first and `TokenDelay + ExtraDelay` between words. The closing chunk has `IsDone`, `FinishReason: "STOP"` and token counts (`PromptTokens` = words in the history, `OutputTokens` = words in the reply).

- [ ] **Step 1: Write the failing tests** — `llm_test.go`:

```go
func TestEchoReplyRepeatsTheCallerWordByWord(t *testing.T)     // "hi there" -> deltas "You ", "said: ", "hi ", "there"; concatenation "You said: hi there"; last chunk IsDone with OutputTokens 4
func TestEchoLabelPrefixesTheReply(t *testing.T)
func TestEchoFirstTokenAndWordDelays(t *testing.T)             // FirstToken 50ms, TokenDelay 10ms, ExtraDelay 5ms: first chunk >=50ms, next gaps >=15ms
func TestEchoStopsOnContextCancel(t *testing.T)                // cancel mid-reply: chunk channel closes, no goroutine left, error channel closed
func TestEchoCallsTheToolNamedByAPhrase(t *testing.T)          // "please transfer to sales" with defs -> one ToolCalls chunk, Name transfer_to_sales, Arguments {}
func TestEchoPassesJSONArgumentsToSpokenTools(t *testing.T)    // `call tool lookup_order {"id":"7"}` -> Arguments {"id":"7"}; malformed JSON -> {}
func TestEchoAnswersAfterToolResults(t *testing.T)             // history ending in a tool_result message -> "Connecting you."; spoken tool success -> "Tool lookup_order returned {…}"; failed status -> "Tool lookup_order failed: <category>"
func TestEchoIgnoresPhrasesWithoutToolDefinitions(t *testing.T) // GenerateStream (no defs) never emits ToolCalls
func TestRealLLMProcessorRunsTheToolLoopAgainstEcho(t *testing.T) // google.NewLLMProcessor with the echo client, ToolDefinitions + a fake core.ToolExecutor + ToolHistory: "transfer to sales" executes the tool once and the turn ends with "Connecting you."; usage Provider "echo"; copy the harness of providers/google/llm_test.go
```

- [ ] **Step 2: Run to see them fail.** `cd providers/echo && go test -race -run 'Echo|RealLLM' .` — Expected: FAIL.

- [ ] **Step 3: Implement** `llm.go`. Port `toolArguments` (first balanced `{...}` that is a JSON object, else `{}`) and `toolFor` (sorted phrases) unchanged from `core/fakes.go`; port `toolReply`'s wording using `core.Message.ToolStatus`/`ToolErrorCategory`/`Content` from the tool-result message. Structure:

```go
func (c *LLMClient) stream(ctx context.Context, history []core.Message, hasTools bool) (<-chan google.LLMStreamChunk, <-chan error, error) {
	chunks := make(chan google.LLMStreamChunk, 8)
	errs := make(chan error, 1)
	go func() {
		defer close(chunks)
		defer close(errs)
		// decide: tool call / tool answer / echo; send each chunk with a select on ctx.Done()
	}()
	return chunks, errs, nil
}
```

`send := func(ch google.LLMStreamChunk) bool { select { case chunks <- ch: return true; case <-ctx.Done(): return false } }`; delays use the `wait` helper of Task 2. Call IDs come from an `atomic.Uint64` counter on the client.

- [ ] **Step 4: Run to see them pass.** `cd providers/echo && go test -race ./... 2>&1 | tail -5` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add providers/echo
git commit -m "feat(echo): a simulated LLM backend with native tool calls, run by the real Google LLM processor and its tool loop

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: The echo STT backend

**Files:**
- Create: `providers/echo/stt.go`, `providers/echo/stt_test.go`

**Interfaces:**
- Consumes: `google.STTStreamOpener`, `google.STTStream`, `google.STTResult`, `google.STTConfig`.
- Produces: `func NewSTTOpener(f STTFigures, audioMode bool) *STTOpener` implementing `OpenStream(ctx, cfg google.STTConfig) (google.STTStream, error)`.

Behaviour: one stream per utterance (as the real processor opens them). `Send(audio)` counts voiced 20 ms frames: a frame is voiced when its peak sample magnitude is at least `VoicedThreshold` (0 = every frame is voiced); one placeholder word `"hello"` is added per `FramesPerWord` voiced frames, and with `Interims` each new word queues a non-final cumulative result (`Transcript`, `Stability 0.9`, `ResultEndOffset = frames × 20 ms`). `CloseSend()` ends the audio: `Recv()` then waits `FinalDelay` and returns one final result with all the words (`IsFinal true`, `Stability 1`) if there are any, then `io.EOF`. In text mode (`audioMode` false) the stream discards audio and `Recv()` returns `io.EOF` after `CloseSend()`: typed turns never produce transcripts. `Close()` and ctx cancel make `Recv()` return `ctx.Err()`/`io.EOF` promptly. The result queue is bounded (capacity 64; interims beyond it are dropped, finals never); no audio is retained.

- [ ] **Step 1: Write the failing tests** — `stt_test.go`:

```go
func TestSTTCountsVoicedFramesIntoWords(t *testing.T)        // FramesPerWord 4, threshold 500: 8 loud + 8 silent frames -> 2 words
func TestSTTInstantCountsEveryFrame(t *testing.T)            // threshold 0, FramesPerWord 1
func TestSTTInterimsAreCumulativeWithMediaOffsets(t *testing.T)
func TestSTTNoInterimsOnlyAFinalAfterCloseSend(t *testing.T) // FinalDelay 60ms: the final arrives >=60ms after CloseSend, then io.EOF
func TestSTTTextModeNeverTranscribes(t *testing.T)
func TestSTTRecvReturnsOnCancelAndClose(t *testing.T)
func TestSTTQueueStaysBounded(t *testing.T)                   // 10,000 voiced frames with Interims: Send never blocks, the queue never exceeds capacity
func TestRealSTTProcessorTranscribesThroughEcho(t *testing.T) // google.NewSTTProcessor with the opener: UserStarted, audio frames, UserStopped -> a final TranscriptCandidateFrame with Provider "echo" and "hello hello" text after FinalDelay; copy the harness of providers/google/stt_test.go
```

- [ ] **Step 2: Run to see them fail.** `cd providers/echo && go test -race -run 'STT' .` — Expected: FAIL.

- [ ] **Step 3: Implement** `stt.go` with a mutex-guarded stream struct, a bounded `results chan *google.STTResult` (cap 64) and a `done chan struct{}` closed once (`sync.Once`) by `CloseSend` (to start the final countdown inside `Recv`, not in a goroutine: `Recv` blocks on `select { case r := <-results; case <-done...; case <-ctx.Done() }`, so no goroutine exists to leak). `Send` computes frames as `len(audio) / (2*rate/50)` using `cfg.SampleRate` if present on `STTConfig`, else 16000 (read the real struct); peak detection loops over the PCM16 samples.

- [ ] **Step 4: Run to see them pass.** `cd providers/echo && go test -race ./... 2>&1 | tail -5` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add providers/echo
git commit -m "feat(echo): a simulated STT stream, run by the real Google STT processor

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 5: The `echo` catalog provider (keyless)

**Files:**
- Modify: `server/catalog/catalog.go`, `server/catalog/resolver.go`
- Create: `server/catalog/data/echo.yaml`
- Test: `server/catalog/catalog_test.go`, `server/catalog/resolver_test.go`, `server/api/providers_test.go`

**Interfaces:**
- Produces: a service may declare `keyless: true` (`Service.Keyless bool`, `yaml:"keyless"`) instead of a `credential` list. `Resolver.Status` returns `Declared && Detected` for it; `Resolver.Secret` returns `(nil, nil)`; `Resolver.Env("echo")` is empty. `echo.yaml` lists the same models, voices and languages as `google.yaml`.

- [ ] **Step 1: Write the failing tests.** `catalog_test.go`: `TestEchoMirrorsGoogleCatalog` (every service of google exists on echo with `reflect.DeepEqual` models, voices, languages: "update server/catalog/data/echo.yaml"); `TestKeylessServiceNeedsNoCredential` (a keyless service without credentials parses; neither keyless nor credential is the existing error; keyless plus credentials is an error "keyless service %q cannot name credentials"). `resolver_test.go`: `TestKeylessProviderIsAlwaysDetected` (empty environment: `Detected("echo")`, `Status("echo","tts")` Declared+Detected, `Secret` nil/nil, `Env("echo")` empty). `api/providers_test.go`: with the real catalog and a registry that supports `echo.*`, `GET /providers` lists `echo` with `detected: true`, `env: []`, `display_name: "Echo (simulated)"` and all three services `available`.
- [ ] **Step 2: Run to see them fail.** `cd server && go test -race ./catalog/ ./api/ -run 'Echo|Keyless' 2>&1 | tail` — Expected: FAIL.
- [ ] **Step 3: Implement.** `catalog.go`: add `Keyless bool \`yaml:"keyless"\`` to `Service`; in `validate`, `if len(svc.Credential) == 0 && !svc.Keyless && !overlay` is the credential error, and `if svc.Keyless && len(svc.Credential) > 0` the new one. `resolver.go`: at the top of `Status` and `Secret`, after the service lookup, `if svc.Keyless { return Status{Declared: true, Detected: true} }` and `{ return nil, nil }`. `data/echo.yaml`: `id: echo`, `name: Echo (simulated)`, no `credentials`, and for `llm`, `stt`, `tts` a `keyless: true` service with the **full** entry lists copied from `google.yaml`.
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race ./catalog/ ./api/ 2>&1 | tail -5` — Expected: PASS (an API test that needs the `echo.*` factories passes after Task 6; note it and continue).
- [ ] **Step 5: Commit**

```bash
git add server/catalog server/api
git commit -m "feat(server): keyless catalog services and the echo provider, mirroring Google's models, voices and languages

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Register echo on every build; delete the build tag

**Files:**
- Create: `server/registry_echo.go`, `server/registry_echo_test.go`
- Modify: `server/config.go` (`Providers.Echo`), `server/config_defaults.go`, `server/config_validate.go`, `server/registry.go` (breakers, drop the hook), `server/config_test.go`, `server/go.mod` (require `govoice/providers/echo` with a replace), `go.mod` (root; whatever `go work` needs)
- Delete: `server/registry_fake.go`, `server/registry_fake_test.go`

**Interfaces:**
- Produces the config block:

```go
		Echo struct {
			Mimic         string        `yaml:"mimic"`           // google | instant
			STT           string        `yaml:"stt"`             // text | audio
			Audible       bool          `yaml:"audible"`         // a quiet tone instead of silence
			LabelReplies  bool          `yaml:"label_replies"`   // prefix replies with the step's prompt
			LLMExtraDelay time.Duration `yaml:"llm_extra_delay"` // added to every reply word (perf-gate self-test)
		} `yaml:"echo"`
```

- Defaults `mimic: google`, `stt: text`. Validation errors name the field: `providers.echo.mimic: "x" must be one of google, instant`; `providers.echo.stt: "y" must be one of text, audio`; `providers.echo.llm_extra_delay must not be negative`.
- `registerEcho(r *Registry)` registers `echo.stt`, `echo.llm`, `echo.tts`, built from the same pieces as the Google factories with the echo backends; `NewRegistry` calls it unconditionally; the registry gets `echoSTTBreaker`, `echoLLMBreaker`, `echoTTSBreaker`.

- [ ] **Step 1: Write the failing tests** — `server/registry_echo_test.go`:

```go
func TestEchoIsRegisteredOnEveryBuild(t *testing.T)            // NewRegistry().Supports("echo", stage) for stt/llm/tts
func TestGoogleFactoriesAreUntouched(t *testing.T)             // google.* still need credentials: building google.tts with an empty secret errors as before
func TestEchoTTSChoosesStreamingLikeGoogle(t *testing.T)       // Chirp3-HD voice -> *google.StreamingTTSProcessor; other voice -> *google.TTSProcessor; providers.google_tts.disable_streaming -> unary
func TestEchoTTSUsesTheGoogleSampleRate(t *testing.T)          // providers.google_tts.sample_rate 16000 -> frames at 16000
func TestEchoBrokenVoiceFailsToBuild(t *testing.T)             // "brokenvoice" -> error, as the old fake
func TestEchoLLMWiresTransferAndMCPPhrases(t *testing.T)       // spec.Tools with transfer_to_sales, end_call and an mcp__ tool: phrases "transfer to sales", "end the call" (-> endCallName), "call tool lookup_order" (the old TestFakeLLMPhrasesForMCPTools, untagged)
func TestEchoFactoriesLabelEverythingEcho(t *testing.T)        // a built processor's frames report Provider "echo"
func TestEchoMimicSelectsFigures(t *testing.T)                 // mimic instant: figures all zero; google: chirp_2 final delay 1200ms
func TestEchoConfigDefaultsAndValidation(t *testing.T)         // defaults google/text; mimic "x", stt "y", negative extra delay are errors naming the field
```

- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -run 'TestEcho|TestGoogleFactories' . 2>&1 | tail` — Expected: FAIL.

- [ ] **Step 3: Implement.** `registry_echo.go` (no build tag):

```go
package server

import (
	"errors"
	"strings"

	"govoice/core"
	"govoice/providers/echo"
	"govoice/providers/google"
)

func registerEcho(r *Registry) {
	r.Register("echo.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		m := echo.Mimic(cfg.Providers.Echo.Mimic)
		sttCfg := google.DefaultSTTConfig()
		sttCfg.Provider = "echo"
		if spec.Model != "" {
			sttCfg.Model = spec.Model
		}
		if spec.Language != "" {
			sttCfg.LanguageCode = spec.Language
		}
		sttCfg.CircuitBreaker = r.echoSTTBreaker
		if spec.Failover {
			sttCfg.RetryPolicy = failoverPolicy("stt", sttCfg.RetryPolicy)
		}
		sttCfg.StreamOpener = echo.NewSTTOpener(m.STT(sttCfg.Model), cfg.Providers.Echo.STT == "audio")
		return google.NewSTTProcessor(sttCfg)
	})

	r.Register("echo.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		o := echo.LLMOptions{Figures: echo.Mimic(cfg.Providers.Echo.Mimic).LLM(), ExtraDelay: cfg.Providers.Echo.LLMExtraDelay,
			Phrases: map[string]string{}}
		if cfg.Providers.Echo.LabelReplies {
			o.Label = "[" + spec.SystemPrompt + "] "
		}
		for _, d := range spec.Tools {
			if step, ok := strings.CutPrefix(d.Name, "transfer_to_"); ok {
				o.Phrases["transfer to "+strings.ReplaceAll(step, "_", " ")] = d.Name
			} else if d.Name == endCallName {
				o.Phrases["end the call"] = endCallName // a step with the end_call tool hangs up after its goodbye
			}
		}
		phrases, spoken := mcpToolPhrases(spec.Tools)
		for p, name := range phrases {
			o.Phrases[p] = name
		}
		o.Spoken = spoken
		llmCfg := google.DefaultLLMConfig()
		llmCfg.Provider, llmCfg.Model, llmCfg.SystemPrompt = "echo", spec.Model, spec.SystemPrompt
		if len(spec.Tools) > 0 {
			llmCfg.ToolDefinitions, llmCfg.ToolExecutor, llmCfg.ToolHistory = spec.Tools, spec.ToolExecutor, spec.History
			llmCfg.ToolLoopConfig = spec.toolLoopConfig()
		}
		llmCfg.Client = echo.NewLLMClient(o)
		llmCfg.CircuitBreaker = r.echoLLMBreaker
		return google.NewLLMProcessor(llmCfg)
	})

	r.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		// A step whose voice is "brokenvoice" cannot be built: the e2e suites use it to make a handoff fail.
		if spec.Voice == "brokenvoice" {
			return nil, errors.New("echo tts: voice brokenvoice is unavailable")
		}
		ttsCfg := google.DefaultTTSConfig()
		ttsCfg.Provider = "echo"
		if spec.Voice != "" {
			ttsCfg.Voice = spec.Voice
		}
		if cfg.Providers.GoogleTTS.SampleRate > 0 {
			ttsCfg.SampleRate = cfg.Providers.GoogleTTS.SampleRate
		}
		ttsCfg.Client = echo.NewTTSClient(echo.Mimic(cfg.Providers.Echo.Mimic).TTS(ttsCfg.Voice), cfg.Providers.Echo.Audible)
		ttsCfg.CircuitBreaker = r.echoTTSBreaker
		if spec.Failover {
			ttsCfg.RetryPolicy = failoverPolicy("tts", ttsCfg.RetryPolicy)
		}
		if google.IsChirp3HD(ttsCfg.Voice) && !cfg.Providers.GoogleTTS.DisableStreaming {
			return google.NewStreamingTTSProcessor(ttsCfg)
		}
		return google.NewTTSProcessor(ttsCfg)
	})
}
```

Move `mcpToolPhrases` (unchanged) from `registry_fake.go` to the bottom of this file. In `registry.go` add the three breakers next to the others (`resilience.NewBreaker(resilience.DefaultConfig())`) and replace the `registerFakeProviders` hook with `registerEcho(r)`; delete the `registerFakeProviders` var. Delete `registry_fake.go` and `registry_fake_test.go` (its test moves into `registry_echo_test.go` as the phrases test above). Add the `Echo` struct to `config.go` (after `OpenAILLM`), defaults to `config_defaults.go`, validation to `config_validate.go` using the file's existing error idiom. Wire the module: `server/go.mod` requires `govoice/providers/echo` with `replace govoice/providers/echo => ../providers/echo` (mirror how it requires `providers/google`), then `go mod tidy` for server and root.

- [ ] **Step 4: Run to see them pass, then the module.** `cd server && go test -race ./... 2>&1 | tail -15; gofmt -l .` — Expected: PASS (tests still carrying `fakeproviders` are skipped until Task 7).
- [ ] **Step 5: Commit**

```bash
git add server providers go.work go.mod go.sum
git commit -m "feat(server): the echo provider is registered on every build and runs the real Google processors; the fakeproviders tag and registry_fake.go go away

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 7: The Go end-to-end tests run on the normal build against `echo`

**Files:**
- Modify: `server/flow_e2e_test.go`, `server/mcp_call_e2e_test.go`, `server/secret_leak_test.go`, `server/context_injection_e2e_test.go`, `Makefile` (`test-e2e-go` and any `-tags ... fakeproviders` use)

- [ ] **Step 1: Migrate `flow_e2e_test.go` and watch it fail for the right reason.** Delete its `//go:build fakeproviders` line; replace `t.Setenv("GOVOICE_FAKE_LLM_LABEL", "1")` with `cfg.Providers.Echo.Mimic = "instant"` and `cfg.Providers.Echo.LabelReplies = true` on the test's `Config`; change every `"google."+stage` probe key and every `"provider":"google"` stage to `echo`; drop the placeholder `GOOGLE_*` environment; update the comments. Run `cd server && go test -race -count=1 -run FlowE2E .`. A failure that is not about naming is a real fidelity difference between echo and the old fake (the real processors are stricter: units, retries, stage fencing): investigate and fix echo, never loosen the test.
- [ ] **Step 2: Migrate the other three** the same way. `secret_leak_test.go` keeps its XAI and Google sentinel secrets in the environment and its intent (no sentinel reaches a log or response); only the stages that were google fakes become echo. `context_injection_e2e_test.go`: drop the tag, use echo.
- [ ] **Step 3: Makefile.** `test-e2e-go` becomes `go test -race -count=1 -tags "$(GOTAGS)" -run '<the same names, plus the context-injection e2e test names>' .` with the comment "against the echo provider"; reword the other `fakeproviders` comments (the targets change in Task 8).
- [ ] **Step 4: Run.** `make test-e2e-go` — Expected: PASS with the same test names as before.
- [ ] **Step 5: Commit**

```bash
git add server Makefile
git commit -m "test(server): the Go end-to-end suite runs on the normal build against the echo provider

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Every harness uses echo

**Files:**
- Modify: `tools/loadtest/server.go`, `tools/loadtest/main.go`, `tools/loadtest/loadtest_test.go`, `tools/livecheck/server.go`, `tools/livecheck/main.go`, `tools/livecheck/profiles.yaml`, `tools/livecheck/profiles.go`, `tools/livecheck/profiles_test.go`, `tools/eval/pipecat/run.sh`, `scripts/perf-gate.sh`, `deploy/docker-compose.perf.yaml`, `config/perf.yaml`, `e2e/global-setup-flows.ts`, `e2e/global-setup-oneport.ts`, `e2e/lib/flows-stack.ts`, `e2e/lib/flows-helpers.ts`, `e2e/lib/dental-helpers.ts`, `e2e/tests-flows/*.spec.ts` (comments, the broken-voice name, and `60-dental.spec.ts` where it names the provider), `scripts/run-container-perf.sh`, `deploy/docker-compose.perf.yaml`, `e2e/README.md`, `docs/benchmarks/capacity.md`, `docs/benchmarks/README.md`, `docs/decisions/pipecat-evals.md`, `docs/pipecat-parity-matrix.md`, `reports/README.md`

- [ ] **Step 1: loadtest.** The server child is built without `-tags` (`./cmd/server`); its generated config gains `providers: {echo: {mimic: instant, stt: audio}}`; delete the placeholder `GOOGLE_*` variables, `GOVOICE_FAKE_STT` and `sa.json`; the flows it creates use `"provider": "echo"`. Add `-llm-delay` (duration, default 0) that writes `providers.echo.llm_extra_delay`. Check the synthetic audio is "voiced" for the instant profile (`VoicedThreshold 0` counts every frame): it must still yield transcripts. Update `loadtest_test.go` and its comments.
- [ ] **Step 2: perf gate.** `scripts/perf-gate.sh` self-test: `( export GOVOICE_FAKE_LLM_DELAY=100ms; expect 1 load-injected load -compare "$LB" )` becomes `expect 1 load-injected load -llm-delay 100ms -compare "$LB"`. `deploy/docker-compose.perf.yaml`: delete `GOVOICE_FAKE_STT=audio`; `config/perf.yaml`: add `providers.echo: {mimic: instant, stt: audio}` (merge with any existing `providers:` block) and use `echo` in whatever creates its flows.
- [ ] **Step 3: livecheck.** The `fake` profile becomes the `echo` profile: stages `stt {provider: echo, model: chirp_2, language: en-US}`, `llm {provider: echo, model: gemini-2.5-flash}`, `tts {provider: echo, voice: en-US-Chirp3-HD-Aoede}`, no `build_tags`; remove the `profiles.go` rule that a fake profile must list the tag; the `sp.fake` branch of `server.go` writes the echo config (instant, audio) instead of placeholders and environment; drop `GOVOICE_FAKE_LLM_DELAY` from its pass-through list; update `profiles_test.go` (`tagUnion`) and the strings that mention the tag.
- [ ] **Step 4: pipecat eval.** `run.sh`: `go build -o "$OUT/server-fake" ./cmd/server`, delete the `GOOGLE_*` placeholders and `sa.json`, write the server config with `providers.echo` (instant, audio), create the flow with `"provider":"echo"`.
- [ ] **Step 5: browser e2e.** `global-setup-flows.ts`: build with `xaispeech` only for both modes; in fake mode `providerEnv = {}` and the server config string gains `providers: { echo: { mimic: instant, label_replies: true } }`; delete the `GOOGLE_*`, `sa.json` and `GOVOICE_FAKE_LLM_LABEL` lines. `flows-stack.ts`: `PROVIDER = LIVE ? "xai" : "echo"`. `10-providers.spec.ts` ("only the suite's provider is available") now sees `echo` and needs no change. Reword the "fake provider" comments.
- [ ] **Step 5b: The one-port and dentistry stacks (new on main).** `e2e/global-setup-oneport.ts`: build with `xaispeech` only (no `fakeproviders`), delete the `GOOGLE_*`, `sa.json` and `GOVOICE_FAKE_LLM_LABEL` lines, add `providers: { echo: { mimic: instant, label_replies: true } }` to the config it templates, keep the placeholder `XAI_API_KEY` and `ELEVENLABS_API_KEY` (the dentistry demo installer needs xai and elevenlabs detected to create its flow; nothing builds them because the helper below rewrites every stage). `e2e/lib/dental-helpers.ts`: every stage `provider: "echo"` (model `gemini-2.5-flash`-style ids are free text; keep `fake-llm`/`v` or use the catalog defaults). `e2e/lib/stack-proc.ts` has no provider logic; `10-providers.spec.ts` keeps its `demoOnly = ["xai","elevenlabs"]` tolerance, and `available` minus those must now equal `["echo"]`. `deploy/docker-compose.perf.yaml` loses `BUILD_TAGS: fakeproviders` and the header text; `scripts/run-container-perf.sh` loses the "-tags fakeproviders" wording (the image builds the normal binary and the compose config sets `providers.echo`). Cloud Run and rehearsal configs (`config/cloudrun.yaml`, `deploy/rehearsal/server.yaml`) are real-provider configs: leave them, and confirm with `rg` that they carry no echo setting.
- [ ] **Step 6: Run every gate.**

```bash
make verify
make test-e2e-go
make e2e-fake
make live-check-selftest
make perf-gate-selftest
make eval-pipecat
make e2e-oneport
```

Expected: each passes. If `make perf-gate` is also run and a latency row moves, find out why (the real processors add per-unit work: `splitUnit`, retry wrappers, tracing) before touching a baseline; a deliberate re-baseline (`make perf-baseline`) is allowed only with the old and new numbers written into `docs/benchmarks/baseline.md` and the reason.
- [ ] **Step 7: Prove the realism.** Add `TestEchoGoogleLatencyAgainstBenchmarks` in `providers/echo` (a real `core.Session` with the three echo-backed processors, google mimic, time scaled by dividing every figure by 10 so it runs in about a second): the STT final after the stop is the largest fixed cost, the LLM's first delta follows its delay, the first audio of an 11-character unary unit comes later than that of a Chirp3-HD unit, in the ratios of `docs/benchmarks/first-audio-latency.md`. Then start the server with the default config, open the browser UI, call a flow on `echo` with `audible: true`, and listen: it should feel like a slow-ish real provider.
- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "refactor: every harness uses the echo provider on the normal build; GOVOICE_FAKE_* and the fakeproviders tag are gone

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 9: Retire the `core.Fake*` doubles

**Files:**
- Modify: `providers/echo/processors.go` (create), `tools/eval/eval.go`, `tools/eval/cmd/govoice-bench/main.go`, `tools/eval/turnquality/turnquality.go`, `tools/compat/whisker/conformance.go`, `tools/compat/client_compat_test.go`, `tools/compat/parity_fixtures_test.go`, `transport/ws/*_test.go` that use `core.NewFake*`, `core/fakes.go`, `core/fake_stt.go`
- Move: the LLM, TTS and STT doubles and the failure-injection processors out of the exported API into `_test.go` files in `core`

**Interfaces:**
- Produces in `providers/echo` (so other modules build the **real** echo-backed processors without the server):

```go
func NewSTTProcessor(m Mimic, model string, audioMode bool) (core.Processor, error)
func NewLLMProcessor(m Mimic, o LLMOptions, cfg google.LLMConfig) (core.Processor, error)
func NewTTSProcessor(m Mimic, voice string, sampleRate int, audible bool) (core.Processor, error)
```

(each builds the Google processor with `Provider: "echo"` and the echo backend; the `echo.*` factories of Task 6 call these instead of repeating the wiring: change them in this task and keep their tests green.)

- [ ] **Step 1: Inventory.** Run `rg -n "core\.(NewFake|Fake[A-Z]|Latency|Error|Panic|Flood|Stall|LateCompletion|IgnoredCancellation|ChildPanic)" --glob '*.go' . | rg -v '^core/'`. The expected users are the files listed above. `core.PassthroughProcessor`, `core.FakeTransportOutput` and `core.FakeTransportConfig` are generic pipeline sinks used by `tools` and `transport` tests, not fake providers: they stay exported in `core/fakes.go` (renamed `core/testsinks.go`) with a header saying they are test utilities.
- [ ] **Step 2: Add the three constructors** to `providers/echo/processors.go` with a test each (`TestNewTTSProcessorSpeaks`, `TestNewLLMProcessorEchoes`, `TestNewSTTProcessorTranscribes`, each driving a `core.Session` as in Tasks 2 to 4), and make the Task 6 factories call them.
- [ ] **Step 3: Migrate each external user to echo, one commit-sized file at a time.** In `tools/eval/*`, `tools/compat/*` and `transport/ws/*_test.go` replace `core.NewFakeLLMProcessor(core.FakeLLMConfig{…})` with `echo.NewLLMProcessor(echo.MimicInstant, …)` (`EchoUser` behaviour is the default reply; a scripted reply text the test relied on becomes an assertion on the echo reply "You said: …"), and `core.NewFakeTTSProcessor(…)` with `echo.NewTTSProcessor(echo.MimicInstant, "", rate, false)` (a test that counted exactly N frames per unit now derives N from the echo length: two 20 ms frames in instant mode). Each module's `go.mod` gains `govoice/providers/echo` (workspace `replace`, as `server` has). Run the module's tests after each file. A test that depended on a failure-injection knob (`FailWith`, `IgnoreCancel`) is a core test, not a transport/tools test: if one exists outside `core`, move it into `core` with its double.
- [ ] **Step 4: Benchmarks.** `govoice-bench` measures framework overhead with the instant doubles; it now runs the real echo-backed processors. Run `make perf-gate`; the framework-overhead rows must stay inside their tolerances (`docs/benchmarks/README.md`). If they move, follow the re-baseline rule of Task 8 Step 6.
- [ ] **Step 5: Move the doubles into core's tests.** `git mv core/fake_stt.go core/fake_stt_test.go`; move the `FakeLLMConfig`/`FakeLLMProcessor`, `FakeTTSConfig`/`FakeTTSProcessor` and the failure-injection processors (`LatencyProcessor`, `ErrorProcessor`, `PanicProcessor`, `ChildPanicProcessor`, `FloodProcessor`, `StallProcessor`, `LateCompletionProcessor`, `IgnoredCancellationProcessor`) from `core/fakes.go` into `core/fakes_test.go`; keep the sinks (Step 1) in `core/testsinks.go`. Drop any field no core test uses. Run `cd core && go test -race ./... && go vet ./...`; nothing outside `core` may reference the moved names (`go build ./... && go vet ./...` at the root and in each module).
- [ ] **Step 6: Gate.** `gofmt -w . && make verify && make test-e2e-go && make e2e-fake` — Expected: PASS.
- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "refactor: the fake LLM, TTS and STT leave core's public API; tools and transport tests use the echo-backed real processors

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Docs, the last sweep, and independent review

**Files:**
- Modify: `docs/providers.md`, `docs/benchmarks/capacity.md`, `docs/quickstart.md`, `README` where they mention fakes

- [ ] **Step 1: Docs.** `docs/providers.md`: an "Echo (simulated)" section: what it is, the module layout (`providers/echo`, real Google processors with echo backends), the four config keys with defaults, the profiles and the measured figures it imitates (link the benchmark docs), text versus audio STT and the placeholder-words caveat, `brokenvoice`, and "never use for real callers; labelled simulated in the UI". Quickstart: "Try it without a key": start the server, pick `Echo (simulated)` in First run, call it. Reword the "fake providers" wording in `docs/benchmarks/capacity.md` and the README targets.
- [ ] **Step 2: Sweep.**

```bash
rg -n "fakeproviders|GOVOICE_FAKE_|NewFakeLLM|NewFakeTTS|NewFakeSTT" --glob '!docs/superpowers/**' --glob '!.git' . ; echo "exit $?"
```

Expected: no matches (exit 1) except `core`'s own test files; fix every other hit.
- [ ] **Step 3: Full gate.** `gofmt -w . && make verify && make test-e2e-go && make e2e-fake && make perf-gate-selftest` — Expected: all green, clean `git status`.
- [ ] **Step 4: Independent review (AGENTS.md).** Dispatch a fresh reviewer on `main..HEAD`: YAGNI and bloat (any echo option or helper without a server caller; catalog duplication and its drift test; leftover fakes), the correctness and concurrency audit (backend goroutines under interruption, rotation and hang-up, channel closing by producers only, bounded queues, no audio retained, generation fencing through the real processors, `-race`), the five Review Focus items, the honesty check that no frame, trace, metric, log or UI string labels echo as `google` or a real provider, and that Google behaves exactly as before. Fix confirmed findings, each with a test that fails first.
- [ ] **Step 5: Commit**

```bash
git add docs
git commit -m "docs: the echo provider, what it imitates and how to tune it

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
