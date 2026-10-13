# Echo Providers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the `-tags fakeproviders` hack (fakes that silently take over `google.*`) with a real, always-built **`echo`** provider: listed in the catalog, always detected, selectable in the UI and in any flow, free to run, and able to **imitate Google's real behaviour** (settings, models, voices, languages, and the measured timing) so performance and UX tests exercise the system the way a real provider would, at no cost.

**Architecture:** The engine lives in `core` (`core/echo.go`, built on the existing `Fake*` processors, which stay as the core test doubles and gain the realism knobs). A keyless catalog provider `echo` mirrors Google's models, voices and languages. `server/registry_echo.go` registers `echo.stt/llm/tts` on every build; a `providers.echo` config block picks the **mimic profile** (`google` for realism, `instant` for framework-overhead measurements) and the test knobs. The build tag, `registry_fake.go` and every `GOVOICE_FAKE_*` variable go away; every harness (Go e2e, browser e2e, loadtest, livecheck, pipecat eval, perf gate, compose) uses `echo` on the normal binary.

**Tech Stack:** Go (core, server, tools), YAML catalog, Playwright e2e (TypeScript), shell harnesses.

**Spec:** Decisions taken with the user in this session (no separate spec file): echo is **always built and always detected** (no env gate, no tag); knobs live in **server config plus catalog presets**; the build tag is **replaced everywhere**; echo must **mimic Google with all its settings**, as realistically as possible. `tools/provtest` (fault-injection fakes for the resilience tests) is out of scope and stays. Measured Google behaviour comes from `docs/benchmarks/first-audio-latency.md` and `docs/benchmarks/agent-matrix.md`. Project rules: `AGENTS.md` (core agnostic of server concerns, every core feature consumed by the server, bounded queues, supervised goroutines, `go test -race`).

## What echo imitates (from the repo's own measurements)

- **STT, `chirp_2`:** no interim results; the final arrives about 1.2 s after the VAD stop. **STT, `latest_long`:** interim results, final about 0.5 s after the stop. Language, model ids as in `server/catalog/data/google.yaml`.
- **LLM:** first token about 0.7 s after the user turn; then a stream of word-sized deltas.
- **TTS:** the default voice `en-US-Chirp3-HD-Aoede` (any `Chirp3-HD` voice) **streams**: first audio about 0.4 s; every other voice is **unary**: nothing plays until the whole unit is synthesized (about 0.6 s plus about 50 ms per character; the benchmark has 1.13 s for 11 characters). Output rate follows `providers.google_tts.sample_rate` (24 kHz default); audio length follows the text (about 15 characters per second).
- **Transcripts of real microphone audio:** echo cannot recognise speech, so in `audio` mode it emits placeholder words ("hello") at a speech-like rate (one per 20 frames, 0.4 s) while the caller is speaking. It is a stand-in, not recognition; text turns pass through untouched.
- **`instant` profile:** every delay zero and one word per frame, the old fake behaviour: the baseline of `docs/benchmarks/baseline.md` (framework overhead) must not move.

## Global Constraints

- `core/` imports nothing from `server/`; nothing in `core` reads the environment or a config file; knobs arrive as plain struct fields.
- Every new core field and function is consumed by the server's echo registration (YAGNI): no profile, mode or helper without a caller on the call path.
- Bounded everything: the echo STT must not record audio (`FakeSTTConfig.DiscardAudio`); delayed finals use the supervised worker and honour `ctx.Done()`; no unbounded channel.
- `go test -race ./core/... ./server/...` clean; `gofmt`; conventional commits ending with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.
- The echo provider id is `echo`, display name `Echo (simulated)`, so no screen can mistake it for a real provider. Echo never sends or reads a secret; it has no credential kinds.
- After the migration no file mentions `fakeproviders` or `GOVOICE_FAKE_`; `make verify`, `make test-e2e-go`, `make e2e-fake`, `make live-check-selftest`, `make perf-gate-selftest` and `make eval-pipecat` still pass.
- Defaults: `providers.echo.mimic: google`, `stt: text`, `audible: false`, `label_replies: false`. Harness configs say `mimic: instant` explicitly.

## Review Focus

1. **A real provider that is not echo.** With `GOOGLE_API_KEY` set and no echo config, google flows must behave exactly as before, and the provider list must show both. Nothing in `google.*` may be replaced by echo; a flow naming `echo` on a server whose build lacks it (cannot happen now) is gone with the tag.
2. **Unbounded growth and leaks.** The echo STT in `audio` mode over a long call (no audio retained, no queue growth); a delayed final or a first-token delay when the call hangs up or the caller interrupts (no goroutine outlives the session, no frame from an old generation reaches the pipeline).
3. **The mimic is honest.** `chirp_2` has no interims, `latest_long` has; Chirp3-HD voices stream and others are unary; first-byte delay grows with the unit's text; an unknown or custom model or voice falls back to the Google defaults rather than erroring; `brokenvoice` still fails to build (failed-handoff tests).
4. **Config validation and defaults.** An unknown `mimic` or `stt` value is a startup error naming the field and the allowed values; an absent block means the documented defaults; durations must be non-negative.
5. **Harness regressions.** The perf baseline (framework overhead) must not change; the self-test's injected delay must still fail the gate; the cue test must still see silence for speech (`audible: false`); the browser suite's "only the suite's provider is available" assertion must now see `echo`.

---

### Task 1: Realism knobs on the fake STT

**Files:**
- Modify: `core/fake_stt.go`
- Test: `core/fake_stt_echo_test.go` (create)

