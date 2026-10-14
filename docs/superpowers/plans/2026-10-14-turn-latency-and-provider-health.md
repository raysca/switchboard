# Turn Latency (S9) and Provider Health (S15) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The server stores the latency of each reply on its turn (S9) and reports, per provider, how it has been doing over the last hour (S15), so the console can show "median reply" figures, per-turn latency pills, and a Providers p50 / Last hour / circuit state.

**Architecture:** Both reuse what the server already measures. `core/observe` already computes `stt_final`, `llm_ttft`, `tts_ttfb` and `voice_to_voice` per turn and feeds Prometheus histograms (`server/server.go` observer sink); today nothing is stored. S9 adds a small bounded per-call collector, keyed by turn id, that the observer sink writes and the turn recorder reads when it builds a turn row (the `latencies_json` column exists, is NOT NULL, and is written as `{}` today). S15 adds an in-memory rolling one-hour window per (provider, stage) fed by the same sink and by the call's end, read together with the existing circuit-breaker gauges, behind one new read-only endpoint. Nothing new is persisted for S15; it is per instance and resets on restart.

**Tech Stack:** Go (`server/`, `core/observe`), SQLite/Postgres unchanged (no migration), React 19 + TypeScript + vitest, Playwright (`make e2e-echo`).

**Spec:** `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (rows S9, S15; Call detail, Providers and Test call boards).

## Global Constraints

- Providers are preconfigured and secrets are only read from the environment. Health never exposes a secret, an env value, a model name or an endpoint URL (AGENTS.md; memory `provider-secrets-env-only`).
- Every queue and map is bounded (AGENTS.md "Resource Bounds"). Hot path (the observer sink, 50 frames/s) takes one short mutex or atomics and allocates nothing per frame.
- Monotonic time only for windows and durations (`time.Since`); no `UnixNano` interval maths.
- `core/` stays server-agnostic: no change to `core/` is needed; `server/` consumes `observe.Measurement` as it does today.
- All code passes `go test -race ./...`; `gofmt -w .`; conventional commits; final independent Opus review (AGENTS.md).
- Dual dialect: nothing here touches SQL.

## Review Focus

1. **A turn with no measurements** (barge-in before any audio, text-only turn, tool-only reply) stores `{}` and the UI shows no pills for it rather than zeros or `NaN`. Pinned in Task 1 and Task 3.
2. **Measurement and turn row arrive in either order** (the observer goroutine is separate from the recorder): a turn row built before its `voice_to_voice` arrives stores what exists; a measurement for a turn that never gets a row does not leak (bounded). Pinned in Task 1.
3. **A provider with no calls in the hour** shows "No calls in the last hour", never `p50 0 ms` or `NaN`. Pinned in Task 4 and Task 6.
4. **Failover attribution:** a stage with fallbacks is attributed to its configured primary provider, so a failed-over call's samples count toward the primary (documented limit, Ruling 2); the Prometheus `provider="failover"` label is unchanged.
5. **Circuit state for a provider without a breaker** (echo, or a provider with no calls yet) is `unknown`, not `closed`. Pinned in Task 5.

## Rulings made while planning (re-read before Task 1)

1. **Keys and units.** `latencies_json` is `{"stt_final_ms":N,"llm_ttft_ms":N,"tts_ttfb_ms":N,"voice_to_voice_ms":N}`, integer milliseconds, only the stages measured for that turn. `startup` has no turn and is not stored. The existing UI pills read `vad_ms/stt_ms/llm_ms/tts_ms`; those keys were never written, so Task 3 replaces them (no compatibility shim).
2. **Attribution.** Stage measurements and call counts go to the stage's configured primary provider (`CallSpec.STT/LLM/TTS.Provider`). Cost if wrong: during a failover the primary's p50 includes the fallback's latency. Per-processor attribution would need the failover wrapper to report which candidate answered; not worth it until someone needs it.
3. **"Requests" is reported as `calls`** (calls that used the provider in that stage). STT has no request counter and LLM/TTS request counts are per processor lifetime; a call count is honest and uniform. `errors` is the stage error count the call already reports at its end (`STTCounts/LLMCounts/TTSCounts`).
4. **Window:** 60 one-minute buckets, median over up to 64 retained samples per bucket (oldest overwritten). A bucket is reused after an hour. In memory only; per instance; resets on restart. Documented in the API doc and runbook.
5. **Test call panel and First run chips (S9 "reply-time chips", "median reply" in the panel) are out of scope:** they need turns while a call is live. Call detail and the Calls list get the figures. Follow-up if wanted.

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

### Task 4: The rolling provider window

**Files:**
- Create: `server/health.go`
- Test: `server/health_test.go`

**Interfaces:**
- Produces:
  ```go
  type providerHealth struct { /* mu, start time.Time, now func() time.Time, series map[healthKey]*series */ }
  type healthKey struct{ provider, stage string }
  type HealthRow struct { Provider, Stage string; Calls, Errors int; P50MS *int64 }   // P50MS nil when there are no samples
  func newProviderHealth(now func() time.Time) *providerHealth
  func (h *providerHealth) call(provider, stage string, errors int)          // a finished call used provider in stage
  func (h *providerHealth) sample(provider, stage string, d time.Duration)   // one latency sample
  func (h *providerHealth) rows() []HealthRow                                // last 60 minutes, sorted by provider then stage
  const healthBuckets, healthSamples = 60, 64
  ```
  Stages are `stt`, `llm`, `tts`. `now` is injected so a test advances time without sleeping; production passes `time.Now` (its monotonic reading is what `Sub` uses).

- [ ] **Step 1: Write the failing tests**

```go
func TestHealthCountsCallsAndErrorsInTheLastHourOnly(t *testing.T) {
	clock := time.Now()
	h := newProviderHealth(func() time.Time { return clock })
	h.call("google", "llm", 0)
	h.call("google", "llm", 2)
	clock = clock.Add(59 * time.Minute)
	h.call("google", "llm", 1)
	r := h.rows()
	if len(r) != 1 || r[0].Calls != 3 || r[0].Errors != 3 {
		t.Fatalf("rows %+v", r)
	}
	clock = clock.Add(2 * time.Minute) // the first two fall out of the hour
	r = h.rows()
	if len(r) != 1 || r[0].Calls != 1 || r[0].Errors != 1 {
		t.Fatalf("rows %+v", r)
	}
	clock = clock.Add(2 * time.Hour)
	if r = h.rows(); len(r) != 0 {
		t.Fatalf("an idle provider must have no row: %+v", r)
	}
}

