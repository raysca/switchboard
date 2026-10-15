# Per-flow Call Behavior (silent caller and audio) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (inline) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A flow can set, from the console, what happens when the caller goes quiet (`idle`) and the call's audio (`audio`: noise reduction, voice volume, background sound). A flow that sets neither behaves as the server defaults say, and the built-in defaults are sensible (quiet-caller handling on).

**Architecture:** Two optional call-level objects on the start step, validated in `server/flow`, carried into `CallSpec`, and resolved against the server config when the call's pipeline is built (flow value wins, absent means server default). Background sounds are a named catalogue in server YAML (never paths). Because the background mixer is created by the transport when the connection is accepted, before the call's flow is known, the transport's `CueUnderlay` gains a background that can be set later. Publish-time checks (unknown sound, ceiling, DTMF floor) go through a small server-policy interface the flow service already has a sibling of (`Availability`). The editor gets Basic (checkbox + wait) and Advanced controls with Explain buttons, placeholders from `GET /server-config`, and Go and browser e2e on the echo providers.

**Tech Stack:** Go (modules `core`, `server`, `transport`), SQLite/Postgres (no SQL change), React 19 + TypeScript + vitest, Playwright echo suite.

**Spec:** `docs/superpowers/specs/2026-10-15-per-flow-call-behavior-design.md` (read it first; it is binding). Conventions mirror the UI plan already shipped: `docs/superpowers/plans/2026-10-15-flow-settings-ui-gaps.md` (Explain buttons via `ui/src/routes/flows/editor/SettingHelp.tsx` + `settingCopy.ts`, Basic/Advanced `<details>` in `FlowSettingsPanel.tsx`).

## Global Constraints

- Ranges (spec §1): `idle.user_timeout_ms` 0 or 1000–300000; `max_prompts` 1–10 (default 2); `prompt` and `goodbye` at most 200 bytes, same character rules as `greeting` (`validateGreeting`); `prompt_mode` `llm` | `say` (default `llm`). `audio.denoise.strength` 0..1 (default 0.5); `output_gain_db` −30..+6; `background.name` a server sound; `gain_db` −60..0 (default −30); `loop` default true.
- A **missing** key means "use the server default". `idle` replaces the server's user-idle behavior as one unit (missing members take the **built-in** defaults, not the server's YAML); `audio` members override independently. Bot-idle (`idle.bot_*`) always stays server-wide.
- Call-level: the start step only (`validateCallLevel`); a flat one-step flow carries them at the top level, like `vad_stop_ms`.
- Backgrounds are **named server sounds**, never paths. YAML: `audio.backgrounds: {name: {file, gain_db, loop}}` and `audio.background_default: <name>` replace the single `audio.background` (nothing is deployed: no compatibility). `audio.allow_flow_background` (default false; true in the dev profile) is the operator's ceiling.
- Built-in defaults change (spec Ruling 6): quiet-caller handling **on** in every profile: `user_timeout` 15 s, `max_prompts` 2, `prompt` "Are you still there?", `goodbye` "I'll end the call now. Goodbye." `idle.user_timeout: 0` turns it off server-wide.
- Every queue bounded, no goroutine per call beyond what the YAML paths already create, no lock across I/O, monotonic time, `go test -race ./...` clean (AGENTS.md). `core/` stays server-agnostic: nothing in `core` imports `server`.
- UI: tokens only, no shadows; every new control has a visible label, a hint with the unit and the default, and an Explain button whose copy is in `settingCopy.ts`; a blank field removes its key; errors name the field. Playwright locators use `exact: true` (Explain buttons share label text).
- **E2E on the echo providers only** (`make e2e-echo`); no live-provider run.

## Review Focus