**Interfaces:**
- Produces on `FakeSTTConfig`: `DiscardAudio bool` (do not retain audio in `Received()`), `NoInterims bool` (candidates only at the final), `FinalDelay time.Duration` (the final is emitted this long after `UserStoppedSpeaking`), `FramesPerWord int` (one word per N voiced frames; 0 or 1 = every frame).
- Changes `work chan func()` to `work chan func(context.Context)` (internal).

- [ ] **Step 1: Write the failing tests** — `core/fake_stt_echo_test.go`. Read `core/fake_stt_test.go` (or the existing STT failover test that drives `FakeSTTProcessor`) first and reuse its session harness; the cases to write, each against a `FakeSTTProcessor` attached to a real `Session` as the existing tests do:

```go
// helpers: feed(n) pushes n InputAudioFrames with Samples[0]=0 between a UserStartedSpeakingFrame
// and (when stop) a UserStoppedSpeakingFrame; collect(...) returns the TranscriptCandidateFrames seen.

func TestFakeSTTDiscardAudioKeepsNothing(t *testing.T) // 500 frames, Received() is empty
func TestFakeSTTNoInterimsOnlyFinal(t *testing.T)      // 10 frames then stop: exactly one candidate, Final=true, text "hello hello …" (10 words)
func TestFakeSTTFinalDelay(t *testing.T)               // FinalDelay=80ms: the final arrives >=80ms after the stop frame, not before
func TestFakeSTTFinalDelayRespectsTeardown(t *testing.T) // close the session during the delay: no candidate, the worker exits (goleak or a done channel)
func TestFakeSTTFramesPerWord(t *testing.T)            // FramesPerWord=4, 20 frames: 5 words
```

(Write each with the assertions in the comments; the exact harness calls follow the neighbouring tests.)

- [ ] **Step 2: Run to see them fail**

Run: `cd core && go test -race -run 'TestFakeSTT(DiscardAudio|NoInterims|FinalDelay|FramesPerWord)' .`
Expected: FAIL — the fields do not exist.

- [ ] **Step 3: Implement** in `core/fake_stt.go`:

Add the four fields to `FakeSTTConfig` (documented as above). Change the worker and `emit`:

```go
	work     chan func(context.Context)

// in NewFakeSTTProcessor
	return &FakeSTTProcessor{cfg: cfg, work: make(chan func(context.Context), 256)}

// in Attach
			case fn := <-p.work:
				fn(ctx)

func (p *FakeSTTProcessor) emit(fr Frame, gen uint64) { p.emitAfter(fr, gen, 0) }

// emitAfter delivers fr after delay, on the worker (so order is kept) and only while the session lives.
func (p *FakeSTTProcessor) emitAfter(fr Frame, gen uint64, delay time.Duration) {
	select {
	case p.work <- func(ctx context.Context) {
		if delay > 0 {
			t := time.NewTimer(delay)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
		_ = p.session.Reenter(fr, Downstream, p.index+1, gen)
	}:
	default:
	}
}
```

In `ProcessFrame`:
- `UserStoppedSpeakingFrame`: `p.emitAfter(p.candidateLocked(true), p.gen, p.cfg.FinalDelay)` instead of `p.emit(...)`.
- `InputAudioFrame`: `if !p.cfg.DiscardAudio { p.received = append(p.received, fr.Samples...) }`; replace the word-append block with:

```go
		if len(fr.Samples) > 0 && int(fr.Samples[0]) < len(p.cfg.Words) {
			p.voiced++
			if every := max(p.cfg.FramesPerWord, 1); p.voiced%every == 0 {
				// Audio before the turn binds (VAD reports onset late) still counts, as in providers.
				p.words = append(p.words, p.cfg.Words[fr.Samples[0]])
				if p.turn != "" && !p.cfg.NoInterims {
					p.emit(p.candidateLocked(false), p.gen)
					if p.cfg.FinalEvery > 0 && p.frames%p.cfg.FinalEvery == 0 {
						p.emit(p.candidateLocked(true), p.gen)
					}
				}
			}
		}
```

with a new field `voiced int` reset together with `words, frames` at the stop (`p.words, p.frames, p.voiced = nil, 0, 0`). Add `"time"` to the imports.

- [ ] **Step 4: Run to see them pass, then the package**

