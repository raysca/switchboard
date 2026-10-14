# Turn Latency (S9) and Provider Test (S15/S16) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The server stores the latency of each reply on its turn (S9), and an operator can check a provider on demand from the Providers screen (S15, shaped with S16): one click sends one tiny real request per service the provider offers and shows ok/error, how long it took, and the circuit-breaker state. Nothing runs in the background.

**Architecture:** S9 reuses what the server already measures: `core/observe` computes `stt_final`, `llm_ttft`, `tts_ttfb` and `voice_to_voice` per turn and feeds Prometheus histograms (`server/server.go` observer sink); nothing is stored. A small bounded per-call collector, keyed by turn id, is written by the sink and read by the turn recorder when it builds a turn row (`latencies_json` exists, is NOT NULL, and is `{}` today). S15 is a live probe: `Registry.BuildStage` (the same factory a call uses) builds the provider's stage processor from the catalog defaults and the environment secret, a throwaway one-processor session drives one minimal request, and the result is returned by `POST /api/v1/providers/{id}/test`. There is no collection, no rolling window and no polling: the console asks only when the operator clicks.

**Tech Stack:** Go (`server/`, `core/observe`, `Registry.BuildStage`), SQLite/Postgres unchanged (no migration), React 19 + TypeScript + vitest, Playwright (`make e2e-echo`).

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (rows S9, S15, S16; Call detail and Providers boards). S15 is revised from "provider health in the management API" (a passive hour window) to an on-demand check, at the user's direction; the spec rows are updated in Task 7.

## Global Constraints

- Providers are preconfigured and secrets are only read from the environment. Health never exposes a secret, an env value, a model name or an endpoint URL (AGENTS.md; memory `provider-secrets-env-only`).
- A probe sends a fixed, harmless prompt (no caller data), reads its secret from the environment at the moment of the click, and never puts the secret, a URL with credentials or a raw provider error body in a response or log (errors pass `s.redactor.Redact` and are cut to 300 bytes).
- Every queue and map is bounded (AGENTS.md "Resource Bounds"). Hot path (the observer sink, 50 frames/s) takes one short mutex or atomics and allocates nothing per frame.
- Monotonic time only for windows and durations (`time.Since`); no `UnixNano` interval maths.
- `core/` stays server-agnostic: no change to `core/` is needed; `server/` consumes `observe.Measurement` as it does today.
- All code passes `go test -race ./...`; `gofmt -w .`; conventional commits; final independent Opus review (AGENTS.md).
- Dual dialect: nothing here touches SQL.

## Review Focus

1. **A turn with no measurements** (barge-in before any audio, text-only turn, tool-only reply) stores `{}` and the UI shows no pills for it rather than zeros or `NaN`. Pinned in Task 1 and Task 3.
2. **Measurement and turn row arrive in either order** (the observer goroutine is separate from the recorder): a row built before its `voice_to_voice` arrives stores what exists; a measurement for a turn that never gets a row does not leak (bounded). Pinned in Task 1.
3. **Clicking Test connection must not hurt live calls.** Probes are rate limited (one at a time and at least 5 s apart per provider), time-boxed (15 s), and a hung provider cannot pin goroutines after the timeout. A probe that fails shares the live breaker (Ruling 3): pinned by a test that five failing probes in a row cannot be issued faster than the limit and that a client error (bad key) does not open the breaker.
4. **A provider with no credential** answers `409 provider_not_detected` and sends nothing; a provider id that does not exist is `404`. Pinned in Task 5.
5. **No secret in any response, log or error.** A probe against a provider that rejects a canary key returns an error that does not contain it. Pinned in Task 4.

## Rulings made while planning (re-read before Task 1)