func TestHealthMedianIsOverTheWindowAndNilWithoutSamples(t *testing.T) {
	h := newProviderHealth(time.Now)
	h.call("xai", "tts", 0)
	if r := h.rows(); r[0].P50MS != nil {
		t.Fatalf("no samples must give no median, got %d", *r[0].P50MS)
	}
	for _, ms := range []int{100, 500, 300} {
		h.sample("xai", "tts", time.Duration(ms)*time.Millisecond)
	}
	if r := h.rows(); r[0].P50MS == nil || *r[0].P50MS != 300 {
		t.Fatalf("median %v", r[0].P50MS)
	}
}

func TestHealthIsBounded(t *testing.T) {
	h := newProviderHealth(time.Now)
	for i := 0; i < 10*healthSamples; i++ {
		h.sample("p", "llm", time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.series {
		for _, b := range s.buckets {
			if len(b.samples) > healthSamples {
				t.Fatalf("%d samples in a bucket, cap %d", len(b.samples), healthSamples)
			}
		}
	}
}
```

- [ ] **Step 2: Run to see them fail.** `cd server && go test -race -count=1 -run Health .` — Expected: FAIL (undefined).
- [ ] **Step 3: Implement.**

```go
package server

import (
	"slices"
	"sort"
	"sync"
	"time"
)

const (
	healthBuckets = 60 // one per minute: the last hour
	healthSamples = 64 // latency samples kept per bucket (oldest overwritten)
)

type healthKey struct{ provider, stage string }

type HealthRow struct {
	Provider, Stage string
	Calls, Errors   int
	P50MS           *int64 // nil: no samples in the window
}

type bucket struct {
	minute  int // minutes since start; -1 = empty
	calls   int
	errors  int
	samples []int64
	next    int
}

type series struct{ buckets [healthBuckets]bucket }

// providerHealth is a rolling one-hour view of how each provider's stages did on this
// instance. Memory only; it starts empty after a restart.
type providerHealth struct {
	mu     sync.Mutex
	start  time.Time
	now    func() time.Time
	series map[healthKey]*series
}

func newProviderHealth(now func() time.Time) *providerHealth {
	return &providerHealth{start: now(), now: now, series: map[healthKey]*series{}}
}

func (h *providerHealth) minute() int { return int(h.now().Sub(h.start) / time.Minute) }

// bucketLocked returns the current minute's bucket for the key, resetting it when it last held an older minute.
func (h *providerHealth) bucketLocked(k healthKey) *bucket {
	s := h.series[k]
	if s == nil {
		s = &series{}
		for i := range s.buckets {
			s.buckets[i].minute = -1
		}
		h.series[k] = s
	}
	m := h.minute()
	b := &s.buckets[m%healthBuckets]
	if b.minute != m {
		*b = bucket{minute: m, samples: b.samples[:0]}
	}
	return b
}

func (h *providerHealth) call(provider, stage string, errors int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.bucketLocked(healthKey{provider, stage})
	b.calls++
	b.errors += errors
}

func (h *providerHealth) sample(provider, stage string, d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.bucketLocked(healthKey{provider, stage})
	if len(b.samples) < healthSamples {
		b.samples = append(b.samples, d.Milliseconds())
		return
	}
	b.samples[b.next%healthSamples] = d.Milliseconds()
	b.next++
}

func (h *providerHealth) rows() []HealthRow {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.minute()
	var out []HealthRow
	for k, s := range h.series {
		row := HealthRow{Provider: k.provider, Stage: k.stage}
		var all []int64
		for i := range s.buckets {
			b := &s.buckets[i]
			if b.minute < 0 || now-b.minute >= healthBuckets {
				continue
			}
			row.Calls += b.calls
			row.Errors += b.errors
			all = append(all, b.samples...)
		}
		if row.Calls == 0 && len(all) == 0 {
			continue
		}
		if len(all) > 0 {
			slices.Sort(all)
			med := all[len(all)/2]
			row.P50MS = &med
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Stage < out[j].Stage
	})
	return out
}
```
  For an even count the lower-middle sample is taken; the test above (odd) and any later one must agree with that choice.

- [ ] **Step 4: Run to see them pass.** Same command with `-race` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): a rolling one-hour health window per provider and stage`.

---

### Task 5: Feed it, and read the circuit states

**Files:**
- Modify: `server/server.go` (observer sink; `finishCall`), `server/metrics.go` (a `circuitState(provider, stage string) string` reader), `NewServer` field `health *providerHealth`
- Test: `server/health_e2e_test.go`, `server/metrics_test.go`

**Interfaces:**
- Consumes: `providerHealth` (Task 4); `stageProviders`' sibling for primaries.
- Produces:
  ```go
  func primaryProviders(c CallSpec) map[string]string    // {"stt": c.STT.Provider, "llm": c.LLM.Provider, "tts": c.TTS.Provider}
  func (m *Metrics) circuitState(provider, stage string) string   // "closed" | "open" | "half_open" | "unknown"
  ```

- [ ] **Step 1: Write the failing tests.**
  - `metrics_test.go`: with `m.LLMCircuitStateGoogle.Store(1)` → `circuitState("google","llm") == "open"`; `2` → `"half_open"`; `0` → `"closed"`; `circuitState("echo","llm") == "unknown"`; a dynamic nvidia breaker (`m.circuits.set("llm","nvidia","host:443",1)`) → `circuitState("nvidia","llm") == "open"`, and with two endpoints (one closed, one open) the **worst** state wins (open > half_open > closed).
  - `health_e2e_test.go` (rig as in Task 2): after one echo call that produced a reply and ended, `r.s.health.rows()` has rows for `echo/stt`, `echo/llm`, `echo/tts` with `Calls == 1`, and the `llm`/`tts` rows have a non-nil `P50MS`. (Wait for the call to end with `eventually`; `finishCall` runs after the websocket closes.)
- [ ] **Step 2: Run to see them fail.** Expected: FAIL (undefined).
- [ ] **Step 3: Implement.**
  - `circuitState`: map the six static atomics (`STT/LLM/TTS` × `Google/XAI`, see the `add("google", "stt", …)` list at `server.go:789`) plus `m.circuits.snapshot()` entries matching the provider and stage; codes 0/1/2 from `circuitCode`. Return `"unknown"` when nothing matches. Reuse `circuitCode`'s inverse; do not invent new codes.
  - Observer sink: `s.health.sample(primary[stage], stage, m.Value)` for `StageSTTFinal→"stt"`, `StageLLMTTFT→"llm"`, `StageTTSTTFB→"tts"` (others ignored). Compute `primaryProviders(call)` once before attaching the sink.
  - `finishCall`: where `recordProviderErrors` runs per processor, also call `s.health.call(primary[stage], stage, errs)` once per stage that ran (reuse the same type switch; do not read the counters twice).
  - `NewServer`: `health: newProviderHealth(time.Now)`.
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 ./...` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): record each call and stage latency against its provider`.

---

### Task 6: `GET /api/v1/provider-health`

**Files:**
- Modify: `server/api_routes.go` (a case beside `operations/status`), `server/api/providers.go` or a new `server/api/health.go` (the DTO)
- Test: `server/api_routes_test.go` or `server/health_e2e_test.go`

**Interfaces:**
- Consumes: `providerHealth.rows()`, `Metrics.circuitState`.
- Produces:
  ```
  GET /api/v1/provider-health  → 200 {"data":[{"provider":"google","stage":"llm","calls":12,"errors":1,"p50_ms":410,"circuit":"closed"}], ...}
  ```
  `p50_ms` is `null` without samples. Rows exist only for provider/stage pairs with activity in the hour; a provider with a breaker but no calls has no row (the UI reads circuit state from nowhere else; see Step 3). Management-key authenticated like its neighbours; `405` for non-GET with `Allow: GET`.

- [ ] **Step 1: Write the failing tests** (rig as in Task 2): without credentials → 401; GET after one echo call → one row per stage for `echo` with `calls: 1`, `circuit: "unknown"`, and valid JSON types (`p50_ms` a number for `llm`); POST → 405; empty server → `data: []` (an array, never `null`).
- [ ] **Step 2: Run to see them fail.** Expected: FAIL (404).
- [ ] **Step 3: Implement.** In `api_routes.go` add `case "/api/v1/provider-health":` mirroring `operations/status`; build the slice from `s.health.rows()`, set `Circuit: s.metrics.circuitState(row.Provider, row.Stage)`, and write `api.SuccessResponse[[]api.ProviderHealthDTO]`. A breaker that is open with no calls in the hour is the case most worth seeing: also append a row (`calls: 0`, `errors: 0`, `p50_ms: null`) for every circuit currently not `closed`, so an open breaker is never hidden by quiet traffic. Test that too (set `m.LLMCircuitStateGoogle.Store(1)` with no calls: one row, `circuit: "open"`).
- [ ] **Step 4: Run to see them pass.** `cd server && go test -race -count=1 ./...` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(api): GET /api/v1/provider-health — the last hour per provider and stage`.

---

### Task 7: Providers shows it

**Files:**
- Modify: `ui/src/api/client.ts` (`providerHealthSchema`, `getProviderHealth`), `ui/src/routes/providers/ProvidersView.tsx` (a p50 figure per row and the panel's "Last hour" section), tests beside them.
- Test: `ui/src/api/client.test.ts`, `ui/src/routes/providers/ProvidersView.test.tsx`

**Interfaces:**
- Consumes: Task 6's endpoint.
- Produces: `ProviderHealth = { provider: string; stage: "stt"|"llm"|"tts"; calls: number; errors: number; p50_ms: number | null; circuit: "closed"|"open"|"half_open"|"unknown" }`; `client.getProviderHealth(): Promise<ProviderHealth[]>`.

- [ ] **Step 1: Write the failing tests.**
  - `client.test.ts`: `getProviderHealth()` GETs `/provider-health` and parses a row with `p50_ms: null`; an unknown `circuit` value is rejected by the schema or mapped to `unknown`, whichever the neighbouring schemas do (`grep -n "z.enum\|catch(" client.ts`) — follow them.
  - `ProvidersView.test.tsx`: for a provider with rows `llm` (calls 12, errors 1, p50 410, closed) and `tts` (calls 3, errors 0, p50 null): the panel's "Last hour" section lists `Language model — 12 calls, 1 error, median 410 ms` and `Voice — 3 calls, no timing yet`; the list row shows the provider's worst circuit as a lamp with the accessible name `Circuit open` when any stage is open (and no lamp text change when all closed); a provider with no rows shows `No calls in the last hour` (Review Focus 3); a failed health request leaves the rest of the panel intact and says `Health unavailable` once.
- [ ] **Step 2: Run to see them fail.** `cd ui && npx vitest run src/api src/routes/providers` — Expected: FAIL.
- [ ] **Step 3: Implement.** A second `useQuery` in `ProvidersView` with `refetchInterval: 30_000`, keyed on the management key like the providers query; join rows to providers by `provider === p.id`. Use the existing `SERVICE_NAMES` for stage names, the existing `lamp` primitive, and `Status` tones; no new styling beyond a list rule if needed.
- [ ] **Step 4: Run to see them pass.** `cd ui && npx vitest run && npx tsc --noEmit` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(ui): Providers shows the last hour and the circuit state`.

---

### Task 8: The browser proves both, docs, gate, review

**Files:**
- Create: `e2e/tests-flows/26-latency-health.spec.ts`
- Modify: `docs/api/v1.md` (turn `latencies`, `GET /provider-health`), `docs/operations_runbook.md` (the window is per instance and memory only), `docs/superpowers/specs/2026-10-12-switchboard-console-design.md` (S9 and S15 rows; Call detail row; Providers row)

- [ ] **Step 1: Write the spec** (echo only: `test.skip(LIVE, …)` as in `25-draft-test.spec.ts`; create a flow through the API, call it with `openFlowTest`, `startCall`, `sendText("hello")`, wait for the bot reply, disconnect): open the call in Calls → the turn shows a `Total … ms` pill and the figures show `Median reply`; open Providers → the echo provider's panel lists `Last hour` with `1 call` for the language model. `step(page, …)` screenshots. Via the API, assert the turn's `latencies.voice_to_voice_ms > 0` and `/provider-health` has an `echo` `llm` row.
- [ ] **Step 2: Run** `make e2e-echo` — Expected: all green. Fix real failures in the code, not the spec.
- [ ] **Step 3: Docs.** `docs/api/v1.md`: the `latencies` object on a turn (keys, integer ms, only measured stages, `{}` when none) and the provider-health endpoint (fields, window, per instance, `p50_ms` null, `circuit` values, attribution to the configured primary provider, calls ≠ requests). Spec rows: S9 and S15 **Done** with this scope, naming what stays open (Test call panel and First run chips, a Calls-list median column, per-candidate failover attribution). Runbook: "Provider health is the last hour on this instance, held in memory; it is empty after a restart."
- [ ] **Step 4: Full gate.** `gofmt -l server`; `cd server && go test -race -count=1 ./...`; `TEST_POSTGRES_DSN` not needed (no SQL changed); `cd ui && npx vitest run && npx tsc --noEmit`; `make e2e-echo` — Expected: all green.
- [ ] **Step 5: Independent review** (AGENTS.md): fresh Opus reviewer on `main..HEAD` with the five Review Focus items; concurrency (the sink takes two short mutexes per measurement, never both at once; no lock across I/O), boundedness (every map and ring), monotonic time (`providerHealth.minute`), secrets (the health DTO exposes nothing but provider id, stage and numbers), YAGNI (is the `circuit` row for quiet providers needed; is `turnLatencies` duplicating `observe`'s own per-turn state), UI honesty (empty/failed health states, units). Fix confirmed findings, each with a test that fails first.
- [ ] **Step 6: Commit** `docs: S9 and S15 done — turn latency and provider health`.
