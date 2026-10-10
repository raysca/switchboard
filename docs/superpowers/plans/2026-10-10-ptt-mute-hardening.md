# Push-to-Talk and Mute Hardening Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Four fixes for the minor gaps reviewers found in the mute/push-to-talk work merged at `aca8a96`:
- Muting mid-sentence ends the open speech segment instead of hanging the turn.
- Push-to-talk calls stop running smart-turn inference and the min_words warning they don't use.
- The hold-to-talk button can't get stuck.
- The turn processor gets tests that prove transcripts are committed, not just routed.

**Architecture:**
- The VAD processor (`vad/processor.go`) already receives `core.InputMutedStateFrame`, because the session sends it from the pipeline head. It will end an open segment and ignore audio and in-flight results while muted.
- `server/registry.go` builds push-to-talk calls on their own branch, before the smart-turn analyzer is created.
- The UI releases on window blur, captures the pointer, and clears `isTalking` on release even when the send fails.
- A new `core/turn` test file wires `transcript.Processor` (with `ManageTurns`) after the turn processor and asserts on `FinalTranscriptionFrame{Committed: true}`.

**Tech Stack:** Go 1.27.1 (`testing`). UI: React + TypeScript, vitest 5, Testing Library.

**Spec:** This plan. The findings come from the final branch review and the two fix reviews of the mute/push-to-talk work on 2026-10-10. The decisions:
- **Push-to-talk keeps `turn.mute` strategies as configured.** An operator who sets `always` or `critical_output` asked for the user not to interrupt, and a button press doesn't override that. Documented, not changed.
- **No other behavior changes.** Out of scope: the held-past-`MaxTurnDuration` behavior, the generation-moved late-final loss (needs investigation first), and Prometheus usage counters.

## Global Constraints

- **Muting ends the open segment.** `core.InputMutedStateFrame{Muted: true}` received while VAD is in a speech segment emits exactly one `core.UserStoppedSpeakingFrame`. While muted, VAD never emits `UserStartedSpeakingFrame`, whether from queued audio or in-flight inference results. Unmuting resumes normal detection. VAD still forwards the state frame downstream.
- **Push-to-talk calls skip unused machinery.** They never create the smart-turn analyzer or its audio tap, never call `turnStopDetector`, and never install the min_words no-interims warning. A hands-free call's pipeline is unchanged.
- **The hold button releases when it should.** Window `blur` while talking sends `push-to-talk {"pressed": false}`. A release always clears `isTalking`, even if the socket is closed. A press only sets `isTalking` when the message was sent.
- **Test commands:**
  - Go tests run from the repo root on the main checkout, where the ONNX runtime is present for `vad`.
  - Darwin cgo needs `CGO_LDFLAGS=-Wl,-w`.
  - The workspace has several modules (`go.work`). To run the whole suite, run `go test ./...` inside each of `.`, `core`, `server`, and `providers/*`.
  - Run UI tests from `ui/` with `npx vitest run <file>`.
- **Commits:** one commit per task, containing only the files that task names. End each message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

- **Mute arrives before queued speech audio.** Mute is an urgent control, so it overtakes audio frames already queued. Those frames reach VAD after the mute state and must not reopen a segment. Task 1 tests this (`TestMuteIgnoresQueuedAudio`).
- **Inference results return after the mute.** Chunks submitted before the mute come back later as `vadResultFrame`. Their high probabilities must not start a segment. Task 1 tests this (`TestMuteEndsOpenSegmentAndBlocksRestart`).
- **A push-to-talk call on a server configured with `turn-analyzer` but no model loaded.** Today that combination errors in `turnStopDetector` ("smart-turn model is not loaded"). Push-to-talk must not fail because of a stop chain it doesn't use. Task 2 tests this (`TestPushToTalkCallSkipsSmartTurn`, no-backend case).
- **Alt-tab while holding the button.** `pointerup` never arrives. The window `blur` listener must release. Task 3 tests this.
- **Release over a closed socket.** The button must not stay "talking". Task 3 tests this.