1. **Keys and units.** `latencies_json` is `{"stt_final_ms":N,"llm_ttft_ms":N,"tts_ttfb_ms":N,"voice_to_voice_ms":N}`, integer milliseconds, only the stages measured for that turn. `startup` has no turn and is not stored. The existing UI pills read `vad_ms/stt_ms/llm_ms/tts_ms`; those keys were never written, so Task 3 replaces them (no compatibility shim).
2. **On demand, not collected (user decision).** The passive rolling window, the per-call health feeding and the polling are dropped. S15's "p50" and "Last hour" figures become the result of the last click: a probe's time to first token / first audio. The Providers list gets no new column.
3. **Probes use the live breakers.** The probe builds its processor through the same factory, which picks the shared breaker for that provider and stage; a probe failure that counts against a breaker (server errors, timeouts) counts, which is also true signal. Client errors (a bad key) do not count. Cost if wrong: an operator hammering a dead provider could open the breaker a few seconds sooner than traffic would; the 5 s rate limit bounds it to one failure per 5 s. The result reports the breaker state read before and after.
4. **What a probe sends.** LLM: system prompt `Reply with the single word ok.`, user `ping`, success at the first streamed token, stop after it. TTS: the text `ok`, success at the first audio frame. STT: 200 ms of silence in the stage's input format, then end of stream; success when the stream opens, takes the audio and ends without error (no transcript is expected; latency is the time to open). Model and voice are the catalog defaults for that provider and service; a provider with no default model is reported `skipped` with the reason, never guessed. A few tokens or a short synthesis per click.
5. **Test call panel and First run chips (S9 "reply-time chips", "median reply" in the panel) are out of scope:** they need turns while a call is live. Call detail gets the figures; the Calls list gets none (a list median would need a session-level aggregate).
6. **Echo** answers probes like any provider (free, instant, no secret) so the browser suite exercises the whole path.

---

### Task 1: The per-call turn latency collector

**Files:**
- Create: `server/turnlatency.go`
- Test: `server/turnlatency_test.go`

**Interfaces:**
- Produces:
  ```go
  type turnLatencies struct { /* mu, per map[core.TurnID]map[observe.Stage]time.Duration */ }
  func newTurnLatencies() *turnLatencies
  func (t *turnLatencies) observe(m observe.Measurement)   // called from the observer sink
  func (t *turnLatencies) take(turn string) string         // JSON object for the turn, "{}" if none; forgets the turn
  const maxTurnLatencies = 32
  ```

- [ ] **Step 1: Write the failing tests**

```go
package server

import (
	"encoding/json"
	"testing"
	"time"

	"govoice/core"
	"govoice/core/observe"
)

func TestTurnLatenciesStoreWhatWasMeasuredInMilliseconds(t *testing.T) {
	l := newTurnLatencies()
	l.observe(observe.Measurement{Stage: observe.StageSTTFinal, Turn: "t1", Value: 180 * time.Millisecond})
	l.observe(observe.Measurement{Stage: observe.StageLLMTTFT, Turn: "t1", Value: 420 * time.Millisecond})
	l.observe(observe.Measurement{Stage: observe.StageVoiceToVoice, Turn: "t1", Value: 900 * time.Millisecond})
	l.observe(observe.Measurement{Stage: observe.StageStartup, Value: time.Second}) // no turn: not stored
	var got map[string]int64
	if err := json.Unmarshal([]byte(l.take("t1")), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"stt_final_ms": 180, "llm_ttft_ms": 420, "voice_to_voice_ms": 900}
	if len(got) != len(want) || got["stt_final_ms"] != 180 || got["llm_ttft_ms"] != 420 || got["voice_to_voice_ms"] != 900 {
		t.Fatalf("got %v, want %v", got, want)
	}
	if l.take("t1") != "{}" {
		t.Fatal("a turn must be forgotten once taken")
	}
}

func TestATurnWithNoMeasurementsIsAnEmptyObject(t *testing.T) {
	if got := newTurnLatencies().take("never-measured"); got != "{}" {
		t.Fatalf("got %q", got)
	}
}

func TestTurnLatenciesAreBounded(t *testing.T) {
	l := newTurnLatencies()
	for i := 0; i < 10*maxTurnLatencies; i++ {
		l.observe(observe.Measurement{Stage: observe.StageVoiceToVoice, Turn: core.TurnID(string(rune('A' + i))), Value: time.Millisecond})
	}
	l.mu.Lock()
	n := len(l.per)
	l.mu.Unlock()
	if n > maxTurnLatencies {
		t.Fatalf("%d turns held, cap %d", n, maxTurnLatencies)
	}
}
```

- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -run TurnLatenc ./` — Expected: FAIL (`undefined: newTurnLatencies`).

- [ ] **Step 3: Implement**

```go
package server

import (
	"encoding/json"
	"sync"
	"time"

	"govoice/core"
	"govoice/core/observe"
)

// maxTurnLatencies bounds the turns held while they wait for their reply row.
const maxTurnLatencies = 32

// stageKeys name the stored stages; startup has no turn and is not stored.
var stageKeys = map[observe.Stage]string{
	observe.StageSTTFinal:     "stt_final_ms",
	observe.StageLLMTTFT:      "llm_ttft_ms",
	observe.StageTTSTTFB:      "tts_ttfb_ms",
	observe.StageVoiceToVoice: "voice_to_voice_ms",
}

// turnLatencies collects a call's per-turn stage latencies from the observer
// goroutine for the turn recorder, which takes a turn's figures when it writes
// that turn's row.
type turnLatencies struct {
	mu  sync.Mutex
	per map[core.TurnID]map[observe.Stage]time.Duration
}

func newTurnLatencies() *turnLatencies {
	return &turnLatencies{per: map[core.TurnID]map[observe.Stage]time.Duration{}}
}

func (t *turnLatencies) observe(m observe.Measurement) {
	if m.Turn == "" {
		return
	}
	if _, ok := stageKeys[m.Stage]; !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.per[m.Turn]
	if st == nil {
		if len(t.per) >= maxTurnLatencies {
			clear(t.per) // ponytail: turns that never got a row; dropping all beats tracking age
		}
		st = map[observe.Stage]time.Duration{}
		t.per[m.Turn] = st
	}
	st[m.Stage] = m.Value
}

// take returns the turn's figures as a JSON object (integer milliseconds) and forgets the turn.
func (t *turnLatencies) take(turn string) string {
	t.mu.Lock()
	st := t.per[core.TurnID(turn)]
	delete(t.per, core.TurnID(turn))
	t.mu.Unlock()
	out := make(map[string]int64, len(st))
	for stage, d := range st {
		out[stageKeys[stage]] = d.Milliseconds()
	}
	b, _ := json.Marshal(out) // a map of strings to integers cannot fail
	return string(b)
}
```

- [ ] **Step 4: Run to see them pass.** Same command — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): collect each turn's stage latencies for its row`.

---

### Task 2: Store them on the turn row

**Files:**
- Modify: `server/call.go` (`newTurnRecorder`, `resolveCall`, `CallSpec`), `server/server.go` (observer sink at ~line 676)
- Modify: `server/call.go` `CallSpec` struct lives where `CallSpec` is declared (`grep -n "type CallSpec" server/*.go`)
- Test: `server/turnlatency_e2e_test.go`

**Interfaces:**
- Consumes: `turnLatencies` (Task 1).
- Produces: `CallSpec.TurnLatencies *turnLatencies` (set by `resolveCall`); `newTurnRecorder(addTurn, sessionID, logger, lat *turnLatencies)`; stored `storage.TurnRecord.LatenciesJSON`.

- [ ] **Step 1: Write the failing test** (Go e2e, in the style of `drafts_test.go`: `newFlowRigE2E`, an echo flow, `dial`, `say`, `waitReplies`). The echo STT/LLM/TTS run the real pipeline so the observer measures real stages.

```go
func TestAReplyIsStoredWithItsLatencies(t *testing.T) {
	r := newFlowRigE2E(t, newProbe())
	f, err := r.s.flows.Create(t.Context(), flow.CreateRequest{Name: "Latency", Config: json.RawMessage(echoFlowConfig("LAT"))})
	if err != nil || f.Readiness.State != flow.ReadinessReady {
		t.Fatalf("flow: %+v err %v", f.Readiness, err)
	}
	c := r.dial(f.ID)
	c.say("hello")
	c.waitReplies("[LAT", 1)
	eventually(t, "a turn row with latencies", func() bool {
		turns, err := r.s.store.Sessions().Turns(t.Context(), c.sessionID)
		if err != nil || len(turns) == 0 {
			return false
		}
		var got map[string]int64
		if json.Unmarshal([]byte(turns[0].LatenciesJSON), &got) != nil {
			return false
		}
		return got["voice_to_voice_ms"] > 0 && got["llm_ttft_ms"] > 0
	})
}
```
(If the store method is not called `Turns`, use the one `history.go:315` implements: `grep -n "FROM turns WHERE session_id" server/storage/history.go`.)

