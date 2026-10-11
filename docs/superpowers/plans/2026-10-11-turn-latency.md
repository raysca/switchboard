# Turn Latency (Pipecat Parity) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turns end Pipecat-style. The turn manager is on by default, with a transcript-gated speech timeout, per-provider STT p99 latencies and a 200 ms VAD stop. The choice of Silero model is backed by an offline comparison.

**Architecture:**
- **core/turn:** `speech-timeout` becomes a stateless, transcript-gated `Detect` over the existing `DetectionInput` (silence from onset, text, final flag), configured per call by `SpeechTimeoutConfig`. The per-provider p99 table lives in `core/turn`, next to the strategy, following the `core.SlackDefaults(provider)` precedent.
- **server:** resolves the call's VAD stop and STT p99 and builds the strategy. The defaults change to manager on and `vad.stop_ms` 200.
- **Pipecat parity goldens:** the two scenarios declared different for lack of a transcript gate now match.
- **Silero comparison:** an opt-in `vad` package test plus a Python probe using Pipecat's own environment.

**Tech Stack:** Go 1.27.1. Python 3.12 from `pipecat-impl/.venv` in the main checkout, used only for the Silero probe.

**Spec:** `docs/superpowers/specs/2026-10-11-turn-latency-design.md` (approved 2026-10-11).

## Global Constraints

- **The turn-ending rule.** `Detect` ends the turn, reason `silence`, when either:
  - all three of these hold:
    - `SilenceFor >= VADStop + UserSpeechTimeout`;
    - `strings.TrimSpace(Text) != ""`;
    - `TextFinal || SilenceFor >= STTP99`;
  - or `SilenceFor >= NoTranscriptTimeout`.

  `SilenceFor` counts from silence onset.
- **Zero-value defaults:** `UserSpeechTimeout` 600 ms, `NoTranscriptTimeout` 5 s, `STTP99` 1 s. `VADStop` zero means 0, because the registry always sets it.
- **The p99 table** (`core/turn`): google 1.57 s, xai 2.14 s, deepgram 0.35 s, cartesia 0.81 s, elevenlabs 0.41 s, nvidia 1.0 s; anything else 1.0 s.
- **p99 resolution order:** the agent's `stt_p99_ms` (100 to 10000, 0 unset), then the server's `turn.stt_p99` (provider → duration), then the table. A call with fallbacks uses its primary STT provider.
- **New defaults:** `turn.manager` true; `vad.stop_ms` 200.
- **Zero still means default** for `turn.speech_timeout` (600 ms resume window) and `turn.no_transcript_timeout` (new, 5 s). `DefaultConfig` keeps them at zero, so `turn.manager: false` remains valid without clearing other keys. Validation keeps rejecting turn keys when the manager is off.
- **Startup warning** when `turn.manager` is false and `vad.stop_ms` is below 500.
- **`turn.speech_timeout` keeps its YAML name;** its documented meaning becomes "resume window after the VAD stop".
- **Unchanged:** push-to-talk (ExternalStop) and the smart-turn analyzer's model path. The analyzer's fallback base becomes `VADStop + UserSpeechTimeout`.
- **Commands.** Go commands run from the repo root. Darwin cgo needs `CGO_LDFLAGS=-Wl,-w`. VAD tests in a worktree need `GOVOICE_ORT_LIB=/Users/raymondottun/Projects/go-voice/third_party/onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.dylib`. Check exit codes, not `tail` output.
- **Commits:** one per task, with only the named files. End each message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

- **A long hesitation with no transcript yet.** Interim text has arrived, then the user pauses longer than the resume window but shorter than p99, and the final is late. The turn must not end at the resume window. It ends when the final arrives or at p99. Task 2 tests this (`TestSpeechTimeoutInterimWaitsForFinalOrP99`).
- **STT dead: VAD speech, no transcript ever.** The turn must end at `NoTranscriptTimeout` (5 s), not hang until `MaxTurnDuration` (60 s). Task 2 tests this through the processor.
- **The user resumes inside the resume window, after a final.** A final transcript arrives, then the user speaks again 300 ms after the VAD stop. That must be one turn, not two. Task 2 tests this.
- **`turn.manager: false` in an existing YAML after upgrade.** It must still load (no "needs turn.manager" error from default values), and with the 200 ms default VAD stop it must log the warning. Task 5 tests this.
- **A provider with a p99 below the resume window** (deepgram 0.35 s). The resume window dominates and the turn ends at `VADStop + 600 ms` once text exists. Task 1's table test includes it.

---

### Task 1: Transcript-gated `SpeechTimeout` and the p99 table (core/turn)

**Files:**
- Modify: `core/turn/stop.go` (lines 46-59: `SpeechTimeout`, `speechTimeout`)
- Create: `core/turn/sttp99.go`
- Modify: `core/turn/stop_test.go` (the `SpeechTimeout(` rows at lines 26-28 and the chains at 53, 62, 117, 179)
- Test: `core/turn/speech_timeout_test.go` (create)

**Interfaces:**
- Produces:
  - `type SpeechTimeoutConfig struct { VADStop, UserSpeechTimeout, STTP99, NoTranscriptTimeout time.Duration }`
  - `func SpeechTimeout(cfg SpeechTimeoutConfig) UserTurnStopStrategy`
  - `func DefaultSTTP99(provider string) time.Duration`
  - `const DefaultUserSpeechTimeout = 600 * time.Millisecond`
  - `const DefaultNoTranscriptTimeout = 5 * time.Second`
  - `const UnknownSTTP99 = time.Second`

- [ ] **Step 1: Write the failing tests** (create `core/turn/speech_timeout_test.go`)

```go
package turn

import (
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestSpeechTimeoutRule(t *testing.T) {
	cfg := SpeechTimeoutConfig{VADStop: ms(200), UserSpeechTimeout: ms(600), STTP99: ms(1570), NoTranscriptTimeout: 5 * time.Second}
	for _, c := range []struct {
		name  string
		in    DetectionInput
		cfg   SpeechTimeoutConfig
		stops bool
	}{
		{"resume window not reached", DetectionInput{SilenceFor: ms(799), Text: "hi", TextFinal: true}, cfg, false},
		{"final text at the resume window", DetectionInput{SilenceFor: ms(800), Text: "hi", TextFinal: true}, cfg, true},
		{"no text yet", DetectionInput{SilenceFor: ms(1600)}, cfg, false},
		{"whitespace is no text", DetectionInput{SilenceFor: ms(1600), Text: "  ", TextFinal: true}, cfg, false},
		{"interim text before p99", DetectionInput{SilenceFor: ms(1569), Text: "hi"}, cfg, false},
		{"interim text at p99", DetectionInput{SilenceFor: ms(1570), Text: "hi"}, cfg, true},
		{"no transcript fallback", DetectionInput{SilenceFor: 5 * time.Second}, cfg, true},
		{"fast STT: resume window dominates", DetectionInput{SilenceFor: ms(800), Text: "hi"}, SpeechTimeoutConfig{VADStop: ms(200), STTP99: ms(350)}, true},
		{"fast STT: still waits the window", DetectionInput{SilenceFor: ms(700), Text: "hi"}, SpeechTimeoutConfig{VADStop: ms(200), STTP99: ms(350)}, false},
		{"zero config: 600ms window from onset", DetectionInput{SilenceFor: ms(600), Text: "hi", TextFinal: true}, SpeechTimeoutConfig{}, true},
		{"zero config: 1s p99", DetectionInput{SilenceFor: ms(999), Text: "hi"}, SpeechTimeoutConfig{}, false},
		{"zero config: 5s fallback", DetectionInput{SilenceFor: 5 * time.Second}, SpeechTimeoutConfig{}, true},
	} {
		d, ok := SpeechTimeout(c.cfg).Detect(c.in)
		if ok != c.stops {
			t.Errorf("%s: stops=%t, want %t", c.name, ok, c.stops)
		}
		if ok && d.Reason != core.TurnReasonSilence {
			t.Errorf("%s: reason %v, want silence", c.name, d.Reason)
		}
	}
}

func TestSpeechTimeoutInterimWaitsForFinalOrP99(t *testing.T) {
	st := SpeechTimeout(SpeechTimeoutConfig{VADStop: ms(200), STTP99: ms(2140)})
	if _, ok := st.Detect(DetectionInput{SilenceFor: ms(1500), Text: "book a table"}); ok {
		t.Fatal("ended on interim text before p99")
	}
	if _, ok := st.Detect(DetectionInput{SilenceFor: ms(1500), Text: "book a table for two", TextFinal: true}); !ok {
		t.Fatal("did not end when the final arrived after the resume window")
	}
}

func TestDefaultSTTP99(t *testing.T) {
	for p, want := range map[string]time.Duration{"google": ms(1570), "xai": ms(2140), "deepgram": ms(350), "cartesia": ms(810), "elevenlabs": ms(410), "nvidia": time.Second, "unknown": time.Second} {
		if got := DefaultSTTP99(p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}
```