---

### Task 1: Muting ends the open VAD segment

**Files:**
- Modify: `vad/processor.go` (struct around line 107, `ProcessFrame` lines 172-193, `detect` line 348)
- Test: `vad/mute_test.go` (create)

**Interfaces:**
- Consumes: `core.InputMutedStateFrame{Muted bool}`, `core.UserStoppedSpeakingFrame`, `core.UserStartedSpeakingFrame` (exist).
- Produces: no new API.

- [ ] **Step 1: Write the failing tests** (create `vad/mute_test.go`)

```go
package vad

import (
	"context"
	"testing"
	"time"

	"govoice/core"
)

type frameLog struct{ starts, stops, muted int }

func (l *frameLog) Push(f core.Frame) error {
	switch f.(type) {
	case *core.UserStartedSpeakingFrame:
		l.starts++
	case *core.UserStoppedSpeakingFrame:
		l.stops++
	case *core.InputMutedStateFrame:
		l.muted++
	}
	return nil
}
func (l *frameLog) PushDirection(f core.Frame, _ core.Direction) error { return l.Push(f) }

func speak(p *Processor, l *frameLog, chunks int) {
	for i := 0; i < chunks; i++ {
		_ = p.detect(0.9, 0, time.Now(), l, nil)
	}
}

// Muting mid-speech ends the segment once; results that land while muted
// cannot start a new one; unmuting restores detection.
func TestMuteEndsOpenSegmentAndBlocksRestart(t *testing.T) {
	cfg := DefaultProcessorConfig()
	p := NewProcessor(nil, cfg)
	l := &frameLog{}
	speak(p, l, cfg.StartChunks)
	if l.starts != 1 {
		t.Fatalf("starts=%d before mute, want 1", l.starts)
	}
	if err := p.ProcessFrame(context.Background(), &core.InputMutedStateFrame{Muted: true}, core.Downstream, l); err != nil {
		t.Fatal(err)
	}
	if l.stops != 1 || l.muted != 1 {
		t.Fatalf("after mute: stops=%d forwarded=%d, want 1 and 1", l.stops, l.muted)
	}
	speak(p, l, cfg.StartChunks*2) // in-flight results arriving while muted
	if l.starts != 1 {
		t.Fatal("a result arriving while muted started a segment")
	}
	_ = p.ProcessFrame(context.Background(), &core.InputMutedStateFrame{Muted: false}, core.Downstream, l)
	speak(p, l, cfg.StartChunks)
	if l.starts != 2 {
		t.Fatalf("starts=%d after unmute, want 2", l.starts)
	}
}

// Muting while silent emits no stop.
func TestMuteWhileSilentEmitsNoStop(t *testing.T) {
	p := NewProcessor(nil, DefaultProcessorConfig())
	l := &frameLog{}
	_ = p.ProcessFrame(context.Background(), &core.InputMutedStateFrame{Muted: true}, core.Downstream, l)
	if l.stops != 0 {
		t.Fatalf("stops=%d, want 0", l.stops)
	}
}

// Audio queued before the mute but delivered after it is not buffered.
func TestMuteIgnoresQueuedAudio(t *testing.T) {
	p := NewProcessor(nil, DefaultProcessorConfig())
	l := &frameLog{}
	_ = p.ProcessFrame(context.Background(), &core.InputMutedStateFrame{Muted: true}, core.Downstream, l)
	_ = p.ProcessFrame(context.Background(), &core.InputAudioFrame{Samples: make([]byte, ChunkSamples*4)}, core.Downstream, l)
	if len(p.sampleBuf) != 0 || len(p.pending) != 0 {
		t.Fatalf("muted audio buffered: samples=%d pending=%d", len(p.sampleBuf), len(p.pending))
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `CGO_LDFLAGS=-Wl,-w go test ./vad/ -run 'TestMute' -count=1 -v`
Expected:
- `TestMuteEndsOpenSegmentAndBlocksRestart` FAILS at "after mute: stops=0 forwarded=1".
- `TestMuteIgnoresQueuedAudio` FAILS: audio is buffered.
- `TestMuteWhileSilentEmitsNoStop` passes. It guards the new code.

- [ ] **Step 3: Implement in `vad/processor.go`**

Add to the `Processor` struct, next to `speaking bool`:

```go
	muted    bool // input muted: ignore audio and results, so no segment opens
