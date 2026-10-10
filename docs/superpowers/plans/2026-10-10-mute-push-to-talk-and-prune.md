# Mute, Push-to-Talk, and Core Pruning Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A caller can mute their mic so the server receives no audio, and can hold a button to talk, with turns started and ended by that button. Core code the server does not use and will not need is removed.

**Architecture:** Both features are RTVI client messages that become frames. `set-muted` maps to the existing `core.InputMuteFrame`/`InputUnmuteFrame`, which the session already handles by dropping input audio. `push-to-talk` maps to a new `core.PushToTalkFrame`, which `turn.Processor` turns into the `ExternalStart`/`ExternalStop` evidence the turn manager already understands. A call opts into push-to-talk with `push_to_talk: true` in `client-ready`. The server then installs the turn processor for that call with only those two strategies. YAML config is unchanged.

**Tech Stack:** Go 1.27.1 (`testing`, `coder/websocket`). UI: React + TypeScript, vitest 5, Testing Library.

**Spec:** This plan. The decisions come from the audit discussion on 2026-10-10:
- Mute and push-to-talk are needed; they are wired, not pruned.
- Push-to-talk is opted into per call by the client.
- These are deleted: `ContextAggregator`, `SpeakingTracker.IsSpeaking`, `WakePhraseStart`, `TranscriptionStart`.
- These are kept, because the audit was wrong about them: `AsyncTracer` (it wraps every production tracer), `record.Sink` (two test fakes depend on it), `SplitNext` (`tools/ttssim` uses it).
- `UsageMetricsFrame` is left alone (it is already traced). Prometheus counters are not added.
- `ValidateToolPairing` and `ConversationContext.AddToolCall` stay as they are (tests across packages use them).

## Global Constraints

- RTVI message names: `set-muted` with `{"muted": true|false}`, and `push-to-talk` with `{"pressed": true|false}`. The `client-ready` opt-in is `data.push_to_talk: true`.
- Both messages are rejected (non-fatal error, connection stays open) before `client-ready` or when the payload is malformed. `push-to-talk` is also rejected when the call did not opt in.
- In a push-to-talk call, voice detection alone never starts a turn, and silence alone never ends one. Only the button does. The manager's `MaxTurnDuration` (default 60s) stays the cap on a held turn.
- `turn.stop: external` in YAML stays rejected. Push-to-talk comes only from the client.
- A failed `session.Post` of a client control frame is reported to the client as a non-fatal RTVI error, not dropped silently.
- While muted, the recording tap gets zeroed samples of the same length, so the user and bot tracks stay aligned and no speech is stored.
- Run Go tests from the repo root. On Darwin, cgo tests need `CGO_LDFLAGS=-Wl,-w`. Run UI tests from `ui/` with `npx vitest run <file>`.
- One commit per task, containing only the files that task names. End each commit message with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

- **Button pressed but nobody speaks, then released.** The press must not carry over: speech that starts later without a press must not start a turn. Task 2 tests this (`TestPushToTalkPressWithoutSpeechDoesNotLinger`).
- **Button pressed while the user is already speaking.** Voice detection started before the press. The turn must still start on the press. Task 2 tests this (`TestPushToTalkPressDuringSpeechStartsTurn`).
- **Push-to-talk while the bot is speaking.** Pressing and speaking must barge in, as in hands-free mode. Task 2 tests this (`TestPushToTalkBargesIn`).
- **Reconnect while the UI shows muted.** The new server session starts unmuted. The client must send `set-muted` again after `bot-ready`. Task 4 tests this.
- **Button released before the STT final arrives.** This needs no new code: `transcript.Finalizer.Complete` waits `FinalizationTimeout` for the final when none has arrived (`core/transcript/finalizer.go:275-279`). Verify it by hand in Task 5, step 6.

---

### Task 1: Server-side mute over RTVI

**Files:**
- Modify: `transport/rtvi/rtvi.go` (constants block ~line 22, `HandleIncomingJSON` switch ~line 152)
- Modify: `transport/ws/transport.go` (`respFrame` post ~line 729, audio tap ~line 751)
- Modify: `docs/quickstart.md` (after the `send-text` paragraph, ~line 211)
- Test: `transport/rtvi/rtvi_test.go`, `transport/ws/client_safety_test.go`

**Interfaces:**
- Consumes: `core.InputMuteFrame`, `core.InputUnmuteFrame`, `(*core.Session).InputMuted() bool` (all exist).
- Produces: `rtvi.TypeSetMuted = "set-muted"`. `HandleIncomingJSON` returns `*core.InputMuteFrame` or `*core.InputUnmuteFrame` for it.

- [ ] **Step 1: Write the failing rtvi test** (append to `transport/rtvi/rtvi_test.go`)