1. **Server default vs flow value:** a flow with no `idle`/`audio` must behave exactly as the server config says; a flow `idle.user_timeout_ms: 0` must turn quiet-caller handling off even though the built-in default is on. Pinned in Tasks 4 and 6.
2. **Default flip blast radius:** quiet-caller handling on by default must not break existing Go or browser e2e (calls that wait or sit silent). Pinned in Task 2 (the whole suite is re-run there).
3. **Path safety:** a flow can never make the server read a file: only catalogue names; names validated at publish and re-checked at call time. Pinned in Tasks 3 and 5.
4. **Background cost ceiling:** `allow_flow_background: false` rejects a flow asking for a sound at publish **and** a stored flow asking later is ignored (not an error) at call time. Pinned in Tasks 3 and 5.
5. **Late background set:** a background set after the connection was accepted must not race the paced writer (`atomic.Pointer`, `-race` test). Pinned in Task 5.
6. **Draft test calls** use the draft's settings. Pinned in Task 6.
7. **Round trip in the editor:** stored `idle`/`audio` open showing the stored values and republish unchanged; units convert (ms in config, seconds in the editor). Pinned in Tasks 8–9.

## Rulings made while planning (overrule any)

1. **Idle is on in the dev profile too** (spec open point): one default everywhere keeps behavior predictable; `make dev` users who want silence can set `idle.user_timeout: 0`.
2. **A flow may set `user_timeout_ms: 0`** in production (spec open point): the call-duration cap still ends every call.
3. **Server policy at publish** is a new small interface `flow.Policy` (backgrounds, ceiling, minimum idle timeout) next to `Availability`, implemented in `server/` — not more methods on `Availability`, whose job is provider availability.
4. **Readiness (revised after review):** an unknown sound at publish is `400 invalid_request`; a sound removed later, or a ceiling turned off later, does **not** take the flow offline (calls play the server default, with a log warning); only a wait that no longer exceeds the DTMF window does (`idle_timeout_too_short`). `{"name":""}` is an explicit "no background".
5. **No new endpoint:** the editor reads placeholders from the existing `GET /server-config`.

---

### Task 1: The flow config carries `idle` and `audio`

**Files:**
- Modify: `server/flow/config.go` (types, `DecodeConfig`), `server/flow/definition.go` (`validateCallLevel`, the flat/steps (de)serialisation if it lists call-level keys — read it)
- Test: `server/flow/config_test.go`, `server/flow/definition_test.go`

**Interfaces:**
- Produces: `type IdleSettings struct { UserTimeoutMS int; MaxPrompts int; Prompt string; PromptMode string; Goodbye string }` (JSON `user_timeout_ms,max_prompts,prompt,prompt_mode,goodbye`, all `omitempty` except `user_timeout_ms` which must serialise `0`: use `*int`/`omitzero` so "present with 0" differs from "absent"); `type AudioSettings struct { Denoise *DenoiseSettings; OutputGainDB *float64; Background *BackgroundSettings }`; `DenoiseSettings{Strength *float64}`; `BackgroundSettings{Name string; GainDB *float64; Loop *bool}`; `Config.Idle *IdleSettings` (`json:"idle,omitempty"`) and `Config.Audio *AudioSettings` (`json:"audio,omitempty"`). Pointers: absent is nil.