```

In `ProcessFrame`, add a case before `case *vadResultFrame:`, and guard the audio case.

Change the start of `case *core.InputAudioFrame:` from:

```go
		if dir != core.Downstream {
			return e.PushDirection(f, dir)
		}
```

to:

```go
		if dir != core.Downstream {
			return e.PushDirection(f, dir)
		}
		if p.muted {
			return nil // queued before the mute; the session drops later audio itself
		}
```

Add:

```go
	case *core.InputMutedStateFrame:
		if err := p.setMuted(v.Muted, e); err != nil {
			return err
		}
		return e.PushDirection(f, dir)
```

Add the method after `ProcessFrame`:

```go
// setMuted ends an open speech segment when the input is muted: muted audio
// never arrives, so the quiet chunks that would end it never come and the
// user's turn would hang. While muted nothing is buffered or detected.
func (p *Processor) setMuted(muted bool, e core.Emitter) error {
	p.muted = muted
	if !muted {
		return nil
	}
	p.sampleBuf, p.pending, p.above, p.below = p.sampleBuf[:0], nil, 0, 0
	if !p.speaking {
		return nil
	}
	p.speaking = false
	var gen uint64
	if p.s != nil {
		gen = p.s.CurrentGeneration()
	}
	return e.Push(&core.UserStoppedSpeakingFrame{Timestamp: time.Now(), Gen: gen})
}
```

At the top of `detect`:

```go
	if p.muted {
		return nil // a result for a chunk submitted before the mute
	}
```

`p.sampleBuf` is a `[]float32` and `p.pending` a `[]pendingChunk`, so `[:0]` and `nil` both compile.

- [ ] **Step 4: Run them and confirm they pass**

Run: `CGO_LDFLAGS=-Wl,-w go test ./vad/ -run 'TestMute' -count=1 -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Run the VAD package**

Run: `CGO_LDFLAGS=-Wl,-w go test ./vad/ -count=1`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add vad/processor.go vad/mute_test.go
git commit -m "fix(vad): muting ends the open speech segment"
```

---

### Task 2: Push-to-talk calls skip smart-turn and the min_words warning

**Files:**
- Modify: `server/registry.go` (turn block, lines 566-603)
- Modify: `docs/quickstart.md` (the push-to-talk paragraph)
- Test: `server/registry_test.go`

**Interfaces:**
- Consumes: `CallSpec.PushToTalk`, `smartBackend` (test fake in `server/turn_stop_test.go:101`), `fakeTransportSink`, `mockVADExecutor`.
- Produces: no new API.

- [ ] **Step 1: Write the failing test** (append to `server/registry_test.go`)

```go
// A push-to-talk call never runs the smart-turn model, even on a server whose
// turn.stop uses it, and builds whether or not the model is loaded.
func TestPushToTalkCallSkipsSmartTurn(t *testing.T) {
	for _, withModel := range []bool{true, false} {
		reg := NewRegistry()
		for _, stage := range []string{"stt", "llm", "tts"} {
			reg.Register("google."+stage, func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })
		}
		if withModel {
			reg.SetTurnBackend(smartBackend{p: 0.9})
		}
		cfg := DefaultConfig(ProfileDevelopment)
		cfg.Turn.Manager = true
		cfg.Turn.Stop = []string{"turn-analyzer", "speech-timeout"}
		cfg.Turn.MinWords = 3
		vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
		call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, PushToTalk: true}
		pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, &fakeTransportSink{})
		if err != nil {
			t.Fatalf("model=%t: %v", withModel, err)
		}
		procs := pipe.Processors()
		if _, ok := procs[1].(*core.PassthroughProcessor); !ok {
			t.Fatalf("model=%t: processor 1 is %T, want STT (no smart-turn tap)", withModel, procs[1])
		}
		if _, ok := procs[2].(*coreturn.Processor); !ok {
			t.Fatalf("model=%t: processor 2 is %T, want the turn processor", withModel, procs[2])
		}
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `CGO_LDFLAGS=-Wl,-w go test ./server/ -run TestPushToTalkCallSkipsSmartTurn -count=1 -v`
Expected: FAIL at "model=true: processor 1 is *smartturn.…, want STT". If the order differs, the `model=false` case fails instead with "smart-turn model is not loaded".

