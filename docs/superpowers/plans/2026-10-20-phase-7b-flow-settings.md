# Phase 7b: Per-flow conversation settings (Implementation Plan)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. One task = one reviewable deliverable (one or a few conventional commits). Order: 7b.0 → 7b.1 → … → 7b.11, strictly sequential: 7b.5–7b.10 all edit `server/flow/config.go`, `server/flow/definition.go`, `server/flow/policy.go`, `server/call.go`, `server/registry.go`, `ui/src/api/client.ts`, `ui/src/routes/flows/editor/{FlowSettingsPanel,model,settingCopy}.ts*` and `e2e/tests-flows/37-conversation-settings.spec.ts`; never run two tasks in parallel.

**Goal:** Build recommendations R1, R2, R3, R5a, R5b and R6 of the settings coverage audit (user decision 2026-10-20): fix four dead or misleading settings, then let a flow choose its interruption rules, its end-of-turn timing, a background sound per step, a hold sound while a slow tool runs, a hang-up tool on a one-step flow, and a shorter call limit than the server's. Every new setting reuses core code that already exists; no new library audio code.

**Architecture:** A call's pipeline is already built per call (`server/registry.go` `BuildCallPipeline(cfg, call CallSpec, …)`), from server YAML (`cfg`) plus the pinned flow's start-step settings (`CallSpec`, built by `server/call.go` `callSpec`). Every new call-level setting follows the existing `idle` pattern: an optional pointer field on the start step's `flow.Config`, strict validation in `DecodeConfig`, server-dependent checks in `flow.Policy` (publish refusal and, where a call could not honour it, a readiness reason), a `CallSpec` field, and a `forCall` merge with the server's value inside `BuildCallPipeline`. Per-step settings (background sound) ride the route lifecycle that already exists (`core.RouteEvent{Kind: RouteActive, To}` → `ws.CueUnderlay.SetBackground`). The hold sound reuses `server/cue.go` `cueController` on the same `CueUnderlay`. Flow config is JSON in the DB: **no migration**.

**Tech stack:** Go 1.27.1 workspace (`server`, `core`, root module `transport/*`, `cmd/server`), React 19 / TS / Vite / TanStack Query / zod (`ui/`), Vitest + Testing Library, Playwright (`e2e/tests-flows`, echo stack).

**Spec:** `docs/superpowers/specs/2026-10-19-settings-coverage-audit.md` (rows 1.5, 1.9, 1.10, 2.11, 2.14, 2.15, 3.2, 3.4–3.7, 4.1, 5.10, 5.17, 7.3, 7.5; recommendations R1–R3, R5a/b, R6). Master: `docs/superpowers/plans/2026-10-15-review-remediation.md` (Global Constraints). Ledger: `.superpowers/sdd/2026-10-15-review-remediation/progress.md` (last line: "User decisions (2026-10-20): settings audit scope = Phase 7b R1-R6 as a SEPARATE phase after Phase 7"). Out of scope: R4 (uninterruptible greeting), R5c (sound preview/upload), R7 (per-flow DTMF), R8 (LLM sampling), ducking, fades.

