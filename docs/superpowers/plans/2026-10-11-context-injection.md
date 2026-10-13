# Live-Call Context Injection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A management client can add operator notes to a live call and optionally make the agent respond, through two new routes under `/api/v1/sessions/{id}`.

**Architecture:** Notes ride on the existing `core.AsyncTrigger` (wait for idle, inject in order, start one model turn). The trigger is built for every call. The server keeps a per-instance map from the stored session id to the call's injector, and the management router serves the two routes from it.

**Tech Stack:** Go (workspace modules `core`, `server`), `net/http`, existing `server/api` helpers.

**Spec:** `docs/superpowers/specs/2026-10-11-context-injection-design.md`

## Global Constraints

- Management key only: the routes live behind `handleAPI`'s authentication; a voice token must not work.
- Limits: 1 to 8 messages per request; each note 1 to 4096 bytes after trimming, no NUL; per session a token bucket of burst 4, refill 1 per second.
- A note is stored as a **user-role** message whose content is `[note from the operator] ` followed by the text. Operators cannot set a role.
- Notes and content are never logged or traced; log the session id, the count and `run` only.
- Responses: 202 `{"data":{"accepted":N}}`, 400 `invalid_request`, 404 `not_found`, 409 `call_not_ready`, 429 `rate_limited`.
- Build and test with `CGO_LDFLAGS=-Wl,-w`; `server` tests also with `-tags xaispeech`. Run Go tests from the module directory (`core`, `server`).
- Do not touch `core/handoff.go` behaviour or `server/graph_call.go`: another line of work (agent graphs) uses the same trigger.

## Review Focus

- Notes posted while the caller is speaking or the bot is replying wait for idle, and never land between a tool call and its result. (Task 1 tests idle waiting; Task 3 tests the whole pipeline.)
- A note with `run` arriving together with a normal queued item starts one turn, not two. (Task 1.)
- A call that ends with notes queued drops them without a panic or a write after close. (Task 1.)
- A note for a call that just ended gets 404 and one whose pipeline has not started gets 409, never 500 or a silent drop. (Tasks 1 and 4.)
- Oversized, empty, whitespace-only, NUL-containing and too-many-message requests are rejected with 400, and a burst past the bucket gets 429 while the call stays healthy. (Tasks 2 and 4.)
- A call without async tools behaves as before with the always-on trigger: same frames to the model, no extra turn. (Task 3.)

---

### Task 1: Core: notes without a turn, and trigger readiness

**Files:**
- Modify: `core/tool_contracts.go` (add `NoTurn` to `AsyncToolResult`)
- Modify: `core/async_trigger.go` (`fire`, add `Ready`)
- Test: `core/async_trigger_noturn_test.go` (create; package `core_test`, reuses `newTriggerRig`, `waitFor`, `never`, `llmSink` from `async_trigger_test.go`)

**Interfaces:**
- Produces: `AsyncToolResult.NoTurn bool`; `(*AsyncTrigger).Ready() bool` (true once attached to a started session).

- [ ] **Step 1: Write the failing tests** (create `core/async_trigger_noturn_test.go`)

```go
package core_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"govoice/core"
)

type orderLog struct {
	mu  sync.Mutex
	got []string
}

func (o *orderLog) add(s string) { o.mu.Lock(); o.got = append(o.got, s); o.mu.Unlock() }
func (o *orderLog) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.got)
}

func (r *triggerRig) deliverItem(o *orderLog, name string, noTurn bool) {
	r.trig.Deliver(core.AsyncToolResult{
		Call:   core.ToolCall{ID: name, Name: name},
		NoTurn: noTurn,
		Inject: func() error { o.add(name); return nil },
	})
}

// A note without run is injected when the call is idle and starts no model turn.
func TestAsyncTriggerNoTurnItemInjectsWithoutATurn(t *testing.T) {
	r := newTriggerRig(t, nil)
	o := &orderLog{}
	r.deliverItem(o, "note", true)
	waitFor(t, "injection", func() bool { return len(o.list()) == 1 })
	never(t, "a model turn", func() bool { return r.llm.count() != 0 || r.trig.TriggeredCount.Load() != 0 })
}

// Items are injected in arrival order; one turn covers a batch that holds any turn-starting item.
func TestAsyncTriggerMixedBatchInjectsInOrderAndStartsOneTurn(t *testing.T) {
	r := newTriggerRig(t, nil)
	o := &orderLog{}
	if err := r.sess.Post(&core.UserStartedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // let the run loop observe the speech
	r.deliverItem(o, "a", true)
	r.deliverItem(o, "b", false)
	r.deliverItem(o, "c", true)
	never(t, "injection while the user speaks", func() bool { return len(o.list()) != 0 })
	if err := r.sess.Post(&core.UserStoppedSpeakingFrame{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the batch", func() bool { return len(o.list()) == 3 && r.llm.count() == 1 })
	never(t, "a second turn", func() bool { return r.llm.count() != 1 })
	if got := o.list(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("injection order = %v", got)
	}
}

// A failed injection (history rejected the note) is counted, and a batch with no successful
// turn-starting item starts no turn.
func TestAsyncTriggerFailedTurnItemStartsNoTurn(t *testing.T) {
	r := newTriggerRig(t, nil)
	r.trig.Deliver(core.AsyncToolResult{Call: core.ToolCall{Name: "run_llm"}, Inject: func() error { return core.ErrContextLimitExceeded }})
	waitFor(t, "drop", func() bool { return r.trig.DroppedCount.Load() == 1 })
	never(t, "a model turn", func() bool { return r.llm.count() != 0 })
}

func TestAsyncTriggerReadyOnlyOnceAttached(t *testing.T) {
	if core.NewAsyncTrigger(core.AsyncTriggerConfig{}).Ready() {
		t.Fatal("a trigger that was never attached reports ready")
	}
	r := newTriggerRig(t, nil) // starts its session, which attaches the trigger
	if !r.trig.Ready() {
		t.Fatal("an attached trigger reports not ready")
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd core && CGO_LDFLAGS=-Wl,-w go test -run 'TestAsyncTrigger(NoTurn|MixedBatch|FailedTurn|ReadyOnly)' -count=1 .`
Expected: build FAIL (`unknown field NoTurn`, `Ready undefined`).