- [ ] **Step 3: Implement in `server/registry.go`**

Replace the turn block from `mc := coreturn.DefaultConfig()` through the `OnFinalWithoutInterims` setup with:

```go
		mc := coreturn.DefaultConfig()
		var stop coreturn.TurnDetector
		if call.PushToTalk {
			// The button replaces voice detection for starting and ending turns,
			// so the smart-turn model, the stop chain and min_words are unused.
			// turn.mute strategies still apply: they are the operator's call.
			// MaxTurnDuration still caps a held button.
			mc.StartStrategies = []coreturn.UserTurnStartStrategy{coreturn.ExternalStart()}
			stop = coreturn.StopChain(coreturn.ExternalStop())
		} else {
			if cfg.Turn.MinWords > 0 {
				mc.StartStrategies = []coreturn.UserTurnStartStrategy{coreturn.MinWordsStart(cfg.Turn.MinWords, true)}
			}
			var analyzer func(time.Duration) coreturn.UserTurnStopStrategy
			if slices.Contains(cfg.Turn.Stop, "turn-analyzer") && r.turnBackend != nil {
				a := smartturn.NewAnalyzer(r.turnBackend, smartturn.Config{Budget: cfg.Turn.Analyzer.Budget, Hold: cfg.Turn.Analyzer.StopSecs})
				turnTap = a.Tap()
				analyzer = func(base time.Duration) coreturn.UserTurnStopStrategy {
					return smartturn.Stop(a, base, cfg.Turn.Analyzer.StopSecs)
				}
			}
			var err error
			if stop, err = turnStopDetector(cfg.Turn.Stop, cfg.Turn.SpeechTimeout, cfg.Turn.EagerGrace, analyzer); err != nil {
				return fail(err)
			}
		}
		mute, err := coreturn.ParseMuteStrategies(cfg.Turn.Mute, muteTools)
		if err != nil {
			return fail(err)
		}
		opts := coreturn.ProcessorOptions{Detector: stop, Mute: mute, MaxMute: cfg.Turn.MaxMute, OnMuteDrop: r.onUserMuteDrop}
		if cfg.Turn.MinWords > 0 && !call.PushToTalk {
```

Keep the body of that `if` (the `slog.Warn` closure) and everything after it unchanged. The `err` declared inside the `else` stays scoped to it. `mute, err := ...` then declares the block's `err`, as `stop, err :=` did before.

- [ ] **Step 4: Run it and confirm it passes**

Run: `CGO_LDFLAGS=-Wl,-w go test ./server/ -run 'TestPushToTalkCall|TestTurnManagerPipeline|TestTurnAnalyzerThroughCallPipeline' -count=1 -v`
Expected: PASS. The hands-free turn tests stay green.

- [ ] **Step 5: Document the mute interplay** (`docs/quickstart.md`, end of the push-to-talk paragraph)

```markdown
Push-to-talk calls ignore `turn.stop`, `turn.min_words` and the smart-turn
model, but `turn.mute` strategies still apply: with `always`, a press while
the bot talks does not interrupt it.
```