```go
func TestSetMuted(t *testing.T) {
	h := rtvi.NewHandler()
	msg := func(data string) string {
		return `{"id":"m","label":"rtvi-ai","type":"set-muted","data":` + data + `}`
	}
	if _, err := h.HandleIncomingJSON(msg(`{"muted":true}`)); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("set-muted before client-ready: %v", err)
	}
	if _, err := h.HandleIncomingJSON(`{"id":"r","label":"rtvi-ai","type":"client-ready","data":{"version":"` + rtvi.ProtocolVersion + `"}}`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{}`, `{"muted":"yes"}`, `null`} {
		if _, err := h.HandleIncomingJSON(msg(bad)); !errors.Is(err, rtvi.ErrRejectedMessage) {
			t.Fatalf("payload %s accepted: %v", bad, err)
		}
	}
	f, err := h.HandleIncomingJSON(msg(`{"muted":true}`))
	if _, ok := f.(*core.InputMuteFrame); err != nil || !ok {
		t.Fatalf("muted=true gave %T, %v", f, err)
	}
	f, err = h.HandleIncomingJSON(msg(`{"muted":false}`))
	if _, ok := f.(*core.InputUnmuteFrame); err != nil || !ok {
		t.Fatalf("muted=false gave %T, %v", f, err)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./transport/rtvi/ -run TestSetMuted -v`
Expected: FAIL. The first assertion passes by accident (an unknown type returns `nil, nil`, which is not a rejection, so the test fails at "set-muted before client-ready").

- [ ] **Step 3: Implement in `transport/rtvi/rtvi.go`**

Add to the constants block, after `TypeDTMF`:

```go
	TypeSetMuted            = "set-muted" // client mic mute: {"muted": true|false}; the session drops input audio while muted
```

Add a case to `HandleIncomingJSON`, before `case TypeDisconnectBot`:

```go
	case TypeSetMuted:
		if !h.ready.Load() {
			return nil, fmt.Errorf("%w: set-muted before client-ready", ErrRejectedMessage)
		}
		var data struct {
			Muted *bool `json:"muted"`
		}
		if err := json.Unmarshal(msg.Data, &data); err != nil || data.Muted == nil {
			return nil, fmt.Errorf(`%w: set-muted needs {"muted": true|false}`, ErrRejectedMessage)
		}
		if *data.Muted {
			return &core.InputMuteFrame{}, nil
		}
		return &core.InputUnmuteFrame{}, nil
```

- [ ] **Step 4: Run the rtvi test and confirm it passes**

Run: `go test ./transport/rtvi/ -run TestSetMuted -v`
Expected: PASS

- [ ] **Step 5: Write the failing end-to-end ws test** (append to `transport/ws/client_safety_test.go`; add `"fmt"` to its imports)

```go
// set-muted over the socket mutes and unmutes the call's session.
func TestSetMutedTogglesSessionInputMute(t *testing.T) {
	ready := make(chan *ws.Connection, 1)
	c, ctx := dialReady(t, "jti-set-muted", func(conn *ws.Connection) { ready <- conn })
	conn := <-ready
	for _, muted := range []bool{true, false} {
		msg, _ := ws.EncodeMessageFrame(fmt.Sprintf(`{"id":"m","label":"rtvi-ai","type":"set-muted","data":{"muted":%t}}`, muted))
		if err := c.Write(ctx, websocket.MessageBinary, msg); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for conn.Session().InputMuted() != muted {
			if time.Now().After(deadline) {
				t.Fatalf("InputMuted() never became %t", muted)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
```

- [ ] **Step 6: Run it**

Run: `go test ./transport/ws/ -run TestSetMutedTogglesSessionInputMute -v`
Expected: PASS already. The existing `respFrame` path posts the frame. This test pins the end-to-end path before step 7 changes it.

- [ ] **Step 7: Report a failed post, and zero muted audio before the recording tap** (`transport/ws/transport.go`)

Replace:

```go
				if session := c.session.Load(); session != nil {
					_ = session.Post(respFrame)
				}
```

with:

```go
				if session := c.session.Load(); session != nil {
					if err := session.Post(respFrame); err != nil {
						// A lost mute or push-to-talk release would leave the call in the wrong state: say so.
						errJSON, _ := c.rtvi.FormatError(rtvi.ErrRejectedMessage.Error()+": message could not be applied", false)
						_ = c.WriteWireMessage(c.ctx, errJSON)
					}
				}
```

In the `*core.InputAudioFrame` case, replace:

```go
			if t := c.tap.Load(); t != nil {
				(*t).User(f)
			}

			if session := c.session.Load(); session != nil {
```

with:

```go
			session := c.session.Load()
			if session != nil && session.InputMuted() {
				// The session drops muted audio. Zero it so the recording keeps its
				// timing but stores no speech. ponytail: InputMuted flips on the run
				// loop, so a few ms after set-muted may still record; the UI also
				// disables the mic track.
				clear(f.Samples)
			}
			if t := c.tap.Load(); t != nil {
				(*t).User(f)
			}

			if session != nil {
```

- [ ] **Step 8: Run the package tests**

Run: `go test ./transport/... -count=1`
Expected: PASS

- [ ] **Step 9: Document it** (`docs/quickstart.md`, after the `send-text` paragraphs)

```markdown
A client mutes the caller with `{"label":"rtvi-ai","type":"set-muted","data":{"muted":true}}`
and unmutes with `"muted":false`. While muted the server drops the caller's
audio (no turns, no STT, no barge-in) and the recording stores silence. A muted
client should still disable its mic track; the message makes the server agree.
```

- [ ] **Step 10: Commit**

```bash
git add transport/rtvi/rtvi.go transport/rtvi/rtvi_test.go transport/ws/transport.go transport/ws/client_safety_test.go docs/quickstart.md
git commit -m "feat(rtvi): set-muted mutes the caller server-side"
```

---

### Task 2: `PushToTalkFrame` drives ExternalStart/ExternalStop in the turn processor

**Files:**
- Modify: `core/frame.go` (after `FrameUsageMetrics`, line 66)
- Modify: `core/control.go`
- Modify: `core/session.go` (trace frame-name switch, ~line 1133)
- Modify: `core/turn/processor.go` (struct ~line 93, `SignalStop` lines 152-160, downstream switch ~line 255, `observeSilence` ~line 384, `advance` ~line 400)
- Modify: `core/turn/stop.go` (comment, lines 131-135)
- Test: `core/turn/stop_test.go`

**Interfaces:**
- Consumes: `turn.ExternalStart()`, `turn.ExternalStop()`, `Observation.ExternalStart`, `Observation.ExternalStop` (exist); `newStopHarness(t, cfg, det)` in `stop_test.go`.
- Produces: `core.FramePushToTalk FrameType = 830`, `core.PushToTalkFrame{Pressed bool}`. `(*turn.Processor).SignalStop` is removed. The frame is the only signal source.

- [ ] **Step 1: Add the frame** (needed for the tests to compile)

`core/frame.go`, after `FrameUsageMetrics           FrameType = 829`:

```go
	FramePushToTalk             FrameType = 830
```

`core/control.go`, after `InputMutedStateFrame`:

```go
// PushToTalkFrame reports the client's push-to-talk button. With the
// ExternalStart/ExternalStop strategies, pressed opens the next user turn and
// released ends it.
type PushToTalkFrame struct{ Pressed bool }

func (*PushToTalkFrame) Type() FrameType { return FramePushToTalk }

// SurvivesGenerationFence: the barge-in a press causes bumps the generation;
// the matching release must still reach the turn processor.
func (*PushToTalkFrame) SurvivesGenerationFence() {}
```

`core/session.go`, in the frame-name switch, after `case FrameUsageMetrics: return "usage_metrics"`:

```go
	case FramePushToTalk:
		return "push_to_talk"
```

- [ ] **Step 2: Write the failing tests.** In `core/turn/stop_test.go`, replace `TestProcessorSignalStopThroughSession` with:

```go
func newPTTHarness(t *testing.T) *harness {
	cfg := DefaultConfig()
	cfg.StartStrategies = []UserTurnStartStrategy{ExternalStart()}
	return newStopHarness(t, cfg, StopChain(ExternalStop()))
}

func (h *harness) ptt(pressed bool) {
	_ = h.sess.Post(&core.PushToTalkFrame{Pressed: pressed})
	h.settle()
}

func TestPushToTalkIgnoresSpeechWithoutPress(t *testing.T) {
	h := newPTTHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	if h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) > 0 }) {
		t.Fatal("voice detection alone started a push-to-talk turn")
	}
}

func TestPushToTalkPressSpeakRelease(t *testing.T) {
	h := newPTTHarness(t)
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	if !h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 }) {
		t.Fatal("press + speech did not start a turn")
	}
	_ = h.sess.Post(&core.UserStoppedSpeakingFrame{Seq: 2, Gen: 1})
	if h.run(3*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) > 0 }) {
		t.Fatal("a pause ended a held push-to-talk turn")
	}
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 3, Gen: 1}) // resumes while held: same turn
	h.settle()
	h.ptt(false)
	if !h.run(time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) == 1 }) {
		t.Fatal("release did not end the turn")
	}
	if n := len(h.events(core.TurnEventStarted)); n != 1 {
		t.Fatalf("%d turns started, want 1", n)
	}
	if err := h.sess.Close(); err != nil || h.sess.LeakedTasks() != 0 {
		t.Fatalf("close %v leaked %d", err, h.sess.LeakedTasks())
	}
}