Run: `cd core && go test -race . 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add core/fake_stt.go core/fake_stt_echo_test.go
git commit -m "feat(core): fake STT can model a final delay, no interims, one word per N frames, and not retain audio

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Realism knobs on the fake LLM and TTS

**Files:**
- Modify: `core/fakes.go`
- Test: `core/fake_echo_realism_test.go` (create)

**Interfaces:**
- Produces on `FakeLLMConfig`: `FirstDelay time.Duration` (before the first delta), `WordChunks bool` (split every chunk into word-sized deltas, spaces kept; `ChunkDelay` then applies per word).
- Produces on `FakeTTSConfig`: `FirstDelay time.Duration`, `FirstDelayPerChar time.Duration` (the delay before a unit's first audio is `FirstDelay + FirstDelayPerChar × runes(text)`), `CharsPerSecond int` (when > 0 the unit's frame count is `ceil(runes / CharsPerSecond / 20 ms)`, at least 1, instead of `FramesPerUnit`), `Audible bool` (frames carry a quiet 220 Hz tone, phase-continuous, sized `SampleRate/50` samples; false keeps the 640 silent bytes).
- Produces: `func splitWords(s string) []string` (unexported; `strings.SplitAfter(s, " ")` without empty tail).

- [ ] **Step 1: Write the failing tests** — `core/fake_echo_realism_test.go`, using the harness of `core/fake_tts_budget_test.go` and `core/fake_llm_tools_test.go`:

```go
func TestSplitWords(t *testing.T) // "You said: hi there" -> ["You ", "said: ", "hi ", "there"]; "" -> nil
func TestFakeLLMWordChunksDeltaPerWord(t *testing.T)   // reply deltas concatenate to the full text, one delta per word
func TestFakeLLMFirstDelay(t *testing.T)               // FirstDelay=60ms: first delta >=60ms after the user message; later deltas not delayed again when ChunkDelay=0
func TestFakeLLMFirstDelayCanceledByInterruption(t *testing.T) // interruption during the delay: no delta arrives
func TestFakeTTSFramesFollowTextLength(t *testing.T)   // CharsPerSecond=15: 30 runes -> 100 frames; 1 rune -> 4 frames (ceil(1/15/0.02)=4); empty text -> 1 frame
func TestFakeTTSFirstDelayGrowsWithText(t *testing.T)  // FirstDelay=20ms, PerChar=2ms: 40 runes first audio >=100ms, 1 rune >=22ms
func TestFakeTTSAudibleToneHasSoundAndRightSize(t *testing.T) // Audible, 24000 Hz: each frame 960 bytes, at least one sample != 0, peak below 4000 (quiet); silent by default (all 640 bytes zero)
func TestFakeTTSFirstDelayCanceledByInterruption(t *testing.T)
```

- [ ] **Step 2: Run to see them fail**

Run: `cd core && go test -race -run 'TestSplitWords|TestFakeLLM(WordChunks|FirstDelay)|TestFakeTTS(Frames|FirstDelay|Audible)' .`
Expected: FAIL — fields and `splitWords` missing.

- [ ] **Step 3: Implement** in `core/fakes.go`:

```go
// splitWords splits s after each space so the pieces concatenate back to s.
func splitWords(s string) []string {
	parts := strings.SplitAfter(s, " ")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
```

LLM: in `ProcessFrame`'s generation goroutine, after the label is prepended and before the loop:

```go
			if p.cfg.WordChunks {
				words := make([]string, 0, len(chunks)*4)
				for _, c := range chunks {
					words = append(words, splitWords(c)...)
				}
				chunks = words
			}
			if d := p.cfg.FirstDelay; d > 0 {
				timer := time.NewTimer(d)
				select {
				case <-taskCtx.Done():
					timer.Stop()
					if !p.cfg.IgnoreCancel {
						return
					}
				case <-timer.C:
				}
			}
```

(Place the label prepend before this block as now; with `WordChunks` the label is split too, harmless.)

TTS: add `unitFrames` and `unitDelay` helpers and use them in the unit goroutine:

```go
func (c FakeTTSConfig) unitFrames(text string) int {
	if c.CharsPerSecond <= 0 {
		return c.FramesPerUnit
	}
	n := len([]rune(text))
	frames := (n*1000 + c.CharsPerSecond*20 - 1) / (c.CharsPerSecond * 20) // ceil(runes / cps / 20 ms)
	return max(frames, 1)
}

func (c FakeTTSConfig) firstDelay(text string) time.Duration {
	return c.FirstDelay + time.Duration(len([]rune(text)))*c.FirstDelayPerChar
}
```

In the unit goroutine, before the frame loop (after the `FailWith ... FailAfterAudio == 0` early failure), wait `firstDelay` the same way as `FrameDelay` is waited (reuse the existing timer select blocks: factor the three-way wait into a closure `wait := func(d time.Duration) bool` returning false when canceled, and call it for `firstDelay(su.Text)` and for `FrameDelay`). Replace `p.cfg.FramesPerUnit` in the loop bounds and `isFinal` with `frames := p.cfg.unitFrames(su.Text)`. For the sample buffer:

```go
				samples := make([]byte, 640)
				if p.cfg.Audible {
					samples = tone(rate, p.Synthesized.Load())
				}
```

and `tone` (phase-continuous by frame sequence, 220 Hz, amplitude 3000):

```go
func tone(rate int, seq uint64) []byte {
	n := rate / 50
	out := make([]byte, n*2)
	base := int(seq) * n
	for i := 0; i < n; i++ {
		v := int16(3000 * math.Sin(2*math.Pi*220*float64(base+i)/float64(rate)))
		out[2*i], out[2*i+1] = byte(v), byte(v>>8)
	}
	return out
}
```

(import `math`). The audio frame's `Seq: p.Synthesized.Add(1)` stays; `tone` reads `Synthesized.Load()` just before, which is one behind: fine for phase continuity.

- [ ] **Step 4: Run to see them pass, then the package**

Run: `cd core && go test -race . 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add core/fakes.go core/fake_echo_realism_test.go
git commit -m "feat(core): fake LLM and TTS can model first-token and first-byte delay, word-sized deltas, text-length audio and an audible tone

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: The echo engine in core

**Files:**
- Create: `core/echo.go`
- Test: `core/echo_test.go`

**Interfaces:**
- Consumes: Task 1 and 2 fields; `NewFakeSTTProcessor`, `NewFakeLLMProcessor`, `NewFakeTTSProcessor`.
- Produces:
  - `type EchoSTT struct { Interims bool; FinalDelay time.Duration; FramesPerWord int; AudioMode bool }`, `func NewEchoSTT(o EchoSTT) Processor`. In text mode (`AudioMode` false) it returns a pass-through that forwards every frame unchanged and records nothing; in audio mode a `FakeSTTProcessor` with `Words` = 256 × "hello", `Provider: "echo"`, `DiscardAudio: true`, `NoInterims: !Interims`, `FinalDelay`, `FramesPerWord`.
  - `type EchoLLM struct { FirstToken, TokenDelay time.Duration; Label string; Executor ToolExecutor; ToolOnPhrase, SayResult map[string]string }`, `func NewEchoLLM(o EchoLLM) Processor`: replies `"You said: " + user text`, word by word, `AfterTool` "Connecting you.", exactly what the old google fake did.
  - `type EchoTTS struct { FirstByte, FirstBytePerChar time.Duration; CharsPerSecond, SampleRate int; Audible bool }`, `func NewEchoTTS(o EchoTTS) Processor` (`FramesPerUnit: 2` when `CharsPerSecond` is 0).
  - Zero values of all three are the **instant** behaviour of the old fakes.

- [ ] **Step 1: Write the failing tests** — `core/echo_test.go`:

```go
func TestEchoSTTTextModePassesFramesThroughAndKeepsNothing(t *testing.T)
func TestEchoSTTAudioModeSaysHelloAtSpeechRate(t *testing.T)     // FramesPerWord=20, 100 frames -> 5 words, final after stop
func TestEchoSTTWithoutInterimsSendsOnlyAFinal(t *testing.T)
func TestEchoLLMEchoesTheUserWordByWord(t *testing.T)            // "hi there" -> "You said: hi there", >=4 deltas
func TestEchoLLMLabelAndTransferTool(t *testing.T)               // Label prefixes; ToolOnPhrase calls the executor and answers "Connecting you."
func TestEchoTTSInstantIsTwoSilentFramesPerUnit(t *testing.T)
func TestEchoTTSImitatesSpeechLength(t *testing.T)               // CharsPerSecond=15, 30 runes -> 100 frames at the configured rate
```

- [ ] **Step 2: Run to see them fail**

Run: `cd core && go test -race -run 'TestEcho' .`
Expected: FAIL — `NewEchoSTT` undefined.

- [ ] **Step 3: Implement** — `core/echo.go`:

```go
package core

import (
	"context"
	"time"
)

// The echo stages are simulated STT, LLM and TTS that cost nothing and need no network. They
// imitate a real provider's timing and shape so performance and UX tests behave like the real
// thing: the LLM repeats the caller, word by word; the TTS makes audio as long as the text;
// the STT stands in for speech recognition (it cannot recognise speech: placeholder words).
// Zero values are the "instant" behaviour (no delays), used to measure the framework itself.

// EchoSTT configures the simulated speech-to-text.
type EchoSTT struct {
	// AudioMode turns audio into placeholder words ("hello"). Off, typed turns pass through and
	// audio is ignored, so a looping test microphone cannot start turns.
	AudioMode     bool
	Interims      bool          // emit cumulative interim results while the caller speaks
	FinalDelay    time.Duration // the final arrives this long after the caller stops
	FramesPerWord int           // one placeholder word per this many audio frames (20 ms each)
}

// NewEchoSTT builds the simulated STT stage.
func NewEchoSTT(o EchoSTT) Processor {
	if !o.AudioMode {
		return passSTT{}
	}
	words := make([]string, 256) // Samples[0] indexes Words: any byte value counts as a word
	for i := range words {
		words[i] = "hello"
	}
	return NewFakeSTTProcessor(FakeSTTConfig{Provider: "echo", Words: words, DiscardAudio: true,
		NoInterims: !o.Interims, FinalDelay: o.FinalDelay, FramesPerWord: o.FramesPerWord})
}

// passSTT forwards frames untouched. (PassthroughProcessor records every frame, which grows
// without bound on a live audio stream.)
type passSTT struct{}

func (passSTT) ProcessFrame(_ context.Context, f Frame, d Direction, e Emitter) error {
	return e.PushDirection(f, d)
}

// EchoLLM configures the simulated language model.
type EchoLLM struct {
	FirstToken time.Duration // before the first word
	TokenDelay time.Duration // between words
	// Label prefixes every reply (a routed flow's step prompt) so a test can tell which step answered.
	Label string
	// Executor and the phrase maps let a message such as "transfer to sales" call that tool, as
	// the e2e suites do. SayResult maps a tool to the name the reply uses for what came back.
	Executor     ToolExecutor
	ToolOnPhrase map[string]string
	SayResult    map[string]string
}

// NewEchoLLM builds the simulated LLM stage: "You said: <the caller's words>".
func NewEchoLLM(o EchoLLM) Processor {
	return NewFakeLLMProcessor(FakeLLMConfig{
		ResponseChunks: []string{"You said: "}, EchoUser: true, WordChunks: true,
		FirstDelay: o.FirstToken, ChunkDelay: o.TokenDelay, Label: o.Label,
		Executor: o.Executor, ToolOnPhrase: o.ToolOnPhrase, SayResult: o.SayResult, AfterTool: "Connecting you.",
	})
}

// EchoTTS configures the simulated text-to-speech.
type EchoTTS struct {
	FirstByte, FirstBytePerChar time.Duration // before a unit's first audio, plus per character
	CharsPerSecond              int           // speech rate: audio is as long as the text takes to say; 0 = two frames
	SampleRate                  int           // output rate (24000 when 0)
	Audible                     bool          // a quiet tone instead of silence
}

// NewEchoTTS builds the simulated TTS stage.
func NewEchoTTS(o EchoTTS) Processor {
	return NewFakeTTSProcessor(FakeTTSConfig{FramesPerUnit: 2, FirstDelay: o.FirstByte, FirstDelayPerChar: o.FirstBytePerChar,
		CharsPerSecond: o.CharsPerSecond, SampleRate: o.SampleRate, Audible: o.Audible})
}
```

- [ ] **Step 4: Run to see them pass, then the package**

Run: `cd core && go test -race . 2>&1 | tail -5; gofmt -l .`
Expected: PASS, nothing from gofmt.

- [ ] **Step 5: Commit**

```bash
git add core/echo.go core/echo_test.go
git commit -m "feat(core): echo stages — simulated STT, LLM and TTS that imitate a real provider's timing and shape

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: The `echo` catalog provider (keyless)

**Files:**
- Modify: `server/catalog/catalog.go`, `server/catalog/resolver.go`
- Create: `server/catalog/data/echo.yaml`
- Test: `server/catalog/catalog_test.go`, `server/catalog/resolver_test.go`, `server/api/providers_test.go`

**Interfaces:**
- Produces: a service may declare `keyless: true` (no `credential` list required): `Service.Keyless bool` (`yaml:"keyless"`). `Resolver.Status` returns `Declared && Detected` for it; `Resolver.Secret` returns `(nil, nil)`; `Resolver.Env("echo")` is `[]` (no variables to set).
- `echo.yaml` lists the same models, voices and languages as `google.yaml`.

- [ ] **Step 1: Write the failing tests.**

`catalog_test.go`:

```go
func TestEchoMirrorsGoogleCatalog(t *testing.T) {
	c, err := Load()
	if err != nil { t.Fatal(err) }
	g, e := mustProvider(t, c, "google"), mustProvider(t, c, "echo")
	for _, name := range []string{ServiceSTT, ServiceLLM, ServiceTTS} {
		gs, es := g.Services[name], e.Services[name]
		if es == nil { t.Fatalf("echo has no %s", name) }
		if !reflect.DeepEqual(gs.Models, es.Models) || !reflect.DeepEqual(gs.Voices, es.Voices) || !reflect.DeepEqual(gs.Languages, es.Languages) {
			t.Errorf("echo %s differs from google: update server/catalog/data/echo.yaml", name)
		}
	}
}
func TestKeylessServiceNeedsNoCredential(t *testing.T) // Parse of a provider whose service has keyless: true and no credential: ok; with neither keyless nor credential: error "needs at least one credential kind"
func mustProvider(...)
```

`resolver_test.go`:

```go
func TestKeylessProviderIsAlwaysDetected(t *testing.T) // empty environment: Detected("echo") true; Status("echo","tts") Declared+Detected; Secret returns nil,nil; Env("echo") empty
```

`api/providers_test.go`: with the real catalog and a registry that supports `echo.*`, `GET /providers` lists `echo` with `detected: true`, `env: []`, all three services `available`, `display_name` "Echo (simulated)".

- [ ] **Step 2: Run to see them fail**

Run: `cd server && go test -race ./catalog/ ./api/ -run 'Echo|Keyless' 2>&1 | tail`
Expected: FAIL.

- [ ] **Step 3: Implement.** `catalog.go`: add `Keyless bool \`yaml:"keyless"\`` to `Service`; in `validate`, the credential-count check becomes `if len(svc.Credential) == 0 && !svc.Keyless && !overlay`; a keyless service with credentials is an error ("keyless service %q cannot name credentials"). `resolver.go`:

```go
func (r *Resolver) Status(provider, service string) Status {
	p, svc, ok := r.service(provider, service)
	if !ok { return Status{} }
	if svc.Keyless { return Status{Declared: true, Detected: true} }
	...
func (r *Resolver) Secret(provider, service string) ([]byte, error) {
	p, svc, ok := r.service(provider, service)
	if !ok { return nil, ErrUnknown }
	if svc.Keyless { return nil, nil }
```

`data/echo.yaml` (copy google's entries exactly; the test guards drift):

```yaml
id: echo
name: Echo (simulated)
services:
  llm:
    keyless: true
    models:
      - {id: gemini-2.5-flash, label: Gemini 2.5 Flash, default: true}
  stt:
    keyless: true
    models:
      - {id: chirp_2, label: Chirp 2, default: true}
      - {id: latest_long, label: Latest long}
    languages:
      - {id: en-US, label: English (US), default: true}
  tts:
    keyless: true
    voices:
      - {id: en-US-Chirp3-HD-Aoede, label: Chirp 3 HD Aoede (en-US), default: true}
```

(Use the full current `google.yaml` lists; the snippet shows the first entries.) `echo` has no `credentials:` block; confirm `validate` accepts a provider with none.

- [ ] **Step 4: Run to see them pass, then the module**

Run: `cd server && go test -race ./catalog/ ./api/ 2>&1 | tail -5`
Expected: PASS (other registry-dependent API tests may need `echo.*` registered: they pass after Task 5; if one fails only for that reason, note it and continue).

- [ ] **Step 5: Commit**

```bash
git add server/catalog server/api
git commit -m "feat(server): keyless catalog services and the echo provider, mirroring Google's models, voices and languages

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Register echo on every build; delete the build tag

**Files:**
- Create: `server/registry_echo.go`, `server/registry_echo_test.go`
- Modify: `server/config.go` (`Providers.Echo`), `server/config_defaults.go`, `server/config_validate.go`, `server/registry.go` (drop the hook), `server/config_test.go`
- Delete: `server/registry_fake.go`, `server/registry_fake_test.go`

**Interfaces:**
- Produces the config block:

```go
		Echo struct {
			Mimic        string        `yaml:"mimic"`         // google | instant
			STT          string        `yaml:"stt"`           // text | audio
			Audible      bool          `yaml:"audible"`       // a quiet tone instead of silence
			LabelReplies bool          `yaml:"label_replies"` // prefix replies with the step's prompt
			LLMExtraDelay time.Duration `yaml:"llm_extra_delay"` // added to every word (perf-gate self-test)
		} `yaml:"echo"`
```

- Defaults: `Mimic "google"`, `STT "text"`. Validation errors name the field, e.g. `providers.echo.mimic: "x" must be one of google, instant`; `llm_extra_delay` must be ≥ 0.
- `registerEcho(r *Registry)` is called from `NewRegistry` and registers `echo.stt`, `echo.llm`, `echo.tts`.
- `echoGoogle(spec StageSpec)` returns the per-stage figures for the google mimic.

- [ ] **Step 1: Write the failing tests** — `server/registry_echo_test.go`:

```go
func TestEchoIsRegisteredOnEveryBuild(t *testing.T)             // NewRegistry().Supports("echo", stage) for stt/llm/tts
func TestEchoDoesNotReplaceGoogle(t *testing.T)                 // the "google.tts" factory is the real one (compare the pointer/behaviour: with a bad secret it still errors as before) — reuse the existing google factory test fixtures
func TestEchoGoogleMimicSTTByModel(t *testing.T)                // chirp_2: no interims, FinalDelay 1200ms; latest_long: interims, 500ms; unknown model: chirp_2 figures (checked through the exported figures func, not by timing)
func TestEchoGoogleMimicTTSStreamingVersusUnary(t *testing.T)   // en-US-Chirp3-HD-Aoede: first byte 400ms, no per-char; "en-US-Journey-F": 600ms + 50ms/char; both 15 chars/s, rate = providers.google_tts.sample_rate
func TestEchoInstantMimicHasNoDelays(t *testing.T)
func TestEchoBrokenVoiceFailsToBuild(t *testing.T)              // spec.Voice "brokenvoice" -> error, as the old fake
func TestEchoLLMPhrasesForTransferAndMCPTools(t *testing.T)     // the old TestFakeLLMPhrasesForMCPTools, untagged, plus transfer_to_sales -> "transfer to sales"
func TestEchoConfigDefaultsAndValidation(t *testing.T)          // defaults google/text; mimic "x", stt "y", negative extra delay are errors naming the field
```

- [ ] **Step 2: Run to see them fail**

Run: `cd server && go test -race -run 'TestEcho' . 2>&1 | tail`
Expected: FAIL.

- [ ] **Step 3: Implement.** `registry_echo.go` (no build tag):

```go
package server

import (
	"errors"
	"strings"
	"time"

	"govoice/core"
)

// echoFigures are the timings the echo stages imitate. The google figures come from the
// repo's own live measurements (docs/benchmarks/first-audio-latency.md, agent-matrix.md).
type echoFigures struct {
	stt core.EchoSTT
	llm core.EchoLLM
	tts core.EchoTTS
}

// echoGoogle imitates Google: chirp_2 has no interim results and finalizes about 1.2 s after the
// caller stops, latest_long streams interims and finalizes in about 0.5 s; the LLM answers its
// first word in about 0.7 s; Chirp3-HD voices stream (first audio about 0.4 s) while every other
// voice is unary (about 0.6 s plus 50 ms per character before any audio). Speech is 15 chars/s.
func echoGoogle(spec StageSpec, sampleRate int) echoFigures {
	f := echoFigures{
		llm: core.EchoLLM{FirstToken: 700 * time.Millisecond, TokenDelay: 30 * time.Millisecond},
		tts: core.EchoTTS{CharsPerSecond: 15, SampleRate: sampleRate},
	}
	if spec.Model == "latest_long" {
		f.stt = core.EchoSTT{Interims: true, FinalDelay: 500 * time.Millisecond, FramesPerWord: 20}
	} else {
		f.stt = core.EchoSTT{FinalDelay: 1200 * time.Millisecond, FramesPerWord: 20}
	}
	if strings.Contains(spec.Voice, "Chirp3-HD") || spec.Voice == "" {
		f.tts.FirstByte = 400 * time.Millisecond
	} else {
		f.tts.FirstByte, f.tts.FirstBytePerChar = 600*time.Millisecond, 50*time.Millisecond
	}
	return f
}

// echoInstant has no delays and one word per audio frame: the framework alone is measured.
func echoInstant(int) echoFigures { return echoFigures{stt: core.EchoSTT{FramesPerWord: 1}} }

func registerEcho(r *Registry) {
	figures := func(cfg *Config, spec StageSpec) echoFigures {
		var f echoFigures
		if cfg.Providers.Echo.Mimic == "instant" {
			f = echoInstant(0)
		} else {
			f = echoGoogle(spec, cfg.Providers.GoogleTTS.SampleRate)
		}
		f.stt.AudioMode = cfg.Providers.Echo.STT == "audio"
		f.llm.TokenDelay += cfg.Providers.Echo.LLMExtraDelay
		f.tts.Audible = cfg.Providers.Echo.Audible
		return f
	}
	r.Register("echo.stt", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		return core.NewEchoSTT(figures(cfg, spec).stt), nil
	})
	r.Register("echo.llm", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		l := figures(cfg, spec).llm
		l.Executor, l.ToolOnPhrase = spec.ToolExecutor, map[string]string{}
		if cfg.Providers.Echo.LabelReplies {
			l.Label = "[" + spec.SystemPrompt + "] "
		}
		for _, d := range spec.Tools {
			if step, ok := strings.CutPrefix(d.Name, "transfer_to_"); ok {
				l.ToolOnPhrase["transfer to "+strings.ReplaceAll(step, "_", " ")] = d.Name
			}
		}
		phrases, spoken := mcpToolPhrases(spec.Tools)
		for p, name := range phrases {
			l.ToolOnPhrase[p] = name
		}
		l.SayResult = spoken
		return core.NewEchoLLM(l), nil
	})
	r.Register("echo.tts", func(cfg *Config, spec StageSpec) (core.Processor, error) {
		// A step whose voice is "brokenvoice" cannot be built: the e2e suites use it to make a handoff fail.
		if spec.Voice == "brokenvoice" {
			return nil, errors.New("echo tts: voice brokenvoice is unavailable")
		}
		return core.NewEchoTTS(figures(cfg, spec).tts), nil
	})
}
```

Move `mcpToolPhrases` (unchanged) from `registry_fake.go` to the bottom of this file. In `registry.go` replace

```go
	if registerFakeProviders != nil {
		registerFakeProviders(r) // -tags fakeproviders only (registry_fake.go)
	}