- [ ] **Step 2: Run to see it fail.** `cd server && go test -race -count=1 -run AReplyIsStoredWithItsLatencies .` — Expected: FAIL (`{}` stored).
- [ ] **Step 3: Implement.** In `resolveCall`: `lat := newTurnLatencies()`; pass `lat` to `newTurnRecorder(…, lat)`; set `call.TurnLatencies = lat`. In `turnRecorder`, keep the field and in the `AssistantMessageFrame` case set `row.LatenciesJSON = r.lat.take(m.Turn)`. In the observer sink in `server.go`, next to `s.metrics.ObserveLatency(...)`, add `call.TurnLatencies.observe(m)` (nil-safe: guard with `if call.TurnLatencies != nil`, since non-flow test paths build a `CallSpec` without one).
- [ ] **Step 4: Run to see it pass.** Same command plus `go test -race -count=1 ./...` in `server/` — Expected: PASS. Fix any caller of `newTurnRecorder` (tests) by passing `newTurnLatencies()`.
- [ ] **Step 5: Commit** `feat(server): turn rows carry their stage latencies`.

---

### Task 3: Show them in the console

**Files:**
- Modify: `ui/src/components/timeline/TurnTimeline.tsx` (pills), `ui/src/routes/sessions/callStats.ts` (`medianReplyMs`), `ui/src/routes/sessions/CallView.tsx` (a "Median reply" figure), `ui/src/routes/sessions/CallsView.tsx` (a column), tests beside each.
- Test: `ui/src/components/timeline/TurnTimeline.test.tsx`, `ui/src/routes/sessions/callStats.test.ts`, `CallView.test.tsx`, `CallsView.test.tsx`

**Interfaces:**
- Consumes: `TurnDTO.latencies` JSON from Task 2.
- Produces: `medianReplyMs(turns: TurnDTO[]): number | null` in `callStats.ts` (median of `voice_to_voice_ms` over turns that have it; `null` when none).

The Calls list does not load turns for every row, so its column shows the figure only if the sessions list API carries it. It does not; **leave the Calls list column out** and show the median in Call detail only (Ruling 5 widened: list median would need a session-level aggregate, i.e. a new column or query; not worth it now).

- [ ] **Step 1: Write the failing tests.**
  - `callStats.test.ts`: `medianReplyMs([])` is `null`; turns with `latencies: {voice_to_voice_ms: 900}`, `{voice_to_voice_ms: 300}`, `{}` and `undefined` give `600` (median of 300 and 900; a turn without the key is skipped); an odd count gives the middle value.
  - `TurnTimeline.test.tsx`: a turn with `latencies: {stt_final_ms: 180, llm_ttft_ms: 420, tts_ttfb_ms: 300, voice_to_voice_ms: 900}` shows pills `Heard 180 ms`, `Model 420 ms`, `Voice 300 ms`, `Total 900 ms`; a turn with `{}` shows no `latency-bar` at all (Review Focus 1).
  - `CallView.test.tsx`: figures include `Median reply` with `0.6 s`-style text (use the same formatter as the neighbouring figures, `grep -n "mmss\|toFixed" CallView.tsx`) when the turns have latencies, and the figure is absent when none do.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/components/timeline src/routes/sessions` — Expected: FAIL.
- [ ] **Step 3: Implement.** `medianReplyMs` sorts the present values and averages the middle pair. `TurnTimeline`: replace the four old pills with the four new keys and labels above, each rendered only when present, and render the `latency-bar` only when at least one is present. `CallView`: add the figure next to the existing ones.
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): per-turn latency pills and a median reply figure in Call detail`.

---

### Task 4: The probe

**Files:**
- Create: `server/probe.go`
- Test: `server/probe_test.go`
- Read first (do not import: `server` must not depend on `tools/`): `tools/provtest/{suite,llm,tts,stt,run}.go` show the exact frames each stage takes and gives; the probe is a strict one-request subset of those checks.

