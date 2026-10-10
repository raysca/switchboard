# Cleanup And STT Bugfix Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A panicking call teardown cannot close the store while a recording is still writing, a wall-clock step cannot start a model turn during speech, and a finished utterance still produces its transcript when Google `CloseSend` fails, when the session generation moves after Google user-stop, or when xAI is interrupted while that utterance is draining.

**Architecture:** Session teardown moves into one `finishCall` method so the test calls the same function as the read loop. `AsyncTrigger` stores a monotonic offset from a process-start `time.Time` instead of `UnixNano`. Google STT half-closes through one helper, and a detached stream's own generation is still delivered. xAI interruption cancels only the active stream.

**Tech Stack:** Go 1.27.1, existing `testing` package, `coder/websocket` unchanged. xAI tests build with `-tags xaispeech`.

**Spec:** This plan. The contract is the seven behaviors under Global Constraints, from the audit of `main` at `1c6584b`. There is no separate spec file.

## Global Constraints

- A panic inside `releaseCall` must not delete `activeCalls` until `stopRecording` has returned.
- A panic inside `stopRecording` must not crash the process, and `activeCalls` must still be cleared after it.
- `fail` during setup must call `releaseCall` and, once recording has started, `stopRecording`, each under its own `recover`.
- `AsyncTrigger` busy marks must ignore a wall-clock step. A mark younger than `MaxWait` stays on. A mark older than `MaxWait` turns off. Default `MaxWait` stays one minute.
- Google `CloseSend` returning an error must cancel that stream's context and emit one `ErrorFrame`. A nil error must not cancel it.
- A Google final from a stream that user-stop already detached must be delivered at that stream's generation after `IncrementGeneration`. A result from a stream that is still `p.active` must still be dropped when its generation is behind `CurrentGeneration`.
- An xAI `InterruptionFrame` must leave streams already in `p.draining` running so `transcript.done` can finalize them. It must still clear and cancel `p.active`.
- Do not change providers, smart-turn, failover, OTel, or `tools/`. Do not enable `turn.stop: external`.
- On Darwin, cgo tests need `CGO_LDFLAGS=-Wl,-w`. Do not print secrets. One commit per task, and only the files that task names.

## Review Focus

- `observe.Attach` fails after `attachRecording` has returned a stop func. The caller must still get an error, and the recording must be finalized. Task 1 tests this.
- A failed Google `CloseSend` cancels and reports. A nil `CloseSend` error does not cancel. All three call sites use `halfClose`. Task 3 tests both outcomes.
- A Google result on a stream that is still active, after the session generation has moved, must not be delivered. Task 3 has that negative test.
- An xAI interruption with no prior user-stop must still clear `p.active`. The existing `TestXAISTT` interruption test in `providers/xai/stt_test.go` must stay green; Task 4 runs it.
- A busy mark stored longer ago than `MaxWait` must already be off, including when the wall clock has not moved. Task 2 tests that.

---

### Task 1: Finish the call only after release and recording

**Files:**
- Modify: `server/server.go` (the `fail` closure near line 473 and the cleanup goroutine near line 616)
- Modify: `server/drain_test.go` (replace `TestDrain_CleanupPanicRecoversAndClearsActiveCall`)
- Test: `server/drain_test.go`

**Interfaces:**
- Consumes: `admission.ReleaseSession`, `admission.AcquireSession`, `apiTestServer`, `SessionTimeline.Record`, `core.Session`
- Produces: `func recoverEach(log *slog.Logger, sessionID uint64, label string, fn func())` and `func (s *Server) finishCall(sessionID uint64, principal, agentID string, session *core.Session, conn *ws.Connection, timeline *SessionTimeline, errProviders map[string]string, release func(), stopRecording func())`. Later tasks do not call these.

- [ ] **Step 1: Write the failing tests**

Replace `TestDrain_CleanupPanicRecoversAndClearsActiveCall` so it calls `finishCall` instead of a copied defer. Add the three tests below in `server/drain_test.go`.