- [ ] **Step 6: Run the server module**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test ./... -count=1`
Expected: PASS. `TestCallPipelineAsyncToolsResultTriggersAssistantTurn` is a known flake that this task doesn't touch. If it fails, rerun it with `-count=20` and note the result.

- [ ] **Step 7: Commit**

```bash
git add server/registry.go server/registry_test.go docs/quickstart.md
git commit -m "fix(server): push-to-talk calls skip smart-turn and the min_words warning"
```

---

### Task 3: The hold-to-talk button cannot stick

**Files:**
- Modify: `ui/src/voice/VoiceClient.ts` (`setTalking`)
- Modify: `ui/src/routes/agents/AgentVoiceTest.tsx` (hold button, plus a `useEffect`)
- Test: `ui/src/routes/agents/AgentVoiceTest.test.tsx`

**Interfaces:**
- Consumes: `VoiceClient.setTalking(pressed)`, `VoiceClientState.isTalking`, `MockWebSocket` (test), `RTVI_TYPE_PUSH_TO_TALK`.
- Produces: no new API.

- [ ] **Step 1: Write the failing tests** (`AgentVoiceTest.test.tsx`; add `fireEvent` to the `@testing-library/react` import). Put them after the existing push-to-talk test. They reuse its setup:

```ts
  async function connectPushToTalk() {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={createMockAgent()} client={createMockClient()} keyValue="mgmt-key-1"
          audioController={mockAudio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByLabelText("Push to talk"));
    await user.click(screen.getByRole("button", { name: "Start voice test" }));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    await waitFor(() => expect(ws.sentData.length).toBeGreaterThan(0));
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });
    await waitFor(() => expect(screen.getByTestId("connection-state")).toHaveTextContent("READY"));
    const presses = () =>
      ws.sentData
        .map((d) => decodeBinaryFrame(d))
        .filter((f) => f?.kind === "message" && f.parsed?.type === RTVI_TYPE_PUSH_TO_TALK)
        .map((f) => (f?.kind === "message" ? f.parsed?.data : null));
    return { user, ws, talk: screen.getByTestId("ptt-btn"), presses };
  }

  it("push-to-talk: leaving the window while holding releases", async () => {
    const { user, talk, presses } = await connectPushToTalk();
    await user.pointer({ keys: "[MouseLeft>]", target: talk });
    expect(talk).toHaveAttribute("aria-pressed", "true");
    fireEvent.blur(window);
    await waitFor(() => expect(talk).toHaveAttribute("aria-pressed", "false"));
    expect(presses()).toEqual([{ pressed: true }, { pressed: false }]);
  });

  it("push-to-talk: a release over a closed socket still stops talking", async () => {
    const { user, ws, talk } = await connectPushToTalk();
    await user.pointer({ keys: "[MouseLeft>]", target: talk });
    ws.readyState = WebSocket.CLOSED;
    await user.pointer({ keys: "[/MouseLeft]", target: talk });
    expect(talk).toHaveAttribute("aria-pressed", "false");
  });
```

If the existing push-to-talk test builds the same setup inline, leave it as it is. This helper only serves the new tests.

- [ ] **Step 2: Run them and confirm they fail**

Run (from `ui/`): `npx vitest run src/routes/agents/AgentVoiceTest.test.tsx`
Expected:
- "leaving the window" FAILS: `aria-pressed` stays "true".
- "closed socket" FAILS: `aria-pressed` stays "true".

- [ ] **Step 3: Implement `setTalking`** (`VoiceClient.ts`). Replace the method:

```ts
  /** Push-to-talk: true when the hold-to-talk button goes down, false when it is released. */
  public setTalking(pressed: boolean): void {
    if (!this.state.pushToTalk || this.state.isTalking === pressed) return;
    const sent = this.sendRTVI(RTVI_TYPE_PUSH_TO_TALK, encodePushToTalkFrame(pressed), { pressed });
    // A release always stops talking here, even if the socket is gone; a press
    // only counts once the server was told.
    if (sent || !pressed) this.updateState({ isTalking: pressed });
  }