**Interfaces:**
- Consumes: `Registry.BuildStage(cfg, stage, spec)`, the catalog defaults (`server/catalog`), `Resolver.Secret(provider, stage)`, `Metrics.circuitState` (below).
- Produces:
  ```go
  type ProbeResult struct {
  	Stage     string `json:"stage"`     // "stt" | "llm" | "tts"
  	Status    string `json:"status"`    // "ok" | "error" | "skipped"
  	LatencyMS *int64 `json:"latency_ms,omitempty"` // to first token / first audio / stream open; nil unless ok
  	Detail    string `json:"detail,omitempty"`     // error class + redacted message, or the skip reason
  	Circuit   string `json:"circuit"`              // "closed" | "open" | "half_open" | "unknown", after the probe
  }
  func (s *Server) probeStage(ctx context.Context, provider, stage string) ProbeResult
  func (m *Metrics) circuitState(provider, stage string) string
  const probeTimeout = 15 * time.Second
  ```

- [ ] **Step 1: Write the failing tests** (`server/probe_test.go`). Register fake factories under a throwaway provider name on a `NewRegistry()` as `buildstage_test.go` does (`reg.Register("probe.llm", …)`), with small test doubles in the same file that implement `core.Processor`:
  - a fake LLM that emits one `LLMTextFrame`/token frame after 20 ms → `probeStage` returns `ok` with `LatencyMS >= 20`;
  - a fake TTS that emits one audio frame; a fake STT that accepts audio and ends cleanly → `ok`;
  - a fake that never answers → `error` with `Detail` starting `timeout`, returned within `probeTimeout` (set a shorter package variable `probeTimeout` for the test), and `runtime.NumGoroutine()` returns to its starting value afterwards (no leaked task);
  - a fake whose build fails with an error containing the canary `sk-secret-canary` → `Detail` does not contain it (Review Focus 5), and an error longer than 300 bytes is cut;
  - a provider with no catalog default model for `llm` → `skipped`, `Detail` says no default model;
  - `circuitState`: `m.LLMCircuitStateGoogle.Store(1)` → `"open"`, `2` → `"half_open"`, `0` → `"closed"`, `("echo","llm")` → `"unknown"`; a dynamic breaker (`m.circuits.set("llm","nvidia","host:443",1)`) → `"open"` for `("nvidia","llm")`, and with two endpoints the **worst** state wins.
  Match the real frame types by reading the provtest `fake.go` doubles (`FakeLLM.reply`, `FakeTTS.speak`, `FakeSTT.ProcessFrame`), which drive exactly these frames.
- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -run 'Probe|CircuitState' .` — Expected: FAIL (undefined).
- [ ] **Step 3: Implement `server/probe.go`.**
  1. Resolve model/voice/language from the catalog default for `(provider, stage)`; none → `skipped`.
  2. `secret, err := s.resolver.Secret(provider, stage)`; on error → `error` (`Detail` = the resolver's message, which names the missing variable, never a value). `defer clear(secret)`.
  3. `proc, err := s.registry.BuildStage(s.cfg, stage, StageSpec{Provider: provider, Model: …, Voice: …, Language: …, Secret: secret, SystemPrompt: probePrompt})`.
  4. Run it in a throwaway session: `core.NewPipeline(proc, sink)` where `sink` is a tiny `core.Processor` that records the first matching frame time and signals a channel; `core.NewSession(probeSessionID, pipe, core.DefaultResourceBudget())`, `Start()`, inject the stage's input frame (see Ruling 4 and the provtest drivers), wait on `ctx.Done()`/the first-frame channel/a session error, then `Close()` in a deferred call that always runs and waits for the session (`LeakedTasks() == 0` is asserted in the timeout test).
  5. Latency is `time.Since(start)` (monotonic) from injection to the first frame.
  6. Errors: `class := ClassifyStageError(stage, err)`; `Detail = class + ": " + truncate(s.redactor.Redact(err.Error()), 300)`; a context deadline is `timeout`.
  7. `Circuit: s.metrics.circuitState(provider, stage)` read after the run. `circuitState` maps the six static atomics (`STT/LLM/TTS` × `Google/XAI`, the `add("google", "stt", …)` list at `server/server.go:789`) plus `m.circuits.snapshot()` entries matching provider and stage, using `circuitCode`'s codes; `"unknown"` when nothing matches.
  Probe session ids come from a counter in a range that cannot collide with `s.nextCallID` (e.g. high bit set), and a probe never touches `s.activeCalls`, admission, timelines, metrics counters or the session store.
- [ ] **Step 4: Run to see them pass.** Same command with `-race` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): probe a provider stage with one minimal real request`.

