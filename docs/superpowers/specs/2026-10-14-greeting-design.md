# Greeting (first thing the agent says) — Design

Status: draft for review. Spec row: S5 in `2026-10-12-switchboard-console-design.md` ("Greeting / first line in config", First run step 2 "First thing it says").

## Goal

A flow can optionally have the agent speak first: a line of exact words, said the moment the call is ready, instead of waiting in silence for the caller. With no greeting configured, calls behave exactly as they do today.

## Agreed decisions

- **Exact words, not a model instruction.** The operator types the line; the agent says it verbatim. No model call, so it is instant, free and predictable.
- **The caller can interrupt it**, like any reply. No "finish first" mode and no per-flow switch.
- **Optional.** Absent or empty means no greeting.
- **Opening line only.** Greetings on handoffs between steps stay model-driven (`transferGreeting`, `server/flow_call.go`); this feature does not touch them.

Out of scope (YAGNI): templating or variables (`{{caller}}`), several greetings or random choice, a per-step greeting, a "model writes the greeting" mode, a "caller cannot interrupt" mode, per-language greetings.

## Today (what exists)

- A session is created only after the RTVI client handshake (`transport/ws/transport.go`, `OnClientReady` then `OnSessionReady` then `session.Start()`), so audio produced right after start is not lost.
- Two ways to speak without the caller going first exist: `IdleController.say` (a `SynthesisUnitFrame` straight to TTS, `core/idle.go`) and a system-notice user message that makes the model produce the line (idle check-in, transfer greeting). Neither records the spoken line the way a model reply is recorded.
- `AssistantContextProcessor` (`core/assistant_context.go`) turns model text into synthesis units, tracks what was actually delivered, commits it to the conversation history, and applies the partial-turn rule when the caller barges in. The turn recorder (`server/call.go`) writes a turn row per `AssistantMessageFrame`.
- Call-level settings (`stt`, `vad_stop_ms`, `stt_p99_ms`, `recording`, `context`, `async_tools`) live on the flow's start step and are rejected on other steps (`server/flow/definition.go` `validateCallLevel`).

## Design

### Approach: play the greeting as the model's first turn

The greeting is fed into the assistant path as if the model had said exactly those words. That gives, without new code paths: sentence splitting for TTS, delivery tracking, the partial-on-interrupt rule (only what was heard counts), the history entry (so the model will not greet again), the turn row (so it shows in Call detail and in turn counts), and the web client's bot transcript.

Rejected: sending a `SynthesisUnitFrame` straight to TTS and recording it by hand. It is simpler to trigger but duplicates the history, turn-row, transcript and partial-on-interrupt logic, which could then drift from how real replies behave.

### 1. Config

One optional string on the start step: `greeting`.

- It is a call-level setting: it joins `CALL_LEVEL` in `ui/src/routes/flows/editor/model.ts` and the list in `validateCallLevel` (the error message names it), so it is rejected on any other step and stripped from them by `toPayload`.
- In `server/flow/config.go`: `Greeting string \`json:"greeting,omitempty"\`` on `Config`, validated in `Config` validation: trimmed; at most 500 bytes (the server counts bytes, as for conditions); no control characters other than `\n`, `\t`; wrongly typed values fail as today (unknown fields and wrong types are rejected).
- Flat one-step configs carry it at the top level, like `vad_stop_ms`.
- Schema stays version 2; the field is additive. A version stored without it reads as "no greeting". An older server reading a config with it rejects it as an unknown field, so a rollback to an older binary needs the greeting removed first (called out in the runbook).
- It is part of the config hash, so changing it is a new version like any other change.

### 2. When it plays

- The server builds the call (`resolveCall` / `callSpec`). `CallSpec` gains `Greeting string` from the start step's config.
- After `session.Start()` (the output gate is open; the session exists only after the handshake) the greeting is queued once, on a session-supervised task (`session.Go`), through the assistant path described above. The exact frame sequence the assistant processor needs to open a turn, take the complete text and finish it is established by the first implementation task (a spike with a failing test); it must produce the same observable result as a model reply of that text.
- It is a single, bounded job per call: it runs once, never re-queued on reconnect, and is dropped if the session is already closing.
- The model is not invoked. The first caller turn after the greeting runs the normal LLM path with the greeting already in history as the assistant's last message.
- **Failure:** if TTS or delivery fails, the greeting is skipped, the failure is logged with the session id and counted in a metric (`voice_greeting_failed_total`, one bounded label-free counter), and the call carries on as if no greeting existed. A greeting never ends a call.
- **Mute strategies:** the greeting is a bot turn like any other, so `until_first_bot_complete` (already documented as "use only with an opening greeting") now has a greeting to release on.