```

- [ ] **Step 4: Implement the page** (`AgentVoiceTest.tsx`)

Next to the other effects:

```tsx
  // Alt-tab while holding: pointerup never arrives, so release on window blur.
  useEffect(() => {
    if (!state.isTalking) return;
    const release = () => voiceClient.setTalking(false);
    window.addEventListener("blur", release);
    return () => window.removeEventListener("blur", release);
  }, [state.isTalking, voiceClient]);
```

On the hold button, replace `onPointerDown={() => voiceClient.setTalking(true)}` with:

```tsx
                  onPointerDown={(e) => {
                    // Keep the press while the pointer drifts off the button.
                    e.currentTarget.setPointerCapture?.(e.pointerId);
                    voiceClient.setTalking(true);
                  }}
```

Remove `onPointerLeave={() => voiceClient.setTalking(false)}`. With pointer capture, the release comes from `pointerup`/`pointercancel`. Keep `onPointerUp`, `onPointerCancel`, `onBlur`, `onKeyDown` and `onKeyUp`.

`voiceClient` comes from a `useRef` set once (`AgentVoiceTest.tsx:30-39`), so it is stable and safe in the dependency list, as in the subscribe effect below it.

- [ ] **Step 5: Run the UI tests and the type check**

Run (from `ui/`): `npx vitest run src/voice src/routes/agents/AgentVoiceTest.test.tsx && npx tsc --noEmit`
Expected: PASS, no type errors. The existing push-to-talk test (pointer down and up, then Space) stays green.

- [ ] **Step 6: Commit**

```bash
git add ui/src/voice/VoiceClient.ts ui/src/routes/agents/AgentVoiceTest.tsx ui/src/routes/agents/AgentVoiceTest.test.tsx
git commit -m "fix(ui): hold-to-talk releases on window blur and over a closed socket"
```

---

### Task 4: Turn tests that prove transcripts are committed

**Files:**
- Create: `core/turn/commit_test.go`
- Test: the same file

**Interfaces:**
- Consumes: `harness`, `fakeClock`, `fakeSTT`, `(*harness).ptt`, `run`, `settle`, `events` (in `core/turn/processor_test.go` and `stop_test.go`); `transcript.NewProcessor`, `transcript.DefaultConfig`, `(*transcript.Processor).ManageTurns` (in `core/transcript`).
- Produces: `newCommitHarness(t) *harness`, `(*harness).commits()`, `(*harness).waitCommit(text) bool`, all test-only.

These tests cover behaviour that already exists, so they pass when first written. A test that has never failed proves nothing, so step 3 breaks the code on purpose to watch each one fail.

- [ ] **Step 1: Write the tests** (create `core/turn/commit_test.go`)

```go
package turn

import (
	"testing"
	"time"

	"govoice/core"
	"govoice/core/transcript"
)

// newCommitHarness is a push-to-talk harness with the real transcript
// finalizer after the turn processor, so tests see committed transcripts.
func newCommitHarness(t *testing.T) *harness {
	t.Helper()
	cfg := DefaultConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{ExternalStart()}
	h := &harness{t: t, clk: &fakeClock{now: time.Unix(1000, 0)}, stt: &fakeSTT{}, sink: &core.PassthroughProcessor{}}
	proc, err := NewProcessor(cfg, ProcessorOptions{Clock: h.clk, Detector: StopChain(ExternalStop())})
	if err != nil {
		t.Fatal(err)
	}
	h.proc = proc
	fin, err := transcript.NewProcessor(transcript.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	fin.ManageTurns()
	acfg := core.DefaultAssistantContextConfig()
	acfg.BargeInOnTurnEvent = true
	h.asst = core.NewAssistantContextProcessor(core.NewConversationContext(core.ContextConfig{MaxMessages: 10, MaxTokens: 1000}), acfg)
	pipe, err := core.NewPipeline(&h.stt.PassthroughProcessor, proc, fin, h.sink, h.asst)
	if err != nil {
		t.Fatal(err)
	}
	if h.sess, err = core.NewSession(1, pipe, core.DefaultResourceBudget()); err != nil {
		t.Fatal(err)
	}
	if err := h.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.sess.Close() })
	return h
}