- [ ] **Step 3: Implement.** In `core/tool_contracts.go`, inside `AsyncToolResult` add after `Inject`:

```go
	// NoTurn: inject the message when the call is idle but do not, on its own, start a model
	// turn (an operator note without run). A batch starts one turn if any item lacks it.
	NoTurn bool
```

In `core/async_trigger.go` add the field `ready atomic.Bool` to `AsyncTrigger`, set it at the end of `Attach` (`t.ready.Store(true)` before `return nil`), add

```go
// Ready reports that the trigger is attached to a started session, so Deliver can run.
func (t *AsyncTrigger) Ready() bool { return t.ready.Load() }
```

and replace the counting part of `fire` (from `var names string` to the `if injected == 0 { return }` check) with:

```go
	var names string
	turns := 0
	for _, r := range batch {
		if r.Inject() != nil {
			t.dropped()
			continue
		}
		if r.NoTurn {
			continue
		}
		if turns++; turns <= 3 {
			names += " " + r.Call.Name
		}
	}
	if turns == 0 {
		return
	}
```

- [ ] **Step 4: Run the new tests and the whole trigger family**

Run: `cd core && CGO_LDFLAGS=-Wl,-w go test -race -run 'TestAsyncTrigger' -count=3 .`
Expected: PASS.

- [ ] **Step 5: Break it** (must fail, then restore): change `if turns == 0 {` to `if false {`; `TestAsyncTriggerNoTurnItemInjectsWithoutATurn` and `...FailedTurnItemStartsNoTurn` must fail. Restore.

- [ ] **Step 6: Run the core module**

Run: `cd core && CGO_LDFLAGS=-Wl,-w go vet ./... && CGO_LDFLAGS=-Wl,-w go test ./... -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add core/tool_contracts.go core/async_trigger.go core/async_trigger_noturn_test.go
git commit -m "feat(core): trigger items can inject without starting a turn; trigger reports readiness"
```

---

### Task 2: Server: injector, live-call registry, request validation

**Files:**
- Create: `server/live_calls.go`
- Test: `server/live_calls_test.go` (create)

**Interfaces:**
- Consumes: `core.AsyncTrigger` (`Deliver`, `Ready`), `core.AsyncToolResult.NoTurn` (Task 1), `core.ConversationContext.AddMessage`.
- Produces: `ContextInjector` interface (`Note(content string, run bool)`, `Run()`, `Ready() bool`); `newCallInjector(*core.AsyncTrigger, *core.ConversationContext) ContextInjector`; `liveCalls` with `newLiveCalls()`, `Put(id, inj)`, `Delete(id)`, `Take(id) (ContextInjector, liveStatus)`; statuses `liveOK`, `liveMissing`, `liveLimited`; `contextRequest` with `validate() error`; constants `maxNotesPerRequest = 8`, `maxNoteBytes = 4096`, `notePrefix`.

- [ ] **Step 1: Write the failing tests** (create `server/live_calls_test.go`)