```

with `registerEcho(r)` and delete the `registerFakeProviders` var and its comment. Delete `registry_fake.go`; move `TestFakeLLMPhrasesForMCPTools` into `registry_echo_test.go` as the phrases test above (no tag). Add the `Echo` struct to `config.go` (next to `OpenAILLM`), defaults in `config_defaults.go` (`cfg.Providers.Echo.Mimic, cfg.Providers.Echo.STT = "google", "text"`) and validation in `config_validate.go` next to the sample-rate check:

```go
	if m := cfg.Providers.Echo.Mimic; !slices.Contains([]string{"google", "instant"}, m) {
		errs = append(errs, fmt.Errorf("providers.echo.mimic: %q must be one of google, instant", m))
	}
	if m := cfg.Providers.Echo.STT; !slices.Contains([]string{"text", "audio"}, m) {
		errs = append(errs, fmt.Errorf("providers.echo.stt: %q must be one of text, audio", m))
	}
	if cfg.Providers.Echo.LLMExtraDelay < 0 {
		errs = append(errs, errors.New("providers.echo.llm_extra_delay must not be negative"))
	}
```

(match the file's existing error-collection idiom).

- [ ] **Step 4: Run to see them pass, then the module**

Run: `cd server && go test -race ./... 2>&1 | tail -15; gofmt -l .`
Expected: PASS. Tests that used `-tags fakeproviders` still carry their tag until Task 6 and are skipped here.

- [ ] **Step 5: Commit**

```bash
git add server core
git commit -m "feat(server): the echo provider is registered on every build and imitates Google; the fakeproviders tag and registry_fake.go go away

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 6: The Go end-to-end tests run on the normal binary against `echo`