---

### Task 5: `POST /api/v1/providers/{id}/test`

**Files:**
- Modify: `server/api/providers.go` (the handler: a `Test` function field and the sub-route), `server/api_routes.go` or wherever `ProvidersHandler` is constructed (`grep -n "ProvidersHandler" server/*.go`), `server/server.go` (a `probeGate`)
- Test: `server/api/providers_test.go`, `server/probe_e2e_test.go`

**Interfaces:**
- Consumes: `probeStage` (Task 4); `ProvidersHandler.Catalog/Resolver/Supports`.
- Produces:
  ```
  POST /api/v1/providers/{id}/test   (no body)
  200 {"data":{"provider":"google","checked_at":"2026-10-14T10:00:00Z","results":[ProbeResult…]}}   // one per service the provider offers and has a credential for, in order stt, llm, tts
  404 not_found            unknown provider id
  409 provider_not_detected   no credential in the environment (nothing is sent)
  429 probe_too_soon       within 5 s of the last probe of this provider, or one is still running; Retry-After header in seconds
  405 method_not_allowed   (Allow: POST)
  ```
  A failed probe is still `200` (the check ran; the result says `error`).

- [ ] **Step 1: Write the failing tests** (`probe_e2e_test.go` with the e2e rig, echo provider, and an `api` unit test for the routing): management key required (401 without); unknown id 404; echo → 200 with three results (`stt`, `llm`, `tts`), each `ok` with a latency and `circuit: "unknown"`; a second POST within 5 s → 429 with `Retry-After`; GET → 405; two concurrent POSTs → exactly one 200 and one 429; a provider that exists but has no credential → 409 and the registry factory was not called (assert with a counting factory); the response body never contains the canary env value set for a provider with a rejecting fake.
- [ ] **Step 2: Run to see them fail.** Expected: FAIL (404/405).
- [ ] **Step 3: Implement.** The gate is a small `probeGate` on `Server`: `map[provider]time.Time` of the last start plus an in-flight set under one mutex, entries bounded by the catalog's provider count; `try(provider) (retryAfter time.Duration, ok bool)` compares with `time.Since` (monotonic). The handler detects the credential with the same check `GET /providers` uses (`Detected`), runs `probeStage` for each service concurrently (`errgroup`-style with `sync.WaitGroup`, at most 3 goroutines, each bounded by `probeTimeout`), and orders results stt, llm, tts. It releases the in-flight mark when all return, even on panic (`defer`).
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 ./...` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(api): POST /providers/{id}/test — check a provider on demand`.

---

### Task 6: Providers: "Test connection"

**Files:**
- Modify: `ui/src/api/client.ts` (`providerTestSchema`, `testProvider`), `ui/src/routes/providers/ProvidersView.tsx` (the button and result lines), tests beside them.
- Test: `ui/src/api/client.test.ts`, `ui/src/routes/providers/ProvidersView.test.tsx`

**Interfaces:**
- Consumes: Task 5's endpoint.
- Produces: `ProviderTest = { provider: string; checked_at: string; results: { stage: "stt"|"llm"|"tts"; status: "ok"|"error"|"skipped"; latency_ms?: number; detail?: string; circuit: "closed"|"open"|"half_open"|"unknown" }[] }`; `client.testProvider(id: string): Promise<ProviderTest>`. A `429` surfaces the server's message and `Retry-After` as the neighbouring error type does (`grep -n "class ApiError\|retryAfter" ui/src/api/client.ts`).