Add `"govoice/core"` to the imports. If `ms` already exists in the package's tests (search for `func ms(`), drop the local definition.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./core/turn/ -run 'TestSpeechTimeout|TestDefaultSTTP99' -count=1`
Expected: build failure (`SpeechTimeoutConfig` and `DefaultSTTP99` are undefined).

- [ ] **Step 3: Implement**

Create `core/turn/sttp99.go`:

```go
package turn

import "time"

// UnknownSTTP99 is the p99 for a provider without a measured value (Pipecat DEFAULT_TTFS_P99).
const UnknownSTTP99 = time.Second

// sttP99 is each STT provider's p99 time from end of speech to final transcript, seeded from
// Pipecat's benchmark (pipecat-impl/src/pipecat/services/stt_latency.py; elevenlabs is its
// realtime Scribe figure). The nightly live job reports our measured value next to these.
var sttP99 = map[string]time.Duration{
	"google":     1570 * time.Millisecond,
	"xai":        2140 * time.Millisecond,
	"deepgram":   350 * time.Millisecond,
	"cartesia":   810 * time.Millisecond,
	"elevenlabs": 410 * time.Millisecond,
	"nvidia":     time.Second,
}

// DefaultSTTP99 returns the provider's built-in p99 (UnknownSTTP99 when not listed).
func DefaultSTTP99(provider string) time.Duration {
	if d, ok := sttP99[provider]; ok {
		return d
	}
	return UnknownSTTP99
}
```

In `core/turn/stop.go`, replace lines 46-59 (the `SpeechTimeout` comment, the constructor, the `speechTimeout` type and its `Detect`) with:

```go
// Defaults for SpeechTimeoutConfig zero values (Pipecat user_speech_timeout and the aggregator's
// user_turn_stop_timeout).
const (
	DefaultUserSpeechTimeout   = 600 * time.Millisecond
	DefaultNoTranscriptTimeout = 5 * time.Second
)

// SpeechTimeoutConfig sets the speech-timeout stop for one call. All durations compare with
// DetectionInput.SilenceFor, which counts from silence onset.
type SpeechTimeoutConfig struct {
	VADStop             time.Duration // the call's VAD stop: onset to the VAD stop event
	UserSpeechTimeout   time.Duration // resume window after the VAD stop (0: 600ms)
	STTP99              time.Duration // primary STT p99, end of speech to final (0: UnknownSTTP99)
	NoTranscriptTimeout time.Duration // end an untranscribed turn after this silence (0: 5s)
}

// SpeechTimeout is Pipecat's speech-timeout stop: the turn ends once the user has been silent
// for the VAD stop plus the resume window, a transcript exists, and it is final or the STT's p99
// has passed. A turn with no transcript ends after NoTranscriptTimeout (dropped downstream), so
// a dead STT cannot wedge it.
func SpeechTimeout(cfg SpeechTimeoutConfig) UserTurnStopStrategy {
	if cfg.UserSpeechTimeout <= 0 {
		cfg.UserSpeechTimeout = DefaultUserSpeechTimeout
	}
	if cfg.STTP99 <= 0 {
		cfg.STTP99 = UnknownSTTP99
	}
	if cfg.NoTranscriptTimeout <= 0 {
		cfg.NoTranscriptTimeout = DefaultNoTranscriptTimeout
	}
	return speechTimeout(cfg)
}

type speechTimeout SpeechTimeoutConfig

func (s speechTimeout) Detect(in DetectionInput) (DetectionDecision, bool) {
	stop := DetectionDecision{Reason: core.TurnReasonSilence}
	if in.SilenceFor >= s.NoTranscriptTimeout {
		return stop, true
	}
	if in.SilenceFor < s.VADStop+s.UserSpeechTimeout || strings.TrimSpace(in.Text) == "" {
		return DetectionDecision{}, false
	}
	if in.TextFinal || in.SilenceFor >= s.STTP99 {
		return stop, true
	}
	return DetectionDecision{}, false
}
```

`stop.go` already imports `strings` (`endsSentence` uses it) and `core`. Add `time` if it is missing.

**A check on `SilenceFor`.** If `DetectionInput.SilenceFor` is zero while VAD still hears speech, `Detect` returns false, which is correct. The manager only calls detectors once a silence onset exists (`hasSilenceSince`). Confirm this by reading `manager.go` around line 399, and do not add a separate speaking check.

Update the old callers in `core/turn/stop_test.go`. Each old row set `SilenceAfter`:
- The three `SpeechTimeout(...)` rows at lines 26-28 become `TestSpeechTimeoutRule`'s job. Delete them, and keep the other rows in that table.
- The chains at lines 53, 62, 117 and 179 use `SpeechTimeout(0)`. Replace each with `SpeechTimeout(SpeechTimeoutConfig{})`.
- Then re-check each affected test's expectations:
  - Line 117 (an eager/silence chain test) and 179 (`newStopHarness` with eager): if they assert that the silence fallback ends a turn with no transcript at 700 ms, they now need a final transcript, or `UserSpeechTimeout` set to the old value. Give each the smallest input change that keeps its intent, and note any change in the commit body.

- [ ] **Step 4: Run the package**

Run: `go test ./core/turn/ -count=1`
Expected: PASS. If a processor test (`processor_test.go` and the like) fails because it relied on transcript-less silence ending a turn through the default `SilenceDetector`, it is unaffected: those harnesses use `SilenceDetector` unless they pass `SpeechTimeout`. Only fix tests that construct `SpeechTimeout`.

- [ ] **Step 5: Commit**

```bash
git add core/turn/stop.go core/turn/sttp99.go core/turn/stop_test.go core/turn/speech_timeout_test.go
git commit -m "feat(turn): transcript-gated speech timeout and per-provider STT p99 (Pipecat parity)"
```

---

### Task 2: Processor timing tests for the gated stop (core/turn)

**Files:**
- Test: `core/turn/speech_timeout_processor_test.go` (create)

