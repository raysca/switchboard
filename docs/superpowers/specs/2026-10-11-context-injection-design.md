# Live-Call Context Injection Design

**Date:** 2026-10-11
**Status:** approved in conversation; awaiting written-spec review.

## Intent

A backend or operator can steer a live call from outside the pipeline: add a note to the agent's context ("the customer is on the billing page", "offer the refund"), and optionally make the agent speak about it now. This is the server-side counterpart of Pipecat's `LLMMessagesAppendFrame` and `LLMRunFrame`. Success:
- a note posted to a live call is in the history the model sees on its next request, in arrival order;
- with `run`, the agent responds without waiting for the caller, but never over the caller or over its own speech;
- the caller's browser cannot use this (management key only), and cannot rewrite the system prompt through it;
- a flood of notes cannot grow history without bound or starve the call.

The user decided on 2026-10-11: server side only (no RTVI message); the first version supports **append notes** and **run the LLM now**. Replace-history, transform and set-tools are out of scope.

## Current behaviour (main at 7346e8b)

- A client can only add a user turn with RTVI `send-text`. Nothing outside the pipeline can add to a live call's history.
- `core.AsyncTrigger` is a pass-through stage before the LLM. `Deliver(AsyncToolResult)` queues an item (a finished async tool call, or a handoff flip). When the call has been idle for `Settle` (300 ms; caller not speaking, bot not speaking or replying) it runs each item's `Inject` func, then re-enters one `UserMessageFrame` (`[async tool result: <names>]`) after itself, which starts one model turn for the whole batch. The queue is capped (`MaxQueue` 8); on overflow the oldest item is injected at once without its own turn. `MaxWait` (60 s) stops a lost "stopped" event from starving delivery.
- `server/registry.go` builds the trigger only when the call is a graph call or has an async tool.
- `server/call.go` knows the call's session ID (the voice token `jti`, also the stored session ID). `CallSpec` already carries per-call hooks (for example `Graph.Commit`).
- `/api/v1/sessions` serves stored history only. Admission counts active calls but keeps no registry of them. Nothing addresses a single live call.
- A precedent for injected text: handoff's `Greet` adds a user-role message `[The caller was just transferred to you (...)...]` to the shared history.

## Design

### 1. Core: notes through the trigger

- `core.AsyncToolResult` gains `NoTurn bool`. When every item in a fired batch has `NoTurn`, `fire()` injects them and starts no model turn. If any item lacks it, the batch starts one turn as today.
- A note is an `AsyncToolResult` with `Call.Name = "context_note"`, `Inject` adding the message to history, and `NoTurn = !run`. A run-only request is a `Call.Name = "run_llm"` item whose `Inject` is a no-op and `NoTurn = false`.
- The turn-starting `UserMessageFrame` content becomes `[async tool result: context_note]` style (the existing names list), so traces show what started the turn.
- Ordering: items are injected in `Deliver` order. The existing overflow rule applies to notes too.

### 2. Notes in history

- A note is stored as a **user-role** message `[note from the operator] <content>`, the same shape handoff's Greet uses. Rationale: providers differ on system messages in mid-history, and a user-attributed message without the marker would let the operator impersonate the caller. The agent's real system prompt is untouched.
- Injection goes through the shared history's `AddMessage` under the tool loop's round lock (`injectAsync`'s rule: never while a tool round is in flight, never after close), so it respects the context budget and eviction.

### 3. Server: always-on trigger and a live-call registry

- `registry.go` builds the `AsyncTrigger` for **every** call (it is a cheap pass-through).
- `CallSpec` gains `OnInjector func(inj ContextInjector)`; the registry calls it with an injector (`Note(content string, run bool) error`, `Run() error`) bound to the call's trigger and history.
- `Server` keeps `liveCalls`: a mutex-guarded map session ID → injector, set when the call starts and deleted in the call's release func. A graph call registers once, at the entry; its injector keeps working across handoffs because it writes to the shared history and uses the call-level trigger.

### 4. API (management listener, management key only)

- `POST /api/v1/sessions/{id}/context` with `{"messages":[{"content":"..."}], "run": false}`
- `POST /api/v1/sessions/{id}/run` (no body)
- Responses: `202 {"data":{"accepted":N}}`; `400 invalid_request` (empty list, more than 8 messages, a message outside 1–4096 bytes of valid UTF-8, unknown fields); `404 not_found` (id not a live call on this instance); `409 call_closing`; `429 rate_limited`.
- Rate limit per session: a token bucket of 4 requests burst, 1 per second. History growth is also bounded by the existing queue cap and context budget.
- A voice token cannot authenticate these routes (the existing `/api/v1` boundary). Calls are per-instance: a request to another instance gets 404, which matches the session-affinity guidance in the runbook.
- Logging and traces record session id, message count and `run`, never the content.

### 5. Docs

`docs/api/v1.md` (routes, errors, limits), `docs/operations_runbook.md` (live-call control section, affinity caveat, rate limit), the parity matrix (remove the gap line, record what was and wasn't built), and a short note in `docs/quickstart.md`.

## Review focus (inputs the tests must pin)

- Notes while the caller is speaking or the bot is replying wait for idle; they never interleave with a tool round.
- A note with `run` after a caller turn already started: one turn, not two.
- The call ends while notes are queued: dropped, counted, no panic, no write after close.
- Notes for a call that has just ended return 404 or 409, not 500.
- Maximum payloads, empty content, non-UTF-8, control characters in content (stored as given; the marker prefix cannot be forged away because it is added server-side).
- A burst from a script (the 8-item queue cap, the 429 rate limit) leaves the call healthy.
- Metrics: `OnTriggered` counts these turns; the async-turn counter's help text and runbook say so.

## Out of scope

Replace-history, transform, tool changes, an RTVI client message, cross-instance routing, a UI control, persistence of notes beyond the stored turns that already record the conversation.

## Risks

- **Always-on trigger** changes pipeline composition for every call: one more pass-through stage. A test pins that a call without async tools behaves as before (frame order and timing).
- **Metric meaning:** the async-turn counter now includes operator-run turns.
- **Operator content is trusted text** placed in the model's context; a compromised management key can steer calls. This is the same trust level as editing the agent.