```go
func TestFinishCallWaitsForRecordingWhenReleasePanics(t *testing.T) {
	s := apiTestServer(t)
	sessionID := uint64(9999)
	principal, agentID := "panic-test-principal", "panic-test-agent"
	if err := s.admission.AcquireSession(principal, agentID, true); err != nil {
		t.Fatal(err)
	}
	pipe, err := core.NewPipeline(&core.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(sessionID, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.activeCalls[sessionID] = &activeCall{sessionID: sessionID, principal: principal}
	s.mu.Unlock()

	started := make(chan struct{})
	releaseRecording := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.finishCall(sessionID, principal, agentID, sess, nil, s.timelines.GetOrCreate(sessionID), nil,
			func() { panic("simulated releaseCall panic") },
			func() {
				close(started)
				<-releaseRecording
			},
		)
	}()
	<-started
	s.mu.Lock()
	_, still := s.activeCalls[sessionID]
	s.mu.Unlock()
	if !still {
		t.Fatal("activeCalls cleared while recording was still running")
	}
	close(releaseRecording)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("finishCall did not return after recording finished")
	}
	s.mu.Lock()
	_, still = s.activeCalls[sessionID]
	s.mu.Unlock()
	if still {
		t.Fatal("activeCalls still set after finishCall returned")
	}
	if err := s.admission.AcquireSession(principal, agentID, true); err != nil {
		t.Fatalf("admission not released: %v", err)
	}
	s.admission.ReleaseSession(principal, agentID)
}

func TestFinishCallRecordingPanicDoesNotStick(t *testing.T) {
	s := apiTestServer(t)
	sessionID := uint64(10000)
	principal, agentID := "rec-panic-principal", "rec-panic-agent"
	if err := s.admission.AcquireSession(principal, agentID, true); err != nil {
		t.Fatal(err)
	}
	pipe, err := core.NewPipeline(&core.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := core.NewSession(sessionID, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.activeCalls[sessionID] = &activeCall{sessionID: sessionID, principal: principal}
	s.mu.Unlock()

	s.finishCall(sessionID, principal, agentID, sess, nil, s.timelines.GetOrCreate(sessionID), nil,
		func() {},
		func() { panic("recording finalize") },
	)
	s.mu.Lock()
	_, still := s.activeCalls[sessionID]
	s.mu.Unlock()
	if still {
		t.Fatal("activeCalls still set after recording panic")
	}
}

func TestRecoverEachRunsStopAfterReleasePanics(t *testing.T) {
	s := apiTestServer(t)
	var stopped bool
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped recoverEach: %v", r)
			}
		}()
		recoverEach(s.logger, 1, "releaseCall", func() { panic("release") })
		recoverEach(s.logger, 1, "stopRecording", func() { stopped = true })
	}()
	if !stopped {
		t.Fatal("stopRecording did not run after releaseCall panicked")
	}
}
```