- [ ] **Step 1: Write the failing tests.**
  - `client.test.ts`: `testProvider("google")` POSTs `/providers/google/test` with no body and parses a result with and without `latency_ms`.
  - `ProvidersView.test.tsx`: the panel has a `Test connection` button, disabled with the reason `No credential. Set the variable first.` when the provider has no credential; clicking it disables it (`Testing…`), then lists per service `Language model — OK, 410 ms`, `Voice — Error: client_error: …`, `Speech to text — Skipped: no default model`; shows `Checked just now`-style text from `checked_at` (use the existing relative-time helper if one exists, else `HH:MM:SS`); a circuit other than `closed`/`unknown` shows `Circuit open` with the warn lamp; a 429 shows `Checked a moment ago. Try again in 5 s.` and re-enables the button; a network failure shows the generic read-failure text the view already uses. Results are cleared when another provider is selected (they belong to the one that was tested) and **no request is made until the button is clicked** (assert the mock was not called on render, Review Focus: nothing automatic).
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/api src/routes/providers` — Expected: FAIL.
- [ ] **Step 3: Implement.** A `useMutation` per panel; result lines in a `<ul aria-live="polite">`; reuse `SERVICE_NAMES`, the `lamp` primitive and `Status` tones. The panel's existing sentence about "no health figures" (if any) is removed.
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): Providers can test a provider on demand`.

---

### Task 7: The browser proves both, docs, gate, review

**Files:**
- Create: `e2e/tests-flows/26-latency-and-provider-test.spec.ts`
- Modify: `docs/api/v1.md` (turn `latencies`; `POST /providers/{id}/test`), `docs/operations_runbook.md` (what a probe sends and costs), `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (S9, S15, S16 rows; the Call detail and Providers rows and deviations)

- [ ] **Step 1: Write the spec** (echo only: `test.skip(LIVE, …)` as in `25-draft-test.spec.ts`). Latency: create a flow through the API, call it with `openFlowTest`, `startCall`, `sendText("hello")`, wait for the bot reply, disconnect; open the call in Calls: the turn shows a `Total … ms` pill and the figures show `Median reply`; via the API assert the turn's `latencies.voice_to_voice_ms > 0`. Provider test: open Providers, select the echo provider, nothing has been requested yet (the network log shows no `/test` call), click `Test connection`: three OK lines with milliseconds appear; a second click inside 5 s shows the try-again message. `step(page, …)` screenshots.
- [ ] **Step 2: Run** `make e2e-echo` — Expected: all green. Fix real failures in the code, not the spec.
- [ ] **Step 3: Docs.** `docs/api/v1.md`: the `latencies` object on a turn (keys, integer ms, only measured stages, `{}` when none) and the provider test (what is sent per stage, status codes, rate limit, that nothing runs unless asked, that the secret never appears). Runbook: a probe sends one tiny request per service (a few tokens, one word of speech, 200 ms of silence), once per click, at most one per 5 s per provider; failures count toward the circuit breaker like any request; paid providers are billed for those few units. Spec rows: S9 **Done** (Call detail only; Test call panel and First run chips remain), S15 **Replaced by an on-demand check** (no p50 column or Last hour: the panel shows the result of the last click), S16 **Done** (same endpoint).
- [ ] **Step 4: Full gate.** `gofmt -l server`; `cd server && go test -race -count=1 ./...`; `cd ui && npx vitest run && npx tsc --noEmit`; `make e2e-echo` — Expected: all green. Live providers are checked by hand with real keys once (`curl -X POST` with the management key), because CI has none; note the result in the ledger.
- [ ] **Step 5: Independent review** (AGENTS.md): fresh Opus reviewer on `main..HEAD` with the five Review Focus items; concurrency (the gate, goroutine leaks after a hung provider, the observer sink's mutex), secrets (the response, logs and errors), breaker side effects (Ruling 3), boundedness (every map), monotonic time, YAGNI (is the `circuit` field worth it; is the probe session machinery duplicating `tools/provtest`), UI honesty (nothing automatic; rate-limit and error states; a11y of the live region). Fix confirmed findings, each with a test that fails first.
- [ ] **Step 6: Commit** `docs: S9, S15 and S16 done — turn latency and on-demand provider test`.