```go
package server

import (
	"strings"
	"testing"
	"time"
)

type nopInjector struct{}

func (nopInjector) Note(string, bool) {}
func (nopInjector) Run()              {}
func (nopInjector) Ready() bool       { return true }

func TestLiveCallsPutTakeDelete(t *testing.T) {
	l := newLiveCalls()
	if _, st := l.Take("s1"); st != liveMissing {
		t.Fatalf("unknown id status = %v, want missing", st)
	}
	l.Put("s1", nopInjector{})
	if inj, st := l.Take("s1"); st != liveOK || inj == nil {
		t.Fatalf("live id status = %v", st)
	}
	l.Delete("s1")
	if _, st := l.Take("s1"); st != liveMissing {
		t.Fatalf("deleted id status = %v, want missing", st)
	}
}

// Burst of 4, then one request per second per session; sessions do not share a bucket.
func TestLiveCallsRateLimitIsPerSession(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLiveCalls()
	l.now = func() time.Time { return now }
	l.Put("a", nopInjector{})
	l.Put("b", nopInjector{})
	for i := 0; i < 4; i++ {
		if _, st := l.Take("a"); st != liveOK {
			t.Fatalf("request %d status = %v, want ok within the burst", i, st)
		}
	}
	if _, st := l.Take("a"); st != liveLimited {
		t.Fatalf("5th request status = %v, want limited", st)
	}
	if _, st := l.Take("b"); st != liveOK {
		t.Fatalf("another session status = %v, want ok", st)
	}
	now = now.Add(1100 * time.Millisecond)
	if _, st := l.Take("a"); st != liveOK {
		t.Fatalf("after 1.1 s status = %v, want ok again", st)
	}
	if _, st := l.Take("a"); st != liveLimited {
		t.Fatalf("only one token refilled, status = %v, want limited", st)
	}
}

func TestContextRequestValidate(t *testing.T) {
	note := func(c string) contextNote { return contextNote{Content: c} }
	many := make([]contextNote, maxNotesPerRequest+1)
	for i := range many {
		many[i] = note("x")
	}
	cases := []struct {
		name string
		req  contextRequest
		ok   bool
	}{
		{"one note", contextRequest{Messages: []contextNote{note("hello")}}, true},
		{"eight notes", contextRequest{Messages: many[:maxNotesPerRequest]}, true},
		{"max length", contextRequest{Messages: []contextNote{note(strings.Repeat("a", maxNoteBytes))}}, true},
		{"no notes", contextRequest{}, false},
		{"nine notes", contextRequest{Messages: many}, false},
		{"empty", contextRequest{Messages: []contextNote{note("")}}, false},
		{"whitespace only", contextRequest{Messages: []contextNote{note(" \n\t ")}}, false},
		{"too long", contextRequest{Messages: []contextNote{note(strings.Repeat("a", maxNoteBytes+1))}}, false},
		{"NUL", contextRequest{Messages: []contextNote{note("a\x00b")}}, false},
	}
	for _, c := range cases {
		if err := c.req.validate(); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%t", c.name, err, c.ok)
		}
	}
	trimmed := contextRequest{Messages: []contextNote{note("  hi  ")}}
	if err := trimmed.validate(); err != nil || trimmed.Messages[0].Content != "hi" {
		t.Errorf("trim: err=%v content=%q, want hi", err, trimmed.Messages[0].Content)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test -run 'TestLiveCalls|TestContextRequest' -count=1 .`
Expected: build FAIL (`undefined: newLiveCalls`).

- [ ] **Step 3: Implement** (create `server/live_calls.go`)

```go
package server

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"govoice/core"
)

const (
	maxNotesPerRequest = 8
	maxNoteBytes       = 4096
	// notePrefix marks operator text in the history. It is added here, never taken from the
	// request, so an operator cannot pass a note off as the caller's words.
	notePrefix = "[note from the operator] "

	noteBurst         = 4
	noteRefillPerSec  = 1.0
	contextNoteName   = "context_note"
	runLLMName        = "run_llm"
)

// ContextInjector steers one live call from outside its pipeline. Both methods return at once;
// the work happens when the call is idle.
type ContextInjector interface {
	// Note adds an operator note to the history; run also starts a model turn.
	Note(content string, run bool)
	// Run starts a model turn from the current history.
	Run()
	// Ready reports that the call's pipeline is running and can take work.
	Ready() bool
}

type callInjector struct {
	trigger *core.AsyncTrigger
	history *core.ConversationContext
}

func newCallInjector(trigger *core.AsyncTrigger, history *core.ConversationContext) ContextInjector {
	return &callInjector{trigger: trigger, history: history}
}

func (c *callInjector) Ready() bool { return c.trigger.Ready() }

func (c *callInjector) Note(content string, run bool) {
	msg := core.Message{Role: core.RoleUser, Content: notePrefix + content}
	c.trigger.Deliver(core.AsyncToolResult{
		Call:   core.ToolCall{Name: contextNoteName},
		NoTurn: !run,
		Inject: func() error { return c.history.AddMessage(msg) },
	})
}

func (c *callInjector) Run() {
	c.trigger.Deliver(core.AsyncToolResult{Call: core.ToolCall{Name: runLLMName}, Inject: func() error { return nil }})
}

type liveStatus int

const (
	liveOK liveStatus = iota
	liveMissing
	liveLimited
)

type liveCall struct {
	inj    ContextInjector
	tokens float64
	last   time.Time
}

// liveCalls maps a stored session id to the injector of the call running on this instance.
type liveCalls struct {
	mu  sync.Mutex
	m   map[string]*liveCall
	now func() time.Time
}

func newLiveCalls() *liveCalls { return &liveCalls{m: map[string]*liveCall{}, now: time.Now} }

func (l *liveCalls) Put(id string, inj ContextInjector) {
	l.mu.Lock()
	l.m[id] = &liveCall{inj: inj, tokens: noteBurst, last: l.now()}
	l.mu.Unlock()
}

func (l *liveCalls) Delete(id string) {
	l.mu.Lock()
	delete(l.m, id)
	l.mu.Unlock()
}

// Take returns the call's injector and spends one rate-limit token.
func (l *liveCalls) Take(id string) (ContextInjector, liveStatus) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.m[id]
	if !ok {
		return nil, liveMissing
	}
	now := l.now()
	c.tokens = min(noteBurst, c.tokens+now.Sub(c.last).Seconds()*noteRefillPerSec)
	c.last = now
	if c.tokens < 1 {
		return nil, liveLimited
	}
	c.tokens--
	return c.inj, liveOK
}

type contextNote struct {
	Content string `json:"content"`
}

type contextRequest struct {
	Messages []contextNote `json:"messages"`
	Run      bool          `json:"run"`
}

// validate checks the request and trims each note in place.
func (r *contextRequest) validate() error {
	if len(r.Messages) == 0 || len(r.Messages) > maxNotesPerRequest {
		return fmt.Errorf("messages must hold 1 to %d notes", maxNotesPerRequest)
	}
	for i := range r.Messages {
		c := strings.TrimSpace(r.Messages[i].Content)
		switch {
		case c == "" || len(c) > maxNoteBytes:
			return fmt.Errorf("each note must be 1 to %d bytes", maxNoteBytes)
		case strings.ContainsRune(c, 0):
			return errors.New("notes must not contain NUL")
		}
		r.Messages[i].Content = c
	}
	return nil
}
```