func (h *harness) commits() (out []string) {
	for _, f := range h.sink.GetFrames() {
		if c, ok := f.(*core.FinalTranscriptionFrame); ok && c.Committed {
			out = append(out, c.Text)
		}
	}
	return out
}

// waitCommit polls in real time: the finalizer's deadlines run on the wall clock.
func (h *harness) waitCommit(text string) bool {
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, c := range h.commits() {
			if c == text {
				return true
			}
		}
	}
	return false
}

// A released turn whose final arrives after new (unpressed) speech still
// commits that final.
func TestCommitReleasedTurnLateFinal(t *testing.T) {
	h := newCommitHarness(t)
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	h.settle()
	h.ptt(false)
	h.run(300*time.Millisecond, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1}) // unpressed
	h.settle()
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "book a table for two", Final: true}, core.Downstream, 1, 1)
	if !h.waitCommit("book a table for two") {
		t.Fatalf("released turn's late final not committed; commits=%q", h.commits())
	}
}

// Speech before the press is never committed; the pressed speech is.
func TestCommitOnlyPressedSpeech(t *testing.T) {
	h := newCommitHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.settle()
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-1", Gen: 1, Text: "side talk", Final: true}, core.Downstream, 1, 1)
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	h.settle()
	h.run(300*time.Millisecond, func() bool { return false })
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1})
	h.settle()
	h.ptt(true)
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	_ = h.sess.Reenter(&core.TranscriptCandidateFrame{Turn: "turn-1-3", Gen: 1, Text: "what is the weather", Final: true}, core.Downstream, 1, 1)
	h.settle()
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 4, Gen: 1})
	h.settle()
	h.ptt(false)
	if !h.waitCommit("what is the weather") {
		t.Fatalf("pressed speech not committed; commits=%q", h.commits())
	}
	for _, c := range h.commits() {
		if c != "what is the weather" {
			t.Fatalf("committed %q, want only the pressed speech", c)
		}
	}
}
```

If `Reenter` needs a different processor index to land at STT output, use the index of the turn processor (1). These tests use `1, 1` in the same way as `stop_test.go`'s `TestProcessorVADBargeInAndSilenceEnd`.

- [ ] **Step 2: Run them**

Run: `go test ./core/turn/ -run 'TestCommit' -count=1 -v`
Expected: PASS (2 tests)

- [ ] **Step 3: Break the code on purpose to watch each test fail, then restore** (do not commit the breakage)

1. In `core/turn/processor.go` `onCandidate`, temporarily replace the loop that routes to `p.prev.segs` with nothing, so released candidates are dropped. Run `go test ./core/turn/ -run TestCommitReleasedTurnLateFinal -count=1`. It must FAIL with "not committed".
2. Restore it. In `onPushToTalk`, temporarily change the guard `(!p.vadSpeaking || len(p.segs) > 1)` to `!p.vadSpeaking`. Run `go test ./core/turn/ -run TestCommitOnlyPressedSpeech -count=1`. It must FAIL with "committed \"side talk what is the weather\"".
3. Restore it. Run `git diff core/turn/processor.go`. It must print nothing.

If either test still passes with the code broken, the test is wrong: fix the test, not the code, until it fails for that break.

- [ ] **Step 4: Run the turn package under the race detector**

Run: `go test ./core/turn/ -race -count=3`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add core/turn/commit_test.go
git commit -m "test(turn): push-to-talk turns commit through the real finalizer"
```

---

## Final check (after all tasks)

Run the whole suite, one module at a time (the workspace has several modules):

```bash
for m in . core server providers/cartesia providers/deepgram providers/elevenlabs providers/google providers/nvidia providers/openai providers/xai; do (cd $m && go vet ./... && CGO_LDFLAGS=-Wl,-w go test ./... -count=1) || echo "FAILED: $m"; done
```

```bash
cd ui && npx vitest run && npx tsc --noEmit
```

Everything must pass, apart from the known async-tools flake, which has to pass on a rerun. Then run a fresh reviewer on the branch diff.
