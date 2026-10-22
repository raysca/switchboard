# The `silence` provider: text-only flows

Status: accepted, implemented with this change. A spike on a real model (xAI Grok) showed that a flow with
no speech stages works end to end.

## Problem

A flow must name a speech-to-text provider, a model and a text-to-speech voice before it can take calls
(`missing_stt`, `missing_tts`, `flow_not_attached`). A client that listens and speaks in the browser (the
Web Speech API, a screen reader, a chat widget) sends typed turns (RTVI `send-text`) and renders the reply
text (`bot-llm-text`); it has no use for server-side speech, but the flow cannot be ready without it.

## Decision

Add a provider `silence` ("Silence (text only)"). It is an ordinary provider with an STT and a TTS service,
so flow validation, readiness, the editor, failover and the API need no new rule. A text-only flow names it
for both speech stages and a real provider for the model.

Not chosen: a "none" value for a stage (every validator, the editor and the call builder would need a new
case), and reusing `echo` (it is labelled simulated, keeps its pacing, shares its breakers and metrics, and
the docs say never to put callers on it).

## Behaviour

- **Catalog** (`server/catalog/data/silence.yaml`): services `stt` (model `text`, language `en-US`) and `tts`
  (voice `none`), keyless, closed lists. There is no `llm` service. It is always detected, like `echo`.
- **STT** never transcribes audio. Typed turns bypass speech recognition, so they work as they do on any flow.
- **TTS** returns one 20 ms frame of silence per unit, with no delay, so the turn's speaking events open and
  close at once and the reply costs no audio time.
- **Idle default.** A caller who types is not silent. When a call's STT provider is `silence` and the flow sets
  no `idle` settings, the quiet-caller prompts and hang-up are off. A flow that sets `idle` explicitly gets
  exactly what it sets. (The server's bot-idle watchdog is unchanged.)
- **Resilience.** `silence` has its own circuit breakers and gauges (`provider="silence"`, endpoint `text`),
  apart from `echo`'s. Its stages are the real Google processors on tiny in-process backends, like `echo`,
  so splitting, retries, usage and tracing are the shared ones.
- **Console.** The Test call panel and the first-run wizard do not ask for a microphone when the call's STT
  provider is `silence`; "Push to talk", "Mute" and the mic status are hidden, and the message box is the way
  to talk. Calls with any other STT provider are unchanged.

## Non-goals

- No change to the wire protocol, `core/`, flow validation, readiness or the API.
- No browser speech integration in this repository: that is the client's job (the examples and
  `docs/providers.md` describe the contract).
- No audio for TTS and no placeholder words for STT, ever: use `echo` for simulated audio.

## Privacy note for operators

The browser's own recognition and synthesis are often cloud services run by the browser vendor. Text-only
flows keep audio off Switchboard, not off the vendor; say so to your users.

## Acceptance

1. A flow with `silence` for STT and TTS and a ready model provider validates as `ready`, and a typed turn
   gets a text reply with `user-transcription`, `bot-llm-*`, `bot-tts-*` and `bot-started/stopped-speaking`.
2. Such a flow stays connected past the server's idle timeout with no input; the same flow with an explicit
   `idle` setting still prompts and hangs up.
3. `silence` does not trip or share `echo`'s breakers, and its circuit gauges appear on `/metrics`.
4. The console starts a call on such a flow without requesting a microphone.
5. `make verify` and `make e2e-echo` pass.

## Test plan

Unit: the backends (a stream never yields a transcript and ends on close; synthesis is one frame, no delay,
cancellable), registration and breaker separation, the idle default and its override, the catalog entry.
Go end to end (real server and WebSocket, echo model): the typed turn, the idle default, the explicit idle.
UI: the panel and client skip the microphone for a `silence` STT provider and keep it for others.
