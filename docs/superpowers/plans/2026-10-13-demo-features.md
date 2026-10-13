# Dentistry Demo Product Features Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give go-voice what the "Smile Dental front desk" demo needs: a flow step that can end the call, a note that travels with a transfer, a dentistry MCP server, and a demo installer reachable from the admin UI.

**Architecture:** `end_call` is a per-step built-in tool backed by a new `IdleController.RequestEnd` in `core` (end after the bot finishes speaking, bounded wait). The transfer tools gain an optional `note` that the next step's greeting carries. The dentistry MCP is a self-contained in-memory server (`server/mcp/dentalmcp`, `cmd/dental-mcp`). A server-side installer (`server/demo`) creates the MCP entry and the three-step flow idempotently, exposed as `GET/POST /api/v1/demo...` plus an optional run at startup (`demo.auto_install`).

**Tech Stack:** Go (modules `core`, `server`), `github.com/modelcontextprotocol/go-sdk/mcp`, React + Vite + vitest (`ui/`), Playwright (`e2e/`).

**Spec:** `docs/superpowers/specs/2026-10-13-cloudrun-dentistry-demo-design.md` (sections A1-A4 and item 20). Plans 2 (one-port server) and 3 (deploy) follow separately.

## Global Constraints

- `core/` never imports `server/`; every `core` addition is consumed by the server (AGENTS.md YAGNI): `RequestEnd` is consumed by `end_call`.
- Bounded everything: note <= 500 bytes; dentistry MCP <= 200 patients and <= 400 appointments; every wait has a cap (`endCallWait` 15 s).
- Monotonic time only (`time.Since`, the controller's clock); no `time.Now().UnixNano()` comparisons.
- `go test -race` clean; `gofmt -w .`; commits are conventional, carry no model identifier in the subject, and end with the attribution trailer the session supplies.
- Wire contract unchanged: RTVI 2.1.0. The hang-up for `end_call` is `Connection.Hangup("ended_by_flow", 2s)` (non-fatal RTVI error carrying the code, then a 1000 close), the same path as idle hang-ups.
- Provider secrets are never stored: the MCP token reaches the voice server only as the env var `MCP_TOKEN_DENTAL` through `auth_env`.
- `end_call` and transfer notes apply to flows written with `steps` (two or more steps). A one-step flow is a plain flow and has no step tools.
- Test commands: `go test -race ./core/...`, `go test -race ./server/...`, `go test -race -tags "xaispeech fakeproviders" ./server/ -run <Name>` for the fake-provider end-to-end tests, `cd ui && npx vitest run <path>` and `npx tsc --noEmit`, `make e2e-fake`.

## Review Focus

- The caller interrupts the goodbye (barge-in) after `end_call`: the call must still end (bounded), not hang open. Pinned in Task 2/3.
- The model calls `end_call` while a transfer is pending, or twice: one end, no panic, no second hang-up. Pinned in Task 2/3.
- A transfer `note` that is huge, empty, not a string, or crafted to look like a server marker (`[The caller was...]`): bounded, ignored when invalid, neutralised in the greeting. Pinned in Task 4.
- The caller speaks before the new step is ready (the flip happens on their turn, not idle): no greeting is injected and a stale note must not leak into a later transfer. Pinned in Task 4.
- Dentistry MCP: a name typed with different case or spacing ("maria  LOPEZ"), a slot taken between `find_slots` and `book_appointment`, a request without or with the wrong bearer token. Pinned in Task 5.
- The demo installer run twice, or after only half of it succeeded (server created, flow missing): no duplicates, no error. Pinned in Task 6.

---

### Task 1: `end_call` flag on a flow step

**Files:**
- Modify: `server/flow/definition.go` (Step, rawStep, Decode)
- Test: `server/flow/definition_test.go`

**Interfaces:**
- Produces: `flow.Step.EndCall bool` (JSON key `end_call`), read by Tasks 3 and 7.

- [ ] **Step 1: Write the failing test** (append to `server/flow/definition_test.go`)

```go
func TestDecodeStepEndCall(t *testing.T) {
	step := `"config":{"llm":{"provider":"google","model":"m"},"tts":{"provider":"google"}}`
	front := `"config":{"stt":{"provider":"google"},"llm":{"provider":"google","model":"m"},"tts":{"provider":"google"}}`
	g, err := Decode(json.RawMessage(`{"start":"a","steps":[` +
		`{"id":"a","name":"A","end_call":true,` + front + `},{"id":"b","name":"B",` + step + `}],` +
		`"routes":[{"id":"r","from":"a","to":"b","condition":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := g.Step("a")
	b, _ := g.Step("b")
	if !a.EndCall || b.EndCall {
		t.Fatalf("EndCall a=%v b=%v, want true,false", a.EndCall, b.EndCall)
	}
}
```

- [ ] **Step 2: Run it to see it fail**

Run: `cd server && go test ./flow -run TestDecodeStepEndCall -v`
Expected: FAIL (`json: unknown field "end_call"` surfaced as an invalid-config error, or `a.EndCall` undefined).

- [ ] **Step 3: Implement** in `server/flow/definition.go`

Add to `Step` (after `Tools`): `EndCall bool `json:"end_call,omitempty"``.
Add to `rawStep`: `EndCall bool `json:"end_call"``.
In `Decode`, change the append to
`g.Steps = append(g.Steps, Step{ID: rn.ID, Name: name, Config: cfg, Tools: rn.Tools, EndCall: rn.EndCall, Position: rn.Position})`.
Add a doc comment on the field: `// EndCall gives the step the end_call tool: the model can hang up after saying goodbye.`

- [ ] **Step 4: Run it to see it pass**

Run: `cd server && go test ./flow -v 2>&1 | tail -5`
Expected: PASS (whole package).

- [ ] **Step 5: Commit**

```bash
git add server/flow/definition.go server/flow/definition_test.go
git commit -m "feat(flow): a step can be given the end_call tool"
```

---

### Task 2: `IdleController.RequestEnd` (core)

**Files:**
- Modify: `core/idle.go` (new method after `SetOnEnd`)
- Test: `core/idle_test.go`

**Interfaces:**
- Consumes: the existing `IdleController` fields (`ending`, `endReason`, `endAt`, `sawStart`, `botSpeaking`, `done`) and `poke()`.
- Produces: `func (c *IdleController) RequestEnd(reason string, wait time.Duration) bool`. It returns false when the call is already ending or ended. The call ends (via `OnEnd(reason)`) when the bot's current or next speech stops, or after `wait` (default `GoodbyeWait`, itself 10 s by default) if it never speaks. Used by Task 3.

- [ ] **Step 1: Write the failing tests** (append to `core/idle_test.go`; they use the existing rig)

```go
// RequestEnd with the controller otherwise idle (no user or bot timers): the call
// ends when the bot's next speech finishes, and not before.
func TestIdleRequestEndWaitsForTheSpeech(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0 })
	if !r.ctl.RequestEnd("ended_by_flow", 0) {
		t.Fatal("first RequestEnd must be accepted")
	}
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopDrained})
	r.endedWith("ended_by_flow")
}

// Speech already in progress when end is requested: its stop ends the call.
func TestIdleRequestEndDuringSpeech(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0 })
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStarted})
	r.ctl.RequestEnd("ended_by_flow", 0)
	r.noEnd()
	r.up(&core.SpeakingStateFrame{State: core.SpeakingStopped, Reason: core.SpeakingStopInterrupted})
	r.endedWith("ended_by_flow") // an interrupted goodbye still ends the call
}

// The bot never speaks: the wait is the cap.
func TestIdleRequestEndWaitIsBounded(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0 })
	r.ctl.RequestEnd("ended_by_flow", 4*time.Second)
	r.clk.parkedAt(t, r.at(4*time.Second))
	r.clk.Advance(4*time.Second - time.Nanosecond)
	r.noEnd()
	r.clk.Advance(time.Nanosecond)
	r.endedWith("ended_by_flow")
}

// A second request (a model calling end_call twice) neither re-arms nor ends twice.
func TestIdleRequestEndOnce(t *testing.T) {
	r := newIdleRig(t, func(c *core.IdleConfig) { c.UserTimeout, c.MaxPrompts = 0, 0 })
	if !r.ctl.RequestEnd("ended_by_flow", 0) || r.ctl.RequestEnd("ended_by_flow", 0) {
		t.Fatal("only the first RequestEnd is accepted")
	}
	r.botSpoke()
	r.endedWith("ended_by_flow")
	r.noEnd() // exactly one hang-up
	if r.ctl.RequestEnd("x", 0) {
		t.Fatal("no request after the call ended")
	}
}
```

Check `core.SpeakingStopInterrupted` exists: `grep -n "SpeakingStop" core/*.go`. If the constant is named differently, use the interrupted-reason constant that exists (the test only needs any reason other than Drained).

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./core -run TestIdleRequestEnd -v`
Expected: FAIL (`r.ctl.RequestEnd undefined`).

- [ ] **Step 3: Implement** in `core/idle.go`, after `SetOnEnd`:

```go
// RequestEnd ends the call once the bot has finished the speech now playing, or
// the next speech if it is silent right now, or after wait if it never speaks
// (wait <= 0 uses GoodbyeWait). Unlike an idle hang-up it needs no timeout
// configuration: a tool-driven goodbye. It reports false when the call is
// already ending or ended, so a repeated request is a no-op.
func (c *IdleController) RequestEnd(reason string, wait time.Duration) bool {
	if wait <= 0 {
		wait = c.cfg.GoodbyeWait
	}
	now := c.clk.Now()
	c.mu.Lock()
	if c.done || c.ending {
		c.mu.Unlock()
		return false
	}
	c.ending, c.endReason = true, reason
	c.userAt, c.botAt = time.Time{}, time.Time{}
	c.endAt, c.sawStart = now.Add(wait), c.botSpeaking
	c.mu.Unlock()
	c.poke()
	return true
}
```

- [ ] **Step 4: Run them to see them pass**

Run: `go test -race ./core -run 'TestIdle' -v 2>&1 | tail -15`
Expected: PASS, including the pre-existing idle tests.

- [ ] **Step 5: Commit**

```bash
git add core/idle.go core/idle_test.go
git commit -m "feat(core): IdleController.RequestEnd ends the call after the bot's speech"
```

---

### Task 3: the `end_call` tool in the server

**Files:**
- Modify: `server/flow_call.go` (tool definition, executor), `server/call.go` (stepToolset), `server/idle.go` (stage), `server/registry.go` (build idle for end-capable flows, wire `Flow.End`, `CallSpec.OnEndCall`), `server/server.go` (set `OnEndCall`), `server/registry_fake.go` (fake phrase), `server/route_obs.go` (span helper)
- Test: `server/flow_call_test.go`, `server/flow_e2e_test.go`

**Interfaces:**
- Consumes: `flow.Step.EndCall` (Task 1), `core.IdleController.RequestEnd` (Task 2).
- Produces:
  - `const EndReasonFlow = "ended_by_flow"`, `const endCallName = "end_call"`.
  - `stepExecutor(inner, allowed, transfer, commit func(target, note string) error, end func() error)` (two new signature changes; Task 4 changes `commit`).
  - `CallFlow.End func(stepID string) error`, `CallFlow.CanEnd bool`; `CallSpec.OnEndCall func(stepID string)`.
  - trace span `call.end` with attributes `reason=ended_by_flow`, `from_step=<step id>` (both already in the trace allowlist), read by Task 7.

- [ ] **Step 1: Write the failing unit tests** (in `server/flow_call_test.go`; update the four existing `stepExecutor(...)` calls to the new signature first, passing `nil` for the new `end` argument and `func(target, _ string) error` for commit)

```go
func TestStepExecutorEndCall(t *testing.T) {
	var ended int
	ex := stepExecutor(nil, nil, nil, nil, func() error { ended++; return nil })
	res, _ := ex.Execute(context.Background(), core.ToolCall{ID: "1", Name: "end_call"})
	if ended != 1 || res.Status != core.ToolSucceeded || !strings.Contains(string(res.Result), "goodbye") {
		t.Fatalf("ended=%d res=%+v", ended, res)
	}
	// A step without the tool refuses it, like any foreign tool.
	ex = stepExecutor(nil, nil, nil, nil, nil)
	res, _ = ex.Execute(context.Background(), core.ToolCall{ID: "2", Name: "end_call"})
	if res.Status != core.ToolFailed {
		t.Fatalf("end_call must be refused on a step without it, got %s", res.Status)
	}
}

func TestStepExecutorEndCallAlreadyEnding(t *testing.T) {
	ex := stepExecutor(nil, nil, nil, nil, func() error { return errCallEnding })
	res, _ := ex.Execute(context.Background(), core.ToolCall{ID: "1", Name: "end_call"})
	if res.Status != core.ToolSucceeded { // idempotent: a second end_call is not an error to the model
		t.Fatalf("a repeated end_call must not fail the model's turn: %+v", res)
	}
}

func TestEndCallDefinition(t *testing.T) {
	d := endCallDefinition()
	if d.Name != "end_call" || !strings.Contains(d.Description, "goodbye") || string(d.InputSchema) != `{"type":"object","properties":{},"additionalProperties":false}` {
		t.Fatalf("%+v", d)
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test -run 'TestStepExecutor|TestEndCall' -v 2>&1 | tail`
Expected: FAIL / build error (new signature, `errCallEnding`, `endCallDefinition` undefined).

- [ ] **Step 3: Implement the tool and executor** in `server/flow_call.go`

```go
const (
	// EndReasonFlow is the hang-up reason, trace attribute and RTVI error code when
	// a step's end_call tool ends the call.
	EndReasonFlow = "ended_by_flow"
	endCallName   = "end_call"
	// endCallWait caps the wait for the goodbye to finish before the call ends anyway.
	endCallWait = 15 * time.Second
	endCallOK   = `{"status":"ending","instruction":"The call ends right after your next words. Say a brief goodbye now if you have not already, then stop."}`
)

var errCallEnding = errors.New("call is already ending")

// endCallDefinition is the end_call tool a step gets when its end_call flag is set.
func endCallDefinition() core.ToolDefinition {
	return core.ToolDefinition{
		Name:        endCallName,
		Description: "Hang up. Call this only after you have said goodbye and the caller needs nothing else; the call ends once you finish speaking.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Policy:      core.ToolPolicy{Timeout: 5 * time.Second},
	}
}
```

Extend `CallFlow` with `CanEnd bool` ("some step has end_call; the call builds the idle controller") and
`End func(stepID string) error` ("set by the pipeline builder; end_call calls it").

Change `stepExecutor` to
`func stepExecutor(inner core.ToolExecutor, allowed map[string]bool, transfer map[string]string, commit func(target, note string) error, end func() error) core.ToolExecutor` and add, before the `unknown_tool` check:

```go
		if call.Name == endCallName && end != nil {
			if err := end(); err != nil && !errors.Is(err, errCallEnding) {
				return res(core.ToolFailed, `{"error":"end_unavailable"}`), nil
			}
			return res(core.ToolSucceeded, endCallOK), nil
		}
```

(Task 4 finishes the `commit` signature change; for this task keep the body's call `commit(target, "")`.)

- [ ] **Step 4: Wire it** 

`server/call.go`, `stepToolset`: after building `defs`, `if n.EndCall { defs = append(defs, endCallDefinition()) }`, and pass `end` to `stepExecutor`:

```go
	var end func() error
	if n.EndCall {
		end = func() error {
			if cg.End == nil {
				return errors.New("end not ready")
			}
			return cg.End(n.ID)
		}
	}
```

`server/call.go` where `graph = &CallFlow{...}`: set `CanEnd: anyEndCall(g)` with

```go
func anyEndCall(g flow.Definition) bool {
	for _, n := range g.Steps {
		if n.EndCall {
			return true
		}
	}
	return false
}
```

`server/idle.go`: change `stage(...)` to take `force bool` and return early only when `!c.enabled() && !force`. A forced controller with zero timeouts is inert until `RequestEnd` (the user and bot deadlines are only armed when their timeouts are set).

`server/registry.go`:
- line ~517: `cfg.Idle.enabled()` stays (a forced controller does not need tool-busy muting).
- `idleProc, err := cfg.Idle.stage(ctxPool, muteTools, turnTyped, asstCtxProc.Active, held, call.Flow != nil && call.Flow.CanEnd)`.
- after the idle processor is inserted, wire the hook:

```go
	if call.Flow != nil && idleProc != nil {
		call.Flow.End = func(stepID string) error {
			if !idleProc.RequestEnd(EndReasonFlow, endCallWait) {
				return errCallEnding
			}
			if call.OnEndCall != nil {
				call.OnEndCall(stepID)
			}
			return nil
		}
	}
```

- add `OnEndCall func(stepID string)` to `CallSpec` ("receives the step whose end_call ended the call (trace)").

`server/route_obs.go`: add

```go
// onEndCall records the call.end span for a call ended by a step's end_call tool.
func (s *Server) onEndCall(storedSessionID string) func(stepID string) {
	return func(stepID string) {
		if s.mgmt == nil || s.mgmt.traces == nil || storedSessionID == "" {
			return
		}
		ev := core.NewTraceEvent("call.end", time.Now(), core.SessionID(storedSessionID), "", 0, core.TracePolicy{},
			map[string]any{"reason": EndReasonFlow, "from_step": stepID})
		core.StartSpanSafe(context.Background(), s.mgmt.traces, ev).End(nil)
	}
}
```

`server/server.go`, next to `call.OnRoute = s.onRoute(...)`: `call.OnEndCall = s.onEndCall(call.StoredSessionID)`.

`server/registry_fake.go`, in the `google.llm` factory, next to the transfer loop:

```go
			for _, d := range spec.Tools {
				if d.Name == endCallName {
					cfg.ToolOnPhrase["end the call"] = endCallName
				}
			}
```

- [ ] **Step 5: Run the unit tests**

Run: `cd server && go test -race -run 'TestStepExecutor|TestEndCall|TestFlow' ./ 2>&1 | tail`
Expected: PASS. Also `go vet ./... && go build -tags xaispeech ./...` from `server/`.

- [ ] **Step 6: Write the failing end-to-end test** (append to `server/flow_e2e_test.go`)

```go
func endCallFlowJSON() json.RawMessage {
	stt := `"stt":{"provider":"google","model":"s"}`
	return json.RawMessage(`{"start":"front","steps":[` +
		`{"id":"front","name":"Front desk","end_call":true,"config":{` + stt + `,"llm":{"provider":"google","model":"m","system_prompt":"FRONT"},"tts":{"provider":"google","voice":"frontvoice"}}},` +
		`{"id":"sales","name":"Sales","config":{"llm":{"provider":"google","model":"m","system_prompt":"SALES"},"tts":{"provider":"google","voice":"salesvoice"}}}],` +
		`"routes":[{"id":"e1","from":"front","to":"sales","condition":"The caller wants to buy."}]}`)
}

func TestFlowE2EEndCallEndsTheCallAfterTheGoodbye(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	a, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Closer", Config: endCallFlowJSON()})
	if err != nil || a.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err=%v", a.Readiness, err)
	}
	r.flowID = a.ID
	c := r.dial(a.ID)
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })

	c.say("hello")
	c.waitReplies("[FRONT", 1)
	if r.activeSessions() != 1 {
		t.Fatal("the call must stay up until end_call")
	}
	c.say("please end the call")
	eventually(t, "the call ended by the flow", func() bool { return r.has(EventDisconnect, "terminated: "+EndReasonFlow) })
	eventually(t, "session closed", func() bool { return r.activeSessions() == 0 })
	eventually(t, "call.end span", func() bool { return r.spanNames(c.sessionID)["call.end"] })
}

// A step without the flag cannot end the call: the fake only offers the phrase to steps that have the tool.
func TestFlowE2EStepWithoutEndCallKeepsTheCall(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	c := r.dial(r.createRoutedFlow("salesvoice"))
	eventually(t, "session", func() bool { return r.activeSessions() == 1 })
	c.say("please end the call")
	c.waitReplies("[FRONT", 1)
	time.Sleep(300 * time.Millisecond)
	if r.activeSessions() != 1 || r.has(EventDisconnect, "terminated: "+EndReasonFlow) {
		t.Fatal("a step without end_call must not hang up")
	}
}
```

Run: `cd server && go test -race -tags "xaispeech fakeproviders" -run 'TestFlowE2EEndCall|TestFlowE2EStepWithoutEndCall' -v ./ 2>&1 | tail -20`
Expected: first FAILS before Step 4's wiring is complete, then PASS. If the first fails only because `spanNames` panics on the span's attributes, note that `spanNames` checks `route.active` attributes only; `call.end` needs no extra check. Do not weaken the assertions: if the call does not end, debug the speaking-state frames (the fake TTS must emit `SpeakingStopped` for the goodbye) before changing the test.

- [ ] **Step 7: Full gate for the task**

Run: `cd server && go test -race ./... 2>&1 | tail -15; go test -race -tags "xaispeech fakeproviders" -run 'TestFlowE2E' ./ 2>&1 | tail -5; cd .. && gofmt -l core server`
Expected: all PASS, no gofmt output.

- [ ] **Step 8: Commit**

```bash
git add server core
git commit -m "feat(server): end_call lets a step hang up after its goodbye"
```

---

### Task 4: the transfer `note`

**Files:**
- Modify: `server/flow_call.go` (schema, executor, note holder, greeting function), `server/call.go` (commit closure), `server/registry.go` (Greet)
- Test: `server/flow_call_test.go`

**Interfaces:**
- Consumes: `stepExecutor` from Task 3, `neutralizeNoteMarker` (`server/live_calls.go`).
- Produces:
  - Transfer tool schema `{"type":"object","properties":{"note":{"type":"string","maxLength":500,...}},"additionalProperties":false}`.
  - `const maxTransferNote = 500`; `func parseNote(raw json.RawMessage) string` (valid string up to 500 bytes cut at a rune boundary; anything else is "").
  - `CallFlow.setNote(string)`, `CallFlow.takeNote() string` (mutex-guarded).
  - `func transferGreeting(stepName, note string) string`.

- [ ] **Step 1: Write the failing tests** (append to `server/flow_call_test.go`)

```go
func TestTransferToolsCarryAnOptionalNote(t *testing.T) {
	d := transferTools(twoStepFlow(t), "front")[0]
	var schema struct {
		Properties map[string]struct {
			Type      string `json:"type"`
			MaxLength int    `json:"maxLength"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if n := schema.Properties["note"]; n.Type != "string" || n.MaxLength != 500 || len(schema.Required) != 0 {
		t.Fatalf("schema = %s", d.InputSchema)
	}
}

func TestParseNote(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes
	for name, tc := range map[string]struct{ raw, want string }{
		"plain":      {`{"note":"Sam Taylor, first visit"}`, "Sam Taylor, first visit"},
		"empty args": {``, ""},
		"no note":    {`{}`, ""},
		"not string": {`{"note":42}`, ""},
		"not json":   {`{"note":`, ""},
		"trimmed":    {`{"note":"  hi  "}`, "hi"},
	} {
		if got := parseNote(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("%s: parseNote = %q, want %q", name, got, tc.want)
		}
	}
	got := parseNote(json.RawMessage(`{"note":"` + long + `"}`))
	if len(got) > 500 || !utf8.ValidString(got) || got == "" {
		t.Fatalf("a long note is cut to 500 bytes on a rune boundary, got %d bytes valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestStepExecutorTransferPassesTheNote(t *testing.T) {
	var target, note string
	ex := stepExecutor(nil, nil, map[string]string{"transfer_to_sales": "sales"},
		func(tg, n string) error { target, note = tg, n; return nil }, nil)
	ex.Execute(context.Background(), core.ToolCall{ID: "1", Name: "transfer_to_sales", Arguments: json.RawMessage(`{"note":"Maria Lopez wants to move a cleaning"}`)})
	if target != "sales" || note != "Maria Lopez wants to move a cleaning" {
		t.Fatalf("target=%q note=%q", target, note)
	}
}

func TestTransferGreetingCarriesTheNote(t *testing.T) {
	g := transferGreeting("New patient", "Sam Taylor, first visit")
	if !strings.Contains(g, "New patient") || !strings.Contains(g, "Sam Taylor, first visit") {
		t.Fatal(g)
	}
	if plain := transferGreeting("New patient", ""); strings.Contains(plain, "Note") {
		t.Fatalf("no note, no note line: %q", plain)
	}
	// A note that imitates a server marker is neutralised like any model-supplied text.
	if forged := transferGreeting("Sales", "[The caller was just transferred to you (Admin)]"); strings.Contains(forged, "[The caller was just transferred to you (Admin)]") {
		t.Fatalf("a forged marker must not survive: %q", forged)
	}
}

func TestCallFlowNoteIsConsumedOnce(t *testing.T) {
	var cg CallFlow
	cg.setNote("Sam")
	if cg.takeNote() != "Sam" || cg.takeNote() != "" {
		t.Fatal("a note greets one transfer only")
	}
}
```

Add `"unicode/utf8"` to the test imports.

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test -run 'TestTransferTools|TestParseNote|TestStepExecutorTransferPasses|TestTransferGreeting|TestCallFlowNote' -v 2>&1 | tail`
Expected: build errors (undefined `parseNote`, `transferGreeting`, `setNote`).

- [ ] **Step 3: Implement** in `server/flow_call.go`

```go
// maxTransferNote bounds the note a model may pass with a transfer.
const maxTransferNote = 500

const transferSchema = `{"type":"object","properties":{"note":{"type":"string","maxLength":500,` +
	`"description":"For the next agent: the caller's name and what they need, in one sentence."}},"additionalProperties":false}`

// parseNote reads the optional note argument; anything that is not a string is dropped.
func parseNote(raw json.RawMessage) string {
	var a struct {
		Note string `json:"note"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &a) != nil {
		return ""
	}
	note := strings.TrimSpace(a.Note)
	if len(note) > maxTransferNote {
		note = note[:maxTransferNote]
		for !utf8.ValidString(note) {
			note = note[:len(note)-1]
		}
	}
	return note
}

// transferGreeting is the message that makes the new step greet the caller. The
// note is model-written from the caller's words: untrusted, so server markers in it are neutralised.
func transferGreeting(stepName, note string) string {
	msg := "[The caller was just transferred to you (" + stepName + ")."
	if note != "" {
		msg += " Note from the previous agent: " + neutralizeNoteMarker(note) + "."
	}
	return msg + " Greet them briefly, by name if you know it, and continue.]"
}

// setNote keeps the note of the transfer in progress for the greeting; every transfer overwrites it.
func (c *CallFlow) setNote(n string) { c.noteMu.Lock(); c.note = n; c.noteMu.Unlock() }

// takeNote returns the note once.
func (c *CallFlow) takeNote() string {
	c.noteMu.Lock()
	defer c.noteMu.Unlock()
	n := c.note
	c.note = ""
	return n
}
```

Add `noteMu sync.Mutex; note string` to `CallFlow` (import `sync`, `unicode/utf8`). In `transferTools` use `InputSchema: json.RawMessage(transferSchema)`. In `stepExecutor`'s transfer branch call `commit(target, parseNote(call.Arguments))`.

- [ ] **Step 4: Wire** 

`server/call.go` `stepToolset`, the commit closure becomes:

```go
	func(target, note string) error {
		if cg.Commit == nil {
			return errors.New("route not ready")
		}
		cg.setNote(note) // every transfer overwrites the previous note
		if err := cg.Commit(target); err != nil {
			cg.setNote("")
			return err
		}
		return nil
	}
```

`server/registry.go` Greet:

```go
			Greet: func(string) error {
				name := "another agent"
				if n := lastStep.Load(); n != nil {
					name = *n
				}
				return ctxPool.AddMessage(core.Message{Role: core.RoleUser, Content: transferGreeting(name, call.Flow.takeNote())})
			},
```

(`call.Flow` is non-nil inside this branch.)

- [ ] **Step 5: Run the gate**

Run: `cd server && go test -race ./... 2>&1 | tail -10; go test -race -tags "xaispeech fakeproviders" -run TestFlowE2E ./ 2>&1 | tail -5`
Expected: PASS. Existing e2e route tests still pass (the fake calls transfer with empty arguments, note "").

- [ ] **Step 6: Commit**

```bash
git add server
git commit -m "feat(server): transfers carry an optional note into the next step's greeting"
```

---

### Task 5: the dentistry MCP server

**Files:**
- Create: `server/mcp/dentalmcp/dentalmcp.go`, `server/mcp/dentalmcp/dentalmcp_test.go`, `cmd/dental-mcp/main.go`
- Modify: `Makefile` (target `dental-mcp`)

**Interfaces:**
- Produces: `dentalmcp.New(Options) (*Server, error)`; `(*Server).Handler() http.Handler` serving `GET /health` (open) and `/mcp` (bearer token); seven tools whose remote names are exactly `find_patient`, `register_patient`, `list_appointments`, `find_slots`, `book_appointment`, `reschedule_appointment`, `cancel_appointment` (Task 6 requires them). `dentalmcp.ToolNames []string` lists them. Binary `cmd/dental-mcp` reads `PORT` (default 8081) and `DENTAL_MCP_TOKEN` (required).

- [ ] **Step 1: Write the failing tests** `server/mcp/dentalmcp/dentalmcp_test.go`

```go
package dentalmcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"govoice/server/mcp"
	"govoice/server/mcp/dentalmcp"
)

// Monday 12 October 2026, 08:00 UTC.
var monday = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

func start(t *testing.T) (call func(tool, args string) (map[string]any, error), base string) {
	t.Helper()
	srv, err := dentalmcp.New(dentalmcp.Options{Token: "tok", Now: func() time.Time { return monday }})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c := mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
	return func(tool, args string) (map[string]any, error) {
		res, err := c.CallTool(context.Background(), ts.URL+"/mcp", "tok", tool, json.RawMessage(args))
		if err != nil {
			return nil, err
		}
		var out map[string]any
		return out, json.Unmarshal(res.JSON, &out)
	}, ts.URL
}

func TestSeededPatientIsFoundWhateverTheCaseAndSpacing(t *testing.T) {
	call, _ := start(t)
	out, err := call("find_patient", `{"name":"  maria   LOPEZ "}`)
	if err != nil || out["found"] != true {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if out, _ = call("find_patient", `{"name":"Nobody Here"}`); out["found"] != false {
		t.Fatalf("unknown patient: %v", out)
	}
}

func TestRegisterThenBookRescheduleCancel(t *testing.T) {
	call, _ := start(t)
	reg, err := call("register_patient", `{"name":"Sam Taylor","phone":"555 0100"}`)
	if err != nil {
		t.Fatal(err)
	}
	pid := reg["patient"].(map[string]any)["id"].(string)

	slots, _ := call("find_slots", `{}`)
	list := slots["slots"].([]any)
	if len(list) == 0 || len(list) > 6 {
		t.Fatalf("slots = %v", slots)
	}
	first := list[0].(map[string]any)["id"].(string)
	second := list[1].(map[string]any)["id"].(string)

	booked, err := call("book_appointment", `{"patient_id":"`+pid+`","slot_id":"`+first+`","kind":"checkup"}`)
	if err != nil {
		t.Fatal(err)
	}
	aid := booked["appointment"].(map[string]any)["id"].(string)

	// The slot is gone for the next caller.
	other, _ := call("register_patient", `{"name":"Lee Wong","phone":"555 0101"}`)
	opid := other["patient"].(map[string]any)["id"].(string)
	if _, err := call("book_appointment", `{"patient_id":"`+opid+`","slot_id":"`+first+`","kind":"checkup"}`); err == nil {
		t.Fatal("a taken slot must not be booked twice")
	}

	if _, err := call("reschedule_appointment", `{"appointment_id":"`+aid+`","slot_id":"`+second+`"}`); err != nil {
		t.Fatal(err)
	}
	// Moving frees the old slot.
	if _, err := call("book_appointment", `{"patient_id":"`+opid+`","slot_id":"`+first+`","kind":"checkup"}`); err != nil {
		t.Fatalf("the old slot is free after a reschedule: %v", err)
	}
	if _, err := call("cancel_appointment", `{"appointment_id":"`+aid+`"}`); err != nil {
		t.Fatal(err)
	}
	apps, _ := call("list_appointments", `{"patient_id":"`+pid+`"}`)
	if got := apps["appointments"]; got != nil && len(got.([]any)) != 0 {
		t.Fatalf("appointments after cancel = %v", got)
	}
}

func TestSlotsAreWeekdaysInsideTheHorizon(t *testing.T) {
	call, _ := start(t)
	out, _ := call("find_slots", `{"from_date":"2026-10-16"}`) // a Friday
	for _, s := range out["slots"].([]any) {
		id := s.(map[string]any)["id"].(string)
		d, err := time.Parse("2006-01-02T15:04", id)
		if err != nil || d.Weekday() == time.Saturday || d.Weekday() == time.Sunday || d.Before(monday) || d.After(monday.AddDate(0, 0, 14)) {
			t.Fatalf("slot %q is outside the clinic's week", id)
		}
	}
	if _, err := call("book_appointment", `{"patient_id":"p1","slot_id":"2026-10-17T09:00","kind":"checkup"}`); err == nil { // Saturday
		t.Fatal("a weekend slot must not be bookable")
	}
}

func TestTheRegistryIsBounded(t *testing.T) {
	call, _ := start(t)
	var err error
	for i := 0; i < dentalmcp.MaxPatients+1 && err == nil; i++ {
		_, err = call("register_patient", `{"name":"Patient `+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('a'+i/26%26))+`","phone":"5`+string(rune('0'+i%10))+`"}`)
	}
	if err == nil {
		t.Fatal("registering beyond MaxPatients must fail")
	}
}

func TestTokenIsRequiredOnMCPNotOnHealth(t *testing.T) {
	_, base := start(t)
	c := mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
	if _, err := c.ListTools(context.Background(), base+"/mcp", "wrong"); !errors.Is(err, mcp.ErrAuthenticationFailed) {
		t.Fatalf("wrong token: %v", err)
	}
	resp, err := http.Get(base + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %v %v", resp, err)
	}
	resp.Body.Close()
	if _, err := dentalmcp.New(dentalmcp.Options{}); err == nil {
		t.Fatal("a server without a token must not start")
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `cd server && go test ./mcp/dentalmcp 2>&1 | tail -5`
Expected: build error (package has no non-test files).

- [ ] **Step 3: Implement** `server/mcp/dentalmcp/dentalmcp.go`

```go
// Package dentalmcp is the scheduling backend of the Smile Dental demo: a small MCP
// server over streamable HTTP with fixed fake patients and in-memory appointments.
// It holds no real data and keeps nothing across restarts. Slots are every weekday
// at four fixed times for the next two weeks, so the demo always has something to offer.
package dentalmcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	MaxPatients     = 200
	MaxAppointments = 400
	horizonDays     = 14
	slotsPerReply   = 6
	maxField        = 80
	slotLayout      = "2006-01-02T15:04"
)

var (
	slotTimes = []string{"09:00", "11:00", "14:00", "16:00"}
	kinds     = map[string]bool{"checkup": true, "cleaning": true, "filling": true, "consultation": true}
	// ToolNames are the remote names of the server's tools.
	ToolNames = []string{"find_patient", "register_patient", "list_appointments", "find_slots", "book_appointment", "reschedule_appointment", "cancel_appointment"}

	ErrNoToken = errors.New("dentalmcp: a bearer token is required")
)

type Options struct {
	Token string           // bearer token /mcp expects (required)
	Now   func() time.Time // the clock (UTC); default time.Now
}

type Patient struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

type Appointment struct {
	ID        string `json:"id"`
	PatientID string `json:"patient_id"`
	Kind      string `json:"kind"`
	SlotID    string `json:"slot_id"`
	When      string `json:"when"`
}

type Slot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Server struct {
	token string
	now   func() time.Time
	srv   *mcp.Server

	mu       sync.Mutex
	patients []Patient
	appts    []Appointment
	nextP    int
	nextA    int
}

func New(o Options) (*Server, error) {
	if o.Token == "" {
		return nil, ErrNoToken
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Server{token: o.Token, now: func() time.Time { return o.Now().UTC() }, nextP: 1, nextA: 1}
	s.seed()
	s.srv = mcp.NewServer(&mcp.Implementation{Name: "smile-dental-scheduling", Version: "1"}, nil)
	s.addTools()
	return s, nil
}

// Handler serves GET /health (open) and /mcp (bearer token).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.srv }, nil)
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	return mux
}

// seed adds three fixed patients, each with one appointment on a free early slot.
func (s *Server) seed() {
	today := s.now()
	for i, p := range []struct{ name, phone, kind string }{
		{"Maria Lopez", "555-0142", "cleaning"},
		{"James Okafor", "555-0188", "checkup"},
		{"Priya Nair", "555-0107", "consultation"},
	} {
		pt := s.addPatient(p.name, p.phone)
		slots := s.freeSlots(today.AddDate(0, 0, 2+i), 1)
		if len(slots) > 0 {
			_, _ = s.addAppointment(pt.ID, slots[0].ID, p.kind)
		}
	}
}

func (s *Server) addPatient(name, phone string) Patient {
	p := Patient{ID: "p" + strconv.Itoa(s.nextP), Name: name, Phone: phone}
	s.nextP++
	s.patients = append(s.patients, p)
	return p
}

func norm(name string) string { return strings.ToLower(strings.Join(strings.Fields(name), " ")) }

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// slotTime parses a slot id and checks it is a real, future, weekday slot inside the horizon.
func (s *Server) slotTime(id string) (time.Time, error) {
	t, err := time.Parse(slotLayout, id)
	hhmm := ""
	if err == nil {
		hhmm = t.Format("15:04")
	}
	ok := false
	for _, st := range slotTimes {
		ok = ok || st == hhmm
	}
	now := s.now()
	if err != nil || !ok || t.Weekday() == time.Saturday || t.Weekday() == time.Sunday ||
		!t.After(now) || t.After(now.AddDate(0, 0, horizonDays)) {
		return time.Time{}, fmt.Errorf("%q is not an available slot; use an id from find_slots", id)
	}
	return t, nil
}

func (s *Server) taken(slotID, exceptAppt string) bool {
	for _, a := range s.appts {
		if a.SlotID == slotID && a.ID != exceptAppt {
			return true
		}
	}
	return false
}

// freeSlots lists up to limit free slots on or after from (a date), inside the horizon.
func (s *Server) freeSlots(from time.Time, limit int) []Slot {
	now := s.now()
	day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	if min := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC); day.Before(min) {
		day = min
	}
	var out []Slot
	for d := 0; d <= horizonDays && len(out) < limit; d++ {
		date := day.AddDate(0, 0, d)
		if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
			continue
		}
		for _, hm := range slotTimes {
			id := date.Format("2006-01-02") + "T" + hm
			if _, err := s.slotTime(id); err != nil || s.taken(id, "") {
				continue
			}
			t, _ := time.Parse(slotLayout, id)
			out = append(out, Slot{ID: id, Label: t.Format("Monday 2 January, 15:04")})
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

func (s *Server) addAppointment(patientID, slotID, kind string) (Appointment, error) {
	t, err := s.slotTime(slotID)
	if err != nil {
		return Appointment{}, err
	}
	if !kinds[kind] {
		return Appointment{}, errors.New("kind must be checkup, cleaning, filling or consultation")
	}
	if s.taken(slotID, "") {
		return Appointment{}, fmt.Errorf("slot %s is already taken; call find_slots for another", slotID)
	}
	if len(s.appts) >= MaxAppointments {
		return Appointment{}, errors.New("the schedule is full")
	}
	a := Appointment{ID: "a" + strconv.Itoa(s.nextA), PatientID: patientID, Kind: kind, SlotID: slotID, When: t.Format("Monday 2 January, 15:04")}
	s.nextA++
	s.appts = append(s.appts, a)
	return a, nil
}

func (s *Server) patient(id string) (Patient, bool) {
	for _, p := range s.patients {
		if p.ID == id {
			return p, true
		}
	}
	return Patient{}, false
}

type (
	findIn struct {
		Name  string `json:"name"`
		Phone string `json:"phone,omitempty"`
	}
	findOut struct {
		Found   bool     `json:"found"`
		Patient *Patient `json:"patient,omitempty"`
	}
	registerIn struct {
		Name  string `json:"name"`
		Phone string `json:"phone"`
	}
	patientOut struct {
		Patient Patient `json:"patient"`
	}
	patientIDIn struct {
		PatientID string `json:"patient_id"`
	}
	listOut struct {
		Appointments []Appointment `json:"appointments"`
	}
	slotsIn struct {
		FromDate string `json:"from_date,omitempty"`
	}
	slotsOut struct {
		Slots []Slot `json:"slots"`
	}
	bookIn struct {
		PatientID string `json:"patient_id"`
		SlotID    string `json:"slot_id"`
		Kind      string `json:"kind"`
	}
	apptOut struct {
		Appointment Appointment `json:"appointment"`
	}
	moveIn struct {
		AppointmentID string `json:"appointment_id"`
		SlotID        string `json:"slot_id"`
	}
	cancelIn struct {
		AppointmentID string `json:"appointment_id"`
	}
	cancelOut struct {
		Cancelled string `json:"cancelled"`
	}
)

func (s *Server) addTools() {
	mcp.AddTool(s.srv, &mcp.Tool{Name: "find_patient", Description: "Find an existing patient by full name (and optionally phone). found is false for a new patient."},
		func(_ context.Context, _ *mcp.CallToolRequest, in findIn) (*mcp.CallToolResult, findOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, p := range s.patients {
				if norm(p.Name) == norm(in.Name) && (in.Phone == "" || digits(p.Phone) == digits(in.Phone)) {
					return nil, findOut{Found: true, Patient: &p}, nil
				}
			}
			return nil, findOut{}, nil
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "register_patient", Description: "Register a new patient with full name and phone number. Returns the patient id."},
		func(_ context.Context, _ *mcp.CallToolRequest, in registerIn) (*mcp.CallToolResult, patientOut, error) {
			name, phone := strings.Join(strings.Fields(in.Name), " "), strings.TrimSpace(in.Phone)
			if name == "" || phone == "" || len(name) > maxField || len(phone) > maxField {
				return nil, patientOut{}, errors.New("name and phone are required (at most 80 characters each)")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, p := range s.patients {
				if norm(p.Name) == norm(name) && digits(p.Phone) == digits(phone) {
					return nil, patientOut{Patient: p}, nil // registering twice returns the same patient
				}
			}
			if len(s.patients) >= MaxPatients {
				return nil, patientOut{}, errors.New("the patient register is full")
			}
			return nil, patientOut{Patient: s.addPatient(name, phone)}, nil
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "list_appointments", Description: "List a patient's upcoming appointments."},
		func(_ context.Context, _ *mcp.CallToolRequest, in patientIDIn) (*mcp.CallToolResult, listOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			out := listOut{Appointments: []Appointment{}}
			for _, a := range s.appts {
				if a.PatientID == in.PatientID {
					out.Appointments = append(out.Appointments, a)
				}
			}
			return nil, out, nil
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "find_slots", Description: "List up to six free appointment slots, starting from_date (YYYY-MM-DD, default today). Offer at most three to the caller."},
		func(_ context.Context, _ *mcp.CallToolRequest, in slotsIn) (*mcp.CallToolResult, slotsOut, error) {
			from := s.now()
			if in.FromDate != "" {
				d, err := time.Parse("2006-01-02", in.FromDate)
				if err != nil {
					return nil, slotsOut{}, errors.New("from_date must be YYYY-MM-DD")
				}
				from = d
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			return nil, slotsOut{Slots: s.freeSlots(from, slotsPerReply)}, nil
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "book_appointment", Description: "Book a slot (id from find_slots) for a patient. kind is checkup, cleaning, filling or consultation."},
		func(_ context.Context, _ *mcp.CallToolRequest, in bookIn) (*mcp.CallToolResult, apptOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, ok := s.patient(in.PatientID); !ok {
				return nil, apptOut{}, errors.New("unknown patient_id; register the patient first")
			}
			a, err := s.addAppointment(in.PatientID, in.SlotID, in.Kind)
			return nil, apptOut{Appointment: a}, err
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "reschedule_appointment", Description: "Move an appointment to another free slot (id from find_slots)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in moveIn) (*mcp.CallToolResult, apptOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			for i, a := range s.appts {
				if a.ID != in.AppointmentID {
					continue
				}
				t, err := s.slotTime(in.SlotID)
				if err != nil {
					return nil, apptOut{}, err
				}
				if s.taken(in.SlotID, a.ID) {
					return nil, apptOut{}, fmt.Errorf("slot %s is already taken; call find_slots for another", in.SlotID)
				}
				s.appts[i].SlotID, s.appts[i].When = in.SlotID, t.Format("Monday 2 January, 15:04")
				return nil, apptOut{Appointment: s.appts[i]}, nil
			}
			return nil, apptOut{}, errors.New("unknown appointment_id")
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "cancel_appointment", Description: "Cancel an appointment. Confirm with the caller before calling."},
		func(_ context.Context, _ *mcp.CallToolRequest, in cancelIn) (*mcp.CallToolResult, cancelOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			for i, a := range s.appts {
				if a.ID == in.AppointmentID {
					s.appts = append(s.appts[:i], s.appts[i+1:]...)
					return nil, cancelOut{Cancelled: a.ID}, nil
				}
			}
			return nil, cancelOut{}, errors.New("unknown appointment_id")
		})
}
```

Note: `find_patient`'s `&p` takes the address of the loop variable; Go 1.22+ gives each iteration its own variable, and the pointer is only marshalled inside the call, so this is safe. The test `TestTheRegistryIsBounded` registers distinct names/phones in a loop; if the generated pairs collide (the "registering twice" rule returns the same patient and the loop never reaches the bound), make the test names unique with `strconv.Itoa(i)` instead of the character arithmetic and re-run.

- [ ] **Step 4: The binary** `cmd/dental-mcp/main.go`

```go
// Command dental-mcp serves the Smile Dental demo scheduling MCP server (server/mcp/dentalmcp).
// PORT (default 8081) is the listen port; DENTAL_MCP_TOKEN is the bearer token (required).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"govoice/server/mcp/dentalmcp"
)

func main() {
	srv, err := dentalmcp.New(dentalmcp.Options{Token: os.Getenv("DENTAL_MCP_TOKEN")})
	if err != nil {
		log.Fatalf("dental-mcp: set DENTAL_MCP_TOKEN: %v", err)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	hs := &http.Server{Addr: ":" + port, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	log.Printf("dental-mcp: listening on :%s (/mcp, /health)", port)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("dental-mcp: %v", err)
	}
}
```

Makefile (next to `mcp-test`, same `## dev|` help style):

```make
dental-mcp: ## dev|Run the Smile Dental demo MCP server on :18767 (token: dev-dental-token)
	DENTAL_MCP_TOKEN=$${DENTAL_MCP_TOKEN:-dev-dental-token} PORT=18767 go run ./cmd/dental-mcp
```

Check the root `go.mod`/`go.work` resolves `govoice/server/mcp/dentalmcp` from `cmd/` the same way `cmd/mcptest` resolves `govoice/server/mcp/mcptest`.

- [ ] **Step 5: Run the gate**

Run: `cd server && go test -race ./mcp/dentalmcp -v 2>&1 | tail -15; cd .. && go build ./cmd/dental-mcp && gofmt -l server cmd`
Expected: PASS, binary builds, no gofmt output.

- [ ] **Step 6: Commit**

```bash
git add server/mcp/dentalmcp cmd/dental-mcp Makefile
git commit -m "feat(mcp): the Smile Dental demo scheduling server"
```

---

### Task 6: the demo installer, its API and auto-install

**Files:**
- Create: `server/demo/dental.go`, `server/demo/dental_test.go`, `server/demo_api.go`, `server/demo_api_test.go`
- Modify: `server/config.go` (+ `config_defaults.go`, `config_validate.go`), `server/api_routes.go`, `server/server.go` (start-up run)
- Test: as above

**Interfaces:**
- Consumes: `dentalmcp.ToolNames` (Task 5), `flow.Step.EndCall` via config JSON (Task 1), `mcp.Service`, `flow.Service`, `storage.MCPRepository.ListServers`.
- Produces:
  - `demo.InstallDental(ctx, Servers, Flows, endpoint string) (Result, error)`, `demo.Result{MCPServerID string; MCPCreated bool; Tools int; FlowID string; FlowCreated bool}`, constants `demo.ServerName`, `demo.FlowName`, `demo.AuthEnv = "MCP_TOKEN_DENTAL"`.
  - `GET /api/v1/demo` -> `{"data":{"dental":{"available":bool,"installed":bool,"flow_id":"..."}}}`; `POST /api/v1/demo/dental` -> `{"data":Result}`. Used by Task 7.
  - Config key `demo.auto_install: dental` (empty = off); env `DENTAL_MCP_URL` is the MCP endpoint.

- [ ] **Step 1: Write the failing installer tests** `server/demo/dental_test.go` (fakes for the two consumer-side interfaces)

```go
package demo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"govoice/server/flow"
	"govoice/server/mcp"
	"govoice/server/mcp/dentalmcp"
	"govoice/server/storage"
)

type fakeServers struct {
	list    []storage.MCPServer
	creates int
}

func (f *fakeServers) List(context.Context) ([]storage.MCPServer, error) { return f.list, nil }
func (f *fakeServers) Create(_ context.Context, c mcp.ServerConfig) (storage.MCPServer, error) {
	f.creates++
	m := storage.MCPServer{ID: "srv1", Name: c.Name, Endpoint: c.Endpoint, AuthEnv: c.AuthEnv, Enabled: c.Enabled}
	f.list = append(f.list, m)
	return m, nil
}
func (f *fakeServers) Refresh(context.Context, string) (storage.MCPDiscovery, error) {
	var d storage.MCPDiscovery
	for _, n := range dentalmcp.ToolNames {
		d.Tools = append(d.Tools, storage.MCPTool{RemoteName: n, SchemaHash: "h-" + n})
	}
	return d, nil
}

type fakeFlows struct {
	list    []flow.Flow
	creates []flow.CreateRequest
}

func (f *fakeFlows) List(context.Context, int, int) ([]flow.Flow, error) { return f.list, nil }
func (f *fakeFlows) Create(_ context.Context, r flow.CreateRequest) (flow.Flow, error) {
	f.creates = append(f.creates, r)
	fl := flow.Flow{ID: "flow1", Name: r.Name}
	f.list = append(f.list, fl)
	return fl, nil
}

func TestInstallDentalCreatesTheServerAndTheFlow(t *testing.T) {
	srv, fl := &fakeServers{}, &fakeFlows{}
	res, err := InstallDental(context.Background(), srv, fl, "https://dental.example.com/mcp")
	if err != nil || !res.MCPCreated || !res.FlowCreated || res.Tools != len(dentalmcp.ToolNames) || res.FlowID != "flow1" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if srv.list[0].AuthEnv != AuthEnv || srv.list[0].Endpoint != "https://dental.example.com/mcp" {
		t.Fatalf("server = %+v", srv.list[0])
	}
	req := fl.creates[0]
	if len(req.MCPTools) != len(dentalmcp.ToolNames) {
		t.Fatalf("flow tool selections = %d", len(req.MCPTools))
	}
	// The stored config is a valid three-step flow the server accepts.
	g, err := flow.Decode(req.Config)
	if err != nil || len(g.Steps) != 3 || len(g.Routes) != 2 {
		t.Fatalf("config: steps=%d routes=%d err=%v", len(g.Steps), len(g.Routes), err)
	}
	for _, s := range g.Steps {
		if !s.EndCall {
			t.Errorf("step %q must be able to end the call", s.Name)
		}
		for _, ref := range s.Tools {
			if !strings.HasPrefix(ref, "srv1/") {
				t.Errorf("step %q tool %q must point at the installed server", s.Name, ref)
			}
		}
	}
	// Providers: three different stacks, one STT on the start step only.
	front, _ := g.Step("front")
	if front.Config.STT.Provider != "google" || front.Config.LLM.Provider != "google" || front.Config.TTS.Provider != "elevenlabs" {
		t.Fatalf("front stack = %+v", front.Config)
	}
	newp, _ := g.Step("new")
	ret, _ := g.Step("returning")
	if newp.Config.LLM.Provider != "xai" || newp.Config.TTS.Provider != "xai" || ret.Config.TTS.Provider != "google" {
		t.Fatalf("step stacks: new=%+v returning=%+v", newp.Config, ret.Config)
	}
	if !json.Valid(req.Config) {
		t.Fatal("config is not JSON")
	}
}

func TestInstallDentalTwiceChangesNothing(t *testing.T) {
	srv, fl := &fakeServers{}, &fakeFlows{}
	if _, err := InstallDental(context.Background(), srv, fl, "https://d/mcp"); err != nil {
		t.Fatal(err)
	}
	res, err := InstallDental(context.Background(), srv, fl, "https://d/mcp")
	if err != nil || res.MCPCreated || res.FlowCreated || srv.creates != 1 || len(fl.creates) != 1 || res.FlowID != "flow1" {
		t.Fatalf("second run: res=%+v err=%v creates=%d/%d", res, err, srv.creates, len(fl.creates))
	}
}

func TestInstallDentalFinishesAHalfDoneInstall(t *testing.T) {
	srv := &fakeServers{list: []storage.MCPServer{{ID: "srv1", Name: ServerName, Endpoint: "https://d/mcp"}}}
	fl := &fakeFlows{}
	res, err := InstallDental(context.Background(), srv, fl, "https://d/mcp")
	if err != nil || res.MCPCreated || !res.FlowCreated || srv.creates != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestInstallDentalRefusesAServerMissingATool(t *testing.T) {
	srv := &fakeServers{}
	srv.list = []storage.MCPServer{{ID: "srv1", Name: ServerName}}
	short := &shortServers{fakeServers: srv}
	if _, err := InstallDental(context.Background(), short, &fakeFlows{}, "https://d/mcp"); err == nil {
		t.Fatal("a server that does not offer every dental tool must not get a flow")
	}
}

type shortServers struct{ *fakeServers }

func (s *shortServers) Refresh(context.Context, string) (storage.MCPDiscovery, error) {
	return storage.MCPDiscovery{Tools: []storage.MCPTool{{RemoteName: "find_patient", SchemaHash: "h"}}}, nil
}
```

- [ ] **Step 2: Run to see failure**

Run: `cd server && go test ./demo 2>&1 | tail -5`
Expected: build error (no non-test Go files).

- [ ] **Step 3: Implement** `server/demo/dental.go`

```go
// Package demo installs the Smile Dental front-desk demo: the scheduling MCP server and a
// three-step flow (front desk -> new patient | returning patient) on three provider stacks.
// Installing is idempotent and only ever adds what is missing.
package demo

import (
	"context"
	"encoding/json"
	"fmt"

	"govoice/server/flow"
	"govoice/server/mcp"
	"govoice/server/mcp/dentalmcp"
	"govoice/server/storage"
)

const (
	ServerName = "Smile Dental scheduling"
	FlowName   = "Smile Dental front desk"
	// AuthEnv is the env var holding the MCP server's bearer token (never stored).
	AuthEnv = "MCP_TOKEN_DENTAL"
)

// Servers and Flows are what the installer needs of the MCP and flow services.
type Servers interface {
	List(ctx context.Context) ([]storage.MCPServer, error)
	Create(ctx context.Context, cfg mcp.ServerConfig) (storage.MCPServer, error)
	Refresh(ctx context.Context, id string) (storage.MCPDiscovery, error)
}

type Flows interface {
	List(ctx context.Context, limit, offset int) ([]flow.Flow, error)
	Create(ctx context.Context, req flow.CreateRequest) (flow.Flow, error)
}

type Result struct {
	MCPServerID string `json:"mcp_server_id"`
	MCPCreated  bool   `json:"mcp_created"`
	Tools       int    `json:"tools"`
	FlowID      string `json:"flow_id"`
	FlowCreated bool   `json:"flow_created"`
}

// InstallDental adds the MCP server (endpoint is its /mcp URL) and the flow unless they exist.
func InstallDental(ctx context.Context, servers Servers, flows Flows, endpoint string) (Result, error) {
	var res Result
	existing, err := servers.List(ctx)
	if err != nil {
		return res, fmt.Errorf("list MCP servers: %w", err)
	}
	for _, s := range existing {
		if s.Name == ServerName && !s.PendingDelete {
			res.MCPServerID = s.ID
		}
	}
	if res.MCPServerID == "" {
		m, err := servers.Create(ctx, mcp.ServerConfig{Name: ServerName, Description: "Fake patients and appointments for the dentistry demo.", Endpoint: endpoint, AuthEnv: AuthEnv, Enabled: true})
		if err != nil {
			return res, fmt.Errorf("create MCP server: %w", err)
		}
		res.MCPServerID, res.MCPCreated = m.ID, true
	}
	disc, err := servers.Refresh(ctx, res.MCPServerID)
	if err != nil {
		return res, fmt.Errorf("discover dental tools: %w", err)
	}
	have := map[string]storage.MCPTool{}
	for _, t := range disc.Tools {
		have[t.RemoteName] = t
	}
	selections := make([]flow.ToolSelection, 0, len(dentalmcp.ToolNames))
	for _, name := range dentalmcp.ToolNames {
		t, ok := have[name]
		if !ok {
			return res, fmt.Errorf("MCP server %q does not offer the %s tool", ServerName, name)
		}
		selections = append(selections, flow.ToolSelection{ServerID: res.MCPServerID, RemoteName: name, SchemaHash: t.SchemaHash, Required: true})
	}
	res.Tools = len(selections)

	all, err := flows.List(ctx, 200, 0)
	if err != nil {
		return res, fmt.Errorf("list flows: %w", err)
	}
	for _, f := range all {
		if f.Name == FlowName && !f.Archived {
			res.FlowID = f.ID
			return res, nil
		}
	}
	cfg, err := dentalConfig(res.MCPServerID)
	if err != nil {
		return res, err
	}
	f, err := flows.Create(ctx, flow.CreateRequest{Name: FlowName, Description: "Front desk takes the name, then hands new and returning patients to the right agent.", Config: cfg, MCPTools: selections})
	if err != nil {
		return res, fmt.Errorf("create flow: %w", err)
	}
	res.FlowID, res.FlowCreated = f.ID, true
	return res, nil
}

type stage = map[string]any

func step(id, name string, endCall bool, tools []string, config stage, x float64) stage {
	s := stage{"id": id, "name": name, "end_call": endCall, "config": config, "position": stage{"x": x, "y": 0}}
	if len(tools) > 0 {
		s["tools"] = tools
	}
	return s
}

func refs(serverID string, names ...string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = serverID + "/" + n
	}
	return out
}

func dentalConfig(serverID string) (json.RawMessage, error) {
	front := stage{
		"stt": stage{"provider": "google", "model": "chirp_2", "language": "en-US"},
		"llm": stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": frontPrompt},
		"tts": stage{"provider": "elevenlabs", "voice": "21m00Tcm4TlvDq8ikWAM"},
	}
	newPatient := stage{
		"llm": stage{"provider": "xai", "model": "grok-4.20-0309-non-reasoning", "system_prompt": newPatientPrompt},
		"tts": stage{"provider": "xai", "voice": "eve"},
	}
	returning := stage{
		"llm": stage{"provider": "google", "model": "gemini-2.5-flash", "system_prompt": returningPrompt},
		"tts": stage{"provider": "google", "voice": "en-US-Chirp3-HD-Aoede"},
	}
	cfg := stage{
		"start": "front",
		"steps": []stage{
			step("front", "Front desk", true, nil, front, 0),
			step("new", "New patient", true, refs(serverID, "register_patient", "find_slots", "book_appointment"), newPatient, 320),
			step("returning", "Returning patient", true, refs(serverID, "find_patient", "list_appointments", "find_slots", "reschedule_appointment", "cancel_appointment"), returning, 320),
		},
		"routes": []flow.Route{
			{ID: "to-new", From: "front", To: "new", Condition: "The caller has never been a patient at Smile Dental and wants to register or book their first appointment."},
			{ID: "to-returning", From: "front", To: "returning", Condition: "The caller is already a patient and wants to change, move or cancel an appointment."},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode dental flow: %w", err)
	}
	return raw, nil
}

const frontPrompt = `You are the front desk voice assistant for Smile Dental, a dental practice, on a phone call. Keep every reply to one or two short sentences. No lists, no markdown.

Greet the caller warmly, say the practice name, and ask for their name. Once you have it, ask whether they have been a patient at Smile Dental before.

- If it is their first visit, or they want to register or book their first appointment: call transfer_to_new_patient with a note containing their name and what they want.
- If they are already a patient and want to change, move or cancel an appointment: call transfer_to_returning_patient with a note containing their name and what they want.

Opening hours, if asked: Monday to Friday, 9am to 5pm, closed at weekends. The practice is at 12 Elm Street. If you cannot help with something, apologise briefly, say a team member will call back, say goodbye and call end_call.

When you transfer, say one short sentence that you are connecting them, then stop talking.`

const newPatientPrompt = `You are the new-patient assistant at Smile Dental, on a phone call. Keep every reply to one or two short sentences. No lists, no markdown. Say dates the way a person would.

The front desk has already taken the caller's name and passed it to you in a note. Greet the caller by first name, welcome them to the practice and say you will get them registered. Do not ask for their name again.

1. Ask for a phone number, then call register_patient with their full name and phone number.
2. Ask what the visit is for (checkup, cleaning, filling or consultation) and roughly when suits them.
3. Call find_slots and offer at most three options out loud.
4. When they choose, call book_appointment with the patient id from register_patient, the slot id and the kind.
5. Confirm the day, time and kind in one sentence and ask if there is anything else. If not, say a warm goodbye and call end_call.

Use only what the tools return; never invent slots or ids. If a tool fails, apologise and offer another slot.`

const returningPrompt = `You are the returning-patient assistant at Smile Dental, on a phone call. Keep every reply to one or two short sentences. No lists, no markdown. Say dates the way a person would.

The front desk has already taken the caller's name and passed it to you in a note. Greet the caller with "Welcome back" and their first name. Do not ask for their name again.

1. Call find_patient with the name from the note. If they are not found, ask them to spell their name and try once more; if still not found, apologise, say a team member will call back, say goodbye and call end_call.
2. Call list_appointments with their patient id and tell them what they have booked.
3. To move an appointment: call find_slots, offer at most three options out loud, then call reschedule_appointment with the appointment id and the slot id they choose.
4. To cancel: ask them to confirm, then call cancel_appointment.
5. Confirm what changed in one sentence and ask if there is anything else. If not, say a warm goodbye and call end_call.

Use only what the tools return; never invent slots or ids. If a tool fails, apologise and offer another slot.`
```

- [ ] **Step 4: Run to see the installer tests pass**

Run: `cd server && go test -race ./demo -v 2>&1 | tail -15`
Expected: PASS. If `flow.Decode` rejects the config (for example the `position` shape or the xAI summarization rule), fix the config, not the test; the test is the contract that the stored config is valid.

- [ ] **Step 5: The API handlers and config — failing tests first** (`server/demo_api_test.go`, build tag-free, using the helpers the other API boundary tests use: `apiRequest`, `enableTestManagement`; read `server/api_flow_boundary_test.go` for the exact helper setup and copy its server construction)

```go
func TestDemoAPIReportsAvailabilityAndInstalls(t *testing.T) {
	s := newManagedTestServer(t) // the same setup api_flow_boundary_test.go uses
	// Without DENTAL_MCP_URL the demo is not available and installing is refused.
	w := apiRequest(s, http.MethodGet, "/api/v1/demo", testMgmtKey, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"available":false`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w := apiRequest(s, http.MethodPost, "/api/v1/demo/dental", testMgmtKey, ""); w.Code != http.StatusConflict {
		t.Fatalf("install without the endpoint: %d", w.Code)
	}
	// Unauthenticated is refused like every management route.
	if w := apiRequest(s, http.MethodGet, "/api/v1/demo", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", w.Code)
	}
}
```

Adapt the helper names (`newManagedTestServer`, `testMgmtKey`) to what the existing boundary tests really define; do not invent new global helpers if one exists. Add a second test that sets `s.env` to return `DENTAL_MCP_URL` pointing at an `httptest` server running `dentalmcp` with the token set through `MCP_TOKEN_DENTAL`, requires `AllowLocalHTTP` (development config as the MCP e2e tests do, see `server/mcp_integration_test.go`), POSTs `/api/v1/demo/dental`, asserts `flow_created: true`, a second POST returns `flow_created: false`, and `GET /api/v1/demo` reports `"installed":true`.

- [ ] **Step 6: Implement the handlers and routing**

`server/demo_api.go`:

```go
package server

import (
	"context"
	"net/http"

	"govoice/server/api"
	"govoice/server/demo"
	"govoice/server/mcp"
	"govoice/server/storage"
)

// demoServers adapts the MCP service and store to demo.Servers.
type demoServers struct {
	svc   *mcp.Service
	store *storage.Store
}

func (d demoServers) List(ctx context.Context) ([]storage.MCPServer, error) {
	return d.store.MCP().ListServers(ctx)
}
func (d demoServers) Create(ctx context.Context, c mcp.ServerConfig) (storage.MCPServer, error) {
	return d.svc.Create(ctx, c)
}
func (d demoServers) Refresh(ctx context.Context, id string) (storage.MCPDiscovery, error) {
	return d.svc.Refresh(ctx, id)
}

// dentalEndpoint is where the demo's MCP server lives; empty when the deployment has none.
func (s *Server) dentalEndpoint() string { return s.env("DENTAL_MCP_URL") }

// InstallDentalDemo runs the demo installer with this server's services.
func (s *Server) InstallDentalDemo(ctx context.Context) (demo.Result, error) {
	return demo.InstallDental(ctx, demoServers{svc: s.mcpService, store: s.mgmt.store}, s.flows, s.dentalEndpoint())
}

type demoStatus struct {
	Dental struct {
		Available bool   `json:"available"`
		Installed bool   `json:"installed"`
		FlowID    string `json:"flow_id,omitempty"`
	} `json:"dental"`
}

func (s *Server) handleDemo(w http.ResponseWriter, r *http.Request, requestID string) {
	if s.mgmt == nil || s.flows == nil || s.mcpService == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "management_unavailable", "management storage is unavailable", requestID)
		return
	}
	switch {
	case r.URL.Path == "/api/v1/demo" && r.Method == http.MethodGet:
		var st demoStatus
		st.Dental.Available = s.dentalEndpoint() != ""
		if flows, err := s.flows.List(r.Context(), 200, 0); err == nil {
			for _, f := range flows {
				if f.Name == demo.FlowName && !f.Archived {
					st.Dental.Installed, st.Dental.FlowID = true, f.ID
				}
			}
		}
		api.WriteJSON(w, http.StatusOK, api.SuccessResponse[demoStatus]{Data: st})
	case r.URL.Path == "/api/v1/demo/dental" && r.Method == http.MethodPost:
		if s.dentalEndpoint() == "" {
			api.WriteError(w, http.StatusConflict, "demo_unavailable", "DENTAL_MCP_URL is not set on this server", requestID)
			return
		}
		res, err := s.InstallDentalDemo(r.Context())
		if err != nil {
			s.logger.Warn("demo install failed", "err", s.redactor.Redact(err.Error()))
			api.WriteError(w, http.StatusBadGateway, "demo_install_failed", "the demo could not be installed; check the server log", requestID)
			return
		}
		api.WriteJSON(w, http.StatusOK, api.SuccessResponse[demo.Result]{Data: res})
	default:
		w.Header().Set("Allow", "GET, POST")
		api.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method is not supported", requestID)
	}
}
```

Route it in `server/api_routes.go` inside the `default:` branch, before the sessions check:

```go
		if r.URL.Path == "/api/v1/demo" || strings.HasPrefix(r.URL.Path, "/api/v1/demo/") {
			s.handleDemo(w, r, requestID)
			return
		}
```

Config: add to `server/config.go` `Config`: `Demo DemoConfig `yaml:"demo"`` and

```go
// DemoConfig turns on demo installs at start-up (the admin UI button works without it).
type DemoConfig struct {
	// AutoInstall installs a demo when the server starts: "" (off) or "dental".
	AutoInstall string `yaml:"auto_install"`
}
```

Validate in `config_validate.go`: `auto_install` must be "" or "dental" (error `demo.auto_install must be empty or "dental"`). Mirror how other sections are registered in `config_defaults.go` (read that file; a zero value needs no default).

Start-up run: in `server/server.go`, in `Start` once management is enabled and listeners are up (read `Start` to find the right spot: after `EnableManagement` has run and before returning from the readiness gate), add

```go
	if s.cfg.Demo.AutoInstall == "dental" {
		s.autoInstallDemo()
	}
```

with, in `demo_api.go`:

```go
// autoInstallDemo installs the dental demo in the background. A failure is logged loudly and
// never stops the server: the admin UI button can retry.
func (s *Server) autoInstallDemo() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if s.dentalEndpoint() == "" {
			s.logger.Error("demo.auto_install is dental but DENTAL_MCP_URL is empty")
			return
		}
		res, err := s.InstallDentalDemo(ctx)
		if err != nil {
			s.logger.Error("demo auto-install failed", "err", s.redactor.Redact(err.Error()))
			return
		}
		s.logger.Info("demo installed", "flow", res.FlowID, "flow_created", res.FlowCreated, "mcp_created", res.MCPCreated)
	}()
}
```

(`demo_api.go` then also imports `time`.) The goroutine ends when the call returns (bounded by the 60 s context). Add a test that a server with `Demo.AutoInstall = "dental"` and `DENTAL_MCP_URL` set to the test dentalmcp ends up with the flow within a few seconds (`eventually`).

- [ ] **Step 7: Gate**

Run: `cd server && go test -race ./... 2>&1 | tail -10; cd .. && gofmt -l server`
Expected: PASS, no gofmt output.

- [ ] **Step 8: Commit**

```bash
git add server
git commit -m "feat(server): demo installer with admin API and optional auto-install"
```

---

### Task 7: admin UI — end-call toggle, "Load demo", the call's ending

**Files:**
- Modify: `ui/src/routes/flows/editor/model.ts`, `ui/src/routes/flows/editor/StepPanel.tsx`, `ui/src/api/client.ts`, `ui/src/routes/dashboard/DashboardView.tsx`, `ui/src/routes/sessions/callStats.ts`, `ui/src/routes/sessions/CallView.tsx`
- Create: `ui/src/routes/dashboard/DemoCard.tsx`, `ui/src/routes/dashboard/DemoCard.test.tsx`
- Test: `ui/src/routes/flows/editor/model.test.ts`, `StepPanel.test.tsx`, `ui/src/routes/sessions/callStats.test.ts`, `CallView.test.tsx`, `DashboardView.test.tsx`

**Interfaces:**
- Consumes: JSON `end_call` on steps (Task 1); `GET /api/v1/demo` and `POST /api/v1/demo/dental` (Task 6); span `call.end` with attributes `reason`, `from_step` (Task 3).
- Produces: `DraftStep.end_call?: boolean`; `ApiClient.getDemo()`, `ApiClient.installDentalDemo()`; `endedByFlow(traces): { step: string } | null` in `callStats.ts`.

Read the neighbouring tests first (`model.test.ts`, `StepPanel.test.tsx`, `callStats.test.ts`) and mirror their fixtures and helper names; the snippets below show the assertions and the production code, not their local fixture helpers.

- [ ] **Step 1: model round-trip — failing test** (`model.test.ts`)

```ts
it("round-trips a step's end_call flag", () => {
  const cfg = {
    start: "a",
    steps: [
      { id: "a", name: "A", end_call: true, config: { llm: {}, tts: {} } },
      { id: "b", name: "B", config: { llm: {}, tts: {} } },
    ],
    routes: [{ id: "r", from: "a", to: "b", condition: "x" }],
  };
  const g = fromFlowConfig(cfg);
  expect(g.steps[0].end_call).toBe(true);
  expect(g.steps[1].end_call).toBeFalsy();
  const out = toPayload(g) as { steps: { id: string; end_call?: boolean }[] };
  expect(out.steps[0].end_call).toBe(true);
  expect("end_call" in out.steps[1]).toBe(false); // the default is not written
});
```

Run: `cd ui && npx vitest run src/routes/flows/editor/model.test.ts` — Expected: FAIL.

- [ ] **Step 2: Implement in `model.ts`**: add `end_call?: boolean;` to `DraftStep` (with the doc comment "the step may hang up"), in `fromFlowConfig` add `end_call: r.end_call === true,` to the step object, in `toPayload` change the step to `{ id: n.id, name: n.name, config, ...(n.tools.length ? { tools: n.tools } : {}), ...(n.end_call ? { end_call: true } : {}), position: n.position }`. Also include `end_call` in `diff.ts`'s step comparison next to the tools line: `if (!!o.end_call !== !!n.end_call) out.push(`${n.name}: end call ${n.end_call ? "enabled" : "disabled"}`);` plus a one-line test in `diff.test.ts`.

Run the model and diff tests — Expected: PASS.

- [ ] **Step 3: Step panel checkbox — failing test** (`StepPanel.test.tsx`; copy how an existing test renders the panel on the tools tab)

```ts
it("lets a step be given the end_call tool", async () => {
  // render StepPanel for a step on the Tools tab, as the neighbouring tools-tab test does
  const onChange = vi.fn();
  /* ...render with a graph whose step "a" has end_call unset... */
  await userEvent.click(screen.getByRole("checkbox", { name: /can end the call/i }));
  expect(onChange).toHaveBeenCalledWith(expect.objectContaining({ steps: expect.arrayContaining([expect.objectContaining({ id: "a", end_call: true })]) }));
});
```

In `StepPanel.tsx`, in the `tab === "tools"` block, render the call-control fieldset above the tools list so it shows even when the flow has no MCP tools. Replace the block's opening so the ternary sits inside a fragment:

```tsx
        {tab === "tools" && (
          <>
            <fieldset className="stage-box">
              <legend>Call control</legend>
              <label className="check-row" htmlFor="step-end-call">
                <input
                  id="step-end-call"
                  type="checkbox"
                  checked={!!step.end_call}
                  onChange={(e) => onChange(updateStep(graph, step.id, { end_call: e.target.checked }))}
                />
                Can end the call
              </label>
              <p className="subtext">Gives this step an end_call tool. The model uses it after saying goodbye; the call ends when it stops speaking. Needs a flow with at least two steps.</p>
            </fieldset>
            {/* the existing flowTools ternary, unchanged */}
          </>
        )}
```

Check `updateStep`'s patch type accepts `end_call` (it takes a `Partial<DraftStep>`; adjust the type if it is narrower). The tab count badge `count("tools")` may also add 1 when `end_call` is set: `t === "tools" ? step.tools.length + (step.end_call ? 1 : 0)`.

Run: `cd ui && npx vitest run src/routes/flows/editor` — Expected: PASS.

- [ ] **Step 4: "Ended by the flow" — failing test** (`callStats.test.ts`)

```ts
it("finds the step that ended the call", () => {
  const traces = { session_id: "s", events: [], tool_calls: [], spans: [
    { id: "1", session_id: "s", name: "call.end", started_at: "2026-10-13T10:00:00Z", attributes: { reason: "ended_by_flow", from_step: "new" } },
  ] } as unknown as SessionTracesDTO;
  expect(endedByFlow(traces)).toEqual({ step: "new" });
  expect(endedByFlow({ ...traces, spans: [] })).toBeNull();
  expect(endedByFlow(null)).toBeNull();
});
```

Implement in `callStats.ts`:

```ts
/** The step whose end_call tool ended the call, from the call.end span; null for any other ending. */
export function endedByFlow(traces: SessionTracesDTO | null | undefined): { step: string } | null {
  for (const s of traces?.spans ?? []) {
    const a = (s.attributes && typeof s.attributes === "object" ? s.attributes : {}) as Record<string, unknown>;
    if (s.name === "call.end" && a.reason === "ended_by_flow" && typeof a.from_step === "string") return { step: a.from_step };
  }
  return null;
}
```

In `CallView.tsx`, add a figure after Duration: when `endedByFlow(traces)` is non-null render
`<div><dt>Ended</dt><dd>By the flow ({label(ended.step)})</dd></div>`, and add a `CallView.test.tsx` case that mocks traces with the `call.end` span and expects the text "By the flow (New patient)" (use the neighbouring test's mocks for session, flow and traces).

Run: `cd ui && npx vitest run src/routes/sessions` — Expected: PASS.

- [ ] **Step 5: Client methods — failing test** (`ui/src/api/client.test.ts`; follow the neighbouring "getOperationsStatus" style that stubs `fetch`)

```ts
it("reads the demo status and installs the dental demo", async () => {
  const fetchMock = vi.fn()
    .mockResolvedValueOnce(json({ data: { dental: { available: true, installed: false } } }))
    .mockResolvedValueOnce(json({ data: { mcp_server_id: "m", mcp_created: true, tools: 7, flow_id: "f", flow_created: true } }));
  const client = createApiClient({ baseUrl: "", getKey: () => "k", fetch: fetchMock }); // use the file's real factory signature
  expect((await client.getDemo()).dental.available).toBe(true);
  expect((await client.installDentalDemo()).flow_id).toBe("f");
  expect(fetchMock.mock.calls[1][1].method).toBe("POST");
  expect(String(fetchMock.mock.calls[1][0])).toContain("/api/v1/demo/dental");
});
```

Implement in `client.ts` (zod schemas next to the operations ones; methods next to `getOperationsStatus`; for the POST copy how `createFlow` passes `method` and `body` to `request`):

```ts
export const demoStatusSchema = z.object({
  dental: z.object({ available: z.boolean(), installed: z.boolean(), flow_id: z.string().optional() }),
});
export type DemoStatus = z.infer<typeof demoStatusSchema>;
export const demoInstallSchema = z.object({
  mcp_server_id: z.string(), mcp_created: z.boolean(), tools: z.number(), flow_id: z.string(), flow_created: z.boolean(),
});
export type DemoInstall = z.infer<typeof demoInstallSchema>;
```

```ts
    getDemo: async () => (await request("/demo", z.object({ data: demoStatusSchema }))).data,
    installDentalDemo: async () =>
      (await request("/demo/dental", z.object({ data: demoInstallSchema }), { method: "POST" })).data,
```

- [ ] **Step 6: The Dashboard card — failing test** `DemoCard.test.tsx` (mirror `DashboardView.test.tsx`'s `renderWithProviders`)

```tsx
it("offers the dental demo only when the server has an MCP endpoint for it", async () => {
  const client = { getDemo: vi.fn().mockResolvedValue({ dental: { available: false, installed: false } }) } as unknown as ApiClient;
  renderWithProviders(<DemoCard client={client} />);
  await waitFor(() => expect(client.getDemo).toHaveBeenCalled());
  expect(screen.queryByRole("button", { name: /load demo/i })).toBeNull();
});

it("installs the demo and links to the flow", async () => {
  const getDemo = vi.fn()
    .mockResolvedValueOnce({ dental: { available: true, installed: false } })
    .mockResolvedValue({ dental: { available: true, installed: true, flow_id: "f1" } });
  const installDentalDemo = vi.fn().mockResolvedValue({ mcp_server_id: "m", mcp_created: true, tools: 7, flow_id: "f1", flow_created: true });
  renderWithProviders(<DemoCard client={{ getDemo, installDentalDemo } as unknown as ApiClient} />);
  await userEvent.click(await screen.findByRole("button", { name: /load demo/i }));
  expect(installDentalDemo).toHaveBeenCalledTimes(1);
  expect(await screen.findByRole("link", { name: /smile dental front desk/i })).toHaveAttribute("href", "/flows/f1");
});

it("shows the failure and lets the user retry", async () => {
  const installDentalDemo = vi.fn().mockRejectedValue(new Error("boom"));
  const client = { getDemo: vi.fn().mockResolvedValue({ dental: { available: true, installed: false } }), installDentalDemo } as unknown as ApiClient;
  renderWithProviders(<DemoCard client={client} />);
  await userEvent.click(await screen.findByRole("button", { name: /load demo/i }));
  expect(await screen.findByRole("alert")).toHaveTextContent(/could not be installed/i);
  expect(screen.getByRole("button", { name: /load demo/i })).toBeEnabled();
});
```

Implement `DemoCard.tsx`:

```tsx
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import type { ApiClient } from "../../api/client";

/** Shown on the Dashboard only when this server has a dental MCP endpoint (DENTAL_MCP_URL). */
export function DemoCard({ client }: { client: ApiClient }) {
  const qc = useQueryClient();
  const status = useQuery({ queryKey: ["demo"], queryFn: () => client.getDemo(), retry: false });
  const install = useMutation({
    mutationFn: () => client.installDentalDemo(),
    onSuccess: () => Promise.all([qc.invalidateQueries({ queryKey: ["demo"] }), qc.invalidateQueries({ queryKey: ["flows"] })]),
  });
  const d = status.data?.dental;
  if (!d?.available) return null;
  return (
    <div className="message-panel">
      <h2>Dentistry demo</h2>
      <p>A front desk that hands new and returning patients to different agents, with a scheduling tool server.</p>
      {d.installed && d.flow_id ? (
        <p>
          Installed: <Link to={`/flows/${encodeURIComponent(d.flow_id)}`}>Smile Dental front desk</Link>
        </p>
      ) : (
        <button type="button" className="action-button-small" disabled={install.isPending} onClick={() => install.mutate()}>
          {install.isPending ? "Loading…" : "Load demo"}
        </button>
      )}
      {install.isError && <p role="alert">The demo could not be installed. Check the server log and try again.</p>}
    </div>
  );
}
```

Render `<DemoCard client={client} />` in `DashboardView.tsx` inside the main (connected) layout, next to the other panels (find where the provider readiness panel renders and add it after it). In `DashboardView.test.tsx`'s `createMockClient`, add `getDemo: vi.fn().mockResolvedValue({ dental: { available: false, installed: false } })` so existing tests do not break.

- [ ] **Step 7: Gate**

Run: `cd ui && npx tsc --noEmit && npx vitest run 2>&1 | tail -8`
Expected: type-check clean; all UI tests PASS (including the token contrast and no-hardcoded-colour tests: use existing CSS classes only).

- [ ] **Step 8: Commit**

```bash
git add ui
git commit -m "feat(ui): end-call toggle, Load demo card and the call's ending"
```

---

### Task 8: browser end-to-end for the whole demo (fake providers)

**Files:**
- Create: `e2e/tests-flows/60-dental.spec.ts`, `e2e/lib/dental-helpers.ts`
- Modify: `e2e/global-setup` (flow-suite stack: start `cmd/dental-mcp` and set `DENTAL_MCP_URL` / `MCP_TOKEN_DENTAL`, `mcp.allow_local_http`), `e2e/README.md` (table row)

**Interfaces:**
- Consumes: everything above, through the browser and the HTTP API.

Read `e2e/lib/mcp-helpers.ts` and the flow-suite global setup first: the dental server is started exactly as `cmd/mcptest` is (build to `bin/`, free port, env passed to the stack), and `DENTAL_MCP_URL=http://127.0.0.1:<port>/mcp` plus `MCP_TOKEN_DENTAL=<token>` reach only the server process. Add `bin/dental-mcp` to the build step beside `bin/mcptest`.

- [ ] **Step 1: Write the spec** `e2e/tests-flows/60-dental.spec.ts`

Flow under test (fake providers; the fake LLM obeys phrases):

1. Open the Dashboard with the management key; the "Dentistry demo" card shows "Load demo"; click it; the link "Smile Dental front desk" appears. (Providers are placeholders in the fake stack, so xAI and ElevenLabs stages are not available: the fake stack only registers `google.*`. **Ruling to apply:** the spec installs the real flow; for the fake suite, the test then edits nothing and instead creates a *fake-provider variant* through the helper by POSTing the same flow shape with `google` providers and the MCP selections taken from the installed flow's `current_version.mcp_tools`, named "Dental (fake)". The Load-demo click itself is still asserted on the card, and the installed flow is asserted to exist with three steps and two routes via `GET /api/v1/flows/<id>`.)
2. Start a Test call on "Dental (fake)" and send the text turns (paced: the RTVI send-text limiter allows a burst of 3 then 1/s): "hello" -> front desk reply; "transfer to new patient" -> reply, route to New patient, the new step greets; `call tool register_patient {"name":"Sam Taylor","phone":"555 0100"}` -> reply contains the returned patient; `call tool find_slots {}`; `call tool book_appointment {...}` using the slot id read from the previous reply; "end the call" -> the call ends.
3. Open the Calls page; the call is listed; open it: Path shows `Front desk → New patient`, the transcript is labelled by step, "Ended: By the flow (New patient)" is visible, and the MCP tool calls are listed with `register_patient`, `find_slots`, `book_appointment`.
4. A second call: "transfer to returning patient" -> `call tool find_patient {"name":"Maria Lopez"}` -> `call tool list_appointments {"patient_id":"p1"}` -> "end the call"; the path is `Front desk → Returning patient`.

Use the helpers in `e2e/lib/flows-helpers.ts` for navigation, the Test call panel and sending text turns. Because the fake LLM names MCP tools by their remote name, no tool-id parsing is needed (`mcpToolPhrases` maps `call tool <remote name>`).

- [ ] **Step 2: Run it**

Run: `make e2e-fake` (needs the ORT library; see Makefile `ort-check`).
Expected: the new spec passes with the rest of the suite; `npm run typecheck` in `e2e/` is clean (it runs as part of `make e2e-fake`).

- [ ] **Step 3: README row and commit**

Add a `60-dental.spec.ts` row to the table in `e2e/README.md`: "Load demo from the Dashboard, a new-patient call and a returning-patient call with the real dentistry MCP server: routing, shared note, MCP tool calls, the call ended by the flow, all visible on the Calls page. Fake only."

```bash
git add e2e Makefile
git commit -m "test(e2e): the dentistry demo end to end on fake providers"
```

---

### Task 9: documentation and the review gate

**Files:**
- Create: `docs/demo/dentistry.md`
- Modify: `docs/` index files that list guides (grep for `docs/` README or `mkdocs`), `server/docs_contract_test.go` fixtures if it lists documented config keys

- [ ] **Step 1: Write `docs/demo/dentistry.md`**: what the demo is, the three-step table (copy from the spec), how to run it locally (`make dev` + `make dental-mcp`, `DENTAL_MCP_URL=http://127.0.0.1:18767/mcp`, `MCP_TOKEN_DENTAL=dev-dental-token`, `mcp.allow_local_http` in development), the acceptance script from the spec, the `end_call` step flag, the transfer `note`, and the config keys `demo.auto_install`.
- [ ] **Step 2: Run the docs contract test** — `cd server && go test -run Docs ./` — if it fails because `demo.auto_install` is undocumented, document the key in the config reference it checks (see the test for where).
- [ ] **Step 3: Full verification** — `gofmt -l .` (root), `make verify`, `make e2e-free`. Expected: all green.
- [ ] **Step 4: Independent review (AGENTS.md)** — dispatch an independent reviewer on the whole branch diff with the Review Focus list above and the AGENTS.md YAGNI, correctness and concurrency audits; fix Critical/Important findings test-first.
- [ ] **Step 5: Commit** — `git add docs && git commit -m "docs(demo): the dentistry front-desk demo"`