Run `gofmt -w server/live_calls.go` (the const block alignment).

- [ ] **Step 4: Run the tests**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test -race -run 'TestLiveCalls|TestContextRequest' -count=3 .`
Expected: PASS.

- [ ] **Step 5: Break it** (must fail, then restore): change `c.tokens--` to nothing; `TestLiveCallsRateLimitIsPerSession` must fail. Restore.

- [ ] **Step 6: Commit**

```bash
git add server/live_calls.go server/live_calls_test.go
git commit -m "feat(server): live-call registry, context injector and request validation"
```

---

### Task 3: Server: always-on trigger, injector hook, live registration

**Files:**
- Modify: `server/registry.go` (CallSpec field; trigger built for every call; hand the injector out)
- Modify: `server/server.go` (Server field; capture and register the injector; unregister on release)
- Modify: `server/metrics.go` (help text only)
- Test: `server/context_injection_test.go` (create)

**Interfaces:**
- Consumes: Task 2 (`ContextInjector`, `newCallInjector`, `liveCalls`).
- Produces: `CallSpec.OnInjector func(ContextInjector)`; `Server.live *liveCalls`; every started call registered under `call.StoredSessionID`.

- [ ] **Step 1: Write the failing test** (create `server/context_injection_test.go`). It builds a real call pipeline with fake stages (as `async_tools_test.go` does), captures the injector through `OnInjector`, and checks the model's view of the history:

```go
package server

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/core"
	"govoice/vad"
)

// noteFakeLLM answers every turn with the operator notes it sees in the history, so a test can
// tell what the model would read.
type noteFakeLLM struct {
	history *core.ConversationContext
	session core.SessionController
	index   int
	turns   atomic.Int32
}

func (p *noteFakeLLM) Attach(s core.SessionController, i int) error {
	p.session, p.index = s, i
	return nil
}

func (p *noteFakeLLM) ProcessFrame(_ context.Context, f core.Frame, dir core.Direction, e core.Emitter) error {
	um, ok := f.(*core.UserMessageFrame)
	if !ok {
		return e.PushDirection(f, dir)
	}
	p.turns.Add(1)
	if err := e.PushDirection(f, dir); err != nil {
		return err
	}
	gen := p.session.CurrentGeneration()
	var seen []string
	for _, m := range p.history.Messages() {
		if strings.HasPrefix(m.Content, notePrefix) {
			seen = append(seen, m.Content)
		}
	}
	text := "seen: " + strings.Join(seen, " | ")
	_ = p.session.Reenter(&core.LLMTextDeltaFrame{Delta: text, Turn: um.Turn, Gen: gen}, core.Downstream, p.index+1, gen)
	return p.session.Reenter(&core.LLMTextFrame{Text: text, Turn: um.Turn, Gen: gen}, core.Downstream, p.index+1, gen)
}

type noteRig struct {
	sess   *core.Session
	llm    *noteFakeLLM
	inj    ContextInjector
	spoken func() string
}