func TestPushToTalkPressDuringSpeechStartsTurn(t *testing.T) {
	h := newPTTHarness(t)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(300*time.Millisecond, func() bool { return false })
	h.ptt(true)
	if !h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 }) {
		t.Fatal("pressing while already speaking did not start the turn")
	}
}

func TestPushToTalkPressWithoutSpeechDoesNotLinger(t *testing.T) {
	h := newPTTHarness(t)
	h.ptt(true)
	h.ptt(false)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	if h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) > 0 }) {
		t.Fatal("an earlier press-and-release started a later unpressed turn")
	}
}

func TestPushToTalkReleaseWithNoTurnIsIgnored(t *testing.T) {
	h := newPTTHarness(t)
	h.ptt(false) // no open turn
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	h.run(time.Second, func() bool { return len(h.events(core.TurnEventStarted)) == 1 })
	if h.run(2*time.Second, func() bool { return len(h.events(core.TurnEventCompleted)) > 0 }) {
		t.Fatal("a stale release ended the next turn")
	}
}

func TestPushToTalkBargesIn(t *testing.T) {
	h := newPTTHarness(t)
	h.botSpeaks()
	h.ptt(true)
	_ = h.sess.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	if !h.run(time.Second, func() bool { return h.asst.InterruptionCount.Load() == 1 }) {
		t.Fatal("press + speech while the bot spoke did not barge in")
	}
}
```

- [ ] **Step 3: Run them and confirm they fail**

Run: `go test ./core/turn/ -run 'TestPushToTalk' -v`
Expected: `IgnoresSpeechWithoutPress` passes. The other tests FAIL (the processor forwards `PushToTalkFrame` and never sets `ExternalStart`).

- [ ] **Step 4: Implement in `core/turn/processor.go`**

In the struct, replace:

```go
	extStop       atomic.Bool // external end-of-turn signal, consumed by the run loop
```

with:

```go
	extStop       bool // push-to-talk released while a turn is open; consumed by the next observation
	held          bool // push-to-talk button is down
```

Delete `SignalStop` and its comment (lines 152-160).

In `onVADStart`, change `p.extStop.Store(false)` to `p.extStop = false`.

In the downstream `switch v := f.(type)` of `ProcessFrame`, add after `case tickFrame:`:

```go
	case *core.PushToTalkFrame:
		return p.onPushToTalk(v.Pressed, e)
```

Add the method after `onVADStart`:

```go
// onPushToTalk applies a push-to-talk press or release and consumes the frame.
// A press is ExternalStart evidence for the current or next VAD segment, so
// pressing just before or while speaking both work; a release ends the open
// turn through ExternalStop. A release with no open turn clears the press, so
// it cannot start a later turn.
func (p *Processor) onPushToTalk(pressed bool, e core.Emitter) error {
	p.held = pressed
	if !pressed && p.open {
		p.extStop = true
	}
	return p.observe(e, "", false) // no-op when no turn is open
}
```

In `observeSilence`, replace `ExternalStop: p.extStop.Swap(false),` with:

```go
	stop := p.extStop
	p.extStop = false
	obs := Observation{ExternalStart: p.held, ExternalStop: stop, At: at, SilenceSince: silenceSince, TurnID: p.id, Generation: p.mgrGen, Speaking: p.vadSpeaking, Activity: p.vadSpeaking,
		BotSpeaking: p.bot, Transcript: text, TranscriptFinal: final}
```

(This replaces the whole existing `obs := Observation{...}` statement.)

In `advance`, delete the cross-goroutine branch. Nothing sets `extStop` off the run loop any more:

```go
	if p.extStop.Load() && p.open {
		return p.observe(e, "", false)
	}