`TestDrain_CleanupPanicRecoversAndClearsActiveCall` currently copies the defer and panics before any recording wait. Delete that test. The two `finishCall` tests replace it.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd server && go test -count=1 -timeout 60s -run 'TestFinishCall|TestRecoverEach' .`

Expected: FAIL to compile. `finishCall` and `recoverEach` are not defined.

- [ ] **Step 3: Write the implementation**

Add `recoverEach` next to `logSetupFailure` in `server/server.go`:

```go
func recoverEach(log *slog.Logger, sessionID uint64, label string, fn func()) {
	if fn == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic during "+label, slog.Uint64("session", sessionID), slog.Any("panic", r))
		}
	}()
	fn()
}
```

Add `finishCall`. The defer waits for the recording goroutine before it touches `activeCalls`, and `recover` in that defer catches a panic from the body.

```go
func (s *Server) finishCall(sessionID uint64, principal, agentID string, session *core.Session, conn *ws.Connection, timeline *SessionTimeline, errProviders map[string]string, release func(), stopRecording func()) {
	recDone := make(chan struct{})
	go func() {
		defer close(recDone)
		recoverEach(s.logger, sessionID, "stopRecording", stopRecording)
	}()
	defer func() {
		<-recDone
		if r := recover(); r != nil {
			s.logger.Error("panic during session cleanup", slog.Uint64("session", sessionID), slog.Any("panic", r))
		}
		s.mu.Lock()
		delete(s.activeCalls, sessionID)
		s.mu.Unlock()
		if timeline != nil {
			timeline.Record(TimelineEvent{Type: EventDisconnect, Details: "session closed"})
		}
		s.metrics.SessionsStoppedTotal.Add(1)
	}()
	defer s.admission.ReleaseSession(principal, agentID)

	if session != nil {
		if n := session.LeakedTasks(); n > 0 {
			s.metrics.LeakedSessionsTotal.Add(1)
			s.metrics.LeakedWorkTotal.Add(uint64(n))
		}
		for _, proc := range session.Processors() {
			s.recordProcessorMetrics(proc)
			s.recordProviderErrors(proc, errProviders)
		}
		if err := session.Err(); err != nil {
			s.metrics.SessionsFailedTotal.Add(1)
			s.logger.Error("session failed", slog.Uint64("session", sessionID), slog.String("err", s.redactor.Redact(err.Error())))
			if conn != nil {
				conn.Close()
			}
		}
	}
	recoverEach(s.logger, sessionID, "releaseCall", release)
}
```

In `handleSession`, change `fail` so a later assignment to `stopRecording` is visible, and so both funcs are recovered:

```go
var stopRecording func()
fail := func(err error) (*core.Session, error) {
	recoverEach(s.logger, sessionID, "releaseCall", releaseCall)
	recoverEach(s.logger, sessionID, "stopRecording", stopRecording)
	s.admission.ReleaseSession(principal, agentID)
	s.metrics.SessionsFailedTotal.Add(1)
	timeline.Record(TimelineEvent{Type: EventError, Details: s.redactor.Redact(err.Error())})
	s.logSetupFailure(sessionID, agentID, phase, err)
	return nil, err
}
```

Replace `stopRecording := s.attachRecording(...)` with `stopRecording = s.attachRecording(...)`.

Replace the cleanup goroutine body (from `<-session.Done()` through `<-recDone`) with:

```go
go func() {
	<-session.Done()
	s.finishCall(sessionID, principal, agentID, session, conn, timeline, errProviders, releaseCall, stopRecording)
}()
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd server && go test -count=1 -timeout 120s -run 'TestFinishCall|TestRecoverEach|TestDrain_' .`

Expected: PASS. On Darwin, export `CGO_LDFLAGS=-Wl,-w` first if the link warns.

- [ ] **Step 5: Commit**

```bash
git add server/server.go server/drain_test.go
git commit -m "fix: finish call teardown only after release and recording"
```

---

### Task 2: Keep async-trigger busy marks on a monotonic clock

**Files:**
- Modify: `core/async_trigger.go` (`set` and `on`)
- Create: `core/async_trigger_mono_test.go`
- Test: `core/async_trigger_mono_test.go`, `core/async_trigger_test.go`

**Interfaces:**
- Consumes: nothing from Task 1
- Produces: nothing other tasks call. `set` and `on` stay unexported. Busy fields stay `atomic.Int64`. `0` still means off.

- [ ] **Step 1: Write the failing test**

Create `core/async_trigger_mono_test.go` with `package core`:

```go
package core

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestBusyMarkIsMonotonicOffset(t *testing.T) {
	var mark atomic.Int64
	set(&mark, true)
	if got := mark.Load(); got <= 0 || got > int64(time.Hour) {
		t.Fatalf("busy mark = %d, want a small monotonic offset, not a Unix timestamp", got)
	}
	tr := NewAsyncTrigger(AsyncTriggerConfig{MaxWait: time.Minute})
	if !tr.on(&mark) {
		t.Fatal("a mark just stored must still be busy")
	}
	mark.Store(int64(time.Since(monoBase)) - int64(2*time.Minute))
	if tr.on(&mark) {
		t.Fatal("a mark older than MaxWait must be off")
	}
	mark.Store(0)
	if tr.on(&mark) {
		t.Fatal("a cleared mark must be off")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd core && go test -count=1 -timeout 30s -run TestBusyMarkIsMonotonicOffset .`

Expected: FAIL. Today's `set` stores `time.Now().UnixNano()`, which is far larger than one hour, or `monoBase` is undefined.

- [ ] **Step 3: Write the implementation**

In `core/async_trigger.go`, add a process-start base and stop using `UnixNano`:

```go
// monoBase carries a monotonic reading. Offsets from it ignore wall-clock steps.
var monoBase = time.Now()

func set(a *atomic.Int64, on bool) {
	if !on {
		a.Store(0)
	} else if a.Load() == 0 {
		a.Store(int64(time.Since(monoBase)))
	}
}

func (t *AsyncTrigger) on(a *atomic.Int64) bool {
	since := a.Load()
	if since == 0 {
		return false
	}
	return time.Duration(int64(time.Since(monoBase))-since) < t.cfg.MaxWait
}
```

Change the field comment on `vadUser` from "Unix-nano since which each signal has been on" to "monotonic offset from monoBase; 0 means off."

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd core && go test -count=1 -timeout 60s -run 'TestBusyMarkIsMonotonicOffset|TestAsyncTrigger' .`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add core/async_trigger.go core/async_trigger_mono_test.go
git commit -m "fix: keep async-trigger busy marks on a monotonic clock"
```

---

### Task 3: Deliver a detached Google utterance

**Files:**
- Modify: `providers/google/stt.go` (the three `CloseSend` sites around lines 415, 426, and 444, and the generation check at line 818)
- Modify: `providers/google/stt_detach_cancel_test.go`
- Test: `providers/google/stt_detach_cancel_test.go`

**Interfaces:**
- Consumes: `STTStream.CloseSend`, `activeStream.cancel`, `core.Session.Reenter`, `core.Session.IncrementGeneration`, `MapError`
- Produces: `func (p *STTProcessor) halfClose(st *activeStream)`. No other task calls it.

- [ ] **Step 1: Write the failing tests**

Append these to `providers/google/stt_detach_cancel_test.go`.

```go
type failCloseSendStream struct{ *mockSTTStream }

func (s failCloseSendStream) CloseSend() error { return errors.New("close send failed") }

func TestGoogleSTTHalfCloseErrorCancelsAndReports(t *testing.T) {
	cfg := DefaultSTTConfig()
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var errs int
	pipe, err := core.NewPipeline(proc, &core.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(36, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	session.OnOutput = func(f core.Frame) {
		if _, ok := f.(*core.ErrorFrame); ok {
			errs++
		}
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, cancel := context.WithCancel(context.Background())
	st := &activeStream{ctx: ctx, cancel: cancel, stream: failCloseSendStream{newMockSTTStream()}, turnID: "turn-1-1", gen: 1}
	proc.halfClose(st)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("halfClose did not cancel after CloseSend error")
	}
	deadline := time.Now().Add(time.Second)
	for errs == 0 {
		if time.Now().After(deadline) {
			t.Fatal("halfClose did not emit an ErrorFrame")
		}
		time.Sleep(time.Millisecond)
	}
	okCtx, okCancel := context.WithCancel(context.Background())
	defer okCancel()
	proc.halfClose(&activeStream{ctx: okCtx, cancel: okCancel, stream: newMockSTTStream(), turnID: "ok", gen: 1})
	select {
	case <-okCtx.Done():
		t.Fatal("halfClose cancelled a stream whose CloseSend succeeded")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestGoogleSTTDetachedFinalSurvivesLaterGeneration(t *testing.T) {
	opener := &mockSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got string
	pipe, err := core.NewPipeline(proc, &core.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(37, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	session.OnOutput = func(f core.Frame) {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok && c.Final {
			mu.Lock()
			got = c.Text
			mu.Unlock()
		}
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	deadline := time.Now().Add(time.Second)
	for {
		opener.mu.Lock()
		n := len(opener.streams)
		opener.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream did not open")
		}
		time.Sleep(time.Millisecond)
	}
	_ = session.Post(&core.UserStoppedSpeakingFrame{})
	time.Sleep(20 * time.Millisecond)
	session.IncrementGeneration()
	opener.streams[0].PushResult(&STTResult{Transcript: "two plus two", IsFinal: true})
	deadline = time.Now().Add(time.Second)
	for {
		mu.Lock()
		text := got
		mu.Unlock()
		if text == "two plus two" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached final = %q, want two plus two", text)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGoogleSTTActiveStreamDropsStaleGeneration(t *testing.T) {
	opener := &mockSTTOpener{}
	cfg := DefaultSTTConfig()
	cfg.StreamOpener = opener
	proc, err := NewSTTProcessor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got int
	pipe, err := core.NewPipeline(proc, &core.PassthroughProcessor{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := core.NewSession(38, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	session.OnOutput = func(f core.Frame) {
		if c, ok := f.(*core.TranscriptCandidateFrame); ok && c.Final {
			got++
		}
	}
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_ = session.Post(&core.UserStartedSpeakingFrame{Seq: 1, Gen: 1})
	_ = session.Post(&core.InputAudioFrame{Samples: make([]byte, 640), Gen: 1})
	deadline := time.Now().Add(time.Second)
	for {
		opener.mu.Lock()
		n := len(opener.streams)
		opener.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream did not open")
		}
		time.Sleep(time.Millisecond)
	}
	session.IncrementGeneration()
	opener.streams[0].PushResult(&STTResult{Transcript: "stale", IsFinal: true})
	time.Sleep(50 * time.Millisecond)
	if got != 0 {
		t.Fatalf("active stream delivered %d finals after the generation moved", got)
	}
}
```

Add `"errors"` and `"sync"` to the imports of that file.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd providers/google && go test -count=1 -timeout 60s -run 'TestGoogleSTTHalfCloseErrorCancelsAndReports|TestGoogleSTTDetachedFinalSurvivesLaterGeneration|TestGoogleSTTActiveStreamDropsStaleGeneration' .`

Expected: FAIL. `halfClose` is undefined, and the detached-final test times out on the generation check at `stt.go` line 818.

- [ ] **Step 3: Write the implementation**

Add the helper and use it at every `CloseSend` site (`UserStartedSpeakingFrame`, `TurnEventStarted`, `UserStoppedSpeakingFrame`):

```go
func (p *STTProcessor) halfClose(st *activeStream) {
	if st == nil {
		return
	}
	if err := st.stream.CloseSend(); err != nil {
		st.cancel()
		if p.session != nil {
			_ = p.session.Reenter(&core.ErrorFrame{
				Err:  MapError("stt", err),
				Turn: core.TurnID(st.turnID),
				Gen:  core.Generation(st.gen),
			}, core.Downstream, p.index+1, st.gen)
		}
	}
}
```

Replace each `_ = old.stream.CloseSend()` with `p.halfClose(old)`.

In `spawnReceiver`, the skip condition is inside `p.mu.Lock()`. A detached stream is no longer `p.active`. Keep dropping empty turns and stale results that are still on the active stream:

```go
detached := p.active != st
if turn == "" || turnGen == 0 || (turnGen < currentGen && !detached) {
	p.mu.Unlock()
	continue
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd providers/google && go test -count=1 -timeout 120s -run 'TestGoogleSTT' .`

Expected: PASS, including `TestGoogleSTTDetachedStreamContextIsCancelledWhenItEnds`. A successful `CloseSend` must still leave the context alive until the provider ends the stream. `rg -n "CloseSend\\(" providers/google/stt.go` shows that call only inside `halfClose`.

- [ ] **Step 5: Commit**

```bash
git add providers/google/stt.go providers/google/stt_detach_cancel_test.go
git commit -m "fix: deliver a detached Google utterance and report a failed half-close"
```

---

### Task 4: Let a draining xAI utterance finish through interruption

**Files:**
- Modify: `providers/xai/stt.go` (the `InterruptionFrame` case around line 413)
- Modify: `providers/xai/stt_ordering_test.go`
- Test: `providers/xai/stt_ordering_test.go`, `providers/xai/stt_test.go`

**Interfaces:**
- Consumes: `newSTTHarness`, `wireSTTStream`, `stopStream`. No types from Tasks 1–3.
- Produces: nothing.

- [ ] **Step 1: Write the failing test**

In `sttHarness`, record finals from the session output:

```go
finals []string
```

Inside `session.OnOutput`, add:

```go
case *core.TranscriptCandidateFrame:
	if v.Final {
		h.finals = append(h.finals, v.Text)
	}
```

Add:

```go
func (h *sttHarness) finalTexts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.finals...)
}

func TestXAISTTInterruptionLeavesDrainingUtterance(t *testing.T) {
	h := newSTTHarness(t)
	h.send(t, &core.UserStartedSpeakingFrame{Gen: 1, Seq: 1})
	h.send(t, &core.InputAudioFrame{Samples: make([]byte, 640), SampleRate: 16000})
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.created"}}
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.partial", Text: "hello there"}}
	waitFor(t, "audio", func() bool { return len(h.ws.snapshot()) == 1 })
	time.Sleep(50 * time.Millisecond)
	h.send(t, &core.UserStoppedSpeakingFrame{})
	waitFor(t, "done", func() bool { return len(h.ws.snapshot()) == 2 })
	h.send(t, &core.InterruptionFrame{Gen: 2})
	h.p.mu.Lock()
	n := len(h.p.draining)
	h.p.mu.Unlock()
	if n != 1 {
		t.Fatalf("draining = %d, want 1 after interruption", n)
	}
	h.ws.recv <- recvItem{msg: &STTMessage{Type: "transcript.done", Text: "hello there"}}
	waitFor(t, "final", func() bool {
		for _, text := range h.finalTexts() {
			if text == "hello there" {
				return true
			}
		}
		return false
	})
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd providers/xai && go test -tags xaispeech -count=1 -timeout 60s -run TestXAISTTInterruptionLeavesDrainingUtterance .`

Expected: FAIL. `draining = 0` because the interruption case cancel-closes every draining stream.

- [ ] **Step 3: Write the implementation**

In the `InterruptionFrame` case, keep the `streamGen` update. Stop cancelling `p.draining`. Cancel only the stream that is still active:

```go
case *core.InterruptionFrame:
	p.mu.Lock()
	generation := uint64(frame.Gen)
	if p.session != nil && p.session.CurrentGeneration() > generation {
		generation = p.session.CurrentGeneration()
	}
	if generation > p.streamGen {
		p.streamGen = generation
	}
	old := p.active
	p.active = nil
	p.mu.Unlock()
	p.stopStream(old)
	return e.PushDirection(f, dir)
```

Do not assign `p.draining = nil` on this path. `CancelFrame` still stops both lists.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd providers/xai && go test -tags xaispeech -count=1 -timeout 120s -run 'TestXAISTT' .`

Expected: PASS. `TestXAISTT` cases that interrupt an active stream must still see `p.active == nil`.

- [ ] **Step 5: Commit**

```bash
git add providers/xai/stt.go providers/xai/stt_ordering_test.go
git commit -m "fix: let a draining xAI utterance finish through interruption"
```