**What this plan supersedes:**
- **"B2" deferral of turn settings.** The per-flow call-behaviour design (`docs/superpowers/specs/2026-10-15-per-flow-call-behavior-design.md:11`) took B1 (silent caller) and B3 (audio) and left "B2 (turn taking and interruptions)" out. Tasks 7b.5 and 7b.6 build B2 (the parts the audit marks `MPF`: min words, mute strategies, max mute, speech timeout, end-of-turn strategy, smart-turn hold). LLM completion markers, `no_transcript_timeout`, `eager_grace` and the analyzer's workers/budget stay server-only.
- **Correction to the brief:** master-plan decision **D-0.6** is *not* about turn settings. It is "Flow version history API (review item B2): leave; no consumer" (`2026-10-15-review-remediation.md:53`), a different "B2" (the deep review's). D-0.6 stands unchanged; this plan does not touch flow version history.
- **Phase 7 addendum placeholder.** `2026-10-19-phase-7-ui-gaps.md` reserves "Tasks 7.9+" for the audit items. The user chose a separate phase instead: Phase 7's 7.8 runs as written and the placeholder stays empty (Phase 7 is being executed in the main checkout; this plan does not edit its file).

---

## Decisions for the user

| # | Question | Recommendation |
|---|---|---|
| D1 | **Smart Turn for flows when the server default is not smart.** The model is loaded only when the server's own `turn.stop` lists `turn-analyzer` (`cmd/server/main.go:89-106`); otherwise `Registry.turnBackend` is nil and a flow asking for `end_of_turn: "smart"` could never be honoured (readiness `smart_turn_unavailable`). Options: (a) per-flow `smart` works only on servers whose default is already smart (then flows mostly use it to opt *out*); (b) one new YAML key `turn.analyzer.for_flows: true` that loads the model at start-up (same fail-closed hash/worker check) without changing the server's default chain. | **(b).** One boolean and a one-line condition in `main.go`; off by default (no worker process, no model memory unless asked); the development profile turns it on only if the model file is present (`config/development.yaml` commented line). Without it R3's main value (a slow-speaker flow picking smart) needs every flow on the server to be smart. |
| D2 | **`turn.max_mute_ms` ceiling.** The brief gives a flat range 1000..60000. The server's `turn.max_mute` (default 30 s) is a safety cap: "a lost signal cannot silence the user" (`server/config.go:178-186`). With `mute: ["always"]` and 60 s a caller cannot interrupt a minute-long reply. | **Tighten only:** static range 1000..60000 (as briefed), **and** publish refuses a value above the server's effective `turn.max_mute`; at call time the effective cap is `min(flow, server)` (so a server lowered later wins silently). Same rule as `max_call_duration_seconds` (R6). |

Everything else is a ruling below (the reviewer may challenge any).

## Rulings

- **K1 Back-compat: stored flows behave exactly as today.** Every new field is optional; absent = the server's value; no schema-version bump (the strict decoder already rejects unknown keys, so an older binary refuses a flow that uses a new field: rollback note in every task). The two dead per-step settings (`tts.clean_text`, `failover.on` on a non-start step) are **refused at publish only** (`Service.attachedConfig`, the single path for create, update, draft and duplicate: `server/flow/service.go:133,187,278`), **not** in `Decode`/`validateCallLevel`: the editor has written them on any step (`StepAdvanced.tsx:34-40,105-113`), so rejecting them in `Decode` would turn existing stored versions `invalid_config` (`service.go:456`) and take live flows offline. Stored versions keep decoding and ignoring them, as today. The editor strips them from non-start steps on save (`toPayload`), so a re-save is clean. Duplicating a legacy flow that has them is refused with the step named (fix: open, save, publish).
- **K2 One merge per setting, server first.** `forCall` methods take the server's value and override member by member (`min_words`, `mute`, `max_mute_ms`, `speech_timeout_ms`, `end_of_turn`, `smart_hold_ms`), unlike `idle` which replaces as one unit (`server/idle.go:51-73`): turn members are independent knobs and the editor shows each with the server's value as placeholder. `forCall` never mutates the shared `*Config` (it clones `Mute`/`Stop` slices: `cfg` is shared by every call; a write would be a data race).
- **K3 `min_words`: 0 = off, 1..10.** Pointer: absent = server's `turn.min_words`; `0` explicitly turns the backchannel filter off for this flow (needed for "Caller can interrupt any time" on a server that sets `min_words`). `use_interim` stays `true` (`registry.go:747`, audit 3.3 `LIB`).
- **K4 `mute`: present replaces the server list.** `*[]string`: absent = server's `turn.mute`; `[]` = no mute for this flow; members unique, from `always`, `first_speech`, `until_first_bot_complete`, `function_call` (core names, `core/turn/mute.go:38-41`). `until_first_bot_complete` only when the start step has a non-empty `greeting` (the YAML comment's rule, `server/config.go:184-185`; without a greeting the cap is the only release). Muted speech still reaches STT (billed) and recording: the help copy says so (audit 3.12).
- **K5 `end_of_turn`: `default | smart | eager`; absent = server's chain.** `default` = the built-in speech timeout (Pipecat's default strategy, what the server ships: `turn.stop` empty, `turn_stop.go:28-30`); `smart` = `[turn-analyzer, speech-timeout]`; `eager` = `[eager, speech-timeout]`. The flow's choice replaces only the detector entries: an `llm-completion` entry in the server's `turn.stop` is kept (prompt protocol, server-only, audit 2.16). Every chain keeps the speech-timeout fallback, so `eager` cannot leave an unpunctuated turn open; there is **no** "punctuating STT" gate (the audit assumed a catalogue mark that does not exist, fact 9): the help copy warns instead. `smart_hold_ms` (500..5000, Pipecat `stop_secs`) only with `end_of_turn: "smart"`. `speech_timeout_ms` 200..3000.
- **K6 Readiness tells the truth.** A flow setting the server cannot honour is refused at publish **and** turns a stored flow `not_ready`: `smart_turn_unavailable` (smart requested, no model loaded: `policy.SmartTurn()` from config, which `main.go` fails closed on), `turn_manager_off` (any `turn` member while `turn.manager: false`; push-to-talk calls are unaffected). A limit that can only tighten (`max_mute_ms`, `max_call_duration_seconds`) is refused at publish when above the server's value; a server lowered later just wins (`min`), no readiness change. A sound or cue name the server stops offering is refused at publish, never offline (today's background rule, `policy.go:20-23`): the call runs without it and logs.
- **K7 Background per step = switch on arrival, else keep.** A step whose config has `audio.background` switches the bed when a route makes it active (`RouteActive`); a step without it leaves whatever is playing. A non-start step may set **only** `audio.background` (denoise, voice volume and the hold cue stay call-level). The start step's background is applied at connect as today and again when a route returns to it.
- **K8 Hold sound = sync MCP tools only, never over a route cue.** `audio.tool_cue{name, after_ms}` (start step; `after_ms` 0..10000, 0 = 1000 ms) plays a server cue (`audio.cues`) once a synchronous MCP tool has run `after_ms` with the call quiet. Not for async tools (the model already speaks an ack), transfer tools, `end_call`, `send_dtmf`. It pauses on bot audio or caller speech and re-arms while the tool still runs; it stops (30 ms fade) when the last running tool returns (result, error, cancel by barge-in or stale generation: the wrapper's `defer`), at a 30 s cap (`tools.DefaultToolLoopConfig` call timeout), or when a route starts preparing. It never starts while the route cue is waiting (one `CueUnderlay`; its `Stop` fades whatever cue is current, `transport/ws/cue.go:74-86`).
- **K9 `end_call` on one step.** Allowed on any flow with steps; a flat flow (no `steps`) cannot carry it (it is a step attribute), so the editor saves a one-step flow with `end_call` in steps form. The one-step call keeps every captured MCP tool (today's one-step rule, `call.go:338-339`) plus `end_call`; no router is built.
- **K10 `max_call_duration_seconds`.** Start step, 60..server cap (`limits.max_call_duration`, also sent as `max_call_duration_seconds` in `GET /server-config`); effective = `min(flow, server)`, computed once in `callSpec`; the existing timer (`server/server.go:786-803`) reads it. The orphan sweep keeps the server's value (a flow can only shorten calls, `server/orphans.go:28`).
- **K11 Field names.** JSON snake_case like the rest of `Config`; durations in `_ms` except the call limit in `_seconds` (matches `GET /server-config`). Error messages name the field path (`turn.mute[1]`, `step "sales": audio may only set background`).
- **K12 UI mirrors the server (Phase 7 ruling R2).** New zod fields: server-config members the server always sends are required; enum-like values (`end_of_turn`, readiness reasons) stay `z.string()`. Controls show the server's value as placeholder or "Server setting (…)" option, never invent a default.
- **K13 `critical_output` is deleted, not fixed** (R4 is out of scope): the strategy, `SpeakingStateFrame.Uninterruptible` (its only consumer) and the `critical` plumbing in `core/turn/mute.go`. A server YAML still listing `critical_output` now fails at start-up with the existing "unknown strategy" error (release note). `core.OutputUninterruptible` stays (ambience and cue frames use it, `transport/ws/pacing.go:743`).

---

## Verified facts (2026-10-20, worktree `feat/phase-7b-flow-settings` @ 8eae42e = Phase 7 plan commits on main dbcb21f; commands actually run)

1. **Dead per-step `tts.clean_text` / `failover.on` (audit 4.1, 5.17): confirmed.** `server/call.go:352-357` reads `cfg.TTS.CleanText` of the start step only; `stepSpec` (`call.go:441-465`) builds `StepSpec{Name, Greeting, LLM, LLMFallbacks, TTS, TTSFallbacks}` (`server/flow_call.go:20-28`, no failover or cleaning field); `buildStep` copies `stepCall := call` (`registry.go:664`), so `call.Failover` and `call.DisableTextCleaning` of the start step apply on every step. `validateCallLevel` (`server/flow/definition.go:170-185`) does not list either. UI: `StepPanel.tsx:354` renders `StepAdvanced` for every step; the failover fieldset `StepAdvanced.tsx:67-83` and the clean-text checkbox `:105-114`.
2. **`send_dtmf` only on the start step (audit 7.3): confirmed.** `server/dtmf.go:24-62` `buildDTMF(cfg, &call)` appends the tool to `call.LLM` and `call.LLMFallbacks` once (`registry.go:587`); `buildStep` (`registry.go:657-680`) runs `prepLLM` (`:619-639`) on the resolved step spec, which adds history, completion prompt, tool-activity wrap and async sink, not DTMF.
3. **Route cue name unchecked (audit 1.10): confirmed.** `policyProblems` (`server/flow/policy.go:31-53`) checks the background name and idle floor only; `api.ServerAudio` (`server/api/api.go:97-103`) has `backgrounds`, `allow_flow_background`, `background_default`, no cues; `newCallCue` (`server/cue.go:168-183`) logs and runs without a cue for an unknown name. UI: free-text `<input id="cue-name" type="text">` (`FlowSettingsPanel.tsx:280-292`). Tests that type into it: `FlowSettingsPanel.test.tsx:88,97,169`, `e2e/tests-flows/20-flow-lifecycle.spec.ts:49` (`fill("chime")`). Go e2e `TestFlowE2EUnknownCueNameStillHandsOff` (`server/flow_e2e_test.go:779`) creates a flow with an unknown cue: it must be reworked (publish will refuse it).
4. **`critical_output` never mutes (audit 3.7): confirmed.** `git grep -n "OutputUninterruptible\|Uninterruptible:" -- '*.go' | grep -v _test | grep -v '[!=]= '` → only `core/control.go:32` (the constant), `core/output_state.go:53` (copies the disposition into `SpeakingStateFrame.Uninterruptible`) and `transport/ws/pacing.go:743` (the ambience frame, `NonSpeech: true`); `pacing.go:832-833` skips speaking state for `NonSpeech` frames. Consumers of the flag: `core/turn/processor.go:245` → `mute.onBot(…, critical)` → `botMute.critical` (`mute.go:62,76,81-85,163-168`). Config mentions: `server/config.go:180`, `config/development.yaml:120`, `docs/pipecat-parity-matrix.md:28` ("Adds `critical_output`"). Tests: `core/turn/mute_test.go:291,296-…`, `core/output_state_test.go:33,43`.
5. **Turn settings are server-only (audit 2.11, 2.14, 2.15, 3.2, 3.4–3.6): confirmed.** `BuildCallPipeline` reads `cfg.Turn.{Stop,Completion,Mute,MinWords,Analyzer.Hold,Analyzer.Budget,SpeechTimeout,EagerGrace,NoTranscriptTimeout,MaxMute,Manager}` (`registry.go:600-606,622-625,734-786`); `Turn` is an anonymous struct (`server/config.go:148-200`), so it has no method set yet. The pipeline is built per call, so a per-call copy is enough. `turnStopDetector(stop, st, eagerGrace, analyzer)` (`server/turn_stop.go:27-61`) validates and builds any chain; `analyzer == nil` with `turn-analyzer` is an error (`:46-48`), which would fail the call build.
6. **Smart Turn model loading:** `cmd/server/main.go:89-106` loads it only `if slices.Contains(cfg.Turn.Stop, "turn-analyzer")`, fatal on failure ("failed closed"); `Registry.turnBackend` is nil otherwise (`registry.go:168,750`). `validateTurn` (`server/config_validate.go:163-196`) checks analyzer mode/limits only when listed; production requires `process` mode. Defaults: `Analyzer.Hold` 3 s, `Budget` 150 ms, `Workers` 1 (`config_defaults.go:63-65`).
7. **Background and cues at run time:** `conn.SetBackground(s.backgroundFor(call.Audio))` once per call (`server/server.go:544`); `CueUnderlay.SetBackground` is atomic and wakes the pacer (`transport/ws/cue.go:30-41`); `RouteEvent{Kind, From, To, Err, Elapsed}` (`core/router.go:29-34`), `RouteActive` emitted with `To: target` (`:245`). The route cue controller is wired in `server.go:558-561` (OnRoute wrapper), `:646,653` (caller speech), `:692` (bot audio). One `CueUnderlay` per connection (`conn.Cue()`, `transport.go:1236`); its `Stop` fades whatever cue is current.
8. **`end_call` needs two steps (audit 5.10): confirmed, and in more places than the audit names.** `definition.go:133-135` rejects it; `callSpec` builds `CallFlow` (which carries `CanEnd`/`End`) only `if len(g.Steps) > 1` (`call.go:340-345`); the idle controller is forced on only for `call.Flow != nil && call.Flow.CanEnd` (`registry.go:812`) and `End` is wired only then (`:819-829`); UI `StepPanel.tsx:312-320` disables the checkbox with one step, `model.ts:259` drops `end_call` with one step, and `isFlat` (`model.ts:244-247`) saves a one-step "main" flow flat (no place for `end_call`). `stepExecutor` (`flow_call.go:141-167`) already works with nil `transfer`/`commit`.
9. **No "punctuating STT" mark exists** (audit R3 assumed one): `git grep -n -i punctuat -- server '*.json' '*.yaml'` → only comments (`server/config.go:160`, `turn_stop.go:16`).
10. **Tool activity:** `coreturn.ToolActivity` (`core/turn/mute.go:95-139`) exists only when `function_call` mute or idle is on (`registry.go:605-608`), wraps the whole step executor (so it also counts transfer tools, `end_call`, `send_dtmf`) and has no start/stop callback: R5b needs its own small server-side wrapper (K8). MCP tool lookup by name: `rs.MCPTool` (already used by `turnRecorder`, `call.go:64,236-240`).
11. **Max call duration:** one timer per call from `s.cfg.Limits.MaxCallDuration` (`server.go:786-803`); validated `> 0` (`config_validate.go:148`); `GET /server-config` and operations status send `max_call_duration_seconds` (`api_routes.go:51`, `server.go:398`); UI zod has it (`ui/src/api/client.ts:49,62`). Test `TestMaxCallDurationFiresOnceWithReason` (`server/maxduration_test.go:21`) uses a 700 ms server cap.
12. **Publish path:** `attachedConfig` (`service.go:556-574`) = `normalizeConfig` (→ `Decode`) + `policyProblems` (first message → 400) + `Problems` (→ `flow_not_attached`). `Problems` adds policy *reasons* to readiness (`definition.go:239-245`). `flow.Policy` is implemented by `serverPolicy` from `policyFrom(cfg)` at `NewServer` (`server/providers.go:20-43`, `server.go:223`) and `fakePolicy` in `server/flow/policy_test.go:20-22`.
13. **Echo rig for caller audio:** typed turns (`send-text`) re-enter after the turn processor (`server.go:1331-1350`), so they bypass min words and mute: barge-in tests need audio. `providers.echo.stt: audio` turns voiced frames into words (`providers/echo/stt.go:58-80`); Google mimic `latest_long` = interims, 20 frames (400 ms) per word, `VoicedThreshold` 500 (`providers/echo/figures.go:48-51`). `callerSpeech` (clip1.wav, 3.34 s measured with Python `wave`), `realVAD` (skips without ONNX Runtime) and `sendAudio` live in `server/offline_e2e_test.go` (`//go:build xaispeech`; `GOTAGS ?= xaispeech`, `Makefile:21`). `make test-e2e-go` runs only names matching `FlowE2E|…|CallBehavior|…` (`Makefile:63`): new Go e2e tests are named `TestCallBehavior…` or `TestFlowE2E…`.
14. **Editor facts:** `CALL_LEVEL` (`model.ts:40`) = `stt, vad_stop_ms, stt_p99_ms, recording, context, async_tools, idle, audio`, deleted from non-start steps in `toPayload` (`:258`) and moved on `setStart` (`:331`). `settingCopy.ts` (183 lines) + `settingCopy.test.ts` enforce copy for every `<SettingHelp id>`. `FlowSettingsPanel.tsx` 370 lines (Listening, Recording, `AudioFields`, Turn-taking `:143-179`, Memory, Route sound `:276-312`). E2E echo stack config has cue `chime` and background `office` (`e2e/global-setup-flows.ts:76-89`), idle off, no smart-turn model.
15. **Baselines run here:** `cd server && go test -count=1 ./flow/` → `ok govoice/server/flow 0.500s`; `cd core && go test -count=1 -run Mute ./turn/` → `ok govoice/core/turn 2.412s`; `git ls-files 'ui/src/**/*.test.ts*' | wc -l` → 61. No `node_modules` and no ONNX Runtime libs in this worktree (as Phase 7 fact 12): 7b.0 installs them.

---

## Per-flow schema (start step unless stated; all optional)

```jsonc
{
  "turn": {                         // start step; {} is 400 ("turn must set at least one member")
    "min_words": 3,                 // 0 = off, 1..10; absent = server turn.min_words           (R2, K3)
    "mute": ["first_speech", "function_call"], // [] = none; absent = server turn.mute         (R2, K4)
    "max_mute_ms": 15000,           // 1000..60000 and <= server turn.max_mute (D2)               (R2)
    "speech_timeout_ms": 800,       // 200..3000; absent = server turn.speech_timeout             (R3)
    "end_of_turn": "smart",         // "default" | "smart" | "eager"; absent = server chain      (R3, K5)
    "smart_hold_ms": 2000           // 500..5000, only with end_of_turn "smart"                   (R3)
  },
  "audio": {
    "background": {"name": "office"},   // now allowed on ANY step (only member a non-start step may set) (R5a, K7)
    "tool_cue": {"name": "chime", "after_ms": 1000} // start step; name from audio.cues; 0..10000, 0 = 1000 (R5b, K8)
  },
  "max_call_duration_seconds": 600, // 60..server limits.max_call_duration                        (R6, K10)
  "tts": {"clean_text": false},     // start step only at publish (R1, K1)
  "failover": {"on": ["timeout"]}   // start step only at publish (R1, K1)
}
// Definition level: steps[].end_call allowed with one step (R6, K9); route_cue.name must be in audio.cues (R1).
```

Go types (`server/flow/call_behavior.go`, next to `IdleSettings`):

```go
// TurnSettings is a flow's turn-taking (call-level). Each member overrides the server's only when present.
type TurnSettings struct {
	MinWords        *int      `json:"min_words,omitempty"`         // 0 = off, 1..10
	Mute            *[]string `json:"mute,omitempty"`              // present replaces turn.mute; [] = none
	MaxMuteMS       *int      `json:"max_mute_ms,omitempty"`       // 1000..60000, <= server
	SpeechTimeoutMS *int      `json:"speech_timeout_ms,omitempty"` // 200..3000
	EndOfTurn       string    `json:"end_of_turn,omitempty"`       // "", default, smart, eager
	SmartHoldMS     *int      `json:"smart_hold_ms,omitempty"`     // 500..5000, smart only
}
// AudioSettings gains:  ToolCue *RouteCue `json:"tool_cue,omitempty"`
// Config gains:         Turn *TurnSettings `json:"turn,omitempty"`; MaxCallDurationSeconds int `json:"max_call_duration_seconds,omitempty"`
```

`flow.Policy` gains (all from config in `policyFrom`, so no race with `SetTurnBackend`): `Cues() []string`, `SmartTurn() bool` (= `cfg.Turn.Stop` lists `turn-analyzer` **or** `cfg.Turn.Analyzer.ForFlows`, D1), `TurnManager() bool`, `MaxMute() time.Duration` (`cfg.Turn.MaxMute` or `coreturn.DefaultMaxMute`), `MaxCallDuration() time.Duration`. `fakePolicy` gains the same. `GET /server-config` gains `audio.cues` (sorted names) and `turn: {manager, min_words, mute, max_mute_ms, speech_timeout_ms, end_of_turn, smart_turn_available, smart_hold_ms}` (`end_of_turn` derived from `turn.stop`: `smart` if it lists `turn-analyzer`, else `eager` if it lists `eager`, else `default`; names and numbers only).

---

## Global Constraints

Inherited from `AGENTS.md`, the master plan and Phase 7.

- `core/` never imports `server/`; this phase adds **no** core code (7b.4 only deletes). Everything new lives in `server/`, `cmd/server`, `ui/`, docs.
- Bounded everything: no new channel or queue; the hold-cue wrapper keeps an atomic in-flight counter; `mute` ≤ 4 entries (unique subset); cue/sound names are catalogue keys, never paths.
- `forCall` merges never mutate the shared server `*Config` (clone slices; `go test -race`).
- Monotonic time only (cue controller already uses `time.Since(cueBase)`); timers stopped on every exit.
- Errors `%w`, `errors.Is/As`; validation errors name the field path; `go test -race` in every touched module; `gofmt -l` empty; `go vet` in all three tag sets for touched modules.
- UI: no new dependency; tokens only (no hard-coded colours, `noHardcodedColors` test); every control labelled and keyboard reachable; errors `role="alert"`; knip stays empty; `npx tsc --noEmit -p .` 0 errors; every new control has a `settingCopy.ts` entry (the existing test enforces it).
- Privacy: no flow setting adds stored or logged content; cue/background names only; muted speech still reaches STT and recording (stated in copy and `docs/api/v1.md`).
- Conventional commits; trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`. Provider secrets env-only. Never run paid/live targets; never `offline-up`/model downloads; never kill processes by pattern; scratch files only under the session scratchpad or `.superpowers/` (git-ignored); run gates on an otherwise idle machine.

### Gate block (after EVERY task; substitute the task id for `7b.X`)

```bash
L=.superpowers/sdd/2026-10-15-review-remediation/flake; mkdir -p "$L"; set -o pipefail
P=.superpowers/sdd/2026-10-15-review-remediation/p7b; B=$(cat $P/base-7b.X)   # recorded at task start: git rev-parse HEAD
gofmt -l $(git ls-files '*.go' | grep -v '^pipecat-impl/') | tee "$L/7b.X-gofmt.log"; test ! -s "$L/7b.X-gofmt.log"
(cd ui && npx tsc --noEmit -p . && npx vitest run) 2>&1 | tee "$L/7b.X-ui.log"
(cd ui && npx --yes knip@5 --no-progress) 2>&1 | tee "$L/7b.X-knip.log"; diff $P/knip-before.txt "$L/7b.X-knip.log"   # no new finding
make verify 2>&1 | tee "$L/7b.X-verify.log"
make e2e-echo 2>&1 | tee "$L/7b.X-e2e-echo.log"          # Go e2e (test-e2e-go) + Playwright e2e-fake (flows + oneport)
grep -nE -- '--- SKIP' "$L/7b.X-e2e-echo.log" | tee "$L/7b.X-skips.log"   # a new test that skipped (no ONNX Runtime) is a FAIL
make provider-matrix-dry 2>&1 | tee "$L/7b.X-provider-matrix-dry.log"
# 7b.4 and 7b.5 only (core/turn or the call builder touched): also
make loadtest-smoke 2>&1 | tee "$L/7b.X-loadtest-smoke.log"
# last, so a down stack skips only this target (report it; never start the stack):
if curl -fsS http://127.0.0.1:8099/healthz >/dev/null && curl -fsS http://127.0.0.1:11434/api/tags >/dev/null; then make offline-test 2>&1 | tee "$L/7b.X-offline-test.log"; else echo "offline stack is down: offline-test NOT run, report it"; fi
git diff --stat "$B"..HEAD | tail -1 | tee "$L/7b.X-diffstat.log"
```

Expected: `ui.log` 0 tsc errors, every Vitest file/test passed (count ≥ 7b.0's plus the task's new tests; `noHardcodedColors` among them); knip identical to 7b.0's (empty); `=== All verify checks passed ===` (the recorded `PLAYBACK_CLEAR_REQUIREMENT_FAILED … relax-unmodified-client-playback-clear` line is expected); `make e2e-echo` exits 0, every Playwright test passed (list the new ones by name), the task's new Go e2e tests show `--- PASS` (and none in `skips.log`); `provider-matrix-dry` exits 0; `loadtest-smoke` `Verdict: PASS`; `offline-test` prints `ok` for both packages. `make test-postgres` is not in the per-task gate (no `server/storage` change); 7b.11 runs it once as a guard. On failure: name the failing test (`grep -nE -- '--- FAIL|^FAIL|✘|failed' <log>`), keep the log, never overwrite it with a rerun. Known intermittent `TestOfflineStackLongCallerSpeechIsOneTurn`: rerun alone 5× and report every result.

**Per-commit compile check (7b.4, 7b.6: `core` or `cmd/server` touched):** the Phase 6 loop over the 13 modules × three tag sets (`go build` + `go vet`), expected no `FAIL` line.

**Mutation rule (every behaviour task):** after GREEN, apply each listed mutation by hand, run the named tests, expect RED, revert (`git checkout -- <file>`); record "mutation → failing test" in the task report.

**Rollback (every task):** `git revert` the task's commits newest first. No storage schema or migration. A flow saved with a new field becomes `invalid_config` on a server rolled back past that task (strict decoder): re-save it without the field (release note, 7b.11).

---

## Task 7b.0 — Baseline (no product change)

**Files (git-ignored):** `.superpowers/sdd/2026-10-15-review-remediation/p7b/{base-7b.0,knip-before.txt,ui-before.log}`, `flake/7b.0-*.log`.

- [ ] **Step 1:** Phase 7 must be merged to `main` first (this branch only holds plan commits). `git fetch . main && git rebase main` (expected: only this plan's commit replays; Phase 7 also edits `ui/src/api/client.ts` (readiness, `getFlows`, live-call methods) and `core/frame.go` (`OutputAudioFrame.Text`), disjoint from the regions 7b edits: `serverConfigSchema` and `SpeakingStateFrame`). `P=.superpowers/sdd/2026-10-15-review-remediation/p7b; mkdir -p $P; git rev-parse HEAD > $P/base-7b.0`.
- [ ] **Step 2:** deps in this worktree only, as real installs (never symlink main's): `(cd ui && npm ci) && (cd e2e && npm ci) && (cd tools/compat && npm ci)`; ONNX Runtime: copy `libonnxruntime*` from main's `third_party/onnxruntime-osx-arm64-1.29.0/lib/` (or `make ort`), then `scripts/fetch-ort.sh check` → exit 0 (the new audio e2e tests of 7b.5 skip without it, fact 13).
- [ ] **Step 3:** `(cd ui && npx tsc --noEmit -p . && npx vitest run) | tee $P/ui-before.log`; `(cd ui && npx --yes knip@5 --no-progress) > $P/knip-before.txt` (expected empty). Full gate block on base. Ledger line: base sha, vitest counts, knip, gate results, offline stack up/down. No commit.

---

## Task 7b.1 — Per-step text cleaning and fallback rules are flow settings (R1, audit 4.1, 5.17)

**Files:** `server/flow/definition.go` (new `deadStepSettings`), `server/flow/service.go` (`attachedConfig`), `server/flow/definition_test.go`, `server/flow/service_test.go`; `ui/src/routes/flows/editor/{StepAdvanced,FlowSettingsPanel,model}.tsx/ts` (+ tests); `docs/api/v1.md:56`.

```go
// deadStepSettings names a setting a non-start step carries that only the start step's applies
// (text cleaning and failover rules are call-wide). Publish refuses it; a stored version keeps
// decoding and ignoring it (K1).
func (g Definition) deadStepSettings() error
```

- [ ] **Step 1 (RED):** `TestPublishRefusesCallWideSettingsOnANonStartStep` (service): create a two-step flow whose step `sales` has `tts.clean_text: false` → `ErrInvalidConfig` with `step "sales": tts.clean_text applies to the whole call; set it on the start step`; same for `failover.on`; on the start step both are accepted. `TestStoredFlowWithCallWideSettingsOnAStepStillDecodes` (definition): `Decode` of that config succeeds (K1). Vitest: `model.test.ts` `toPayload` drops `tts.clean_text` and `failover` from non-start steps and keeps them on the start step; `setStart` moves them to the new start; `FlowSettingsPanel.test.tsx` shows "Clean text before speaking" and the "When to move to a fallback" group once, under Flow settings > Advanced, writing the start step; `StepAdvanced.test.tsx` no longer renders them (update `:75,138,171`).
- [ ] **Step 2:** implement (`deadStepSettings` called in `attachedConfig` after `Decode`); `model.ts`: `CALL_LEVEL` gains `failover`, `toPayload`/`setStart` handle the nested `tts.clean_text`; move the two fieldsets (and their `SettingHelp`) from `StepAdvanced` into `FlowSettingsPanel` Advanced, reading/writing the start step. Docs: add both to the start-step list in `docs/api/v1.md:56`.
- [ ] **Mutations:** drop the `deadStepSettings` call → service test RED; make `Decode` call it → stored-decode test RED.
- [ ] **Commit:** `fix(flow): text cleaning and failover rules are call-wide settings`. **Gate.**

## Task 7b.2 — `send_dtmf` on every step (R1, audit 7.3)

**Files:** `server/dtmf.go`, `server/registry.go` (`prepLLM`), `server/dtmf_test.go`.

- [ ] **Step 1 (RED):** `TestSendDTMFToolOnEveryStep`: `dtmf.send_tool: true`, a routed `CallSpec` built with a capturing LLM factory (pattern of `TestDTMFEnabledStagesAndTool`, `dtmf_test.go:51`); after `call.Flow.Commit("sales")` builds the step, the step's captured `StageSpec.Tools` (and its fallback's) contain `send_dtmf` exactly once, and the start step's too. Fails today (fact 2).
- [ ] **Step 2:** `buildDTMF` keeps building `st.out` but no longer touches `call`; `prepLLM` appends `dtmf.ToolDefinition()` and wraps `ToolExecutor` with `dtmf.Executor(dt.out, …)` for the spec and each fallback when `dt.out != nil` (before the tool-activity wrap, as today's order). No other change.
- [ ] **Mutation:** skip the append for fallbacks → RED.
- [ ] **Commit:** `fix(server): send_dtmf stays available after a route`. **Gate.**

## Task 7b.3 — Route sound is a checked choice (R1, audit 1.10)

**Files:** `server/flow/policy.go` (+ test), `server/providers.go` (`serverPolicy.cues`), `server/api/api.go` (`ServerAudio.Cues`), `server/api_routes.go:54`, `server/flow_e2e_test.go` (rework `TestFlowE2EUnknownCueNameStillHandsOff`), `server/call_behavior_e2e_test.go` (publish refusal), `ui/src/api/client.ts` (`serverConfigSchema.audio.cues`), `FlowSettingsPanel.tsx` (+ test), `e2e/tests-flows/20-flow-lifecycle.spec.ts:49`, `docs/api/v1.md:11,56`, `docs/audio-filters.md:128`.

- [ ] **Step 1 (RED):** policy table test: `route_cue.name` not in `Cues()` → publish message `route_cue.name "chimes" is not a sound on this server (available: chime)` (and "this server has no cues"); readiness unchanged (K6). `TestCallBehaviorThePublishAPIRefusesAnUnknownRouteCue` (real server, `PUT /flows/{id}`) → 400 naming the field. `GET /server-config` lists `audio.cues` sorted. Vitest: route sound is a `<select>` labelled "Route sound" with "None" + the server's cue names; an unknown stored name shows as "chimes (not on this server)" selected (like `AudioFields`' unknown background, `CallBehaviorFields.tsx:164`).
- [ ] **Step 2:** implement; the Go e2e that proves a runtime fallback keeps its intent: create the flow with cue `chime` in a rig whose config has it, then `delete(r.s.cues, "chime")` before dialing (simulates a restart with a changed catalogue) and assert the call still hands off. Playwright `20-flow-lifecycle.spec.ts:49`: `selectOption({ label: "chime" })`.
- [ ] **Mutation:** remove the cue branch in `policyProblems` → policy test and the API e2e RED.
- [ ] **Commit:** `fix(flow): route sound names are checked against the server's cues`. **Gate.**

## Task 7b.4 — Delete the `critical_output` mute strategy (R1, audit 3.7, K13)

**Files:** `core/turn/mute.go`, `core/turn/processor.go:245`, `core/turn/mute_test.go`, `core/output_state.go:53`, `core/output_state_test.go:33,43`, `core/frame.go` (`SpeakingStateFrame.Uninterruptible`), `server/config.go:180`, `config/development.yaml:120`, `docs/pipecat-parity-matrix.md:28`.

- [ ] **Step 1 (RED):** `TestParseMuteStrategiesRejectsCriticalOutput` (`ParseMuteStrategies([]string{"critical_output"}, nil)` errors and the message lists the four names); server `TestTurnMuteConfigValidation` (`server/turn_mute_test.go:8`) gains the YAML case → start-up error.
- [ ] **Step 2:** delete the constant, the case, `TestMuteCriticalOutput`, the `critical` argument of `bot()`/`onBot()`, `botMute.critical`, `SpeakingStateFrame.Uninterruptible` and the two output-state assertions on it (the tracker's disposition logic for interruption stays). `git grep -n 'critical_output\|MuteCriticalOutput\|\.Uninterruptible\b'` → only `docs/superpowers` history. `cd core && go test -race ./...`; per-commit compile check.
- [ ] **Commit:** `refactor(core): drop the critical_output mute strategy (no producer)`. **Gate** (+`loadtest-smoke`). Release note: a YAML `turn.mute` listing `critical_output` now fails at start-up.

## Task 7b.5 — Per-flow interruptions: `turn.min_words`, `turn.mute`, `turn.max_mute_ms` (R2, D2)

**Files:** `server/flow/{call_behavior,config,definition,policy}.go` (+ tests), `server/config.go` (name the anonymous struct `TurnConfig`, yaml unchanged), `server/turn_flow.go` (new: `func (t TurnConfig) forCall(f *flow.TurnSettings) TurnConfig`), `server/call.go` (`CallSpec.Turn`), `server/registry.go` (use the merged config everywhere `cfg.Turn` is read in `BuildCallPipeline`), `server/providers.go`, `server/api/api.go` + `api_routes.go` (`turn` in server-config), `server/turn_flow_test.go` (new), `server/call_behavior_audio_e2e_test.go` (new, `//go:build xaispeech`), `ui/src/api/client.ts`, `ui/src/routes/flows/editor/TurnFields.tsx` (new, + test), `FlowSettingsPanel.tsx`, `settingCopy.ts`, `e2e/tests-flows/37-conversation-settings.spec.ts` (new), `docs/api/v1.md` (new "Turn-taking and interruptions" paragraph).

Validation (static, `DecodeConfig`): `turn` non-empty; `min_words` 0..10; `mute` unique, each one of the four (message names the index), `until_first_bot_complete` requires the step's trimmed `greeting`; `max_mute_ms` 1000..60000. Call level: `validateCallLevel` rejects `turn` on a non-start step. Policy (publish + readiness, K6): any `turn` member while `!TurnManager()` → reason `turn_manager_off`; `max_mute_ms` above `MaxMute()` → publish message only (D2).

`forCall` (K2, K3, K4): `MinWords`, `Mute` (clone), `MaxMute = min(server effective, flow)`. `BuildCallPipeline`: `tc := cfg.Turn.forCall(call.Turn)` at the top; replace every `cfg.Turn.X` in the function with `tc.X` (the `muteTools` condition `:606`, `ParseMuteStrategies` `:766`, `MaxMute` `:770`, `MinWords` `:746-747,771-775`); push-to-talk keeps ignoring `MinWords` and keeps the mute (today's rule, `:739-744`).

- [ ] **Step 1 (RED, Go unit):** `TestTurnSettingsValidation` table (every bound ±1, `0` min words OK, `[]` mute OK, duplicate, unknown, `critical_output` refused, `until_first_bot_complete` without greeting, `{}`, non-start step, negative values, `NaN`-free ints); policy: `turn_manager_off` readiness + publish, `max_mute_ms` 40000 on a 30 s server → publish refused, 20000 OK. `TestTurnForCall` table: absent → server copy; each member alone; `max_mute` min both ways; `Mute` result does not alias the server slice (mutate the result, the server's unchanged); `-race` with 8 goroutines calling `forCall` on one `*Config`. Builder: `TestBuildCallPipelineUsesTheFlowsTurnSettings` (turn processor built with the flow's mute when the server has none; `function_call` from the flow creates the tool activity).
- [ ] **Step 2 (RED, real stack, echo audio):** rig `bargeInRig` + `c.Providers.Echo.STT = "audio"`, STT model `latest_long` (interims, fact 13), `realVAD`. First measure and record how many interim words clip1 yields (expect 5–8); then pick thresholds far apart. `TestCallBehaviorAFlowThatMutesWhileTheAgentSpeaksIsNotInterrupted`: flow A `turn.mute: ["always"]`, flow B none, same server; `c.say(longReply)`, wait for ≥10 audio frames, `sendAudio(callerSpeech)`; B gets `msg:bot-interrupted`, A gets none and its reply plays to `bot-stopped-speaking`. `TestCallBehaviorAFlowsMinWordsIgnoresAShortBackchannel`: flow `min_words` > measured words → no `bot-interrupted`; flow `min_words: 1` → interrupted. Mutation targets below must turn these RED.
- [ ] **Step 3:** implement Go (incl. server-config `turn` block and `docs/api/v1.md`).
- [ ] **Step 4 (UI, RED first):** `TurnFields.tsx` "Interruptions" fieldset in Flow settings > Advanced (after Turn-taking): radio group "When can the caller interrupt?" — *Server setting (…)* (absent) / *Any time* (`min_words: 0`, and no `always` in mute) / *After N words* (number 1..10, hint "Needs a speech-to-text with interim results.") / *Never while the agent speaks* (`mute` contains `always`); checkboxes *Protect the agent's first reply or greeting* (`first_speech`, needs no greeting) and *Not while a tool runs* (`function_call`); `until_first_bot_complete` is API-only (no control) but a stored value is shown as a read-only note and kept on save (review item 7); "Longest mute (seconds)" 1..min(60, server). A note "Muted speech still reaches speech-to-text (billed) and recordings." Controls disabled with "This server has the turn manager off." when `serverConfig.turn.manager` is false. Copy ids: `turn.interruptions`, `turn.mute`, `turn.max_mute_ms`. Vitest: each choice writes exactly the JSON above (and removes `turn` when back to server setting), placeholders show the server's values, keyboard order, labels by role/name.
- [ ] **Step 5 (Playwright):** `37-conversation-settings.spec.ts` test "interruption settings are published, read back and shown after a reload": own flow via `api(stack, "/flows", …)` (echo `stage()` config), open editor, Flow settings > Advanced, choose *After N words* = 3, tick *Not while a tool runs*, longest mute 20 s, publish; `expect.poll` on `GET /flows/{id}` start step `turn` = `{min_words:3, mute:["function_call"], max_mute_ms:20000}`; `page.reload()`, sign in, values shown. No `waitForTimeout`.
- [ ] **Mutations:** `forCall` ignores `Mute` → mute e2e RED; ignores `MinWords` → min-words e2e RED; `MaxMute` takes the flow value unclamped → `TestTurnForCall` RED; drop the `turn_manager_off` reason → policy test RED.
- [ ] **Commits:** `feat(flow): per-flow interruption settings (turn.min_words, mute, max_mute_ms)`; `feat(ui): interruption settings in the flow editor`. **Gate** (+`loadtest-smoke`).

## Task 7b.6 — Per-flow end of turn: `speech_timeout_ms`, `end_of_turn`, `smart_hold_ms` (R3, D1)

**Files:** `server/flow/{call_behavior,policy}.go` (+ tests), `server/turn_flow.go` (+ test), `server/config.go` (`Analyzer.ForFlows bool yaml:"for_flows"`), `server/config_validate.go` (`validateTurn` checks analyzer settings also when `ForFlows`), `cmd/server/main.go:89` (`|| cfg.Turn.Analyzer.ForFlows`), `server/providers.go`, server-config `turn`, `config/development.yaml` (commented `for_flows`), `TurnFields.tsx` (+ test), `settingCopy.ts`, spec 37, `docs/api/v1.md`.

`forCall` (K5): `SpeechTimeout` from `speech_timeout_ms`; `Stop` = `{default: [], smart: [turn-analyzer, speech-timeout], eager: [eager, speech-timeout]}[end_of_turn]` plus `llm-completion` when the server's list has it; `Analyzer.Hold` from `smart_hold_ms`. The analyzer is built when the merged `Stop` lists `turn-analyzer` and `r.turnBackend != nil`; with a nil backend the build fails with the existing error (`turn_stop.go:47`), which readiness prevents (K6).

- [ ] **Step 1 (RED):** validation table (bounds, `smart_hold_ms` without smart, unknown `end_of_turn`, zero/negative); policy: `smart` with `SmartTurn() == false` → readiness `smart_turn_unavailable` + publish refused ("Smart end-of-turn needs the Smart Turn model on this server (turn.analyzer.for_flows)"); `TestTurnForCall` adds: each `end_of_turn` → exact chain, server `[llm-completion, speech-timeout]` + flow `eager` → `[eager, speech-timeout, llm-completion]`, `smart_hold_ms` → `Analyzer.Hold`; `turnStopDetector` accepts every produced chain. Config: `for_flows: true` with a bad analyzer mode fails validation; `main.go` condition covered by a small `smartTurnWanted(cfg)` helper test (keep it in `server` as `cfg.LoadsSmartTurn()`, used by `main.go` and `policyFrom`).
- [ ] **Step 2 (real stack):** `TestCallBehaviorPublishRefusesSmartEndOfTurnWithoutTheModel` (echo rig has no model): 400 with the message; a stored flow made `smart` on a rig with `ForFlows` and the fake backend `smartBackend` (`server/turn_stop_test.go:105`, `s.SetTurnBackend(smartBackend{p: 0.9})` as `registry_test.go:523`) is `ready` and a call connects and replies. `TestCallBehaviorAFlowsSpeechTimeoutDelaysTheReply` is **not** added (timing-sensitive on echo); the builder unit test pins the value instead (reported as a coverage note).
- [ ] **Step 3 (UI, RED first):** in the Turn-taking fieldset after `stt_p99_ms`: "Wait for the caller to resume (ms)" number 200..3000 (placeholder = server `speech_timeout_ms`); select "End-of-turn detection": *Server setting (…)* / *Standard: wait for silence* (`default`) / *Smart: a model judges if the caller is done* (`smart`; option disabled with "(not available on this server)" when `smart_turn_available` is false) / *Fast: end on a full sentence* (`eager`; copy warns it can split "Book a table. For two." into two turns); "Smart hold (ms)" 500..5000 shown only for smart. Copy ids `turn.speech_timeout_ms`, `turn.end_of_turn`, `turn.smart_hold_ms`. Vitest as 7b.5.
- [ ] **Step 4 (Playwright):** spec 37 test "end-of-turn settings…": the Smart option is disabled on the echo stack (no model: the readiness truth, visible); choose Fast + resume 900 ms, publish, API read-back, reload, values shown.
- [ ] **Mutations:** `forCall` drops `llm-completion` → table RED; `SmartTurn()` always true → publish e2e RED.
- [ ] **Commits:** `feat(flow): per-flow end-of-turn timing and strategy`; `feat(server): turn.analyzer.for_flows loads Smart Turn for flows`; `feat(ui): end-of-turn settings in the flow editor`. **Gate** (+ per-commit compile check: `cmd/server`).

## Task 7b.7 — Background sound on any step (R5a, K7)

**Files:** `server/flow/{call_behavior,definition,policy}.go` (+ tests: non-start `audio` may carry only `background`; policy checks every step's background), `server/call.go` (`CallFlow.Backgrounds map[string]*flow.BackgroundSettings`, steps that set one, start included), `server/server.go:553-561` (OnRoute wrapper: on `RouteActive`, if `To` has one, `conn.SetBackground(s.backgroundFor(&flow.AudioSettings{Background: bg}))`), `server/flow_e2e_test.go`, `ui/src/routes/flows/editor/{model.ts,StepPanel.tsx,CallBehaviorFields.tsx}` (extract `BackgroundFields` used by `AudioFields` and the step panel), `settingCopy.ts` (`audio.step_background`), spec 37, `docs/api/v1.md:56,67`, `docs/audio-filters.md`.

- [ ] **Step 1 (RED):** definition: `sales` with `audio: {background: {name: "office"}}` decodes; with `denoise` → `step "sales": audio may only set background on a step other than the start step`; policy: unknown or disallowed background on `sales` → publish refused. `TestFlowE2EAStepsBackgroundStartsWhenTheRouteArrives` (rig with background `office` + `allow_flow_background`, echo energy as `TestCallBehaviorAFlowsBackgroundSoundIsHeardAtItsLevel…`, `call_behavior_e2e_test.go:120`): start step none, `sales` office → silent frames before the route are near zero, after `RouteActive` the bed is heard (peak > threshold) while the bot is quiet; a step without `background` keeps the current bed (route `sales`→`support` keeps office).
- [ ] **Step 2:** implement; `toPayload` keeps `audio.background` on non-start steps (removes the other audio members); `setStart` moves the call-level audio members and leaves each step's background in place. StepPanel Advanced: "Background sound" select: *Keep the current sound* (absent) / *None* / server names (same disabled-with-reason as start when flows may not choose).
- [ ] **Step 3 (Playwright):** spec 37: give step 2 "office", publish, API read-back, reload, shown.
- [ ] **Mutation:** wrapper ignores `RouteActive` → e2e RED.
- [ ] **Commits:** `feat(flow): a step can switch the background sound`; `feat(ui): background sound per step`. **Gate.**

## Task 7b.8 — Hold sound while a slow tool runs (R5b, K8)

**Files:** `server/flow/call_behavior.go` (`AudioSettings.ToolCue`, validation as `route_cue`), `server/flow/policy.go` (name in `Cues()`), `server/cue.go` (hold mode: `holdStart()`, `holdDone()` with an atomic in-flight count; `botAudio`/caller speech pause and re-arm instead of ending; `cap` 30 s; `blocked func() bool` = route cue waiting), `server/cue_test.go`, `server/call.go` (`CallSpec.isMCP func(string) bool` from `rs.MCPTool`), `server/registry.go` (`prepLLM` wraps `ToolExecutor` with `holdTools(exec, defs, call.toolHold, call.isMCP)` for sync MCP tools), `server/server.go` (create the hold controller from `call.Audio.ToolCue`, finish it on `RoutePreparing`, feed bot audio and caller speech), `server/mcp_call_e2e_test.go`, `CallBehaviorFields.tsx` (`AudioFields`: "Hold sound" select + "Start after (ms)"), `settingCopy.ts` (`audio.tool_cue`), spec 37, `docs/api/v1.md`, `docs/audio-filters.md`.

- [ ] **Step 1 (RED, unit):** `cue_test.go` with the fake player: sync tool starts → nothing before `after`, cue starts after quiet `after`; bot audio pauses it and it re-arms; two parallel tools → stops only when both return; tool cancelled (ctx) → stops; route preparing → stops and does not restart; `blocked()` true → never starts; cap → stops. `holdTools`: async tool, transfer tool, `end_call`, `send_dtmf` never arm it (table).
- [ ] **Step 2 (RED, real stack):** `TestMCPCallAHoldSoundPlaysWhileASlowToolRuns` (name matches the `MCPCall` filter of `make test-e2e-go`): `newMCPCallRig` with `c.Audio.Cues["chime"]` (the WAV helper the `FlowE2ECue…` tests use) and the slow test tool as in `TestMCPCallBargeInCancelsSlowTool` (`mcp_call_e2e_test.go:175`: `c.say(`call tool slow_lookup {"delay_ms":2000}`)`), `tool_cue {name: chime, after_ms: 300}`: cue frames (`c.cueFrames()`, `flow_e2e_test.go:335`) appear during the call and stop before the reply's audio; timeline `cue_start`/`cue_stop` with reason `tool_done`; same tool marked async → no cue frames. Publish with an unknown cue → 400.
- [ ] **Step 3:** implement; UI + Playwright (spec 37: choose "chime", after 1000 ms, publish, read back, reload).
- [ ] **Mutations:** `holdTools` arms for async tools → table RED; `holdDone` on first of two tools → unit RED; no stop on route preparing → unit RED.
- [ ] **Commits:** `feat(server): hold sound while a slow tool runs (audio.tool_cue)`; `feat(ui): hold sound setting`. **Gate.**

## Task 7b.9 — `end_call` on a one-step flow (R6, K9)

**Files:** `server/flow/definition.go:133-135` (drop the rule), `server/flow/definition_test.go`, `server/flow_call.go` (move `CanEnd`/`End` from `CallFlow` to `CallSpec.Ender *callEnder{End func(stepID string) error}`; `stepToolset` takes it), `server/call.go` (`callSpec`: one step with `EndCall` → tools = captured MCP tools + `endCallDefinition()`, executor = `stepExecutor(rs.Executor, allMCP, nil, nil, end)`), `server/registry.go:812-829` (force idle and set `End` when `call.Ender != nil`), `server/flow_e2e_test.go`, `ui/src/routes/flows/editor/{StepPanel.tsx,model.ts}` (+ tests), `docs/api/v1.md`, `docs/demo/dentistry.md:13` (unchanged meaning; check).

- [ ] **Step 1 (RED):** definition: one step + `end_call: true` decodes. `TestFlowE2EAOneStepFlowCanEndTheCall` (mirror `TestFlowE2EEndCallEndsTheCallAfterTheGoodbye`, `:800`): echo phrase that calls `end_call` → goodbye spoken, call ends, `end_reason` `ended_by_flow`; the same one-step flow without the flag keeps the call (the tool is absent: echo's call gets `unknown_tool`). Vitest: checkbox enabled with one step; `toPayload` of a one-step "main" flow with `end_call` is steps form with `end_call: true` (`isFlat` false); without it stays flat.
- [ ] **Step 2:** implement; remove the "Add a second step first" copy.
- [ ] **Step 3 (Playwright):** spec 37: one-step flow, tick "Can end the call", publish, API read-back shows `steps[0].end_call`, reload, ticked.
- [ ] **Mutations:** `callSpec` ignores the one-step flag → e2e RED; `isFlat` ignores `end_call` → Vitest RED.
- [ ] **Commits:** `feat(flow): a one-step flow can have the end_call tool`. **Gate.**

## Task 7b.10 — Per-flow call limit that only tightens (R6, K10)

**Files:** `server/flow/config.go` (`MaxCallDurationSeconds`, static 0 or 60..86400), `server/flow/definition.go` (`validateCallLevel`), `server/flow/policy.go` (above `MaxCallDuration()` → publish refused), `server/call.go` (`CallSpec.MaxCallDuration = min(server, flow)`), `server/server.go:786-803` (timer reads it), tests, `FlowSettingsPanel.tsx` ("Longest call (minutes)", max = server's), `settingCopy.ts` (`max_call_duration_seconds`), spec 37, `docs/api/v1.md`.

- [ ] **Step 1 (RED):** validation (59, 60, negative, non-start step); policy (above server → message naming both values); `TestCallSpecTakesTheShorterCallLimit` (flow 600 s / server 30 min → 600 s; flow absent → server; server lowered below the flow → server). `TestMaxCallDurationFiresOnceWithReason` keeps passing (it now reads the `CallSpec` value). `TestCallBehaviorThePublishAPIRefusesALongerCallLimit` → 400. (No e2e waits 60 s: the timer path is the existing test's; the `min` is unit-pinned; recorded as a coverage note.)
- [ ] **Step 2:** implement; UI minutes ↔ seconds, placeholder = server minutes, error when above.
- [ ] **Step 3 (Playwright):** spec 37: 10 minutes, publish, API `600`, reload, shown.
- [ ] **Mutation:** use the flow value without `min` → `TestCallSpecTakesTheShorterCallLimit` RED.
- [ ] **Commit:** `feat(flow): a flow can shorten the call limit`. **Gate.**

## Task 7b.11 — Phase gate, adversarial review, PR notes

- [ ] **Step 1:** `git diff --stat $(cat $P/base-7b.0)..HEAD | tail -1`; numstat per area (Go non-test, Go test, ui, e2e, docs).
- [ ] **Step 2:** full gate block plus `make test-postgres` (guard; expected unchanged), `make loadtest-smoke`; Playwright twice in a row (`make e2e-fake` ×2) for spec 37 and the edited 20; the timing-sensitive new Go e2e tests (7b.5's two audio tests, 7b.7's background, 7b.8's hold sound) 5× each (`go test -race -count=5 -tags xaispeech -run '<names>' .` in `server/`), all results reported.
- [ ] **Step 3: adversarial review (independent agent, opus) of `main...HEAD`.** Hunt for:
  1. **Contract drift:** Go struct tags vs zod vs `docs/api/v1.md` vs `settingCopy` ranges for every field; server-config `turn`/`cues` shape; readiness reason strings identical everywhere.
  2. **"Can only tighten" bypasses:** `max_mute_ms`, `max_call_duration_seconds`: publish refusal, call-time `min`, a server lowered after publish, a draft test call, a duplicate, a flat config, a non-start step carrying them; `min_words: 0` cannot loosen anything server-protected.
  3. **Zero/negative/absent values:** every pointer field absent vs `0` vs negative; `mute: []` vs absent; `{}` objects; JSON `null`; huge ints.
  4. **Per-step vs call-level leakage across route handoffs:** `turn`, `tool_cue`, call limit, clean text, failover on a non-start step (refused at publish; ignored if stored); background inheritance (K7) incl. a route back to start and a failed route; hold cue across a route; `send_dtmf` on every step exactly once; `end_call` on one step without a router.
  5. **Readiness lies:** `smart_turn_unavailable` and `turn_manager_off` both ways (server changed after publish); `SmartTurn()` from config equals what `main.go` loads; no setting accepted that a call silently ignores (push-to-talk documented).
  6. **Races and shared state:** `forCall` never writes `cfg.Turn` slices; hold controller counters/timers under `-race`; `SetBackground` from the route callback goroutine; no lock across I/O; timers stopped on all exits.
  7. **UI/server schema drift and UX:** a stored value the UI cannot show (unknown cue/sound, a mute set the radio cannot express, e.g. `["always","first_speech"]` must render faithfully, not be rewritten on load); disabled options say why; a11y (labels, radio group name, keyboard), tokens only.
  8. **YAGNI:** no knob beyond the schema; `holdTools` and `forCall` the only new helpers; `TurnConfig` naming is the only refactor; deleted `critical_output` leaves no reference.
  9. **Docs:** `docs/api/v1.md`, `docs/audio-filters.md`, `config/development.yaml`, parity matrix, the audit's rows 1.5, 1.9, 1.10, 2.11, 2.14, 2.15, 3.2, 3.4–3.7, 4.1, 5.10, 5.17, 7.3, 7.5 (append a "Status after Phase 7b" line to the audit).
- [ ] **Step 4:** fix round (own commits + scoped re-review). Ledger line + `p7b/pr-notes.md`: (a) user-visible: Interruptions and End-of-turn controls, route/hold sound selects, background per step, end call on one step, call limit, two settings moved to Flow settings; (b) API: new flow fields, `GET /server-config` `audio.cues` + `turn`, readiness reasons `smart_turn_unavailable`, `turn_manager_off`, publish refusals (clean text/failover on a non-start step, unknown route/hold cue, limits above the server's); (c) config: `turn.analyzer.for_flows` (D1), `critical_output` removed (start-up error); (d) library API: `SpeakingStateFrame.Uninterruptible` and `turn.MuteCriticalOutput` removed; (e) decisions D1/D2 as answered, rulings K1–K13; (f) coverage notes (speech timeout and call-limit tightening pinned by unit tests, not timed e2e; denied smart path visible on echo only as a disabled option); (g) rollback note (K1).

---

## Audit corrections found while planning

| Audit claim | Finding |
|---|---|
| R3 "`eager` only with an STT the catalogue marks punctuating" | No such mark exists (fact 9). Ruling K5: no gate; the fallback timeout is always in the chain; copy warns. |
| R3 smart readiness "when the server loaded the model" | Loaded only when the server's own default is smart (fact 6): per-flow smart is otherwise dead. D1. |
| R1 "reject in `validateCallLevel`" for clean text / failover, "migration note (no deployed data)" | `validateCallLevel` runs in `Decode`, so stored versions written by the editor would become `invalid_config`. Ruling K1: publish-time only. |
| R6 "drop the ≥ 2 steps rule; build the end hook" | Four more places: `callSpec` builds `CallFlow` only with > 1 step, idle `force`, UI checkbox/`toPayload`, and `isFlat` saving a one-step flow flat (fact 8). |
| R5a "`Router` route-active calls `SetBackground`" | Also: `validateCallLevel` rejects `audio` wholesale and the editor strips/moves `audio` as call-level (`model.ts:40,258,331`). |
| R5b "armed by the `turn.ToolActivity` the server already creates" | It exists only with idle or `function_call` mute, counts transfer/`end_call`/`send_dtmf`, and has no start/stop callback; and the route cue shares the one `CueUnderlay` whose `Stop` fades any current cue (fact 7, 10). K8. |
| R2 `min_words` "0 (server) or 1..10" | "0 = use server" cannot express "off" on a server that sets it. K3: pointer, absent = server, 0 = off. |
| 3.7 "delete `critical_output`" | Also leaves `SpeakingStateFrame.Uninterruptible` and the `critical` plumbing dead (fact 4). K13 deletes them. |
| Brief: "D-0.6/B2 deferring the turn settings" | D-0.6 is flow version history (deep-review B2); the turn deferral is "B2" of the per-flow call-behaviour design. See "What this plan supersedes". |
| 1.10 test impact | `TestFlowE2EUnknownCueNameStillHandsOff` and `20-flow-lifecycle.spec.ts:49` must change (fact 3). |