```

- [ ] **Step 5: Update the `ExternalStop` comment in `core/turn/stop.go`**

```go
// ExternalStop ends the turn only when the caller signals it
// (Observation.ExternalStop; the turn processor sets it when a
// core.PushToTalkFrame reports the button released).
```

- [ ] **Step 6: Run the turn and core tests**

Run: `go test ./core/... -count=1`
Expected: PASS. If `TestPushToTalkPressDuringSpeechStartsTurn` fails, the manager did not evaluate starters on that observation. Check `manager.go:280-290`. The speaking branch must reach `advanceLocked` with `pendingExt` set (line 265 sets it first). Fix it there, not in the test.

- [ ] **Step 7: Commit**

```bash
git add core/frame.go core/control.go core/session.go core/turn/processor.go core/turn/stop.go core/turn/stop_test.go
git commit -m "feat(turn): push-to-talk frame drives ExternalStart and ExternalStop"
```

---

### Task 3: Per-call push-to-talk opt-in from `client-ready` through the server

**Files:**
- Modify: `transport/rtvi/rtvi.go` (`ClientReadyData` line 61, `Handler` struct line 92, constants, `HandleIncomingJSON`)
- Modify: `transport/ws/transport.go` (add method next to `AgentID()`, ~line 1140)
- Modify: `server/registry.go` (`CallSpec` line 69-94, turn block line 563-605)
- Modify: `server/server.go` (`onSessionReady`, after `resolveCall` ~line 463)
- Modify: `server/turn_stop.go` (comment line 16, `case "external"` line 55)
- Modify: `docs/quickstart.md`, `docs/pipecat-parity-matrix.md` (row "Start: external", line 20; "Strategies are library-only", line 150)
- Test: `transport/rtvi/rtvi_test.go`, `server/registry_test.go`

**Interfaces:**
- Consumes: `core.PushToTalkFrame` (Task 2).
- Produces: `rtvi.TypePushToTalk = "push-to-talk"`, `ClientReadyData.PushToTalk bool`, `(*rtvi.Handler).PushToTalk() bool`, `(*ws.Connection).PushToTalk() bool`, `server.CallSpec.PushToTalk bool`.

- [ ] **Step 1: Write the failing rtvi test** (append to `transport/rtvi/rtvi_test.go`)

```go
func TestPushToTalk(t *testing.T) {
	ptt := func(pressed string) string {
		return `{"id":"p","label":"rtvi-ai","type":"push-to-talk","data":{"pressed":` + pressed + `}}`
	}
	ready := func(h *rtvi.Handler, extra string) {
		t.Helper()
		if _, err := h.HandleIncomingJSON(`{"id":"r","label":"rtvi-ai","type":"client-ready","data":{"version":"` + rtvi.ProtocolVersion + `"` + extra + `}}`); err != nil {
			t.Fatal(err)
		}
	}

	off := rtvi.NewHandler()
	ready(off, "")
	if off.PushToTalk() {
		t.Fatal("push-to-talk on without opt-in")
	}
	if _, err := off.HandleIncomingJSON(ptt("true")); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("push-to-talk accepted on a call that did not opt in: %v", err)
	}

	on := rtvi.NewHandler()
	if _, err := on.HandleIncomingJSON(ptt("true")); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("push-to-talk before client-ready: %v", err)
	}
	ready(on, `,"push_to_talk":true`)
	if !on.PushToTalk() {
		t.Fatal("opt-in not recorded")
	}
	if _, err := on.HandleIncomingJSON(`{"id":"p","label":"rtvi-ai","type":"push-to-talk","data":{}}`); !errors.Is(err, rtvi.ErrRejectedMessage) {
		t.Fatalf("missing pressed accepted: %v", err)
	}
	for _, want := range []bool{true, false} {
		f, err := on.HandleIncomingJSON(ptt(fmt.Sprint(want)))
		p, ok := f.(*core.PushToTalkFrame)
		if err != nil || !ok || p.Pressed != want {
			t.Fatalf("pressed=%t gave %#v, %v", want, f, err)
		}
	}
}
```

Add `"fmt"` to the test file's imports.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./transport/rtvi/ -run TestPushToTalk -v`
Expected: FAIL to compile (`PushToTalk` undefined).

- [ ] **Step 3: Implement in `transport/rtvi/rtvi.go`**

Constants, after `TypeSetMuted`:

```go
	TypePushToTalk          = "push-to-talk" // {"pressed": true|false}; only on calls whose client-ready set push_to_talk
```

`ClientReadyData`:

```go
type ClientReadyData struct {
	Version string           `json:"version"`
	About   *json.RawMessage `json:"about,omitempty"`
	// PushToTalk: turns start and end on push-to-talk messages, not on voice detection.
	PushToTalk bool `json:"push_to_talk,omitempty"`
}
```

`Handler` struct, after `ready     atomic.Bool`:

```go
	pushToTalk atomic.Bool // set once by client-ready
```

Add next to `IsReady`:

```go
// PushToTalk reports whether client-ready opted this call into push-to-talk.
func (h *Handler) PushToTalk() bool { return h.pushToTalk.Load() }
```

In the `TypeClientReady` case, right after the duplicate-ready check (`if h.ready.Swap(true) {...}`) and before `OnClientReady`:

```go
		h.pushToTalk.Store(readyData.PushToTalk)
```

New case before `case TypeDisconnectBot`:

```go
	case TypePushToTalk:
		if !h.ready.Load() {
			return nil, fmt.Errorf("%w: push-to-talk before client-ready", ErrRejectedMessage)
		}
		if !h.pushToTalk.Load() {
			return nil, fmt.Errorf("%w: push-to-talk is not enabled for this call", ErrRejectedMessage)
		}
		var data struct {
			Pressed *bool `json:"pressed"`
		}
		if err := json.Unmarshal(msg.Data, &data); err != nil || data.Pressed == nil {
			return nil, fmt.Errorf(`%w: push-to-talk needs {"pressed": true|false}`, ErrRejectedMessage)
		}
		return &core.PushToTalkFrame{Pressed: *data.Pressed}, nil
```

- [ ] **Step 4: Run the rtvi tests**

Run: `go test ./transport/rtvi/ -count=1`
Expected: PASS

- [ ] **Step 5: Expose it on the connection** (`transport/ws/transport.go`, next to `AgentID()`)

```go
// PushToTalk reports whether the client opted this call into push-to-talk.
func (c *Connection) PushToTalk() bool { return c.rtvi.PushToTalk() }
```

- [ ] **Step 6: Write the failing registry test** (append to `server/registry_test.go`)

```go
// A push-to-talk call gets the turn processor even with turn.manager off,
// and a hands-free call with turn.manager off does not.
func TestPushToTalkCallInstallsTurnProcessor(t *testing.T) {
	reg := NewRegistry()
	for _, stage := range []string{"stt", "llm", "tts"} {
		reg.Register("google."+stage, func(*Config, StageSpec) (core.Processor, error) { return &core.PassthroughProcessor{}, nil })
	}
	cfg := DefaultConfig(ProfileDevelopment)
	cfg.Turn.Manager = false
	for _, ptt := range []bool{true, false} {
		vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
		call := CallSpec{STT: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, PushToTalk: ptt}
		pipe, err := reg.BuildCallPipeline(cfg, call, vadProc, &fakeTransportSink{})
		if err != nil {
			t.Fatal(err)
		}
		_, installed := pipe.Processors()[2].(*coreturn.Processor)
		if installed != ptt {
			t.Fatalf("push_to_talk=%t: turn processor installed=%t", ptt, installed)
		}
	}
}
```

- [ ] **Step 7: Run it and confirm it fails**

Run: `go test ./server/ -run TestPushToTalkCallInstallsTurnProcessor -v`
Expected: FAIL to compile (`PushToTalk` unknown field).

- [ ] **Step 8: Implement in `server/registry.go`**

`CallSpec`, after `VADStopMS int`:

```go
	// PushToTalk: the client opted in at client-ready. Turns start and end
	// only on its push-to-talk messages (ExternalStart/ExternalStop).
	PushToTalk bool
```

Turn block: change `if cfg.Turn.Manager {` to `if cfg.Turn.Manager || call.PushToTalk {`. Then, immediately after `stop, err := turnStopDetector(...)` and its error check, add:

```go
		if call.PushToTalk {
			// The button replaces voice detection for starting and ending turns;
			// MaxTurnDuration still caps a held button.
			mc.StartStrategies = []coreturn.UserTurnStartStrategy{coreturn.ExternalStart()}
			stop = coreturn.StopChain(coreturn.ExternalStop())
		}
```

- [ ] **Step 9: Pass the opt-in from the connection** (`server/server.go`, in `onSessionReady`, right after the `resolveCall` error check)

```go
	call.PushToTalk = conn.PushToTalk()
```

- [ ] **Step 10: Update the YAML rejection** (`server/turn_stop.go`)

Comment line 16: replace `// external (no signal source yet) is rejected until it has a consumer.` with `// external is rejected: push-to-talk is chosen per call by the client (client-ready push_to_talk), not by server config.`

Case:

```go
		case "external":
			return nil, errors.New(`turn.stop "external" is not a server setting; clients opt in with client-ready push_to_talk`)
```

- [ ] **Step 11: Run the server tests**

Run: `CGO_LDFLAGS=-Wl,-w go test ./server/... ./transport/... -count=1`
Expected: PASS. If a test asserts the old `"is not available yet"` text, update it to the new message.

- [ ] **Step 12: Document it**

`docs/quickstart.md`, after the `set-muted` paragraph:

```markdown
For push-to-talk, send `"push_to_talk": true` in `client-ready` data. Then
`{"label":"rtvi-ai","type":"push-to-talk","data":{"pressed":true}}` on button
down and `"pressed":false` on release. Speech only starts a turn while the
button is held, pauses while holding do not end it, and release ends it.
Pressing and speaking while the bot talks barges in. A call that did not opt
in gets a non-fatal error for `push-to-talk`.
```

`docs/pipecat-parity-matrix.md`:
- Line 20, row "Start: external": change the status cell from `library only` to `client opt-in`. Change the notes to `ExternalStart/ExternalStop are driven by the RTVI push-to-talk message on calls whose client-ready sets push_to_talk. Krisp is a vendor SDK: not planned.` Add `core/turn/stop_test.go` to its tests cell.
- Line 150: replace the bullet with `- **Start strategies:** \`turn.min_words\` selects MinWordsStart; push-to-talk (ExternalStart/ExternalStop) is opted into per call by the client.`

- [ ] **Step 13: Commit**

```bash
git add transport/rtvi/rtvi.go transport/rtvi/rtvi_test.go transport/ws/transport.go server/registry.go server/server.go server/turn_stop.go server/registry_test.go docs/quickstart.md docs/pipecat-parity-matrix.md
git commit -m "feat: per-call push-to-talk from client-ready through the turn processor"
```