**Interfaces:**
- Consumes:
  - `SpeechTimeout`, `SpeechTimeoutConfig` (Task 1);
  - `newStopHarness(t, cfg, det)`, `(*harness).run`, `settle`, `events` (in `stop_test.go` and `processor_test.go`);
  - `newCommitHarness` and `(*harness).waitCommit` (in `commit_test.go`).

These tests drive behaviour from Task 1 through the real processor. They are expected to pass when written. Step 3 breaks the code on purpose to prove each one.

- [ ] **Step 1: Write the tests**

```go
package turn

import (
	"testing"
	"time"

	"govoice/core"
)

// gated is the production stop with a 200ms VAD stop and Google's p99.
func newGatedHarness(t *testing.T) *harness {
	return newStopHarness(t, DefaultConfig(), SpeechTimeout(SpeechTimeoutConfig{VADStop: 200 * time.Millisecond, STTP99: 1570 * time.Millisecond}))
}

func (h *harness) candidate(seg, text string, final bool) {
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: core.TurnID(seg), Gen: 1, Text: text, Final: final}, core.Downstream, 1, 1)
	h.settle()
}

// A final that arrives before the window closes: the turn ends at VADStop + 600ms.
func TestGatedStopEndsAtResumeWindow(t *testing.T) {
	h := newGatedHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.candidate("turn-1-1", "what time is it", true)
	stopAt := h.clk.Now()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("turn never ended")
	}
	if got := h.clk.Now().Sub(stopAt); got < 580*time.Millisecond || got > 700*time.Millisecond {
		t.Fatalf("ended %v after the VAD stop, want about 600ms (VADStop+600ms from onset)", got)
	}
}

// Interim text only: the turn waits for the final (or p99), not the resume window.
func TestGatedStopWaitsForLateFinal(t *testing.T) {
	h := newGatedHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.candidate("turn-1-1", "book a table", false)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	if h.run(time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) > 0 }) {
		t.Fatal("ended on interim text before the final or p99")
	}
	h.candidate("turn-1-1", "book a table for two", true)
	if !h.run(500*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("the late final did not end the turn")
	}
}

// No transcript at all: the turn ends at the 5s fallback, not at MaxTurnDuration.
func TestGatedStopNoTranscriptFallback(t *testing.T) {
	h := newGatedHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	if h.run(4*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) > 0 }) {
		t.Fatal("ended before the no-transcript fallback")
	}
	if !h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("no-transcript turn never ended")
	}
}

// Speech resumes 300ms after a VAD stop that followed a final: still one turn.
func TestGatedStopResumeInsideWindowIsOneTurn(t *testing.T) {
	h := newGatedHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	h.candidate("turn-1-1", "I would like", true)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	h.run(100*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	h.settle()
	h.candidate("turn-1-3", "a coffee", true)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1, SilenceDuration: 200 * time.Millisecond})
	h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 })
	if s, c := len(h.events(core.TurnEventStarted)), len(h.events(core.TurnEventCompleted)); s != 1 || c != 1 {
		t.Fatalf("started=%d completed=%d, want one turn", s, c)
	}
}
```

- [ ] **Step 2: Run them**

Run: `go test ./core/turn/ -run TestGatedStop -count=1 -v`
Expected: PASS (4 tests).
- **If `TestGatedStopEndsAtResumeWindow` misses the window,** read how the processor turns `SilenceDuration` into `SilenceSince` (`processor.go`, the `UserStoppedSpeakingFrame` case). Adjust the expected window to "VAD stop event + `UserSpeechTimeout` + `VADStop` − `SilenceDuration`", which is exactly 600 ms here because `SilenceDuration` equals `VADStop`. Do not change production code to make the test pass.
- **If a test fails because the harness's candidate doesn't reach the manager** (the turn id is wrong, for example), match the segment ids `onVADStart` derives (`turn-<gen>-<seq>`).

- [ ] **Step 3: Break the code on purpose to prove each test (do not commit the breakage)**

1. In `core/turn/stop.go`, temporarily delete `|| strings.TrimSpace(in.Text) == ""`. `TestGatedStopWaitsForLateFinal` and `TestGatedStopNoTranscriptFallback` must FAIL.
2. Restore it. Temporarily change `in.SilenceFor >= s.NoTranscriptTimeout` to `false`. `TestGatedStopNoTranscriptFallback` must FAIL.
3. Restore it. Temporarily drop `s.VADStop+` from the window check. `TestGatedStopEndsAtResumeWindow` must FAIL.
4. Restore it. Run `git diff core/turn/stop.go`. It must print nothing.

- [ ] **Step 4: Race-check the package**

Run: `go test ./core/turn/ -race -count=3`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add core/turn/speech_timeout_processor_test.go
git commit -m "test(turn): gated speech timeout through the processor"
```

---

### Task 3: Pipecat parity fixtures and the other `SpeechTimeout` callers

**Files:**
- Modify: `tools/compat/parity_fixtures_test.go:179`
- Modify: `tools/compat/testdata/parity/turn.json` (remove the `intentional_difference` of `speech_timeout_waits_for_transcript` and `speech_timeout_no_transcript_never_stops`)
- Modify: `tools/compat/parity/gen_goldens.py` (remove those two entries from `DIFFS`, lines 508-513)
- Modify: `tools/eval/turnquality/turnquality.go:159`
- Modify: `docs/pipecat-parity-matrix.md` (line 9: "16 with a declared" becomes "14 with a declared"; the "Stop: speech timeout" row)

**Interfaces:**
- Consumes: `turn.SpeechTimeout`, `turn.SpeechTimeoutConfig` (Task 1).

- [ ] **Step 1: Point the fixture driver at the new strategy and watch the two declared differences fail**

In `tools/compat/parity_fixtures_test.go`, replace `m, err := turn.NewManager(cfg, turn.SpeechTimeout(0))` with:

```go
		// Pipecat's speech-timeout strategy alone: vad_stop events are explicit (no VAD stop
		// offset), stop_ms is user_speech_timeout, finals are finalized (no STT p99 wait), and
		// the aggregator's no-transcript timeout is outside the strategy.
		m, err := turn.NewManager(cfg, turn.SpeechTimeout(turn.SpeechTimeoutConfig{
			UserSpeechTimeout: ms(p.StopMs), STTP99: time.Hour, NoTranscriptTimeout: time.Hour,
		}))
```

`STTP99: time.Hour` models Pipecat's fixtures, which send finalized transcripts. Interim-only turns would never end, as in Pipecat without an `STTMetadataFrame`.

Run (from the main checkout, where `pipecat-impl` exists, against this worktree's files): `cd tools/compat && go test -run TestParityTurnStartStop -count=1 .`
Expected: FAIL on `speech_timeout_waits_for_transcript` and `speech_timeout_no_transcript_never_stops`. The go-voice output now equals Pipecat's, so it no longer equals the declared difference.

In the worktree the test needs `pipecat-impl` only for `TestParityGoldensPinToReference`; `TestParityTurnStartStop` reads only `testdata/`, so it runs in the worktree too.

- [ ] **Step 2: Remove the two declared differences**

In `tools/compat/testdata/parity/turn.json`, delete the `"intentional_difference": {...}` object from both scenarios, leaving each scenario's other keys intact. Use `python3` with the `json` module, writing it back with the file's existing indentation (check the first lines for the indent width), so only those keys change. In `tools/compat/parity/gen_goldens.py`, delete the two `DIFFS` entries for those ids.

Run: `cd tools/compat && go test -run TestParityTurnStartStop -count=1 .`
Expected: PASS (the transcription and wake scenarios are still skipped).

- [ ] **Step 3: Update the other callers and the matrix**

`tools/eval/turnquality/turnquality.go:159`: replace `coreturn.SpeechTimeout(700 * time.Millisecond)` with:

```go
	d := coreturn.SpeechTimeout(coreturn.SpeechTimeoutConfig{UserSpeechTimeout: 700 * time.Millisecond})