- [ ] **Step 1: Write the failing tests.** Boundary tables for every range and byte limit (`user_timeout_ms` 999, 1000, 300000, 300001, 0; `max_prompts` 0 when a timeout is set, 1, 10, 11; `prompt`/`goodbye` 200 and 201 bytes, 100 two-byte characters, control characters, unknown `prompt_mode`; `strength` −0.1, 0, 1, 1.1, NaN-by-JSON-absent; `output_gain_db` −30.1, −30, 6, 6.1; `gain_db` −60.1, −60, 0, 0.1; background `name` empty or a path-like name `../x`, `a/b`, a name over 64 bytes or not matching `^[a-z0-9][a-z0-9_-]*$`); wrong JSON types; unknown member names rejected (`DisallowUnknownFields` already); an **empty** `idle: {}` is rejected (it would mean nothing) and `max_prompts` without a timeout is allowed only when timeout is 0 is ignored (decide: a timeout of 0 with other members set is accepted and means "off"); call-level only (the same object on a non-start step → `invalid`); flat and steps forms both round-trip; the config hash changes when a member changes and does **not** change when an absent key stays absent (hash stability for existing flows: assert an old fixture's hash is unchanged).
- [ ] **Step 2: Run to see them fail.** `cd server && go test ./flow/ -run 'Idle|Audio|CallLevel' -count=1` — Expected: FAIL.
- [ ] **Step 3: Implement.** Types above; `validateIdle`/`validateAudio` called from `DecodeConfig` after `validateGreeting` (reuse its character rules via a shared helper `validateSpokenText(field, s, max)`); wrap errors with `ErrInvalidConfig` naming the field (`idle.prompt exceeds 200 bytes`). Extend the non-start-step check in `validateCallLevel` and its message list. Keep `Config` comparable where `Recording != (RecordingConfig{})` is used (compare pointers with `!= nil`).
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race ./flow/ -count=1` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server/flow): idle and audio settings on the start step`.

---

### Task 2: Server config: sound catalogue, ceiling, quiet-caller on by default

**Files:**
- Modify: `server/config.go` (`Audio` struct: drop `Background`, add `Backgrounds map[string]BackgroundConfig`, `BackgroundDefault string`, `AllowFlowBackground bool`), `server/config_defaults.go` (idle defaults; dev profile `AllowFlowBackground = true`), `server/config_validate.go`, `server/audiofilter.go` (`validateAudio`, `loadBackground` → `loadBackgrounds`), `server/server.go` (load), `server/idle.go` (comment), the example YAML under `examples/` and `docs/audio-filters.md`
- Test: `server/audiofilter_test.go`, `server/config_defaults_test.go`, `server/idle_test.go`, plus every test the default flip breaks

**Interfaces:**
- Produces: `type BackgroundConfig struct { File string; GainDB *float64; Loop *bool }` (reuse `CueConfig` if the shapes are identical — one type); `loadBackgrounds(cfg *Config, rate int) (map[string]func() ws.Underlay, error)` returning a per-call cursor factory by name; `Server.backgrounds map[string]func() ws.Underlay`.

- [ ] **Step 1: Write the failing tests.** `validateAudio`: a backgrounds map with at most 8 entries (same bound as cues), names match the cue name rule, file required, `gain_db` −60..0, `background_default` must name an existing entry, the old `audio.background` YAML key is an **unknown field** error (strict decode) so a stale file fails loudly; `loadBackgrounds` loads a real WAV (reuse the existing temp-WAV helper from `audiofilter_test.go`) and fails naming `audio.backgrounds.<name>.file` for a missing file. Defaults: `cfg.Idle.UserTimeout == 15s`, `MaxPrompts == 2`, `Prompt == "Are you still there?"`, `Goodbye == "I'll end the call now. Goodbye."`; `AllowFlowBackground` true only for the dev profile; `idle.user_timeout: 0` in YAML really turns it off (`enabled()` false when bot timeout is also 0).
- [ ] **Step 2: Run to see them fail.** `cd server && go test -run 'Audio|Defaults|Idle' -count=1 .` — Expected: FAIL.
- [ ] **Step 3: Implement.** Replace the single background by the catalogue; the server-wide default background is `backgrounds[background_default]` (nil when empty) and is what `ws.ServerConfig.Underlay` gets when no flow overrides (Task 5 changes who sets it). Apply the idle defaults in `config_defaults.go`. Keep `IdleConfig.enabled()` as is.
- [ ] **Step 4: Run the whole server suite and fix the blast radius.** `cd server && go test -race -count=1 ./... 2>&1 | tail -40` — tests that dial a call and stay silent or construct `Config` by hand now get an idle controller: set `UserTimeout = 0` in those test configs (a helper `noIdle(cfg)` next to `testConfig`), do not weaken what they assert. Then `make e2e-fake`'s specs are checked in Task 10 (a 15 s silence is not expected in them; note any that wait that long).
- [ ] **Step 5: Commit** `feat(server): named background sounds, a flow-background ceiling, and quiet-caller handling on by default`.

---

### Task 3: Publish-time policy and readiness

**Files:**
- Modify: `server/flow/config.go` (`Policy` interface, `Problems`), `server/flow/service.go` (use it in create/update/draft validation and `flowReadiness`), `server/providers.go` or a new `server/flowpolicy.go` (implementation), `server/server.go` (wire it where `Availability` is wired)
- Test: `server/flow/service_test.go`, `server/flowpolicy_test.go`

**Interfaces:**
- Produces: `type Policy interface { Backgrounds() []string; AllowFlowBackground() bool; MinUserTimeout() time.Duration }` (names sorted; `MinUserTimeout` is the DTMF floor `max(dtmf.aggregate_timeout, 2s)` when `dtmf.aggregate` is on, else 0); `Service` takes it (a nil policy means "no server knowledge": unknown names are accepted only in unit tests that pass nil — document that); `Definition.PolicyProblems(p Policy) []string` returning machine reasons, and a publish-time error helper returning `400`-style text naming the field and the available sounds.

- [ ] **Step 1: Write the failing tests.** Publishing a flow with `audio.background.name: "nope"` → `ErrInvalidConfig`-class error whose message names `audio.background.name` and lists the available names; a known name while `AllowFlowBackground() == false` → rejected naming `audio.allow_flow_background`; a flow `idle.user_timeout_ms` below `MinUserTimeout()` (and non-zero) → rejected with the server's existing wording (`must exceed dtmf.aggregate_timeout`); the same flow with the policy permitting it is accepted. Readiness of a stored version: sound removed → `not_ready` with `missing_audio_background`; ceiling off → `audio_background_not_allowed`; both appear in `Readiness.Reasons`. A draft (`ResolveDraft`) gets the same checks.
- [ ] **Step 2: Run to see them fail.** `cd server && go test ./flow/ -run 'Policy|Readiness|Background' -count=1` — Expected: FAIL.
- [ ] **Step 3: Implement.** The server implementation reads `cfg.Audio.Backgrounds`, `cfg.Audio.AllowFlowBackground`, `cfg.DTMF`. Reasons are added to `parsed.Problems` output so the existing readiness plumbing carries them; the UI will show the raw reason strings the same way it shows `missing_credential` (check `ui` for the reasons map in Task 8).
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 ./... 2>&1 | tail` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): publish-time and readiness checks for flow sounds and silence timeouts`.

---

### Task 4: Per-call resolution of idle, noise reduction and gain

**Files:**
- Modify: `server/call.go` (`CallSpec.Idle`, `CallSpec.Audio` filled from the start step's config), `server/idle.go` (`resolveIdle`), `server/audiofilter.go` (`buildInputFilter`, `buildOutputGain` take the per-call settings), `server/registry.go` (use them; the `cfg.Idle.enabled()` at line ~555 and `cfg.Idle.stage(...)` at ~761)
- Test: `server/idle_test.go`, `server/audiofilter_test.go`, `server/call_spec_test.go` (or the nearest existing test file for `BuildCallPipeline`)

**Interfaces:**
- Produces: `func (c IdleConfig) forCall(f *flow.IdleSettings) IdleConfig` — returns the server config with the user-idle members replaced by the flow's (built-in defaults for missing members; `UserTimeoutMS == 0` present means off) and the bot-idle members untouched; `func effectiveAudio(cfg *Config, f *flow.AudioSettings) (denoise *float64, gainDB float64)`; `CallSpec.Idle *flow.IdleSettings`, `CallSpec.Audio *flow.AudioSettings`.

- [ ] **Step 1: Write the failing tests.** Table for `forCall`: nil flow value → the server config unchanged; flow `{user_timeout_ms: 8000}` → 8 s, 2 prompts, built-in prompt/goodbye, `llm` mode, and the server's `bot_*` intact; flow `{user_timeout_ms: 0}` → user idle off while `bot_timeout` stays; `prompt_mode: say` → `Say` true. `effectiveAudio`: nil → server `input_filters`/`denoise_strength`/`output_gain_db`; flow denoise `{}` → on at 0.5; flow `strength 0.8` → 0.8; flow `output_gain_db: 0` explicit → no gain stage even if the server has −6 (explicit 0 overrides); server filters on and flow silent → server behavior. A pipeline-level test: build a call pipeline with a flow idle value and assert the idle controller is present/absent (use the existing processor-list helpers in `idle_test.go`). Idle validation of the resolved config reuses `IdleConfig.validate`: a resolved timeout below the DTMF floor fails the call build with a clear error (it can only happen if the policy changed after publish).
- [ ] **Step 2: Run to see them fail.** `cd server && go test -run 'ForCall|EffectiveAudio|CallSpec' -count=1 .` — Expected: FAIL.
- [ ] **Step 3: Implement.** `BuildCallSpec` copies `entry.Config.Idle/Audio`; `BuildCallPipeline` computes `idle := cfg.Idle.forCall(call.Idle)` once and uses it for both the `enabled()` check and `stage(...)`; `buildInputFilter(cfg, spec.Audio, obs)` and `buildOutputGain(cfg, spec.Audio)`.
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 . 2>&1 | tail` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): a call's quiet-caller handling, noise reduction and volume come from its flow`.

---

### Task 5: Per-flow background sound in the transport

**Files:**
- Modify: `transport/ws/cue.go` (`CueUnderlay.bg` becomes `atomic.Pointer`-backed with `SetBackground(Underlay)`), `transport/ws/transport.go` (always create a `CueUnderlay` when `ServerConfig.Cues` **or** a new `ServerConfig.FlowBackgrounds` is true; expose `Connection.SetBackground`), `server/server.go` (`onSessionReady` sets the call's background after the flow is resolved), `server/call.go` (resolve `CallSpec.Background`)
- Test: `transport/ws/cue_test.go` (race test), `server/background_test.go`

**Interfaces:**
- Consumes: `Server.backgrounds` (Task 2), `flow.AudioSettings.Background` (Task 1), `Policy.AllowFlowBackground()` (Task 3).
- Produces: `(*ws.CueUnderlay).SetBackground(Underlay)`; `(*ws.Connection).SetBackground(Underlay)`; `func (s *Server) backgroundFor(f *flow.AudioSettings) func() ws.Underlay` — the sound to play: the flow's named sound if it exists **and** the ceiling allows, else the server default (`backgrounds[background_default]`), else nil. A name that is missing or not allowed is logged once and ignored at call time (publish-time and readiness already refuse it; Review Focus 4).

- [ ] **Step 1: Write the failing tests.** `transport/ws`: `SetBackground` concurrently with `Mix` and `Active` under `-race` (a goroutine mixing 1,000 frames while another swaps the background); `Rate()` follows the current background; setting nil stops it; a connection with `FlowBackgrounds` but no background mixes nothing and reports `Active() == false` (no ambient frames, no traffic). `server`: `backgroundFor` table (flow name wins; unknown name → default; ceiling off → default; nothing configured → nil; the flow's `gain_db`/`loop` override the catalogue entry's — for that the factory must be built per call: `mixer.LoadAsset` returns an asset with a cursor of a given loop; apply the gain once per asset at load, so a per-flow `gain_db` means `loadBackgrounds` keeps the **raw** asset and the gain is applied by the cursor: read `mixer` to see which is cheaper; if gain is baked in at load, load one asset per (name, gain) lazily under a `sync.Map` with a bound of 8 names × distinct gains seen, evicting nothing — a flow can only choose from −60..0 in whole dB, so cap at 61 gains per name and document it).
- [ ] **Step 2: Run to see them fail.** `cd transport && go test -race ./ws/ -run Background -count=1; cd ../server && go test -run Background -count=1 .` — Expected: FAIL.
- [ ] **Step 3: Implement.** `onSessionReady` already knows the call (read it): after the flow's `CallSpec` is built, call `conn.SetBackground(s.backgroundFor(spec.Audio)())` when the factory is non-nil. The server-wide default is no longer passed as `wsCfg.Underlay`; the connection starts with no background and `onSessionReady` always sets one (the default or the flow's), so a call that has none costs nothing. `wsCfg.Cues` stays true when cues are configured; set `FlowBackgrounds` when the catalogue is non-empty.
- [ ] **Step 4: Run to see them pass.** `cd transport && go test -race ./... && cd ../server && go test -race -count=1 . 2>&1 | tail` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server,transport): a call plays its flow's background sound`.

---

### Task 6: Go end-to-end on the echo providers

**Files:**
- Create: `server/call_behavior_e2e_test.go`
- Modify: `Makefile` (`test-e2e-go -run` pattern gains `CallBehavior`)

**Interfaces:**
- Consumes: the e2e rig (`newFlowRigE2E`, `r.dial`, `c.waitReplies`, `eventually`, the `greetingFlow` style config helper), a test WAV helper from `audiofilter_test.go`.

- [ ] **Step 1: Write the failing tests (read `server/idle_test.go` `newIdleE2E` and `greeting_e2e_test.go` first).**
  - `TestAFlowsQuietCallerSettingsPromptThenEndTheCall`: a flow with `idle: {user_timeout_ms: 1000, max_prompts: 1, prompt: "Still there?", prompt_mode: "say", goodbye: "Goodbye."}` → after about a second of silence the exact text `Still there?` is spoken (no model call: the model probe sees no extra request), then `Goodbye.`, then the call ends with the idle end reason; the same server with **another flow without `idle`** and the server default set to 60 s gets neither within 3 s.
  - `TestAFlowCanSwitchQuietCallerHandlingOff`: server default on (1 s), flow `idle: {user_timeout_ms: 0}` → silent for 3 s, nothing is said and the call stays up.
  - `TestAFlowWithoutIdleUsesTheServerDefault`: the server config's own `user_timeout` applies.
  - `TestAFlowsBackgroundSoundIsHeardAndAnotherFlowsIsNot`: the catalogue has `office` (a short generated WAV of a constant tone), ceiling on; a flow naming it produces ambient audio frames during a 500 ms silence (count frames with energy above a threshold at the client; reuse the existing mixer e2e approach in `server` tests), a flow without it produces none; `output_gain_db: -20` lowers the energy of the spoken reply by about 20 dB against the same reply without it (assert a ratio within a tolerance, not exact samples).
  - `TestADraftCallUsesTheDraftsQuietCallerSettings`: a draft (`draft.config`) with a short `idle` prompts while the published flow does not.
- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -tags xaispeech -run CallBehavior .` (use the tags from the Makefile's `GOTAGS`) — Expected: FAIL.
- [ ] **Step 3: Implement** only what the failures show missing (the rig helpers for silence and frame energy if absent: keep them test-only).
- [ ] **Step 4: Run to see them pass,** then `make test-e2e-go` — Expected: PASS.
- [ ] **Step 5: Commit** `test(server): quiet-caller and audio settings run per flow on the echo providers`.

---

### Task 7: `server-config` exposes what the editor needs

**Files:**
- Modify: `server/api/api.go` (`ServerConfig`), `server/api_routes.go`, `docs/api/v1.md`
- Test: `server/api_test.go` (or the existing `server-config` test)

**Interfaces:**
- Produces: `ServerConfig.Audio { Backgrounds []string; AllowFlowBackground bool }` (`json:"audio"`) and `ServerConfig.Idle { UserTimeoutMS int; MaxPrompts int; PromptMode string }` (`json:"idle"`). Names only: never a file path, never the prompt text.

- [ ] **Step 1: Write the failing test:** a server with two sounds and `allow_flow_background: true` returns the sorted names and flags; a server with none returns `[]` (not null); the response body contains none of the configured file paths (assert the JSON text does not contain the temp dir); idle fields reflect the effective server defaults (15000 / 2 / `llm` by default).
- [ ] **Step 2: Run to see it fail.** `cd server && go test -run ServerConfig -count=1 .` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run to see it pass.** `cd server && go test -race -count=1 ./... 2>&1 | tail -3` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): server-config lists the sound names and the quiet-caller defaults`.

---

### Task 8: Editor model, API client and Explain copy

**Files:**
- Modify: `ui/src/api/client.ts` (ServerConfig type + `getServerConfig` if absent — check), `ui/src/routes/flows/editor/model.ts` (`CALL_LEVEL` gains `idle`, `audio`; constants; `flowIssues`), `ui/src/routes/flows/editor/diff.ts` (`FLOW_SETTINGS` gains `["idle","silent-caller behavior"]`, `["audio","audio"]`), `ui/src/routes/flows/editor/settingCopy.ts`
- Test: `model.test.ts`, `diff.test.ts`, `settingCopy.test.ts` (the existing guards)

**Interfaces:**
- Produces: `IDLE_RANGE = { timeoutMs: [1000, 300000], prompts: [1, 10], text: 200 }`, `AUDIO_RANGE = { gain: [-30, 6], bgGain: [-60, 0], strength: [0, 1] }`; setting ids `idle.user_timeout_ms`, `idle.max_prompts`, `idle.prompt`, `idle.prompt_mode`, `idle.goodbye`, `audio.denoise`, `audio.output_gain_db`, `audio.background`, `audio.background_gain`, `audio.background_loop` with the copy the spec lists (silence = time since the agent stopped speaking; asks every wait; the call ends one wait after the last ask; default 15 s; exact wording costs no model call; denoise removes steady hum and hiss, not other voices, costs about 0.1% of one core per call, recordings keep the raw audio; positive gain can clip; backgrounds are named server sounds and add continuous audio traffic to every call).

- [ ] **Step 1: Write the failing tests.** `flowIssues`: every range boundary above with the message naming the field (`Wait for a silent caller must be 1 to 300 seconds, or 0 to never hang up.`; `Ask this many times must be 1 to 10.`; `What to say is over 200 bytes.` (bytes, with `é`×101); `Goodbye is over 200 bytes.`; `Voice volume must be -30 to +6 dB.`; `Background volume must be -60 to 0 dB.`); valid values and absent keys give no issue; `CALL_LEVEL` strips `idle`/`audio` from non-start steps in `toPayload`; `describeChanges` lists the two new entries; the copy guard test fails until the ten ids exist and are referenced (Task 9 references them).
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/routes/flows/editor/model.test.ts src/routes/flows/editor/diff.test.ts` — Expected: FAIL.
- [ ] **Step 3: Implement.** Add the entries to `settingCopy.ts` in the same voice as the existing ones (the "is used by controls" guard stays red until Task 9, so land Tasks 8 and 9 in two commits on the same branch and run the full vitest suite only at the end of Task 9; the model tests above run alone here).
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run src/routes/flows/editor/model.test.ts src/routes/flows/editor/diff.test.ts && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): editor model for silent-caller and audio settings`.

---

### Task 9: The controls in Flow settings

**Files:**
- Modify: `ui/src/routes/flows/editor/FlowSettingsPanel.tsx` (+ a small `CallBehaviorFields.tsx` if the panel passes ~300 lines), `ui/src/routes/flows/editor/FlowEditor.tsx` (fetch `server-config` once through the existing react-query client and pass it down), `ui/src/styles.css` (tokens only)
- Test: `FlowSettingsPanel.test.tsx`, `FlowEditor.test.tsx`

**Interfaces:**
- Consumes: `ServerConfig` (Task 7/8). The panel takes `serverConfig?: ServerConfig` (absent while loading or on error: placeholders fall back to `Server default`).

- [ ] **Step 1: Write the failing tests.**
  - **Basic:** outside Advanced, a checkbox `End the call if the caller stays silent` (checked when `idle` is absent or its timeout > 0; unchecking writes `idle: {user_timeout_ms: 0}`; checking removes the `idle` key when no other member is set, else sets the timeout back to the server default shown) and `Wait this long (seconds)` (placeholder from `serverConfig.idle.user_timeout_ms / 1000`, e.g. `15`; typing 8 writes `user_timeout_ms: 8000` and, when `idle` was absent, `max_prompts` is **not** written — the built-in 2 applies; blank removes the member and the whole `idle` object when empty). Hint: `The call ends one wait after the last question.`
  - **Advanced:** `Ask this many times` (1–10, placeholder `2`), `What to say` (textarea, placeholder `Are you still there?`, hint with the bytes limit), radios `Let the model word it` / `Say it exactly` (default model), `Goodbye before hanging up` (placeholder the built-in goodbye), `Reduce background noise` (checkbox writes `audio.denoise: {}`; a `Strength (%)` number 0–100 shown while checked writes `strength` as 0..1; unchecking removes `denoise`), `Voice volume (dB)` (−30..+6, blank removes; `0` is written as `0` because explicit 0 overrides the server), `Background sound` (select: `None` — which writes nothing — and the server's names; **disabled** with the reason `This server does not allow flow background sounds.` when `allow_flow_background` is false and no sound is stored; a stored sound the server no longer lists is shown as `<name> (not on this server)` and flagged by an issue), `Background volume (dB)` (−60..0, placeholder `-30`), `Repeat` (checkbox, default checked; unchecking writes `loop: false`). Every field has its Explain button (ids from Task 8) and `aria-describedby` hint.
  - `Advanced · N set` counts `idle` and `audio` as one setting each; a stored `idle`/`audio` opens Advanced.
  - Round trip: a stored `{idle:{user_timeout_ms:8000,max_prompts:3,prompt:"Hello?",prompt_mode:"say",goodbye:"Bye."},audio:{denoise:{strength:0.8},output_gain_db:-3,background:{name:"office",gain_db:-20,loop:false}}}` opens showing every value and the Publish payload equals it.
  - `FlowEditor.test.tsx`: the Publish dialog lists `Flow settings: silent-caller behavior changed` and `Flow settings: audio changed`; the server-config query failing leaves the panel usable with `Server default` placeholders.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/routes/flows/editor` — Expected: FAIL.
- [ ] **Step 3: Implement.** Seconds ↔ milliseconds conversion in one place (`secondsToMs`, `msToSeconds`); reuse `numberOrNothing`; the explicit-0 gain case must not be swallowed by `numberOrNothing` (blank → remove, `0` → `0`). A mirrored issue disables nothing by itself: Publish is blocked by `flowIssues` as for the other fields.
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS (the Task 8 copy guard is green now).
- [ ] **Step 5: Commit** `feat(ui): silent-caller and audio controls in Flow settings`.

---

### Task 10: The browser proves it, docs, gate, review

**Files:**
- Create: `e2e/tests-flows/38-call-behavior.spec.ts`
- Modify: `docs/api/v1.md`, `docs/audio-filters.md`, `docs/operations_runbook.md`, `docs/superpowers/specs/2026-10-12-switchboard-console-design.md`, `examples/` YAML (the new keys), the e2e stack config (`e2e/lib/flows-stack.ts` or the Go test server config it starts: add a short generated WAV under `audio.backgrounds` and `allow_flow_background: true`; set `idle.user_timeout` so other specs are unaffected: the default 15 s must not end their calls: set it to `0` for the stack and let this spec's flow turn it on)

- [ ] **Step 1: Write the spec** (echo providers, serial, `test.skip(LIVE, …)` like the other echo specs). Flow created through the API; in Flow settings: untick/tick the Basic checkbox and see the wait placeholder equal the server default; open Advanced, set `Wait this long (seconds)` 2, `Ask this many times` 1, choose `Say it exactly`, `What to say` `Still there for the test?`, `Goodbye before hanging up` `Goodbye for now.`; tick `Reduce background noise`, set `Voice volume (dB)` −3, pick the background sound; open `Explain Voice volume (dB)` and see `Default and range`; run a **draft test call** (before publishing): connect, mute, say nothing → the transcript shows the exact prompt text after about 2 s and then the goodbye and the call ends; publish; read the version through `GET /flows` and assert `idle`/`audio` values (`user_timeout_ms: 2000`, `prompt_mode: "say"`, `output_gain_db: -3`, `background.name`, `denoise` present); reopen in-app and see Advanced open with the stored values; set `Wait this long` to 0.5 and see the Publish dialog block with `Wait for a silent caller must be 1 to 300 seconds`.
- [ ] **Step 2: Run** `cd e2e && npx playwright test -c playwright.flows.config.ts tests-flows/38-call-behavior.spec.ts` (after `make ui-embed`) — Expected: PASS. Fix real failures in the code, not the spec.
- [ ] **Step 3: Docs.** `docs/api/v1.md`: the `idle` and `audio` objects, ranges, defaults, server-default fallback, error texts, `server-config` fields. `docs/audio-filters.md`: per-flow settings and the sound catalogue (`audio.backgrounds`, `background_default`, `allow_flow_background`), the removal of `audio.background`. `docs/operations_runbook.md`: the new YAML keys, the on-by-default change and how to turn it off, the rollback note (an older server rejects the unknown keys, as for `greeting`). The console spec: the two groups.
- [ ] **Step 4: Full gate.** `gofmt -l .` empty; `make verify` or `go test -race ./core/... ./server/... ./transport/...`; `make test-postgres` (no SQL change, run once); `cd ui && npx vitest run && npx tsc --noEmit`; `make e2e-echo` — Expected: all green.
- [ ] **Step 5: Independent review** (AGENTS.md): a fresh Opus reviewer on `main..HEAD` with the Review Focus items, YAGNI/bloat/dead-code, concurrency (the late `SetBackground`), goroutine lifecycle, and the default-flip blast radius. Fix Critical/Important findings with a failing test first; ledger the rest.
- [ ] **Step 6: Commit** `docs: per-flow quiet-caller and audio settings`.