func newNoteRig(t *testing.T) *noteRig {
	t.Helper()
	r := &noteRig{}
	reg := NewRegistry()
	reg.Register("google.stt", func(*Config, StageSpec) (core.Processor, error) { return passSTTStage{}, nil })
	reg.Register("google.tts", func(*Config, StageSpec) (core.Processor, error) {
		return core.NewFakeTTSProcessor(core.FakeTTSConfig{FramesPerUnit: 2}), nil
	})
	reg.Register("google.llm", func(_ *Config, s StageSpec) (core.Processor, error) {
		r.llm = &noteFakeLLM{history: s.History}
		return r.llm, nil
	})
	// No tools at all: the trigger and the injector exist anyway.
	call := CallSpec{STT: StageSpec{Provider: "google"}, TTS: StageSpec{Provider: "google"}, LLM: StageSpec{Provider: "google"},
		MaxMessages: 20, MaxTokens: 4000, OnInjector: func(i ContextInjector) { r.inj = i }}
	vadProc := vad.NewProcessor(&mockVADExecutor{ready: true, available: 1, total: 1}, vad.DefaultProcessorConfig())
	pipe, err := reg.BuildCallPipeline(DefaultConfig(ProfileDevelopment), call, vadProc, core.NewFakeTransportOutput(core.FakeTransportConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	r.sess, err = core.NewSession(9, pipe, core.DefaultResourceBudget())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var spoken strings.Builder
	r.sess.OnFrame = func(f core.Frame, _ core.Direction) {
		if su, ok := f.(*core.SynthesisUnitFrame); ok {
			mu.Lock()
			spoken.WriteString(su.Text + "\n")
			mu.Unlock()
		}
	}
	r.spoken = func() string { mu.Lock(); defer mu.Unlock(); return spoken.String() }
	if err := r.sess.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.sess.Close()
		if n := r.sess.LeakedTasks(); n != 0 {
			t.Errorf("leaked %d tasks", n)
		}
	})
	return r
}

func (r *noteRig) waitSpoken(t *testing.T, want string) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !strings.Contains(r.spoken(), want); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("spoken %q, want it to contain %q", r.spoken(), want)
		}
	}
}

// A call with no async tools still gets an injector; a note with run reaches the model's history
// and starts one reply.
func TestContextInjectionNoteWithRunStartsAReply(t *testing.T) {
	r := newNoteRig(t)
	if r.inj == nil || !r.inj.Ready() {
		t.Fatalf("injector = %v (ready=%t), want a ready injector on a call without async tools", r.inj, r.inj != nil && r.inj.Ready())
	}
	r.inj.Note("the caller is on the billing page", true)
	r.waitSpoken(t, notePrefix+"the caller is on the billing page")
	time.Sleep(700 * time.Millisecond) // past the settle window
	if n := r.llm.turns.Load(); n != 1 {
		t.Fatalf("model turns = %d, want exactly 1", n)
	}
}

// A note without run is in the history for the next turn but starts no reply by itself.
func TestContextInjectionNoteWithoutRunWaitsForTheNextTurn(t *testing.T) {
	r := newNoteRig(t)
	r.inj.Note("offer the refund", false)
	time.Sleep(700 * time.Millisecond)
	if n := r.llm.turns.Load(); n != 0 {
		t.Fatalf("model turns = %d, want none without run", n)
	}
	gen := r.sess.CurrentGeneration()
	if err := r.sess.Reenter(&core.FinalTranscriptionFrame{Text: "hello", Turn: "t1", Gen: gen, Committed: true, Timestamp: time.Now()}, core.Downstream, CallUserContextIndex, gen); err != nil {
		t.Fatal(err)
	}
	r.waitSpoken(t, notePrefix+"offer the refund")
}