(If step 11 changed a server test's expected error text, add that file too.)

---

### Task 4: UI sends mute to the server

**Files:**
- Modify: `ui/src/voice/protocol.ts`
- Modify: `ui/src/voice/VoiceClient.ts` (`toggleMute` line 367, bot-ready case line 503, new private helper)
- Test: `ui/src/voice/protocol.test.ts`, `ui/src/routes/agents/AgentVoiceTest.test.tsx` (the mute block, ~line 344)

**Interfaces:**
- Produces: `RTVI_TYPE_SET_MUTED`, `encodeSetMutedFrame(muted: boolean): Uint8Array`, and the private `VoiceClient.sendRTVI(type, frame, data): boolean`. Task 5 uses `sendRTVI`.

- [ ] **Step 1: Write the failing protocol test** (`ui/src/voice/protocol.test.ts`; add `encodeSetMutedFrame, RTVI_TYPE_SET_MUTED` to the import)

```ts
  it("encodes set-muted with the muted flag", () => {
    const decoded = decodeBinaryFrame(encodeSetMutedFrame(true));
    expect(decoded?.kind === "message" && decoded.parsed).toMatchObject({
      label: RTVI_LABEL,
      type: RTVI_TYPE_SET_MUTED,
      data: { muted: true },
    });
  });
```

- [ ] **Step 2: Extend the page test** (`AgentVoiceTest.test.tsx`, in the mute block after `await user.click(muteBtn);` and the `[Mic Muted]` assertion; import `RTVI_TYPE_SET_MUTED`)

```ts
    const muteMessages = () =>
      ws.sentData
        .map((d) => decodeBinaryFrame(d))
        .filter((f) => f?.kind === "message" && f.parsed?.type === RTVI_TYPE_SET_MUTED)
        .map((f) => (f?.kind === "message" ? f.parsed?.data : null));
    expect(muteMessages()).toEqual([{ muted: true }]);
```

After the second click (unmute), add:

```ts
    expect(muteMessages()).toEqual([{ muted: true }, { muted: false }]);
```

If this test's `ws` variable has a different name in that block, use the `MockWebSocket.instances[0]` the test already holds.

- [ ] **Step 3: Run them and confirm they fail**

Run (from `ui/`): `npx vitest run src/voice/protocol.test.ts src/routes/agents/AgentVoiceTest.test.tsx`
Expected: FAIL (`encodeSetMutedFrame` is not exported; no set-muted message is sent).

- [ ] **Step 4: Implement `protocol.ts`**

After `RTVI_TYPE_SEND_TEXT`:

```ts
export const RTVI_TYPE_SET_MUTED = "set-muted";
```

After `encodeSendTextFrame`:

```ts
/** Encodes set-muted: the server drops the caller's audio while muted. */
export function encodeSetMutedFrame(muted: boolean): Uint8Array {
  return encodeRTVIMessage(RTVI_TYPE_SET_MUTED, { muted });
}
```

- [ ] **Step 5: Implement `VoiceClient.ts`**

Import `encodeSetMutedFrame` and `RTVI_TYPE_SET_MUTED`. Replace `toggleMute`:

```ts
  /**
   * Toggles microphone mute: disables the mic track and tells the server, which
   * then drops the caller's audio (no turns, STT, or recorded speech).
   */
  public toggleMute(): void {
    const nextMuted = !this.state.isMuted;
    this.audioController.setMuted(nextMuted);
    this.updateState({ isMuted: nextMuted });
    this.sendRTVI(RTVI_TYPE_SET_MUTED, encodeSetMutedFrame(nextMuted), { muted: nextMuted });
  }
```

Add a private helper above `sendClientReady`:

```ts
  /** Sends one RTVI client message and logs it. Returns false when the socket is not open. */
  private sendRTVI(type: string, frame: Uint8Array, data: unknown): boolean {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false;
    this.ws.send(frame as unknown as ArrayBufferView<ArrayBuffer>);
    this.recordEvent("out", type, JSON.stringify({ label: "rtvi-ai", type, data }), data);
    return true;
  }
```

If `recordEvent`'s fourth parameter is typed narrower than `unknown`, cast `data` to that type at the call.

In `case RTVI_TYPE_BOT_READY:`, after its `this.updateState({... sessionState: "ready" ...})` call:

```ts
        // A reconnect starts an unmuted server session; restate a mute the UI still shows.
        if (this.state.isMuted) {
          this.sendRTVI(RTVI_TYPE_SET_MUTED, encodeSetMutedFrame(true), { muted: true });
        }
```

- [ ] **Step 6: Add the reconnect test** (`AgentVoiceTest.test.tsx`, new `it` modelled on the text-turn test at line 385: render, start, receive `bot-ready`)

```ts
  it("restates mute to the server after bot-ready", async () => {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={createMockAgent()} client={createMockClient()} keyValue="mgmt-key-1"
          audioController={mockAudio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Start voice test" }));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    await waitFor(() => expect(ws.sentData.length).toBeGreaterThan(0));
    await user.click(screen.getByTestId("mute-btn")); // muted before the session is ready
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });
    await waitFor(() => expect(screen.getByTestId("connection-state")).toHaveTextContent("READY"));
    const muted = ws.sentData
      .map((d) => decodeBinaryFrame(d))
      .filter((f) => f?.kind === "message" && f.parsed?.type === RTVI_TYPE_SET_MUTED);
    expect(muted.length).toBe(2); // the click, then the restatement after bot-ready
  });
```

The mute button renders while the state is `connected` (`isConnectedOrReady`, `AgentVoiceTest.tsx:54`), so it can be clicked before `bot-ready`. At that point the socket is open, so the click itself sends one `set-muted`.

- [ ] **Step 7: Run the UI tests**

Run (from `ui/`): `npx vitest run src/voice src/routes/agents/AgentVoiceTest.test.tsx`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add ui/src/voice/protocol.ts ui/src/voice/protocol.test.ts ui/src/voice/VoiceClient.ts ui/src/routes/agents/AgentVoiceTest.test.tsx
git commit -m "feat(ui): mute tells the server to drop caller audio"
```

---

### Task 5: UI push-to-talk (opt-in toggle and hold-to-talk button)

**Files:**
- Modify: `ui/src/voice/protocol.ts`
- Modify: `ui/src/voice/VoiceClient.ts` (`VoiceClientState` line 82, initial state line 111, `ConnectOptions` line 99, `connect`, `disconnect`, `sendClientReady` line 400)
- Modify: `ui/src/routes/agents/AgentVoiceTest.tsx` (state ~line 28, `handleConnect` line 60, buttons ~line 215-262)
- Test: `ui/src/voice/protocol.test.ts`, `ui/src/routes/agents/AgentVoiceTest.test.tsx`

**Interfaces:**
- Consumes: `VoiceClient.sendRTVI` (Task 4).
- Produces: `RTVI_TYPE_PUSH_TO_TALK`, `encodePushToTalkFrame(pressed)`, `encodeClientReadyFrame(version?, pushToTalk?)`, `ConnectOptions.pushToTalk?: boolean`, `VoiceClientState.pushToTalk`, `VoiceClientState.isTalking`, `VoiceClient.setTalking(pressed: boolean): void`.

- [ ] **Step 1: Write the failing protocol test** (`protocol.test.ts`; import `encodePushToTalkFrame, RTVI_TYPE_PUSH_TO_TALK`)

```ts
  it("encodes push-to-talk and the client-ready opt-in", () => {
    const ptt = decodeBinaryFrame(encodePushToTalkFrame(false));
    expect(ptt?.kind === "message" && ptt.parsed).toMatchObject({ type: RTVI_TYPE_PUSH_TO_TALK, data: { pressed: false } });
    const ready = decodeBinaryFrame(encodeClientReadyFrame("2.1.0", true));
    expect(ready?.kind === "message" && ready.parsed?.data).toEqual({ version: "2.1.0", push_to_talk: true });
    const plain = decodeBinaryFrame(encodeClientReadyFrame("2.1.0"));
    expect(plain?.kind === "message" && plain.parsed?.data).toEqual({ version: "2.1.0" });
  });
```

- [ ] **Step 2: Write the failing page test** (`AgentVoiceTest.test.tsx`; import `RTVI_TYPE_PUSH_TO_TALK`, `RTVI_TYPE_CLIENT_READY`)

```ts
  it("push-to-talk: opts in at client-ready and sends press and release", async () => {
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

    const messages = (type: string) =>
      ws.sentData
        .map((d) => decodeBinaryFrame(d))
        .filter((f) => f?.kind === "message" && f.parsed?.type === type)
        .map((f) => (f?.kind === "message" ? f.parsed?.data : null));
    expect(messages(RTVI_TYPE_CLIENT_READY)[0]).toMatchObject({ push_to_talk: true });
    expect(screen.getByLabelText("Push to talk")).toBeDisabled();

    const talk = screen.getByTestId("ptt-btn");
    await user.pointer({ keys: "[MouseLeft>]", target: talk });
    expect(talk).toHaveAttribute("aria-pressed", "true");
    await user.pointer({ keys: "[/MouseLeft]", target: talk });
    expect(talk).toHaveAttribute("aria-pressed", "false");
    talk.focus();
    await user.keyboard("[Space>]");
    await user.keyboard("[/Space]");
    expect(messages(RTVI_TYPE_PUSH_TO_TALK)).toEqual([
      { pressed: true }, { pressed: false }, { pressed: true }, { pressed: false },
    ]);
  });

  it("hands-free calls show no hold-to-talk button", async () => {
    const mockAudio = new BrowserAudioController();
    vi.spyOn(mockAudio, "startCapture").mockResolvedValue(undefined);
    render(
      <MemoryRouter>
        <AgentVoiceTest agent={createMockAgent()} client={createMockClient()} keyValue="mgmt-key-1"
          audioController={mockAudio} wsBaseUrl="ws://127.0.0.1:9090/ws" />
      </MemoryRouter>,
    );
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: "Start voice test" }));
    await waitFor(() => expect(MockWebSocket.instances.length).toBe(1));
    const ws = MockWebSocket.instances[0];
    await waitFor(() => expect(ws.sentData.length).toBeGreaterThan(0));
    ws.receiveRTVIMessage(RTVI_TYPE_BOT_READY, { version: "2.1.0" });
    await waitFor(() => expect(screen.getByTestId("connection-state")).toHaveTextContent("READY"));
    expect(screen.queryByTestId("ptt-btn")).toBeNull();
  });