```

Then read 20 lines around it. If the evaluator feeds transcript-less observations and expects silence alone to end turns, keep that behaviour: also set `NoTranscriptTimeout: 700 * time.Millisecond`. Record which you chose in the commit body.

`docs/pipecat-parity-matrix.md`:
- Line 9: change `16 with a declared` to `14 with a declared`.
- In the "Stop: speech timeout" row:
  - set the status cell to `parity`;
  - set the default cell to `on: turn.manager default; resume window turn.speech_timeout (600 ms), STT p99 per provider, no-transcript fallback 5 s`;
  - set the notes to `Same rule: stop when the resume window after the VAD stop has passed, a transcript exists, and it is final or the STT p99 has passed. go-voice treats a final transcript as finalized (Pipecat distinguishes them) and adds a 5 s no-transcript fallback in the strategy (Pipecat's user_turn_stop_timeout lives in the aggregator).`

Run: `go build ./tools/... && cd tools/compat && go test -run 'TestParityTurnStartStop|TestParityMatrix' -count=1 .`
Expected: the build succeeds and `TestParityTurnStartStop` passes. `TestParityMatrix` passes on the main checkout; in the worktree it reports only missing-`pipecat-impl` path errors. Count the non-path errors (there must be 0) as in the earlier plans.

- [ ] **Step 4: Commit**

```bash
git add tools/compat/parity_fixtures_test.go tools/compat/testdata/parity/turn.json tools/compat/parity/gen_goldens.py tools/eval/turnquality/turnquality.go docs/pipecat-parity-matrix.md
git commit -m "test(compat): speech timeout now matches Pipecat's transcript gate"
```

---

### Task 4: Server wiring (config, agent override, stop chain)

**Files:**
- Modify: `server/config.go` (the `Turn` struct: `NoTranscriptTimeout`, `STTP99`; the `SpeechTimeout` comment)
- Modify: `server/agent/config.go` (`STTP99MS` with validation, next to `VADStopMS` at lines 23-24 and 171-173)
- Modify: `server/registry.go` (`CallSpec.STTP99MS`; turn block lines ~566-598)
- Modify: `server/call.go:203` (pass `STTP99MS` from the agent)
- Modify: `server/turn_stop.go` (signature and body)
- Modify: `server/config_validate.go:141` (call site; new validation)
- Test: `server/turn_stop_test.go` (update the calls; new tests)

**Interfaces:**
- Consumes: `coreturn.SpeechTimeout`, `SpeechTimeoutConfig`, `DefaultSTTP99`, `DefaultUserSpeechTimeout` (Task 1).
- Produces:
  - `turnStopDetector(stop []string, st coreturn.SpeechTimeoutConfig, eagerGrace time.Duration, analyzer func(base time.Duration) coreturn.UserTurnStopStrategy) (coreturn.TurnDetector, error)`;
  - `(*Config).sttP99(provider string, agentMS int) time.Duration`;
  - `(*Config).vadStop(agentMS int) time.Duration`.

- [ ] **Step 1: Write the failing tests** (append to `server/turn_stop_test.go`)

```go
func TestSTTP99Resolution(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	if got := cfg.sttP99("google", 0); got != 1570*time.Millisecond {
		t.Fatalf("table: %v", got)
	}
	cfg.Turn.STTP99 = map[string]time.Duration{"google": 900 * time.Millisecond}
	if got := cfg.sttP99("google", 0); got != 900*time.Millisecond {
		t.Fatalf("server override: %v", got)
	}
	if got := cfg.sttP99("google", 1200); got != 1200*time.Millisecond {
		t.Fatalf("agent override: %v", got)
	}
	if got := cfg.sttP99("someone-new", 0); got != time.Second {
		t.Fatalf("unknown provider: %v", got)
	}
}

func TestVADStopResolution(t *testing.T) {
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.VAD.StopMS = 800
	if got := cfg.vadStop(0); got != 800*time.Millisecond {
		t.Fatalf("server: %v", got)
	}
	if got := cfg.vadStop(300); got != 300*time.Millisecond {
		t.Fatalf("agent: %v", got)
	}
}

// An empty turn.stop still builds the gated speech timeout (not the bare silence detector).
func TestEmptyTurnStopIsGatedSpeechTimeout(t *testing.T) {
	d, err := turnStopDetector(nil, coreturn.SpeechTimeoutConfig{VADStop: 200 * time.Millisecond, STTP99: time.Second}, 0, nil)
	if err != nil || d == nil {
		t.Fatalf("d=%v err=%v", d, err)
	}
	if _, ok := d.Detect(coreturn.DetectionInput{SilenceFor: 2 * time.Second}); ok {
		t.Fatal("ended a transcript-less turn before the no-transcript fallback")
	}
	if _, ok := d.Detect(coreturn.DetectionInput{SilenceFor: 800 * time.Millisecond, Text: "hi", TextFinal: true}); !ok {
		t.Fatal("did not end a transcribed turn at the resume window")
	}
}
```

Update the existing `turnStopDetector(...)` calls in this file: `(stop, speechTimeout, eagerGrace, analyzer)` becomes `(stop, coreturn.SpeechTimeoutConfig{UserSpeechTimeout: speechTimeout}, eagerGrace, analyzer)`, keeping each test's values. The test at line 27 asserted that `turnStopDetector(nil, 0, 0, nil)` returns `nil`. That now returns the gated strategy, so change it to assert non-nil and note why in a comment: an empty chain is the gated speech timeout.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./server/ -run 'TestSTTP99Resolution|TestVADStopResolution|TestEmptyTurnStop' -count=1`
Expected: build failure (`sttP99`, `vadStop` and the new signature are undefined).

- [ ] **Step 3: Implement**

`server/config.go`, in the `Turn` struct:
- Replace the `SpeechTimeout` field's comment with: `// SpeechTimeout is the resume window after the VAD stop (Pipecat user_speech_timeout; 0: 600ms).`
- Add these fields after `EagerGrace`:

```go
		// NoTranscriptTimeout ends a turn that never got a transcript after this much silence
		// (0: 5s), so a dead STT cannot hold a turn open until max_turn_duration.
		NoTranscriptTimeout time.Duration `yaml:"no_transcript_timeout"`
		// STTP99 overrides the built-in per-provider STT p99 (end of speech to final transcript)
		// the speech timeout waits for when the final is late.
		STTP99 map[string]time.Duration `yaml:"stt_p99"`
```

Add next to `VADProcessorConfigForStopMS`:

```go
// vadStop is the call's VAD stop: the agent override (ms > 0), else vad.stop_ms, rounded up to
// whole 32 ms chunks like the VAD processor.
func (c *Config) vadStop(agentMS int) time.Duration {
	pc := c.VADProcessorConfigForStopMS(agentMS)
	return time.Duration(pc.StopChunks*vad.ChunkSamples*1000/vad.SampleRate) * time.Millisecond
}

// sttP99 resolves the primary STT provider's p99: agent override, server map, built-in table.
func (c *Config) sttP99(provider string, agentMS int) time.Duration {
	if agentMS > 0 {
		return time.Duration(agentMS) * time.Millisecond
	}
	if d, ok := c.Turn.STTP99[provider]; ok && d > 0 {
		return d
	}
	return coreturn.DefaultSTTP99(provider)
}
```

`TestVADStopResolution` expects exactly 800 ms and 300 ms. 800 is 25 chunks of 32 ms. 300 rounds up to 10 chunks, which is 320 ms, so change that test's expectation to `320*time.Millisecond` and its message to "agent, rounded to 32 ms chunks". Add the `coreturn` import to `config.go` if missing (it is `govoice/core/turn`).

`server/agent/config.go`: add after `VADStopMS`:

```go
	// STTP99MS overrides the STT p99 the speech timeout waits for (100..10000 ms). 0 uses the
	// server's turn.stt_p99 or the built-in table.
	STTP99MS int `json:"stt_p99_ms,omitempty"`
```

And add after the `vad_stop_ms` check:

```go
	if c.STTP99MS < 0 || (c.STTP99MS > 0 && (c.STTP99MS < 100 || c.STTP99MS > 10000)) {
		return c, fmt.Errorf("%w: stt_p99_ms must be 0 (default) or between 100 and 10000", ErrInvalidConfig)
	}
```

`server/registry.go`: add `STTP99MS int` to `CallSpec` after `VADStopMS`, with the comment `// STTP99MS overrides the STT p99 for this call if > 0.` In `server/call.go` next to `VADStopMS: cfg.VADStopMS,`, add `STTP99MS: cfg.STTP99MS,`.

In the registry's turn block (the `else` branch for hands-free calls), replace:

```go
			if stop, err = turnStopDetector(cfg.Turn.Stop, cfg.Turn.SpeechTimeout, cfg.Turn.EagerGrace, analyzer); err != nil {
```

with:

```go
			st := coreturn.SpeechTimeoutConfig{
				VADStop: cfg.vadStop(call.VADStopMS), UserSpeechTimeout: cfg.Turn.SpeechTimeout,
				STTP99: cfg.sttP99(call.STT.Provider, call.STTP99MS), NoTranscriptTimeout: cfg.Turn.NoTranscriptTimeout,
			}
			if stop, err = turnStopDetector(cfg.Turn.Stop, st, cfg.Turn.EagerGrace, analyzer); err != nil {
```

`server/turn_stop.go`: change the signature to take `st coreturn.SpeechTimeoutConfig` in place of `speechTimeout time.Duration`, and the body to:
- **Empty `stop`:** `return coreturn.SpeechTimeout(st), nil`. This replaces the `if speechTimeout > 0 {...} return nil, nil` branch.
- **`case "speech-timeout":`** with no model, `chain = append(chain, coreturn.SpeechTimeout(st))`.
- **`case "turn-analyzer":`** `chain = append(chain, analyzer(analyzerBase(st)))`, where:

```go
// analyzerBase is the smart-turn fallback silence: the gated speech timeout's resume window
// from onset (VAD stop + user speech timeout).
func analyzerBase(st coreturn.SpeechTimeoutConfig) time.Duration {
	us := st.UserSpeechTimeout
	if us <= 0 {
		us = coreturn.DefaultUserSpeechTimeout
	}
	return st.VADStop + us
}
```

Update the function comment: "Empty means the gated speech timeout (core/turn SpeechTimeout)".

`server/config_validate.go:141`: change the call to `turnStopDetector(cfg.Turn.Stop, coreturn.SpeechTimeoutConfig{UserSpeechTimeout: cfg.Turn.SpeechTimeout}, cfg.Turn.EagerGrace, func(time.Duration) coreturn.UserTurnStopStrategy { return nil })`. In the same function, extend the negative-duration check to cover the new keys:

```go
	if cfg.Turn.SpeechTimeout < 0 || cfg.Turn.EagerGrace < 0 || cfg.Turn.NoTranscriptTimeout < 0 {
		return errors.New("turn.speech_timeout, turn.eager_grace and turn.no_transcript_timeout must be >= 0")
	}
	for p, d := range cfg.Turn.STTP99 {
		if d < 100*time.Millisecond || d > 10*time.Second {
			return fmt.Errorf("turn.stt_p99.%s must be between 100ms and 10s", p)
		}
	}
```

Extend the "need turn.manager: true" condition with `|| cfg.Turn.NoTranscriptTimeout > 0 || len(cfg.Turn.STTP99) > 0`, and its message with `turn.no_transcript_timeout, turn.stt_p99`.

- [ ] **Step 4: Run the server module**

Run: `go vet ./server/... && CGO_LDFLAGS=-Wl,-w go test ./server/... -count=1`
Expected: PASS.
- **A server pipeline test that ends turns by silence without transcripts** (search `turn_stop_test.go`, `registry_test.go`, `idle_test.go`, `dtmf_test.go` for `Turn.Manager = true`) now waits for the 5 s fallback or never ends. Fix the test input (add a final candidate, or set `cfg.Turn.NoTranscriptTimeout` short) rather than the code. List every test you touched in the commit body.
- **`TestTurnAnalyzerThroughCallPipeline`** measured stops against `speech_timeout` 400 ms. Its base is now `vadStop + speech_timeout`. Update its bounds from the formula (VAD stop 800 ms in that test's config unless set) and say so in the commit body.

- [ ] **Step 5: Commit**

```bash
git add server/config.go server/agent/config.go server/registry.go server/call.go server/turn_stop.go server/config_validate.go server/turn_stop_test.go
git commit -m "feat(server): per-call gated speech timeout with VAD stop and STT p99 resolution"
```

Add any other test files from Step 4 to the commit.

---

### Task 5: New defaults, the startup warning, shipped configs

**Files:**
- Modify: `server/config_defaults.go:58` (`cfg.VAD.StopMS = 200`; add `cfg.Turn.Manager = true`)
- Modify: `server/config.go` (`VADProcessorConfig` comment: "0 keeps the 800 ms default" becomes 200; the `Turn` comment "Off by default…")
- Modify: `server/server.go` or `server/management.go` (startup warning; see Step 3)
- Modify: `config/development.yaml` (line 53 `stop_ms: 800` becomes `200`, with its comment; the commented `turn:` block at lines 108-117)
- Modify: `config/production.yaml` (add `stop_ms: 200` under `vad:`, next to `model_path`)
- Modify: `server/testdata/effective_development.json`, `server/testdata/effective_production.json` (regenerated)
- Modify: `docs/quickstart.md` (the turn section)
- Test: `server/config_defaults_test.go` (or the file holding `TestLoadConfig_TurnStopValidatedAtLoad`; append there)

**Interfaces:**
- Consumes: Task 4's config fields.

- [ ] **Step 1: Write the failing tests** (append to `server/config_defaults_test.go`)

```go
func TestDefaultsAreTurnManagerOnWith200msVADStop(t *testing.T) {
	for _, p := range []Profile{ProfileDevelopment, ProfileProduction} {
		c := DefaultConfig(p)
		if !c.Turn.Manager || c.VAD.StopMS != 200 {
			t.Fatalf("%s: manager=%t stop_ms=%d, want true and 200", p, c.Turn.Manager, c.VAD.StopMS)
		}
	}
}

// An existing YAML that turns the manager off still loads with the new defaults.
func TestManagerOffStillLoads(t *testing.T) {
	c := DefaultConfig(ProfileDevelopment)
	c.Turn.Manager = false
	if err := validateTurn(c); err != nil {
		t.Fatalf("turn.manager: false with defaults rejected: %v", err)
	}
	if !lowVADStopWithoutManager(c) {
		t.Fatal("200ms VAD stop with the manager off must warn")
	}
	c.VAD.StopMS = 800
	if lowVADStopWithoutManager(c) {
		t.Fatal("800ms VAD stop with the manager off must not warn")
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./server/ -run 'TestDefaultsAreTurnManagerOn|TestManagerOffStillLoads' -count=1`
Expected: build failure (`lowVADStopWithoutManager` is undefined).

- [ ] **Step 3: Implement**

In `server/config_defaults.go`, replace `cfg.VAD.StopMS = 800` with:

```go
	cfg.VAD.StopMS = 200 // Pipecat VAD_STOP_SECS; the gated speech timeout guards mid-sentence cuts
	cfg.Turn.Manager = true
```

In `server/config_validate.go` (or `config.go`), add:

```go
// lowVADStopWithoutManager reports a config where turns end on a short raw VAD stop: with the
// turn manager off nothing waits for the transcript, so callers get cut off mid-sentence.
func lowVADStopWithoutManager(c *Config) bool {
	ms := c.VAD.StopMS
	if ms == 0 {
		ms = 200
	}
	return !c.Turn.Manager && ms < 500
}
```

Log it once at server start. Find where `s.logger.Warn("retention disabled…")` runs (`server/management.go:102`). In the server constructor that has `s.cfg` and `s.logger` (search `server/server.go` for where `s.logger` is set and the config is stored), add:

```go
	if lowVADStopWithoutManager(s.cfg) {
		s.logger.Warn("turn.manager is off with a short vad.stop_ms: turns end on the raw VAD stop and cut callers off mid-sentence; set turn.manager: true or vad.stop_ms: 800")
	}
```

Update the comments in `server/config.go`:
- `VADProcessorConfig`: "0 keeps the 200 ms default".
- The `Turn` struct comment: "Turn routes live calls through the turn manager (core/turn.Processor). On by default: turns end on the transcript-gated speech timeout (Pipecat parity). Off: turns end on the raw VAD stop, which needs vad.stop_ms around 800."

Shipped configs:
- `config/development.yaml` line 53: `stop_ms: 200         # silence that ends a VAD speech segment (200..2000, 32 ms steps); the turn manager's speech timeout guards mid-sentence cuts`.
- In its commented block:
  - `#   manager: true` becomes `#   manager: true                        # on by default; false ends turns on the raw VAD stop (set vad.stop_ms: 800)`;
  - `#   speech_timeout: 700ms` becomes `#   speech_timeout: 600ms                # resume window after the VAD stop`;
  - add the line `#   no_transcript_timeout: 5s            # end a turn that never got a transcript`;
  - add the line `#   stt_p99: {}                          # provider: duration, overrides the built-in STT p99`.
- `config/production.yaml`: add `  stop_ms: 200` under `vad:`.

Regenerate the goldens and review the diff:

```bash
UPDATE_GOLDEN=1 go test ./server/ -run 'Effective' -count=1 && git diff --stat server/testdata/
```

Expected: only `Turn.Manager` (true) and `VAD.StopMS` (200) change, plus the new zero-valued `Turn.NoTranscriptTimeout` and `Turn.STTP99` keys. Any other change is a bug: stop and investigate.

`TestDevelopmentYAMLExamplesLoad` (development.yaml says "Each block loads as written once uncommented") must still pass with the new commented lines.

`docs/quickstart.md`, in the turn section (search `turn.manager`), state:
- the manager is on by default;
- turns end Pipecat-style: the VAD stop (200 ms) plus a 600 ms resume window, once a transcript exists and is final, or after the STT's p99 (table in `core/turn/sttp99.go`, overridable with `turn.stt_p99` and the agent's `stt_p99_ms`);
- the 5 s no-transcript fallback;
- the upgrade note: `turn.manager: false` restores the old behaviour, but then needs `vad.stop_ms: 800`.

- [ ] **Step 4: Run the server module**

Run: `go vet ./server/... && CGO_LDFLAGS=-Wl,-w go test ./server/... -count=1`
Expected: PASS.
- With the manager on by default, server tests that build pipelines from `DefaultConfig` now get the turn processor. If a test asserts a processor position or count (for example, `procs[2]` being user context), set `cfg.Turn.Manager = false` in that test if it was written for manager-off behaviour, or update the index if the test means "default pipeline".
- List every test you touched in the commit body.

- [ ] **Step 5: Commit**

```bash
git add server/config_defaults.go server/config.go server/config_validate.go server/server.go config/development.yaml config/production.yaml server/testdata/effective_development.json server/testdata/effective_production.json docs/quickstart.md server/config_defaults_test.go
git commit -m "feat(server): turn manager on and 200 ms VAD stop by default (Pipecat parity)"
```

Add any test files Step 4 touched, and `server/management.go` if the warning landed there.

---

### Task 6: Silero comparison and the model decision

**Files:**
- Create: `tools/vadcompare/pipecat_probs.py`
- Create: `vad/compare_test.go`
- Create: `docs/benchmarks/silero-compare.md`
- Possibly modify (only if Step 5 decides to switch): `third_party/silero_vad.onnx`, `vad/silero.go` (`PinnedModelSHA256`), and the matrix VAD row.

**Interfaces:**
- Consumes: `vad.NewRunner`, `vad.ConfigureArtifacts`, `Processor.detect`, `readWavPCM` (`vad/vad_test.go`), `DefaultProcessorConfig`.

- [ ] **Step 1: The Python probe** (create `tools/vadcompare/pipecat_probs.py`)

```python
"""Per-chunk Silero speech probabilities from Pipecat's own model wrapper.

Run with Pipecat's environment (main checkout):
  pipecat-impl/.venv/bin/python tools/vadcompare/pipecat_probs.py assets/speech > /tmp/pipecat_probs.json
Output: {"<clip>.wav": [p0, p1, ...]} with one probability per 512-sample chunk at 16 kHz.
"""
import glob, json, os, sys, wave

import numpy as np
from pipecat.audio.vad.silero import SileroOnnxModel

MODEL = os.path.join(os.path.dirname(__import__("pipecat").__file__), "audio", "vad", "data", "silero_vad.onnx")

def probs(path, model):
    with wave.open(path, "rb") as w:
        assert w.getframerate() == 16000 and w.getnchannels() == 1 and w.getsampwidth() == 2, path
        pcm = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16).astype(np.float32) / 32768.0
    model.reset_states()
    return [float(model(pcm[i:i + 512], 16000)) for i in range(0, len(pcm) - 511, 512)]

def main(d):
    model = SileroOnnxModel(MODEL, force_onnx_cpu=True)
    print(json.dumps({os.path.basename(p): probs(p, model) for p in sorted(glob.glob(os.path.join(d, "*.wav")))}))

if __name__ == "__main__":
    main(sys.argv[1])
```

Check the real class and method names before running:

```bash
grep -n "class SileroOnnxModel\|def __call__\|def reset_states\|force_onnx_cpu" /Users/raymondottun/Projects/go-voice/pipecat-impl/src/pipecat/audio/vad/silero.py
```

Adjust the script to them. If the clips are not 16 kHz mono 16-bit, the assert says so: resample with `numpy` linear interpolation instead, and note it in the report.

Run: `/Users/raymondottun/Projects/go-voice/pipecat-impl/.venv/bin/python tools/vadcompare/pipecat_probs.py assets/speech > /private/tmp/pipecat_probs.json && python3 -c "import json;d=json.load(open('/private/tmp/pipecat_probs.json'));print(len(d), sum(len(v) for v in d.values()))"`
Expected: `18` and a positive chunk count.

- [ ] **Step 2: The Go comparison test** (create `vad/compare_test.go`). It is opt-in, costs nothing, and is skipped unless `VAD_COMPARE` is set.

```go
package vad

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"govoice/core"
)

// segLog records VAD segment boundaries as chunk indices.
type segLog struct {
	chunk       int
	starts, end []int
}

func (l *segLog) Push(f core.Frame) error {
	switch f.(type) {
	case *core.UserStartedSpeakingFrame:
		l.starts = append(l.starts, l.chunk)
	case *core.UserStoppedSpeakingFrame:
		l.end = append(l.end, l.chunk)
	}
	return nil
}
func (l *segLog) PushDirection(f core.Frame, _ core.Direction) error { return l.Push(f) }

// modelProbs runs one Silero model file over a clip, framed exactly like Processor.trySubmit.
func modelProbs(t *testing.T, r *Runner, pcm []float32) []float32 {
	var ctx [ContextSamples]float32
	var state [StateLen]float32
	var out []float32
	for i := 0; i+ChunkSamples <= len(pcm); i += ChunkSamples {
		j := &Job{}
		copy(j.Input[:ContextSamples], ctx[:])
		copy(j.Input[ContextSamples:], pcm[i:i+ChunkSamples])
		j.State = state
		if err := r.Run([]*Job{j}); err != nil {
			t.Fatal(err)
		}
		state = j.State
		copy(ctx[:], pcm[i+ChunkSamples-ContextSamples:i+ChunkSamples])
		out = append(out, j.Prob)
	}
	return out
}

// segments runs our detector over per-chunk probabilities at a VAD stop.
func segments(probs []float32, stop time.Duration) (starts, ends []int) {
	cfg := DefaultProcessorConfig()
	chunkMS := ChunkSamples * 1000 / SampleRate
	cfg.StopChunks = (int(stop/time.Millisecond) + chunkMS - 1) / chunkMS
	p := NewProcessor(nil, cfg)
	l := &segLog{}
	for i, pr := range probs {
		l.chunk = i
		_ = p.detect(pr, uint64(i), time.Time{}, l, nil)
	}
	return l.starts, l.end
}

// TestCompareSileroModels (VAD_COMPARE=1, VAD_COMPARE_PIPECAT_MODEL=<pipecat silero_vad.onnx>,
// optional VAD_COMPARE_PIPECAT_PROBS=<json from tools/vadcompare/pipecat_probs.py>,
// VAD_COMPARE_OUT=<markdown path>) compares our model file with Pipecat's on assets/speech.
func TestCompareSileroModels(t *testing.T) {
	if os.Getenv("VAD_COMPARE") == "" {
		t.Skip("set VAD_COMPARE=1 (offline, no cost)")
	}
	theirs := os.Getenv("VAD_COMPARE_PIPECAT_MODEL")
	if theirs == "" {
		t.Fatal("VAD_COMPARE_PIPECAT_MODEL is required")
	}
	files, _ := filepath.Glob("../assets/speech/*.wav")
	sort.Strings(files)
	run := func(model string) map[string][]float32 {
		ConfigureArtifacts(model, "")
		r, err := NewRunner(1, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out := map[string][]float32{}
		for _, f := range files {
			pcm, err := readWavPCM(f)
			if err != nil {
				t.Fatal(err)
			}
			out[filepath.Base(f)] = modelProbs(t, r, pcm)
		}
		return out
	}
	ours := run(filepath.Join(Root(), "third_party", "silero_vad.onnx"))
	pc := run(theirs)
	ConfigureArtifacts(filepath.Join(Root(), "third_party", "silero_vad.onnx"), "")

	var b strings.Builder
	fmt.Fprintf(&b, "| clip | model | stop | segments | first start (ms) | last end (ms) |\n|---|---|---|---|---|---|\n")
	splits := map[string]map[time.Duration]int{"ours": {}, "pipecat": {}}
	endSum := map[string]map[time.Duration]int{"ours": {}, "pipecat": {}}
	for _, f := range files {
		name := filepath.Base(f)
		for _, m := range []struct {
			n string
			p []float32
		}{{"ours", ours[name]}, {"pipecat", pc[name]}} {
			for _, stop := range []time.Duration{200 * time.Millisecond, 800 * time.Millisecond} {
				s, e := segments(m.p, stop)
				chunkMS := ChunkSamples * 1000 / SampleRate
				first, last := -1, -1
				if len(s) > 0 {
					first = s[0] * chunkMS
				}
				if len(e) > 0 {
					last = e[len(e)-1] * chunkMS
					endSum[m.n][stop] += last
				}
				if len(s) > 1 {
					splits[m.n][stop] += len(s) - 1
				}
				fmt.Fprintf(&b, "| %s | %s | %v | %d | %d | %d |\n", name, m.n, stop, len(s), first, last)
			}
		}
	}
	fmt.Fprintf(&b, "\n| model | stop | extra segments (mid-utterance splits) | summed last end (ms) |\n|---|---|---|---|\n")
	for _, n := range []string{"ours", "pipecat"} {
		for _, stop := range []time.Duration{200 * time.Millisecond, 800 * time.Millisecond} {
			fmt.Fprintf(&b, "| %s | %v | %d | %d |\n", n, stop, splits[n][stop], endSum[n][stop])
		}
	}
	if path := os.Getenv("VAD_COMPARE_PIPECAT_PROBS"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var py map[string][]float64
		if err := json.Unmarshal(data, &py); err != nil {
			t.Fatal(err)
		}
		worst := 0.0
		for name, ps := range py {
			g := pc[name]
			for i := 0; i < len(ps) && i < len(g); i++ {
				worst = math.Max(worst, math.Abs(ps[i]-float64(g[i])))
			}
		}
		fmt.Fprintf(&b, "\nInference parity (our Go runner with Pipecat's file vs Pipecat's Python wrapper): max |Δp| = %.4f over %d clips.\n", worst, len(py))
	}
	if out := os.Getenv("VAD_COMPARE_OUT"); out != "" {
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("\n" + b.String())
}
```

Check that `Root()` resolves the repo root in the worktree (it is used by `ModelPath`). Check that `ConfigureArtifacts` followed by `NewRunner` picks up the new model path: `NewRunner` reads `ModelPath()`, which prefers the override. If `InitORT` can only run once, that is fine: it initializes ORT, not the model. Also check that `readWavPCM` returns 16 kHz mono float32 (it does for `loadTestLib`).

- [ ] **Step 3: Run it**

```bash
VAD_COMPARE=1 VAD_COMPARE_PIPECAT_MODEL=/Users/raymondottun/Projects/go-voice/pipecat-impl/src/pipecat/audio/vad/data/silero_vad.onnx VAD_COMPARE_PIPECAT_PROBS=/private/tmp/pipecat_probs.json VAD_COMPARE_OUT=/private/tmp/silero-compare-table.md GOVOICE_ORT_LIB=/Users/raymondottun/Projects/go-voice/third_party/onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.dylib CGO_LDFLAGS=-Wl,-w go test ./vad/ -run TestCompareSileroModels -count=1 -v
```

Expected: PASS, with the tables in the log and `/private/tmp/silero-compare-table.md` written.
- **Inference parity:** max |Δp| should be small (below about 0.02). If it is large, our framing differs from Pipecat's wrapper (context or state handling). Investigate before using any numbers: compare `pipecat-impl/src/pipecat/audio/vad/silero.py`'s context handling with `modelProbs`, and record the finding.
- **Without parity:** do not switch models. Report the gap instead.

Also run the package without the variable: `go test ./vad/ -count=1`. It must pass, with `TestCompareSileroModels` skipped.

- [ ] **Step 4: Write the report** (create `docs/benchmarks/silero-compare.md`)

It must contain:
- what was compared: both sha256 values, the 18 clips, stops of 200 and 800 ms, our detector thresholds (start 0.5 / 2 chunks, stop 0.35);
- the inference parity figure;
- the summary table;
- the per-clip table (paste `/private/tmp/silero-compare-table.md`);
- the reproduction commands from Steps 1 and 3;
- the decision.

- [ ] **Step 5: Decide and act**

**Rule (spec section 4):** switch to Pipecat's file only if, at 200 ms, it has no more extra segments than ours and its summed last end is not later.

- **If it qualifies (switch):**
  - Copy Pipecat's file over `third_party/silero_vad.onnx`.
  - Set `PinnedModelSHA256` in `vad/silero.go` to the new sha256 (`shasum -a 256 third_party/silero_vad.onnx`).
  - Update the matrix VAD row: drop the "not the same file" difference and add "same model file as Pipecat".
  - Run `CGO_LDFLAGS=-Wl,-w go test ./vad/ ./server/... -count=1`. Recorded-probability tests may need their goldens refreshed: list them, and refresh only goldens whose diff is explained by the model change.
  - Check whether the production image copies the model (search `deploy/` and `Makefile` for `silero_vad.onnx`), and update any recorded sha there.
- **Otherwise (keep ours):** record why in the report, and add the measured result to the matrix VAD row's notes.

- [ ] **Step 6: Commit**

```bash
git add tools/vadcompare/pipecat_probs.py vad/compare_test.go docs/benchmarks/silero-compare.md docs/pipecat-parity-matrix.md
git commit -m "test(vad): offline Silero model comparison with Pipecat's file; decision recorded"
```

If switching, also add `third_party/silero_vad.onnx`, `vad/silero.go` and any refreshed goldens or deploy files. First check that `third_party/silero_vad.onnx` is tracked (`git ls-files third_party/silero_vad.onnx`). If it is git-ignored and fetched by a script, update that script's URL and sha instead.

---

### Task 7: Live report line and docs

**Files:**
- Modify: `tools/livecheck/report.go` (after the S1 breakdown tables, ~line 290)
- Test: `tools/livecheck/report_test.go`
- Modify: `docs/pipecat-parity-matrix.md` (rows "Turn manager in live calls" and "VAD", remaining-work item 2)

**Interfaces:**
- Consumes: `coreturn.DefaultSTTP99` (Task 1), `med(rs, key)`, `ProfileReport.STT` (`report.go`).

- [ ] **Step 1: Write the failing test** (append to `tools/livecheck/report_test.go`)

```go
func TestReportComparesMeasuredFinalToSTTP99(t *testing.T) {
	rep := &Report{Modes: []string{"off"}, Scenarios: []string{"S1"},
		Profiles: []ProfileReport{{Name: "google", STT: "google chirp_2 en-US"}},
		Results: []*Result{{Profile: "google", Mode: "off", Scenario: "S1", Status: stPass, Metrics: []KV{{"t1.final_tr_ms", int64(1400)}}}}}
	md := renderMarkdown(rep)
	if !strings.Contains(md, "STT final vs built-in p99") || !strings.Contains(md, "| google | off | 1400 | 1570 |") {
		t.Fatalf("missing STT p99 comparison:\n%s", md)
	}
}
```

Check how `med` formats values (it may print `1400` or `1400 ms`) and match that in the expected row.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./tools/livecheck/ -run TestReportComparesMeasuredFinalToSTTP99 -count=1`
Expected: FAIL ("missing STT p99 comparison").

- [ ] **Step 3: Implement.** In `renderMarkdown`, after the additive-split table loop and before `renderRequests(w, rep)`:

```go
	w("\n## STT final vs built-in p99\n\nMedian clip end to final user transcription (S1) next to the p99 the speech timeout waits for (core/turn DefaultSTTP99); a measured value near or above the p99 means the table is stale.\n\n")
	w("| profile | mode | final (ms) | p99 (ms) |\n|---|---|---|---|\n")
	for _, p := range rep.Profiles {
		for _, m := range rep.Modes {
			rs := g[[3]string{p.Name, m, "S1"}]
			if len(rs) == 0 || rs[0].Status == stNA || p.STT == "" {
				continue
			}
			w("| %s | %s | %s | %d |\n", p.Name, m, med(rs, "t1.final_tr_ms"), coreturn.DefaultSTTP99(strings.Fields(p.STT)[0]).Milliseconds())
		}
	}
```

Add the import `coreturn "govoice/core/turn"`.

- [ ] **Step 4: Run the harness tests and the self-test**

Run: `go vet ./tools/livecheck/ && go test ./tools/livecheck/ -count=1 && GOVOICE_ORT_LIB=/Users/raymondottun/Projects/go-voice/third_party/onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.dylib CGO_LDFLAGS=-Wl,-w go run ./tools/livecheck -selftest -scenarios S1,S4,S11,S12`
Expected: everything PASSes.
- The self-test now runs with the new server defaults (manager on, 200 ms VAD stop). S1, S4, S11 and S12 must still pass.
- **If S4 (mid-utterance pause) changes status,** read its checks. With a 200 ms VAD stop, the pause splits segments, but the turn must stay whole when its transcript is final only at the end. Investigate before changing anything.

- [ ] **Step 5: Docs** (`docs/pipecat-parity-matrix.md`)

- **"Turn manager in live calls":** the default cell becomes `on (default since 2026-10-11)`, and the notes describe the gated stop. Remove "Off: turns end on raw VAD stop…" as the default description, and keep the manager-off behaviour as an option.
- **"VAD":** the default is now 200 ms. Add the Task 6 result (or the switch).
- **Remaining work, item 2:** replace it with the parts done (gated stop, defaults, Silero comparison), plus what is left: "measure the new defaults live (nightly S1/S4 numbers in `docs/benchmarks/agent-matrix.md`) and tune `vad.stop_ms`/`turn.speech_timeout` or the p99 table from them".

Run: `cd tools/compat && go test -run TestParityMatrix -count=1 .` There must be 0 non-path errors.

- [ ] **Step 6: Commit**

```bash
git add tools/livecheck/report.go tools/livecheck/report_test.go docs/pipecat-parity-matrix.md
git commit -m "feat(livecheck): report measured STT final against the built-in p99; matrix updated"
```

---

## Final check

Run the whole suite, module by module (the workspace has several modules):

```bash
for m in . core server providers/cartesia providers/deepgram providers/elevenlabs providers/google providers/nvidia providers/openai providers/xai; do (cd $m && go vet ./... && CGO_LDFLAGS=-Wl,-w go test ./... -count=1) || echo "FAILED: $m"; done
```

```bash
cd ui && npx vitest run
```

In a worktree, `tools/compat` and VAD/smartturn tests need the environment notes above. Everything must pass, except the known environment-only failures, which must pass on the main checkout after the merge.

**Optional, needs explicit approval (paid):** run `GOVOICE_LIVE=1 make live-critical PROFILES=google SCENARIOS=S1,S4,S11 MAX_CALLS=20` and compare `v2v_ms` and `vad_stop_ms` with the google S1 rows in `docs/benchmarks/agent-matrix.md`.