// Notes wait while the caller speaks.
func TestContextInjectionWaitsForTheCaller(t *testing.T) {
	r := newNoteRig(t)
	if err := r.sess.Reenter(&core.UserStartedSpeakingFrame{}, core.Downstream, CallUserContextIndex, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	r.inj.Note("be brief", true)
	time.Sleep(700 * time.Millisecond)
	if r.llm.turns.Load() != 0 || strings.Contains(r.spoken(), "be brief") {
		t.Fatalf("the agent spoke over the caller: turns=%d spoken=%q", r.llm.turns.Load(), r.spoken())
	}
	if err := r.sess.Reenter(&core.UserStoppedSpeakingFrame{}, core.Downstream, CallUserContextIndex, 0); err != nil {
		t.Fatal(err)
	}
	r.waitSpoken(t, notePrefix+"be brief")
}

// The call ends with a note queued: nothing is written after close and nothing panics.
func TestContextInjectionAfterCloseIsHarmless(t *testing.T) {
	r := newNoteRig(t)
	if err := r.sess.Close(); err != nil {
		t.Fatal(err)
	}
	r.inj.Note("too late", true)
	r.inj.Run()
	time.Sleep(100 * time.Millisecond)
	if r.llm.turns.Load() != 0 {
		t.Fatalf("model turns = %d after close", r.llm.turns.Load())
	}
}
```

If `CallUserContextIndex` or `passSTTStage`/`mockVADExecutor` do not resolve (they are defined in the existing `server` tests, same package), use the same names `async_tools_test.go` uses.

- [ ] **Step 2: Run it and confirm it fails**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test -run TestContextInjection -count=1 .`
Expected: build FAIL (`unknown field OnInjector`).

- [ ] **Step 3: Implement `registry.go`.**
  1. In `CallSpec` after `OnHandoff` add:
```go
	// OnInjector, if set, receives the call's context injector once the pipeline is built.
	OnInjector func(ContextInjector)
```
  2. Replace the trigger construction block (the comment starting `// Async tools: finished results go through a trigger stage`, `var asyncTrigger ...`, `var asstActive ...`, and the `if call.Graph != nil || slices.ContainsFunc(...) { ... }`) with:
```go
	// The trigger is on every call: finished async tools, handoff flips and operator notes
	// all wait here for the call to be idle, then inject and start one model turn.
	var asstActive func() bool
	asyncTrigger := core.NewAsyncTrigger(core.AsyncTriggerConfig{Busy: func() bool { return asstActive != nil && asstActive() },
		OnTriggered: r.onAsyncTurn, OnDropped: r.onAsyncDrop})
	if call.OnInjector != nil {
		call.OnInjector(newCallInjector(asyncTrigger, ctxPool))
	}
```
  3. Remove the two `if asyncTrigger != nil { ... }` guards (in `prepLLM` and where the trigger is inserted into `procs`), keeping their bodies unguarded.
  4. Run `go vet` and fix an unused import (`slices` is still used elsewhere; if not, drop it).

- [ ] **Step 4: Implement `server.go`.**
  1. Add the field `live *liveCalls` to `Server` (next to `activeCalls`) and `live: newLiveCalls(),` in the `NewServer` struct literal (next to `timelines: timelines,`).
  2. After `call.PushToTalk = conn.PushToTalk()` add:
```go
	var injector ContextInjector
	call.OnInjector = func(i ContextInjector) { injector = i }
```
  3. After `s.activeCalls[sessionID] = active` (inside the same `s.mu` block's `Unlock()` — add after the unlock), add:
```go
	if injector != nil && call.StoredSessionID != "" {
		id, base := call.StoredSessionID, releaseCall
		s.live.Put(id, injector)
		releaseCall = func() { s.live.Delete(id); base() }
	}
```
  This must come before the `go func() { <-session.Done(); s.finishCall(... releaseCall ...) }()` line so the wrapped release is the one used.

- [ ] **Step 5: Metrics help text.** In `server/metrics.go` change the help of `voice_async_tool_turns_total` to `Model turns started to deliver finished async tool results or operator context notes.` (keep the metric name). If `TestDocsAndAlertRulesNameRealMetrics` or a docs test quotes the old help text, update it.

- [ ] **Step 6: Run the new tests, then the server module in both builds**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test -race -run TestContextInjection -count=3 . && CGO_LDFLAGS=-Wl,-w go vet ./... && CGO_LDFLAGS=-Wl,-w go test ./... -count=1 && CGO_LDFLAGS=-Wl,-w go test -tags xaispeech ./... -count=1`
Expected: PASS. An existing test that counts pipeline processors or indices may fail because the trigger is now always present: update only such counts, and say so in the commit message.

- [ ] **Step 7: Break it** (must fail, then restore): make `Note` ignore `run` (always `NoTurn: true`); `TestContextInjectionNoteWithRunStartsAReply` must fail. Restore.

- [ ] **Step 8: Commit**

```bash
git add server/registry.go server/server.go server/metrics.go server/context_injection_test.go
git commit -m "feat(server): always-on trigger and per-call context injector, registered while the call is live"
```

---

### Task 4: Management API routes

**Files:**
- Modify: `server/api_routes.go` (route the two paths before the generic sessions branch)
- Create: `server/live_calls_api.go` (handler)
- Test: `server/live_calls_api_test.go` (create; uses `apiTestServer`, `apiRequest` from `api_boundary_test.go`)

**Interfaces:**
- Consumes: Tasks 2 and 3. Produces: `POST /api/v1/sessions/{id}/context`, `POST /api/v1/sessions/{id}/run`.

- [ ] **Step 1: Write the failing tests** (create `server/live_calls_api_test.go`)

```go
package server

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

const testMgmtKey = "management-test-key-32-bytes-long-and-separate"

type recInjector struct {
	mu    sync.Mutex
	notes []string
	runs  []bool
	ran   int
	ready bool
}

func (r *recInjector) Note(c string, run bool) {
	r.mu.Lock()
	r.notes, r.runs = append(r.notes, c), append(r.runs, run)
	r.mu.Unlock()
}
func (r *recInjector) Run()        { r.mu.Lock(); r.ran++; r.mu.Unlock() }
func (r *recInjector) Ready() bool { return r.ready }

func TestLiveCallRoutesNeedTheManagementKey(t *testing.T) {
	s := apiTestServer(t)
	s.live.Put("sess1", &recInjector{ready: true})
	body := `{"messages":[{"content":"hi"}]}`
	for _, key := range []string{"", "wrong-key", s.cfg.Token.Secret} { // none, bad, the voice secret
		if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/context", key, body); w.Code != http.StatusUnauthorized {
			t.Errorf("key %q: status = %d, want 401", key, w.Code)
		}
		if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/run", key, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("run, key %q: status = %d, want 401", key, w.Code)
		}
	}
}

func TestLiveCallContextAcceptsNotes(t *testing.T) {
	s := apiTestServer(t)
	inj := &recInjector{ready: true}
	s.live.Put("sess1", inj)
	w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/context", testMgmtKey, `{"messages":[{"content":" a "},{"content":"b"}],"run":true}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"accepted":2`) {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if len(inj.notes) != 2 || inj.notes[0] != "a" || inj.notes[1] != "b" || !inj.runs[0] || !inj.runs[1] {
		t.Fatalf("notes = %v runs = %v, want trimmed a, b both with run", inj.notes, inj.runs)
	}
}