### 3. During and after the call

- **Barge-in:** the caller speaking over it stops it through the normal interruption path; the history keeps only the heard part (existing partial-turn semantics) and the turn row is marked interrupted.
- **Records:** the greeting is turn 1 of the call: a turn row with empty `transcript` and the greeting as `assistant_text`; counted in `turn_count`; shown in Call detail's turns and transcript; sent to the web client as a normal bot message. Per-turn latency (S9) records only the stages it measures; a greeting has no model or end-of-speech stage, so it stores `{}` unless TTS first-audio is measured on the unit.
- **Redaction and retention:** it is stored like any assistant text, so the existing redaction and retention rules apply unchanged.
- **Test calls:** a draft test call plays the draft's greeting, because the call is built from the draft snapshot (`resolveCall` takes the draft config). The Test call panel needs no new code.

### 4. Console

- **Flow settings** gets a "Greeting" group under "Listening": a labelled textarea ("First thing it says", 3 rows, `maxLength` 500) with the hint "The agent says this, word for word, as soon as the call connects. Leave it empty to let the caller speak first." A byte-length counter appears near the limit (bytes, not characters). It edits the start step's `greeting`.
- **Start step Basics** is unchanged (Listening, Model, Voice); the greeting lives in Flow settings because it is a call-level, flow-wide choice. The step panel's note for non-start steps is unchanged.
- **First run, step 2 ("flow")** gets the same optional field, "First thing it says", sent in the created flow's config. First run's copy loses "no greeting field".
- **Publish dialog's changes list** gets `Flow settings: greeting changed` (`diff.ts` `FLOW_SETTINGS`).
- Validation in the editor mirrors the server (`model.ts`): over-length shows an error and disables Publish with the reason.

### 5. Error handling and limits

- Invalid greeting at publish: `400 invalid_request` with the message (`greeting must be at most 500 bytes`, `greeting must be text`), nothing published; same for a draft token request.
- Greeting and an unusable TTS stage: readiness is unchanged (a flow without TTS is already not ready); the greeting adds no readiness reason.
- Resource bounds: one string per flow version; one queued job per call; no new queue, goroutine or map that grows with traffic.

### 6. Testing

- **Server unit:** config validation (empty, trimmed, 500-byte boundary, multibyte over the byte limit, control characters, wrong type, rejected on a non-start step, stripped by the UI payload); config hash changes with the greeting.
- **Go e2e (echo provider, real WebSocket calls, `server/flow_e2e_test.go` rig):** a flow with a greeting speaks first and it appears as turn 1; the next model reply sees it in history (the echo LLM's labelled reply or a history probe); interrupting it records the heard part and marks the turn interrupted; a flow without a greeting behaves exactly as before (no extra turn); a draft call plays the draft's greeting while the published flow has none; TTS failure (`brokenvoice`) skips the greeting and the call carries on and the failure metric increments.
- **UI (vitest):** Flow settings field (typing, byte counter, limit, hint), `toPayload` puts it only on the start step and in the flat form, First run step 2 sends it, the Publish dialog lists the change.
- **Browser (Playwright, echo):** set a greeting in Flow settings, Test call (draft): the greeting appears first in the transcript before any typed message; publish; a second call greets; clearing it removes it.
- **Gate:** `go test -race ./...`, `make test-postgres` (no SQL changes expected), `npx vitest run && tsc`, `make e2e-echo`, independent Opus review per AGENTS.md.

### 7. Documentation

`docs/api/v1.md` (the `greeting` field: call-level, start step only, 500 bytes, spoken verbatim at call start, interruptible, recorded as turn 1), `docs/operations_runbook.md` (rollback note; the failure metric), `docs/quickstart.md` if it lists config keys, the console spec (S5 done; First run step 2).

## Risks and open points

- **Frame sequence for the assistant path** is the one technical unknown; the implementation plan's first task is a failing test that pins the observable behavior (speaks, recorded, interruptible), then the minimal injection that satisfies it. If the assistant path cannot be driven without a model, the fallback is Approach B for the audio plus explicit history and turn-row writes, and this spec is updated before proceeding.
- **Latency of the first audio** depends on TTS warm-up; not a new problem (the first model reply has it too), but a greeting makes it the caller's first experience. Measurement of time-to-first-greeting-audio is a follow-up, not part of this spec.
- **Web client transcript:** whether the greeting appears as a bot message in the stock Pipecat client depends on which RTVI frames the assistant path emits for model text; the Go e2e asserts it; if it does not, the injection must emit them too.