```

- [ ] **Step 3: Run them and confirm they fail**

Run (from `ui/`): `npx vitest run src/voice/protocol.test.ts src/routes/agents/AgentVoiceTest.test.tsx`
Expected: FAIL (no exports, no "Push to talk" control).

- [ ] **Step 4: Implement `protocol.ts`**

```ts
export const RTVI_TYPE_PUSH_TO_TALK = "push-to-talk";
```

Add `push_to_talk?: boolean;` to `RTVIClientReadyData`. Replace `encodeClientReadyFrame`:

```ts
export function encodeClientReadyFrame(
  version: string = RTVI_PROTOCOL_VERSION,
  pushToTalk = false,
): Uint8Array {
  return encodeRTVIMessage<RTVIClientReadyData>(
    RTVI_TYPE_CLIENT_READY,
    pushToTalk ? { version, push_to_talk: true } : { version },
  );
}

/** Encodes push-to-talk: pressed on button down, released on button up. */
export function encodePushToTalkFrame(pressed: boolean): Uint8Array {
  return encodeRTVIMessage(RTVI_TYPE_PUSH_TO_TALK, { pressed });
}
```

- [ ] **Step 5: Implement `VoiceClient.ts`**

`VoiceClientState`: add `pushToTalk: boolean;` and `isTalking: boolean;`. Initial state: `pushToTalk: false, isTalking: false,`.

`ConnectOptions`: add

```ts
  /** Turns start and end on the hold-to-talk button instead of voice detection. */
  pushToTalk?: boolean;
```

In `connect`, right after the `agentId` validation block:

```ts
    this.updateState({ pushToTalk: options.pushToTalk === true, isTalking: false });
```

In `disconnect`'s final `updateState` (the one setting `sessionState: "disconnected"`), add `isTalking: false,`.

In `sendClientReady`, replace `const readyFrame = encodeClientReadyFrame();` with `const readyFrame = encodeClientReadyFrame(RTVI_PROTOCOL_VERSION, this.state.pushToTalk);`. In the same method's logged JSON, change `data: { version: "2.1.0" },` to `data: this.state.pushToTalk ? { version: "2.1.0", push_to_talk: true } : { version: "2.1.0" },`. Import `RTVI_PROTOCOL_VERSION`, `RTVI_TYPE_PUSH_TO_TALK`, and `encodePushToTalkFrame`.

Add after `toggleMute`:

```ts
  /** Push-to-talk: true when the hold-to-talk button goes down, false when it is released. */
  public setTalking(pressed: boolean): void {
    if (!this.state.pushToTalk || this.state.isTalking === pressed) return;
    if (this.sendRTVI(RTVI_TYPE_PUSH_TO_TALK, encodePushToTalkFrame(pressed), { pressed })) {
      this.updateState({ isTalking: pressed });
    }
  }
```

- [ ] **Step 6: Implement the page** (`AgentVoiceTest.tsx`)

State, next to `textMessage`:

```tsx
  const [pushToTalk, setPushToTalk] = useState(false);