func TestLiveCallRunTriggersAReply(t *testing.T) {
	s := apiTestServer(t)
	inj := &recInjector{ready: true}
	s.live.Put("sess1", inj)
	if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/run", testMgmtKey, ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}
	if inj.ran != 1 {
		t.Fatalf("Run calls = %d, want 1", inj.ran)
	}
}

func TestLiveCallRoutesRejectBadRequests(t *testing.T) {
	s := apiTestServer(t)
	s.live.Put("sess1", &recInjector{ready: true})
	long := strings.Repeat("a", maxNoteBytes+1)
	for name, body := range map[string]string{
		"no messages":   `{"messages":[]}`,
		"empty note":    `{"messages":[{"content":"  "}]}`,
		"too long":      `{"messages":[{"content":"` + long + `"}]}`,
		"unknown field": `{"messages":[{"content":"a"}],"role":"system"}`,
		"role in note":  `{"messages":[{"content":"a","role":"system"}]}`,
		"not json":      `nope`,
	} {
		w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/context", testMgmtKey, body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_request") {
			t.Errorf("%s: status = %d body = %s, want 400 invalid_request", name, w.Code, w.Body.String())
		}
	}
}

func TestLiveCallRoutesStatuses(t *testing.T) {
	s := apiTestServer(t)
	body := `{"messages":[{"content":"hi"}]}`
	if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/nope/context", testMgmtKey, body); w.Code != http.StatusNotFound {
		t.Errorf("unknown session: status = %d, want 404", w.Code)
	}
	s.live.Put("starting", &recInjector{ready: false})
	if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/starting/context", testMgmtKey, body); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "call_not_ready") {
		t.Errorf("starting session: status = %d body = %s, want 409 call_not_ready", w.Code, w.Body.String())
	}
	s.live.Put("sess1", &recInjector{ready: true})
	if w := apiRequest(s, http.MethodGet, "/api/v1/sessions/sess1/context", testMgmtKey, ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", w.Code)
	}
	s.live.Delete("sess1")
	if w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/run", testMgmtKey, ""); w.Code != http.StatusNotFound {
		t.Errorf("ended session: status = %d, want 404", w.Code)
	}
}

func TestLiveCallRoutesAreRateLimited(t *testing.T) {
	s := apiTestServer(t)
	s.live.Put("sess1", &recInjector{ready: true})
	body := `{"messages":[{"content":"hi"}]}`
	var limited bool
	for i := 0; i < noteBurst+2; i++ {
		w := apiRequest(s, http.MethodPost, "/api/v1/sessions/sess1/context", testMgmtKey, body)
		if w.Code == http.StatusTooManyRequests {
			limited = strings.Contains(w.Body.String(), "rate_limited")
			break
		}
	}
	if !limited {
		t.Fatal("a burst past the bucket was never answered with 429 rate_limited")
	}
}

// The stored-session routes keep working: the new paths do not shadow them.
func TestLiveCallRoutesDoNotShadowSessionHistory(t *testing.T) {
	s := apiTestServer(t)
	if w := apiRequest(s, http.MethodGet, "/api/v1/sessions", testMgmtKey, ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("sessions list without storage: status = %d, want 503 as before", w.Code)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test -run TestLiveCall -count=1 .`
Expected: FAIL (the routes return 503 `session_management_unavailable`, not 202/404/...).

- [ ] **Step 3: Implement** (create `server/live_calls_api.go`)

```go
package server

import (
	"log/slog"
	"net/http"
	"strings"

	"govoice/server/api"
)

// liveCallRoute matches /api/v1/sessions/{id}/context and /api/v1/sessions/{id}/run.
func liveCallRoute(path string) (id, action string, ok bool) {
	rest, found := strings.CutPrefix(path, "/api/v1/sessions/")
	if !found {
		return "", "", false
	}
	id, action, found = strings.Cut(rest, "/")
	if !found || id == "" || (action != "context" && action != "run") {
		return "", "", false
	}
	return id, action, true
}

// handleLiveCall serves operator control of a call running on this instance. The caller has
// already been authenticated as the management client.
func (s *Server) handleLiveCall(w http.ResponseWriter, r *http.Request, requestID, id, action string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		api.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST", requestID)
		return
	}
	var req contextRequest
	if action == "context" {
		if err := api.DecodeJSON(r, &req); err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
			return
		}
		if err := req.validate(); err != nil {
			api.WriteError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID)
			return
		}
	}
	inj, status := s.live.Take(id)
	switch status {
	case liveMissing:
		api.WriteError(w, http.StatusNotFound, "not_found", "no live call with this id on this instance", requestID)
		return
	case liveLimited:
		w.Header().Set("Retry-After", "1")
		api.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests for this call", requestID)
		return
	}
	if !inj.Ready() {
		api.WriteError(w, http.StatusConflict, "call_not_ready", "the call is still starting; retry shortly", requestID)
		return
	}
	accepted := 0
	if action == "context" {
		for _, n := range req.Messages {
			inj.Note(n.Content, req.Run)
		}
		accepted = len(req.Messages)
	} else {
		inj.Run()
		accepted = 1
	}
	s.logger.Info("live call control", slog.String("session", id), slog.String("action", action), slog.Int("count", accepted), slog.Bool("run", req.Run || action == "run"))
	api.WriteJSON(w, http.StatusAccepted, api.SuccessResponse[map[string]int]{Data: map[string]int{"accepted": accepted}})
}
```

In `server/api_routes.go`, inside the `default:` branch, before the `if r.URL.Path == "/api/v1/sessions" || ...` block, add:

```go
		if id, action, ok := liveCallRoute(r.URL.Path); ok {
			s.handleLiveCall(w, r, requestID, id, action)
			return
		}