**Files:**
- Modify: `server/flow_e2e_test.go`, `server/mcp_call_e2e_test.go`, `server/secret_leak_test.go`, `server/context_injection_e2e_test.go`, `Makefile` (`test-e2e-go` and any `-tags ... fakeproviders` use)

**Interfaces:**
- Consumes: Task 5's `providers.echo` config and the `echo.*` factories.

- [ ] **Step 1: Migrate one file and watch it fail for the right reason.** In `flow_e2e_test.go`: delete the `//go:build fakeproviders` line; set the echo config on the test's `Config` (`cfg.Providers.Echo.Mimic = "instant"`, `cfg.Providers.Echo.LabelReplies = true`) in place of `t.Setenv("GOVOICE_FAKE_LLM_LABEL", "1")`; change every `"google."+stage` probe key and every `"provider":"google"` stage to `echo`; drop the placeholder `GOOGLE_*` environment (echo needs none); update the comments ("the fake providers" → "the echo provider"). Run `cd server && go test -race -count=1 -run 'FlowE2E' .` — it must pass; if a step fails because a model such as `"m"` or voice `"v"` is rejected, echo's catalog allows custom entries (`custom` defaults true), so the failure is real: investigate before continuing.
- [ ] **Step 2: Migrate the other three** the same way (`mcp_call_e2e_test.go`: the `agentWith` configs and rig comments; `secret_leak_test.go`: keep the XAI and Google sentinel secrets in the environment and a mixed flow (`llm` on `xai` is not buildable without the network: keep the existing structure and only swap the google stages for echo; the test asserts no sentinel reaches a log or response, so keeping `GOOGLE_API_KEY` sentinels set while no stage uses google still proves the point; read the test and keep its intent); `context_injection_e2e_test.go`: drop the tag and use echo).
- [ ] **Step 3: Makefile.** `test-e2e-go` becomes `go test -race -count=1 -tags "$(GOTAGS)" -run 'FlowE2E|NoSecretLeavesTheServer|MCPCall|ContextInjection' .` (add `ContextInjection` only if that file's tests match; check their names), with the comment "against the echo provider". Replace every remaining `fakeproviders` mention in the Makefile (lines about `bin/server-fake`, `eval-pipecat`, the loadtest comment) with the new wording; the targets themselves change in Task 7.
- [ ] **Step 4: Run**

Run: `make test-e2e-go`
Expected: PASS, identical test names to before.

- [ ] **Step 5: Commit**

```bash
git add server Makefile
git commit -m "test(server): the Go end-to-end suite runs on the normal build against the echo provider

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Every harness uses echo

**Files:**
- Modify: `tools/loadtest/server.go`, `tools/loadtest/main.go`, `tools/loadtest/loadtest_test.go`, `tools/livecheck/server.go`, `tools/livecheck/main.go`, `tools/livecheck/profiles.yaml`, `tools/livecheck/profiles.go`, `tools/livecheck/profiles_test.go`, `tools/eval/pipecat/run.sh`, `tools/eval/cmd/govoice-bench/main.go` (only if it names the tag), `scripts/perf-gate.sh`, `deploy/docker-compose.perf.yaml`, `config/perf.yaml`, `e2e/global-setup-flows.ts`, `e2e/lib/flows-stack.ts`, `e2e/lib/flows-helpers.ts`, `e2e/tests-flows/*.spec.ts`, `transport/ws/*_test.go` (only where they name the tag), `docs/benchmarks/capacity.md`, `docs/providers.md`

**Interfaces:**
- Consumes: the `providers.echo` config. Harness configs set `mimic: instant` unless the harness is about UX realism.

- [ ] **Step 1: loadtest.** The server child is built without `-tags` (`./cmd/server`); the generated server config gains `providers: {echo: {mimic: instant, stt: audio}}`; the placeholder `GOOGLE_*` variables, `GOVOICE_FAKE_STT` and the `sa.json` file are removed; the flows it creates use `"provider": "echo"` (`model`/`voice` any string, as now). Add `-llm-delay` (duration, default 0) that writes `providers.echo.llm_extra_delay` into that config. Update `loadtest_test.go` accordingly and its comments.
- [ ] **Step 2: perf gate.** `scripts/perf-gate.sh` self-test: replace `( export GOVOICE_FAKE_LLM_DELAY=100ms; expect 1 load-injected load -compare "$LB" )` with `expect 1 load-injected load -llm-delay 100ms -compare "$LB"`. `deploy/docker-compose.perf.yaml`: delete `GOVOICE_FAKE_STT=audio`; `config/perf.yaml`: add `providers:` → `echo: {mimic: instant, stt: audio}` (merge with an existing `providers:` block if present) and make its flows use `echo` (check how the compose perf run creates flows).
- [ ] **Step 3: livecheck.** The `fake` profile becomes `echo` profile: `stages: stt {provider: echo, model: chirp_2, language: en-US}`, `llm {provider: echo, model: gemini-2.5-flash}`, `tts {provider: echo, voice: en-US-Chirp3-HD-Aoede}`; remove `build_tags: [fakeproviders]` and the rule in `profiles.go` that a fake profile must list it (a fake profile now needs no tag); `server.go`: the `sp.fake` branch writes the echo config (`mimic: instant, stt: audio`) into the server config instead of placeholders and env; drop `GOVOICE_FAKE_LLM_DELAY` from its pass-through list (the self-test's delay variable is gone: if the self-test needs an injected delay, the livecheck config template sets `llm_extra_delay`); update `profiles_test.go` (`tagUnion` expectations) and the docs strings that mention the tag.
- [ ] **Step 4: pipecat eval.** `run.sh`: `go build -o "$OUT/server-fake" ./cmd/server` (no tag), delete the `GOOGLE_*` placeholders and `sa.json`, write the server config with `providers.echo` (instant, audio) and create the flow with `"provider":"echo"`.
- [ ] **Step 5: browser e2e.** `global-setup-flows.ts`: build without `fakeproviders` (`xaispeech` only for both modes); in fake mode the provider environment is empty (`providerEnv = {}`), and the server config (the string it already templates) gains `providers: { echo: { mimic: instant, label_replies: true } }`; delete the `GOOGLE_*`/`sa.json`/`GOVOICE_FAKE_LLM_LABEL` lines. `flows-stack.ts`: `PROVIDER = LIVE ? "xai" : "echo"`. In the specs, voice `brokenvoice` is unchanged; `10-providers.spec.ts` asserts "only the suite's provider is available": with echo detected and nothing else it holds as written. Update comments that say "fake provider".
- [ ] **Step 6: Run every gate**

```bash
make verify
make test-e2e-go
make e2e-fake
make live-check-selftest
make perf-gate-selftest
make eval-pipecat
```

Expected: each passes. `make perf-baseline` must NOT be needed: if `perf-gate` fails on a latency row against the stored baseline, the `instant` figures differ from the old fake (word-sized chunks, first-delay plumbing); fix the figures, never the baseline.

- [ ] **Step 7: Prove the realism by hand and by a test.** Add `TestEchoGoogleLatencyAgainstBenchmarks` in `server/registry_echo_test.go`: a real `core.Session` with the three echo stages (google mimic, scaled by a `timeScale` of 0.1 so it runs in a second) — assert that for the default config the order and rough ratios match `docs/benchmarks/first-audio-latency.md`: STT final after stop is the largest fixed cost, LLM first delta follows its delay, first TTS audio of an 11-character unary unit is later than a Chirp3-HD unit. Then start the server with the default config, open the browser UI, call a flow on `echo` and listen: with `audible: true` and the default google mimic the call should feel like a slow-ish real provider.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "refactor: every harness uses the echo provider on the normal build; GOVOICE_FAKE_* and the fakeproviders tag are gone

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Docs, the last sweep, and independent review

**Files:**
- Modify: `docs/providers.md`, `docs/benchmarks/capacity.md`, `docs/benchmarks/baseline.md` (wording only), `README`/`docs/quickstart.md` where they mention fakes

**Interfaces:**
- Consumes: everything above.

- [ ] **Step 1: Docs.** In `docs/providers.md` add an "Echo (simulated)" section: what it is, the four config keys with defaults, the profiles and the measured figures it imitates (with the benchmark doc links), the text-versus-audio STT modes and the placeholder-words caveat, `brokenvoice`, and "never use for real callers; it is labelled simulated in the UI". Quickstart: "Try it without a key": start the server, pick `Echo (simulated)` in First run, call it. Replace the "fake providers" wording in `docs/benchmarks/capacity.md` and the README targets.
- [ ] **Step 2: Sweep**

```bash
rg -n "fakeproviders|GOVOICE_FAKE_" --glob '!docs/superpowers/**' --glob '!.git' . ; echo "exit $?"
```

Expected: no matches (exit 1). Fix every hit that is not an old plan or spec under `docs/superpowers/`.
- [ ] **Step 3: Full gate**

```bash
gofmt -w . && make verify && make test-e2e-go && make e2e-fake
```

Expected: all green, clean `git status`.
- [ ] **Step 4: Independent review (AGENTS.md).** Dispatch a fresh reviewer on `main..HEAD` with: the YAGNI and bloat audit (is every `Fake*` field added consumed by echo? any leftover `fakeproviders` code; duplicate catalog data and the drift test), the correctness and concurrency audit (delayed finals and first-token delays under interruption and teardown, no goroutine leak, generation fencing, no audio retained, `-race`), the five Review Focus items above, and the honesty check that echo cannot be mistaken for a real provider in the UI or API. Fix confirmed findings, each with a test that fails first.
- [ ] **Step 5: Commit**

```bash
git add docs
git commit -m "docs: the echo provider, what it imitates and how to tune it

Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
