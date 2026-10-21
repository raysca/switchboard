> **Historical** (moved to docs/history by Phase 8, 2026-10-21): written for the design at the time; it may name packages, files, endpoints and targets that no longer exist. Paths inside are as written then; see docs/history/README.md.

# Per-flow call behavior: silent-caller handling and audio — Design

Status: draft for review. Two settings groups that exist in the library and the server's YAML but not in the flow config, so a flow cannot set them and the editor cannot show them: **B1** what happens when the caller goes quiet, **B3** audio (noise filtering, output gain, background ambience). Both become optional flow settings with editor controls. (The missing controls for settings the server already accepts are a separate, UI-only plan: `docs/superpowers/plans/2026-10-15-flow-settings-ui-gaps.md`.)

## Goal

An operator can say, per flow and from the console, "if the caller stays silent for 8 s, ask once, then end the call politely" and "clean up the line, lower the bot's volume 3 dB and play office ambience under the voice", without editing server YAML. A flow that sets none of this behaves exactly as today (the server's YAML values apply).

## Agreed scope (from you)

B1 (silent-caller behavior) and B3 (audio and ambience). Not in this spec: B2 (turn taking and interruptions), the bot-stuck watchdog (`idle.bot_*`), DTMF, providers' infrastructure settings.

## Today

- **Silent caller:** `server/idle.go` `IdleConfig` (YAML `idle.*`) is server-wide, with the note "per-flow idle settings belong in the flow config (not built yet)". The registry builds one `core.IdleController` per call from it (`cfg.Idle.stage(...)`, `server/registry.go`). User-idle fields: `user_timeout`, `max_prompts`, `prompt`, `prompt_mode` (`llm`: the model is told to say something like it; `say`: spoken verbatim), `goodbye` (spoken, no model, before hanging up). Bot-idle fields (`bot_timeout`, `bot_max_stuck`, `bot_fallback`) are a failure watchdog, not behavior.
- **Audio:** `server/config.go` `audio.*` (YAML): `input_filters` (`denoise`) + `denoise_strength` (0..1, default 0.5), `output_gain_db` (−30..+6, 0 = no stage), `background` (`file`, `gain_db` −60..0 default −30, `loop` default true), `cues` (named sounds for `route_cue`). `docs/audio-filters.md`: server-wide only, "per-flow filter settings are left for later"; background mixing adds continuous WebSocket traffic (about 48 kB/s at 24 kHz) per call; recordings keep the raw inbound audio.
- Per-flow settings live on the **start step** of the flow config (`stt`, `vad_stop_ms`, `recording`, `context`, `async_tools`, rejected on other steps by `validateCallLevel`), validated in `server/flow/config.go`, part of the version hash, and shown in the editor's Flow settings panel.

## Defaults, Basic and Advanced, and how it is proven

### Sensible defaults

An option left alone writes nothing to the flow; the **server's built-in default** applies and the editor shows it. The built-in defaults are chosen to be safe and useful with no configuration:

| Option | Built-in default | Why |
|---|---|---|
| Quiet-caller handling | **on**: ask after 15 s of silence, up to 2 times, then end the call | A call nobody is on costs money until `max_call_duration` (30 min). Today the default is off. |
| Ask how | the model words it (`llm`) | Sounds natural and fits the flow's persona. |
| What to say | "Are you still there?" | The existing built-in prompt. |
| Goodbye before hanging up | "I'll end the call now. Goodbye." | Today the default is empty (a silent hang-up). |
| Noise reduction | **off** | Do not alter the caller's audio unless asked. |
| Voice volume | 0 dB (no gain stage) | No change. |
| Background sound | none | It costs continuous bandwidth per call. |

YAML can still change every default (`idle.user_timeout: 0` turns quiet-caller handling off server-wide), so an operator who wants today's behavior keeps it with one line. The built-in change is deliberate and listed as Ruling 6.

### Basic and Advanced

Only what a typical flow needs is visible; everything else is under the collapsed **Advanced** group of Flow settings (opened automatically when a stored value in it is set or has an error, see the UI plan):

- **Basic:** *If the caller goes quiet*: one checkbox `End the call if the caller stays silent` (on by default) and its wait in seconds (placeholder: the server default).
- **Advanced:** how many times to ask (1–10), what to say and how (the model words it / exactly as written), the goodbye, noise reduction (and strength), voice volume, background sound (and its volume and repeat). Each shows its default as a placeholder or hint.

### Explain-this help

Every new field in the quiet-caller and audio groups carries the same `?` button as the other obscure settings (spec of the mechanism: the UI plan's "Setting help"), with copy in the one `settingCopy.ts`: `idle.user_timeout_ms` (what "silence" means: the time since the agent stopped speaking, prompts every wait, the call ends one wait after the last ask; default 15 s), `idle.max_prompts`, `idle.prompt` and `idle.prompt_mode` (model-worded vs exactly as written, and that exact wording costs no model call), `idle.goodbye`, `audio.denoise` (what it removes: steady hum and hiss, not other voices; costs about 0.1% of one core per call; recordings keep the raw audio), `audio.output_gain_db` (positive values can clip), `audio.background` (named server sounds only; adds continuous audio traffic to every call), its gain and loop. The same guards apply (no unreferenced or missing entries).

### Proven end to end on the echo providers

Each option group has a browser e2e on the echo providers (`make e2e-echo`: free, deterministic, no credentials):

- **Quiet caller:** a flow with `prompt_mode: "say"` (an exact prompt, spoken by the echo TTS) and a short wait: after the silence the transcript shows the exact prompt text (the model is not involved, so the assertion is deterministic), then the goodbye and the end of the call.
- **Audio:** a call with `output_gain_db` and a server background sound connects, the echo TTS audio reaches the client (frames with energy) and the client sees continuous traffic during a silence only when a background is set.
- Wording-dependent behavior is pinned by the Go e2e on echo; no live-provider run is required.

## Design

### 1. Config

Two optional objects on the start step (call-level), both rejected on other steps and in flat configs carried at the top level:

```json
"idle":  { "user_timeout_ms": 8000, "max_prompts": 2, "prompt": "Are you still there?", "prompt_mode": "llm", "goodbye": "I'll let you go. Goodbye." },
"audio": { "denoise": { "strength": 0.5 }, "output_gain_db": -3, "background": { "name": "office", "gain_db": -30, "loop": true } }
```

- **`idle`** (silent caller only): `user_timeout_ms` 0 or 1000–300000 (0 = off for this flow even when the server default is on); `max_prompts` 1–10 (required when the timeout is set; default 2); `prompt` at most 200 bytes; `prompt_mode` `llm` | `say` (default `llm`); `goodbye` at most 200 bytes. The same character rules as `greeting` (no control characters other than newline and tab). Units are milliseconds in the config (the API is milliseconds elsewhere: `vad_stop_ms`, `stt_p99_ms`) while the YAML keeps durations.
- **`audio`**: `denoise` present = denoise on (an object so strength can be set; `strength` 0..1, default 0.5); `output_gain_db` −30..+6; `background` = `{name, gain_db, loop}` where `name` must be a **server-configured sound**, never a path (Ruling 2), `gain_db` −60..0 (default −30), `loop` default true.
- A **missing** key means "use the server default"; a present key replaces it for that call (not a merge field by field: `idle` is one unit, `audio` has three independent members that each override only when present).
- Additive, schema stays v2, part of the config hash; an older server rejects the unknown keys (rollback note in the runbook, as for `greeting`).

### 2. Server behavior

- `CallSpec` gains `Idle *flow.IdleSettings` and `Audio *flow.AudioSettings` from the start step's config. `BuildCallPipeline` resolves each against the server config: `idleFor(call) = flow value if present else cfg.Idle`; the bot-idle watchdog (`bot_*`) always comes from the server config. The same resolution for the audio chain (`denoise`), the output gain stage and the background mixer.
- **Denoise availability:** the flow may ask for `denoise` only if the server knows the filter (`knownInputFilters`); that is always true today (pure Go), so there is no readiness reason, but the resolver keeps the check so a future non-bundled filter produces a readiness reason `unsupported_audio_filter` instead of a failed call.
- **Background sounds** come from a server catalog, `audio.backgrounds: {name: {file, gain_db, loop}}` in YAML (like `audio.cues`). The existing single `audio.background` becomes a default: `audio.background_default: <name>` (nothing is deployed, so the old key is replaced, not kept). A flow naming an unknown sound is rejected at publish (`400 invalid_request` naming it and listing the available names). If the sound is removed later the flow stays ready and its calls play the server default (or none): taking a flow offline over an ambience sound would be worse than losing the sound. `{"name": ""}` means "no background" even when a server default is set.
- **Safety bounds stay with the operator:** a server-wide ceiling `audio.allow_flow_background: true|false` (default false in production, true in development) because a background sound costs continuous bandwidth per call; when false, flows that ask for one are rejected at publish. The idle and gain ranges above are the only other bounds; `max_call_duration` still ends every call.
- The idle controller's existing rules are unchanged (prompts skipped while a tool runs, the caller is muted, a reply is in progress or either side speaks; the call ends one `user_timeout` after the last prompt). `prompt_mode: say` speaks the prompt through TTS without the model, as today.
- **Draft test calls** use the draft's `idle` and `audio` (they are built from the draft snapshot).

### 3. API

- `GET /api/v1/server-config` gains, read-only: `audio.backgrounds` (names only, never paths), `audio.allow_flow_background`, and the server's idle defaults (`user_timeout_ms`, `max_prompts`, `prompt_mode`) so the editor can show "server default: …". No file paths or secrets leave the server.
- Flow create/update/draft validate the new objects (`400 invalid_request` with the field named); no new endpoint.

### 4. Console

- **Flow settings** (Basic: the quiet-caller checkbox and wait; Advanced: everything below). Two groups:
  - **If the caller goes quiet (Advanced part):** `Wait this long (seconds)` (empty = server default, `0` = never), `Ask this many times` (1–10), `What to say` (textarea; hint "Said in the agent's own words by default, or exactly as written if you choose Say it exactly"), a radio `Let the model word it` / `Say it exactly`, and `Goodbye before hanging up`. Each field's placeholder shows the server default from `server-config`; a short line states the rule "the call ends one wait after the last ask".
  - **Audio:** `Reduce background noise` (checkbox + a 0–100% strength slider with a number input, default 50%), `Voice volume (dB)` (−30 to +6), `Background sound` (a select of the server's sound names plus "None", disabled with the reason when the server does not allow flow backgrounds), its volume (−60 to 0 dB), `Repeat` (checkbox).
- Editor validation mirrors the ranges above; Publish is blocked with the field named.
- The Publish dialog lists `Flow settings: silent-caller behavior changed` / `audio changed`.
- First run is unchanged (these are advanced settings).

### 5. Failure handling and limits

- Invalid values: `400 invalid_request`, nothing published. Unknown background name: the message lists the available names.
- A flow with `idle.user_timeout_ms` below the DTMF aggregate window when DTMF aggregation is on: rejected at publish with the server's existing reason (`idle.user_timeout must exceed dtmf.aggregate_timeout`), resolved against the effective values.
- Resource bounds: three strings of at most 200 bytes, a few numbers; no new queue or goroutine per call beyond the stages the YAML already creates; the background mixer's cost is the existing one.

### 6. Testing

- **`server/flow` unit:** every range and byte limit (boundary values), control characters, wrong types, call-level only (rejected on a non-start step), flat form, config hash changes, missing keys read as nil.
- **Server:** resolution order (flow overrides server; absent uses server) for idle and audio, unit-tested without a call; unknown background name rejected at publish and `not_ready` when removed; `allow_flow_background: false` rejects; the dtmf rule.
- **Go e2e (echo, real WebSocket calls):** a flow with `idle.user_timeout_ms: 400, max_prompts: 1, prompt_mode: say` gets the exact prompt after the silence and then the goodbye and a hangup, while the same server with another flow gets neither; a flow with `output_gain_db` and a background sound produces audio frames with the expected extra traffic (the existing mixer tests' approach), and a flow without them produces none.
- **UI (vitest):** the two groups, placeholders from `server-config`, ranges, units, clearing a field removes its key, round trip of stored values; **browser (Playwright, echo):** set a quiet-caller wait in the designer, Test call (draft) and hear the prompt after the silence; publish; the stored config carries the values.
- **Gate:** `go test -race ./...`, `make test-postgres` (no SQL), `npx vitest run && tsc`, `make e2e-echo`, independent Opus review (bugs, bloat, dead code).

### 7. Documentation

`docs/api/v1.md` (the `idle` and `audio` objects, ranges, defaults, server-default fallback, errors, `server-config` fields), `docs/audio-filters.md` (per-flow settings; the sound catalog), `docs/operations_runbook.md` (new YAML keys, rollback note), the console spec.

## Rulings made while writing this (overrule any)

1. **Flow replaces server default per object member**, not a deep merge of `idle`: a flow that sets `idle` sets all of its user-idle behavior (the missing members take built-in defaults, not the server's YAML), so what the editor shows is what the call does. `audio` members override independently. Cost if wrong: a flow that wants only a different prompt must also state the wait; the editor pre-fills the server default so this is one click.
2. **Backgrounds are named server sounds, never paths**: a flow must not make the server read an arbitrary file.
3. **Nothing deployed, so no compatibility:** the single `audio.background` key is replaced by `audio.backgrounds` + `background_default`.
4. **Bot-idle stays server-wide:** it is an operational safety net, not a conversation choice.
5. **Units:** milliseconds in flow config, seconds in the editor field (the editor converts).
6. **Quiet-caller handling is on by default** (15 s, 2 asks, then end the call, with a spoken goodbye), in every profile, instead of off. Cost if wrong: an operator who relied on calls staying open through long silences must set `idle.user_timeout: 0` (server) or `idle.user_timeout_ms: 0` (flow). Nothing is deployed, so no existing behavior is broken.
7. **E2E on the echo providers only** (exact `say` prompt, audio energy, acceptance of the config); no live-provider run.

## Open points

- Whether on-by-default quiet-caller handling should also apply to the **dev profile** (it does here; `make dev` calls would then end after 15 s of silence plus asks).
- Whether a flow-level `user_timeout_ms: 0` ("never hang up on silence") should be allowed in production: this spec allows it (the call duration cap still applies); a server flag could forbid it.
- Denoise strength is a single knob; per-band tuning is out of scope.
- A background sound uploaded from the console (instead of a server-configured name) would need file storage and review; out of scope.