```

If `api.SuccessResponse` is not generic over a map type as written, use the same form `handleAPI` uses for `VoiceTokenResponse` (`api.SuccessResponse[T]{Data: ...}`) with a small named struct `struct{ Accepted int \`json:"accepted"\` }`.

- [ ] **Step 4: Run the tests**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test -race -run TestLiveCall -count=3 .`
Expected: PASS.

- [ ] **Step 5: Break it** (must fail, then restore): delete the `Ready()` check; `TestLiveCallRoutesStatuses` must fail. Restore.

- [ ] **Step 6: Run the server module in both builds**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go vet ./... && CGO_LDFLAGS=-Wl,-w go test ./... -count=1 && CGO_LDFLAGS=-Wl,-w go test -tags xaispeech ./... -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add server/api_routes.go server/live_calls_api.go server/live_calls_api_test.go
git commit -m "feat(server): POST /api/v1/sessions/{id}/context and /run for live calls"
```

---

### Task 5: Docs and matrix

**Files:**
- Modify: `docs/api/v1.md`, `docs/operations_runbook.md`, `docs/quickstart.md`, `docs/pipecat-parity-matrix.md`

- [ ] **Step 1: API doc.** In `docs/api/v1.md`, add two rows to the route table:

```
| POST | `/api/v1/sessions/{id}/context` | `{ "messages": [{ "content": "..." }], "run": false }` | 202 `data`: `accepted`; 400 `invalid_request`; 404 `not_found`; 409 `call_not_ready`; 429 `rate_limited` |
| POST | `/api/v1/sessions/{id}/run` | none | 202 `data`: `accepted`; 404 `not_found`; 409 `call_not_ready`; 429 `rate_limited` |
```

and below the table a short paragraph: `{id}` is the session id of a call running **on this instance** (the voice token's `jti`, also the stored session id); each note is 1 to 4096 bytes (trimmed, no NUL), at most 8 per request; per call the bucket is 4 requests then 1 per second; notes enter the history as a user message beginning `[note from the operator]` once the call is idle (caller and agent quiet), in order, and `run` (or the `/run` route) starts one reply for everything pending; a note never changes the system prompt and cannot name a role; a note for a call that ended or lives on another instance is 404.

- [ ] **Step 2: Runbook.** In `docs/operations_runbook.md` add a section "Live-call control" saying the same operational facts: management key only, per-instance (route by session affinity or try each instance), the rate limit, that operator-run replies count in `voice_async_tool_turns_total`, and that a compromised management key can steer calls (same trust as editing an agent). Do not name a metric that does not exist (the docs contract test checks metric names).

- [ ] **Step 3: Quickstart.** In `docs/quickstart.md` after the "Text turns" section add three lines with one `curl` example posting a note with `run: true` to `$ADMIN/api/v1/sessions/$SESSION_ID/context` using the management key.

- [ ] **Step 4: Matrix.** In `docs/pipecat-parity-matrix.md`, in "Remaining work" item 4, replace the first bullet with `- context update frames: **done 2026-10-11** for append and run, server side (management API `POST /api/v1/sessions/{id}/context` and `/run`, operator notes through the async trigger). Not built: replace-history, transform, mid-call tool changes, a client-side RTVI message, cross-instance routing.` and add a row or sentence in the table where Pipecat's context frames are compared, if one exists (search for `LLMMessagesAppendFrame`).

- [ ] **Step 5: Run the docs contract and matrix tests**

Run: `cd server && CGO_LDFLAGS=-Wl,-w go test -run 'TestDocs' -count=1 . && cd ../tools/compat && go test -run TestParityMatrix -count=1 .`
Expected: PASS (in a worktree without `pipecat-impl`, only "cited path does not exist" errors may appear; count the others, must be 0).

- [ ] **Step 6: Commit**

```bash
git add docs/api/v1.md docs/operations_runbook.md docs/quickstart.md docs/pipecat-parity-matrix.md
git commit -m "docs: live-call context injection"
```

---

## Final check

Run for every module with `CGO_LDFLAGS=-Wl,-w` and `GOVOICE_ORT_LIB=/Users/raymondottun/Projects/go-voice/third_party/onnxruntime-osx-arm64-1.29.0/lib/libonnxruntime.dylib`: `go vet ./... && go test ./... -count=1` in `core`, `server` (also `-tags xaispeech`), and the provider modules, then `go test -race -run 'TestAsyncTrigger|TestContextInjection|TestLiveCall' -count=3` in `core` and `server`.