```

`handleConnect`: add `pushToTalk,` to the `connect({...})` options.

Before the `{!isConnectedOrReady && !isConnecting && (` Start button, inside the same flex row:

```tsx
          <label style={{ fontSize: "0.85rem", display: "flex", gap: "4px", alignItems: "center" }}>
            <input
              type="checkbox"
              checked={pushToTalk}
              disabled={isConnectedOrReady || isConnecting}
              onChange={(e) => setPushToTalk(e.target.checked)}
            />
            Push to talk
          </label>
```

Inside the `isConnectedOrReady` fragment, after the mute button:

```tsx
              {state.pushToTalk && (
                <button
                  type="button"
                  className="action-button-small"
                  data-testid="ptt-btn"
                  aria-pressed={state.isTalking}
                  onPointerDown={() => voiceClient.setTalking(true)}
                  onPointerUp={() => voiceClient.setTalking(false)}
                  onPointerCancel={() => voiceClient.setTalking(false)}
                  onPointerLeave={() => voiceClient.setTalking(false)}
                  onBlur={() => voiceClient.setTalking(false)}
                  onKeyDown={(e) => {
                    if ((e.key === " " || e.key === "Enter") && !e.repeat) {
                      e.preventDefault();
                      voiceClient.setTalking(true);
                    }
                  }}
                  onKeyUp={(e) => {
                    if (e.key === " " || e.key === "Enter") {
                      e.preventDefault();
                      voiceClient.setTalking(false);
                    }
                  }}
                  style={{
                    background: state.isTalking ? "#e4f3ec" : "#ffffff",
                    borderColor: state.isTalking ? "#2f7a55" : "#c9d3d2",
                  }}
                >
                  {state.isTalking ? "Talking… release to send" : "Hold to talk"}
                </button>
              )}
```

- [ ] **Step 7: Run the UI tests and typecheck**

Run (from `ui/`): `npx vitest run src/voice src/routes/agents/AgentVoiceTest.test.tsx && npx tsc --noEmit -p tsconfig.app.json`
Expected: PASS, no type errors. If `ui/` has no `tsconfig.app.json`, use the `typecheck` or `build` script from `ui/package.json`.

- [ ] **Step 8: Manual end-to-end check** (needs a running server with credentials. If none is available, write that in the commit body instead.)

Start the server and UI as described in `docs/quickstart.md`. Open an agent's voice test and tick **Push to talk**. Check:
- Speaking without holding gets no reply.
- Hold, speak, pause for 2 seconds, keep speaking, release: one reply to the whole utterance.
- Release mid-word: the final transcript still arrives (finalizer timeout).
- Hold and speak while the bot talks: the bot stops.

- [ ] **Step 9: Commit**

```bash
git add ui/src/voice/protocol.ts ui/src/voice/protocol.test.ts ui/src/voice/VoiceClient.ts ui/src/routes/agents/AgentVoiceTest.tsx ui/src/routes/agents/AgentVoiceTest.test.tsx
git commit -m "feat(ui): push-to-talk toggle and hold-to-talk button"
```

---

### Task 6: Delete core code the server does not use and will not need

**Files:**
- Delete: `core/context_aggregator.go`
- Modify: `core/context_hardening_test.go` (delete `TestContextAggregatorConsumesPublicToolFramesInOrder` at line 138; line 262 uses `&ContextAggregator{}`)
- Modify: `providers/google/llm_async_test.go:48`, `providers/google/llm_usage_test.go:31`, `providers/google/tool_adapter_test.go:195,282,324`
- Create: `providers/google/tool_frames_test.go`
- Modify: `core/output_state.go` (delete `IsSpeaking`, lines 100-104), `core/output_state_test.go` (lines 46-48, 59-61)
- Modify: `core/turn/start.go` (delete `TranscriptionStart` and `WakePhraseStart` with their types and helpers), `core/turn/start_test.go` (delete the tests that use them: line 77's test, `TestWakePhraseGatesThenTimesOut`, `TestWakePhraseStaysAwakeThroughLongBotSpeech`, `TestWakePhraseNonASCII`)
- Modify: `core/turn/processor.go` (comments at lines 60 and 415 that mention wake phrases)
- Modify: `tools/compat/parity_fixtures_test.go` (lines 173-176)
- Modify: `docs/pipecat-parity-matrix.md` (rows "Start: transcription" line 18 and "Start: wake phrase" line 19)

**Interfaces:**
- Consumes: nothing from earlier tasks. This task is independent and can run first.
- Produces: no new API. Removes `core.ContextAggregator`, `(*core.SpeakingTracker).IsSpeaking`, `turn.TranscriptionStart`, `turn.WakePhraseStart`.

- [ ] **Step 1: Prove each symbol has no production caller** (the baseline. Every listed symbol must show only its definition and test files.)

```bash
for s in ContextAggregator 'IsSpeaking\(' TranscriptionStart WakePhraseStart wakePhraseStart transcriptionStart; do
  echo "== $s"; grep -rnE "$s" --include='*.go' . | grep -v -e third_party -e pipecat-impl -e _test.go
done
```

Expected: only the defining lines in `core/context_aggregator.go`, `core/output_state.go`, and `core/turn/start.go`. If any other file appears, stop and report it. The symbol is used, and this plan was wrong about it.

- [ ] **Step 2: Give the Google tests their own tool-frame recorder**

The Google LLM tests need tool calls and results written to the history so the continuation request carries them. In production the tool loop does this (`ClaimToolCall` / `AddToolResultStatus`). In these provider unit tests a small recorder stands in. Create `providers/google/tool_frames_test.go`:

```go
package google

import (
	"context"

	"govoice/core"
)

// toolFrameRecorder writes tool frames into history, standing in for the tool
// loop so these provider tests can check continuation requests in isolation.
type toolFrameRecorder struct{ history *core.ConversationContext }

func (r *toolFrameRecorder) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	if dir == core.Downstream {
		switch v := f.(type) {
		case *core.ToolCallFrame:
			if err := r.history.AddToolCall(string(v.Call.TurnID), uint64(v.Call.Generation), v.Call.ID, v.Call.Name, v.Call.Arguments, v.Timestamp); err != nil {
				return err
			}
		case *core.ToolResultFrame:
			if err := r.history.AddToolResultStatus(string(v.Turn), uint64(v.Gen), v.Result.CallID, v.Result.Status, v.Result.Result, v.Result.ErrorCategory, v.Timestamp); err != nil {
				return err
			}
		}
	}
	return e.PushDirection(f, dir)
}
```

Check the package clause of `providers/google/tool_adapter_test.go` first. If it is `package google_test`, use that instead.

Replace every `&core.ContextAggregator{Context: history}` in the three Google test files with `&toolFrameRecorder{history: history}`. At `tool_adapter_test.go:195`, the `aggregator :=` variable becomes `recorder :=`.

- [ ] **Step 3: Fix the core tests**

In `core/context_hardening_test.go`, delete `TestContextAggregatorConsumesPublicToolFramesInOrder`. Tool-frame ordering is covered by the tool loop's tests. At line 262, change `NewPipeline(&ContextAggregator{}, nil)` to `NewPipeline(&PassthroughProcessor{}, nil)`. In `core/output_state_test.go`, delete the two `if !tracker.IsSpeaking() {...}` blocks. The `StopThrough`/`StopOutput` return values next to them already check that state.

- [ ] **Step 4: Delete the production code**

```bash
git rm core/context_aggregator.go
```

In `core/output_state.go`, delete `func (t *SpeakingTracker) IsSpeaking() bool {...}`.

In `core/turn/start.go`, delete `TranscriptionStart`, `transcriptionStart` and its methods, `WakePhraseStart`, `wakePhraseStart` and its methods, and any helper only they use (run `go vet ./core/turn/` after deleting to find leftovers such as an unused `regexp` import). Keep `VADStart`, `MinWordsStart`, and `ExternalStart`.

In `core/turn/start_test.go`, delete the four tests listed under **Files**.

In `core/turn/processor.go`, edit the two comments. Line 60: `(min_words, wake ...` becomes `(min_words ...`. Line 415: `(backchannel, wake phrase idle)` becomes `(backchannel, push-to-talk not pressed)`.

- [ ] **Step 5: Skip the removed strategies in the Pipecat parity fixtures** (`tools/compat/parity_fixtures_test.go`). The goldens come from Pipecat, so keep the scenarios and skip them:

```go
			case "transcription", "wake":
				t.Skipf("start strategy %q is not implemented in go-voice (unused by the server)", s.Kind)
```

This replaces the two existing `case "transcription":` and `case "wake":` arms.

- [ ] **Step 6: Update the parity matrix** (`docs/pipecat-parity-matrix.md`)

Lines 18 and 19: set the status cell to `not implemented` and the server cell to `—`. Replace the notes with `Removed 2026-10-10: unused by the server (YAGNI). Fixtures skip it.` Set the tests cell to `tools/compat/parity_fixtures_test.go (skipped)`.

- [ ] **Step 7: Run everything**

```bash
go build ./... && CGO_LDFLAGS=-Wl,-w go test ./core/... ./providers/google/... ./tools/compat/... -count=1
```

Expected: PASS. The parity test shows SKIP for the transcription and wake scenarios.

- [ ] **Step 8: Commit**

```bash
git add -A core/context_aggregator.go core/context_hardening_test.go core/output_state.go core/output_state_test.go core/turn/start.go core/turn/start_test.go core/turn/processor.go providers/google/ tools/compat/parity_fixtures_test.go docs/pipecat-parity-matrix.md
git commit -m "refactor(core): delete ContextAggregator, IsSpeaking, wake-phrase and transcription start"
```

---

## Final check (after all tasks)

```bash
go vet ./... && CGO_LDFLAGS=-Wl,-w go test ./... -count=1
(cd ui && npx vitest run)
```

Both must pass. Then run the full-branch review before merging.
