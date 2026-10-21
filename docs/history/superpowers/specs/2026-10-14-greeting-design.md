> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# Greeting (first thing the agent says) — Design

Status: draft for review (extended to cover handoffs). Spec row: S5 in `2026-10-12-switchboard-console-design.md` ("Greeting / first line in config", First run step 2 "First thing it says").

## Goal

Any step of a flow can optionally open with a line of exact words, spoken the moment the step takes the call: the start step when the call connects, any other step when a route hands the call to it. With no greeting configured, a step behaves exactly as it does today.

## Agreed decisions

- **Exact words, not a model instruction.** The operator types the line; the agent says it verbatim. No model call, so it is instant, free and predictable.
- **The caller can interrupt it**, like any reply. No "finish first" mode and no per-step switch.
- **Optional, per step.** Absent or empty means no greeting for that step.
- **Handoffs are covered.** A step's greeting is spoken when a route lands on it, in that step's voice. It replaces the model-written transfer greeting for that arrival (the caller never hears two).

Out of scope (YAGNI): templating or variables (`{{caller}}`), several greetings or random choice, a "model writes the greeting" mode, a "caller cannot interrupt" mode, per-language greetings, a different greeting for returning to the start step.

## Today (what exists)

- A session is created only after the RTVI client handshake (`transport/ws/transport.go`: `OnClientReady`, then `OnSessionReady`, then `session.Start()`), so audio produced right after start is not lost.
- Speaking without the caller going first: `IdleController.say` (a `SynthesisUnitFrame` straight to TTS, `core/idle.go`), and a system-notice user message that makes the model produce the line (idle check-in; the transfer greeting). Neither records the spoken line the way a model reply is recorded.
- `AssistantContextProcessor` (`core/assistant_context.go`) turns model text into synthesis units, tracks what was delivered, commits it to the conversation history, and applies the partial-turn rule on barge-in. The turn recorder (`server/call.go`) writes a turn row per `AssistantMessageFrame`.
- **Handoffs** (`core/handoff.go`, wired in `server/registry.go`): a route tool commits a transfer; the old step says its goodbye while the target's LLM and TTS are built in parallel. The built target is delivered to the call's `AsyncTrigger`:
  - **Idle flip** (`flip(target, viaTrigger=true)`): once the call is idle the stages are swapped (TTS first, then LLM), the generation is bumped, and `Greet` runs. Today `Greet` adds a user-role message built by `transferGreeting` (the transfer note from the previous agent plus "Greet them briefly, by name if you know it") and the trigger starts a model reply: the model-written transfer greeting.
  - **Turn flip** (`flip(target, false)`, from `beforeLLM`): if the caller speaks before the idle flip, the target is swapped in first and answers the caller's words directly. No `Greet` runs.
- Call-level settings (`stt`, `vad_stop_ms`, `stt_p99_ms`, `recording`, `context`, `async_tools`) live on the start step only (`validateCallLevel`, `server/flow/definition.go`). Step-level settings (model, voice, instructions, tools, `end_call`) live on each step.

## Design

### Approach: play the greeting as the model's turn

The greeting is fed into the assistant path as if the model had said exactly those words. That gives, without new code paths: sentence splitting for TTS, delivery tracking, the partial-on-interrupt rule (only what was heard counts), the history entry (so the model will not greet again), the turn row (so it shows in Call detail and in turn counts), and the web client's bot transcript.

Rejected: sending a `SynthesisUnitFrame` straight to TTS and recording it by hand. It is simpler to trigger but duplicates the history, turn-row, transcript and partial-on-interrupt logic, which could then drift from how real replies behave.

### 1. Config

`greeting` is an optional string on **any step** (a step-level setting, not call-level).

- In `server/flow/definition.go`/`config.go`: `Greeting string \`json:"greeting,omitempty"\`` on the step's stage config, validated: trimmed; at most 500 bytes (bytes, as for route conditions); no control characters other than `\n` and `\t`; a wrong type is rejected as today. It is not added to `validateCallLevel`'s list, so any step may carry it.
- A flat one-step config carries it at the top level, like its other settings; the UI keeps a flat flow flat.
- Schema stays version 2; the field is additive. A version stored without it reads as "no greeting". An older server rejects the field as unknown, so a rollback to an older binary needs the greetings removed first (runbook note).
- It is part of the config hash, so changing a greeting is a new version like any other change.

### 2. When it plays

A single shared server function, `speakAsAssistant(text)`, drives the assistant path (the frames the assistant processor needs to open a turn, take complete text and finish it are fixed by the first implementation task, with a failing test that pins the observable result: spoken, recorded, interruptible). Both triggers below use it, so a greeting behaves the same wherever it comes from.

**Call opening (start step).**
- `CallSpec` gains `Greeting string` from the start step's config. After `session.Start()` (the output gate is open) the greeting is queued once, on a session-supervised task (`session.Go`). It runs once per call, never re-queued, and is dropped if the session is closing.
- The model is not invoked. The caller's first turn runs the normal LLM path with the greeting already in history as the assistant's last message.

**Handoff (any step reached by a route).**
- The step's greeting is read when the step is built (`stepSpec`, `server/call.go`) into `StepSpec.Greeting`, and used when the **idle flip** completes, inside `Handoff.Greet` (`server/registry.go`). By then the new step's TTS is already swapped in, so the greeting is spoken in the **new step's voice**, and the generation was bumped, so the greeting is injected at the new generation.
- **It replaces the transfer greeting.** When the step has a greeting, `Greet` does not send "greet them briefly" to the model. It speaks the fixed text instead. A step without a greeting keeps today's model-written transfer greeting, unchanged.
- **The transfer note is never lost.** The previous agent's note (the caller's name and need, `takeNote`) still reaches the new step: when it is non-empty, `Greet` first adds a context-only message `[The caller was just transferred to you (<step>). Note from the previous agent: <note>.]` to the history (no reply requested, same neutralising of server markers as today), then speaks the greeting. The model therefore sees, in order: the note, then its own greeting, then the caller's next words.
- **A turn flip plays no greeting.** If the caller speaks before the idle flip, the new step answers their words directly (today's behavior); a canned greeting would only delay the answer. As today, no transfer-note message is added on a turn flip.
- **Returning to the start step.** The start step's greeting is the call opening only. If a route later brings the call back to the start step, it receives the model-written transfer greeting as today (no repeat of "thanks for calling").
- **Ordering with the route sound and the old step's goodbye.** The idle flip only happens once the old step has finished speaking, and the route cue (`route_cue`) plays during the build gap; the greeting follows the flip, so the caller hears goodbye, optional cue, then the new greeting.

**Failure.** If TTS or delivery fails (including a step whose voice cannot be built, which already fails the handoff), the greeting is skipped, the failure is logged with the session id and counted in `voice_greeting_failed_total` (one label-free counter), and the call carries on as if there were no greeting. A greeting never ends a call or fails a handoff.

**Mute strategies.** The opening greeting is a bot turn like any other, so `until_first_bot_complete` (documented as "use only with an opening greeting") now has a greeting to release on.

### 3. During and after the call

- **Barge-in:** speaking over a greeting stops it through the normal interruption path; the history keeps only the heard part (existing partial-turn semantics) and the turn row is marked interrupted.
- **Records:** a greeting is a turn row with empty `transcript` and the greeting as `assistant_text`; counted in `turn_count`; shown in Call detail's turns and transcript (labelled by step like any reply); sent to the web client as a normal bot message. Per-turn latency (S9) stores only measured stages: `{}` for a greeting unless TTS first audio is measured on its unit.
- **Redaction and retention:** stored like any assistant text; existing rules apply.
- **Test calls:** a draft test call is built from the draft snapshot, so it plays the draft's greetings, opening and handoff. The Test call panel needs no new code.

### 4. Console

- **Step panel, Basics tab:** every step gets a "Greeting" textarea (3 rows, `maxLength` 500, a byte counter near the limit) below Instructions or next to the step's voice. The label and hint depend on the step:
  - start step: "Greeting" — "Said word for word as soon as the call connects. Leave empty to let the caller speak first.";
  - other steps: "Greeting" — "Said word for word when a call is transferred here, in this step's voice. Leave empty to let the model greet the caller.".
- **Flow settings** has no greeting group (it is per step now).
- **First run, step 2 ("flow")** gets "First thing it says", which sets the start step's greeting; its copy loses "no greeting field".
- **Step node on the canvas:** unchanged. A step with a greeting is not marked specially (YAGNI).
- **Publish dialog's changes list:** `<step>: greeting changed` (`diff.ts`, with the per-step settings).
- **Editor validation** mirrors the server (`model.ts`): over-length (by bytes) is an error in the step's issues and blocks Publish with the reason.

### 5. Error handling and limits

- Invalid greeting at publish: `400 invalid_request` with the message (`step "a" greeting must be at most 500 bytes`, `… must be text`), nothing published; the same for a draft token request.
- Readiness is unchanged; a greeting adds no readiness reason.
- Resource bounds: one string per step per version; at most one greeting job per call opening and one per idle handoff (handoffs are already capped, default 10 per call); no new queue, map or goroutine that grows with traffic.

### 6. Testing

- **Server unit:** step config validation (empty, trimmed, 500-byte boundary, multibyte over the byte limit, control characters, wrong type, accepted on a non-start step, flat form); config hash changes with the greeting; `StepSpec.Greeting` carried from the config.
- **Go e2e (echo provider, real WebSocket calls, the routed-flow rig in `server/flow_e2e_test.go`):**
  - opening: a flow with a greeting speaks first and it is turn 1; the next model reply sees it in history; interrupting it records the heard part and marks the turn interrupted; a flow without a greeting has no extra turn;
  - handoff: after the route to a step with a greeting, the exact greeting is spoken in the new step's voice (the voice label), it replaces the model-written transfer greeting (the model is not called for it: no transfer message reaches the model), and the previous agent's note is in the history before it; a step without a greeting still gets today's transfer greeting; a turn-driven flip (the caller speaks first) plays no greeting and answers the caller; a route back to the start step does not replay the opening greeting;
  - failure: an unbuildable voice (`brokenvoice`) skips the greeting, the call carries on, `voice_greeting_failed_total` increments;
  - a draft call plays the draft's greetings while the published flow has none.
- **UI (vitest):** Basics field for a start step and a non-start step (labels, hints, byte counter, limit), `toPayload` keeps it on the right step and in the flat form, First run step 2 sends it, the Publish dialog lists the change.
- **Browser (Playwright, echo):** set an opening greeting and a Sales greeting in the designer, Test call (draft): the opening greeting appears before any typed message; the transfer to Sales shows Sales' exact greeting; publish; a second call greets; clearing a greeting removes it.
- **Gate:** `go test -race ./...`, `make test-postgres` (no SQL changes expected), `npx vitest run && tsc`, `make e2e-echo`, independent Opus review per AGENTS.md.

### 7. Documentation

`docs/api/v1.md` (the `greeting` step field, 500 bytes, spoken verbatim, interruptible, recorded as a turn; handoff behavior: idle flips only, replaces the transfer greeting, the note is kept), `docs/operations_runbook.md` (rollback note; the failure metric), `docs/quickstart.md` if it lists config keys, the console spec (S5 done; First run step 2; the step panel).

## Risks and open points

- **Assistant-path frames** are the one technical unknown; the plan's first task is a failing test that pins the observable behavior (spoken, recorded, interruptible), then the minimal injection. If the assistant path cannot be driven without a model, the fallback is a direct TTS unit with explicit history and turn-row writes, and this spec is updated before proceeding.
- **Injecting inside `Greet`**: `Greet` runs inside the trigger's inject callback with the handoff lock released but on the trigger path; speaking from there must not block (it only queues a job) and must use the post-flip generation. The handoff e2e covers it; a deadlock-style test with a pending second transfer guards it.
- **Turn flip skips the greeting** by design; an operator who always wants a greeting on arrival must accept that a caller who speaks during the build gap is answered instead.
- **First-audio latency** of the opening depends on TTS warm-up; measuring time-to-first-greeting-audio is a follow-up, not part of this spec.
- **Web client transcript:** whether a greeting appears as a bot message in the stock Pipecat client depends on which RTVI frames the assistant path emits for model text; the Go e2e asserts it; if it does not, the injection must emit them too.
