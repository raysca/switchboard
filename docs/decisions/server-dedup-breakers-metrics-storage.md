# Server deduplication: breakers, metrics table, storage row helpers

Status: accepted (2026-10-25). A refactor: no wire, schema or API change, except the breaker metric additions listed under Decisions.

## Why

An over-engineering review of `server/` found three places where the same code is written many times:

1. **Circuit breakers.** The registry has 19 named breaker fields (`googleLLMBreaker`, `echoSTTBreaker`, `deepgramBreaker`, …), three switch functions mapping a provider to its field (`sttBreaker`, `llmBreaker`, `ttsBreaker`), six hand-written `SetObserver` blocks for google and xai, six static gauge fields (`STTCircuitStateGoogle`, …) and the special cases for those gauges in `WritePrometheus`, `circuitReport` and `circuitState`. Nvidia already does the general thing: one breaker per key, created on first use, exported through `Metrics.circuits`.
2. **Metrics.** `WritePrometheus` writes about 45 metrics with no labels as three `Fprintf` lines each.
3. **Storage.** About 38 `for rows.Next()` loops repeat Query, `defer rows.Close`, Scan, append and `rows.Err`; about 14 places repeat "Exec, RowsAffected, `n != 1` means `sql.ErrNoRows`"; about six loops select one id column into a `[]string`, each closing `rows` by hand on every error path.

## Decisions

### 1. One breaker map

- `Registry.breakers sync.Map` keyed by `circuitKey{stage, provider, endpoint}` (the type metrics already uses). `r.breaker(stage, provider)` looks the provider up in one table, `breakerSpecs` (`provider -> {endpoint, observed}`), and loads or creates the breaker. openai is the one provider not in the table: `r.openaiBreaker(baseURL)` keys by base URL and is never observed.
- **Observed providers, as today:** google, xai (endpoint `""`), echo (`"simulated"`), silence (`"text"`), offline (`"local"`), nvidia (`endpointLabel(endpoint)`, the host; the map is now keyed by the label, since only the three hosted endpoints exist). **Unobserved, as today:** deepgram, cartesia, elevenlabs, openai. An unobserved breaker gets no gauge and no observer, so it reports nothing to `/metrics`, `/ready`, the probe or call timelines. That keeps docs/observability.md:114, docs/operations.md:72 and the `GovoiceProviderCircuitOpen` page exactly as they are. openai must stay unobserved: `recordCircuit` keys by (stage, provider), so tenant A's broken base URL would mark tenant B's calls. Observing deepgram, cartesia and elevenlabs would add on-call pages, which is a product decision outside a refactor.
- **Ordering:** a new observed breaker gets its observer *before* `LoadOrStore`. Only the winner of the store sets its gauge to 0. The observer reads `r.metrics`, which becomes an `atomic.Pointer[Metrics]`, at call time. So a breaker created before `BindMetrics` still exports once metrics are bound, nothing is lost between store and observe, and a racing 1 is never overwritten by the creator's 0 (the 0 is set before the breaker is visible). `BindMetrics` stores the pointer and sets the gauge to 0 for the fixed set that shows from start-up, as today: google and xai (all stages), echo (all stages), silence (stt, tts), offline (stages `Supports` reports). It creates those breakers through `r.breaker`. There is no `Range`. Precondition (true in production, server.go:176-178): `BindMetrics` runs before any call is built.
- **Factories and failover use the same lookup.** Every factory takes its breaker from `r.breaker(stage, provider)` (or `r.openaiBreaker`/`r.nvidiaBreaker`), never a literal endpoint. `sttBreaker`/`ttsBreaker`/`llmBreaker` stay as one-line wrappers, so failover's `WouldRefuse` checks the same breaker the processor records on. A table-driven test asserts this for every registered `provider.stage`.
- The static gauge fields go. `dynamicCircuits.write` leaves out the `endpoint` label when it is empty, so google and xai keep exactly `voice_<stage>_circuit_state{provider="google"} N`. Within one family, samples are sorted by provider then endpoint (google and xai no longer come first; this is cosmetic, and deploy/smoke.sh only greps names).
- `/ready`'s `circuits` keeps its keys: `<provider>_<stage>` without an endpoint, `<provider>_<stage>:<endpoint>` with one. Every entry is gated by `Supports(provider, stage)`. Before, only google and xai were gated; only offline is ever pruned, and it is gated at creation. So `xai_stt` stays exported as a gauge but absent from `/ready` in a build without xaispeech, as today.
- `circuitState` (probe) reads the snapshot only. Its results are unchanged. Its stale comment, which says echo is "unknown", is corrected.
- Tests that set the removed fields (metrics_test.go:28-45, health_ready_test.go:152, probe_test.go:116) set `m.circuits` instead. A `Metrics` with nothing bound no longer prints the google/xai lines; production always binds.

### 2. Metrics table

- One helper `writeScalars(w, []scalar)` with `scalar{name, help, kind string; v int64}` (typed, so passing an atomic instead of its `.Load()` does not compile; the uint64 counters are converted, as none comes near 2^63), writing `# HELP name help\n# TYPE name kind\nname v\n` with `%d`, the exact bytes written today. Values are loaded when the slice is built (as today, at the same point).
- The table is split into runs, in the current order, between the labelled families written by hand, so the output is byte-identical apart from the circuit family order above. `voice_retention_last_success_age_seconds` (`%.0f`) and the conditional `OTelStats`/`SmartTurnWorkers` blocks stay as they are.

### 3. Storage helpers (in `storage/rows.go`)

- `collect[T any](rows *sql.Rows, out []T, scan func(rowScanner) (T, error)) ([]T, error)`: closes rows and appends to `out`. Each site passes what it starts with today: `nil` at the `var out []T` sites, `make([]T, 0, n)` at the history, eval and eval_queue sites. Empty results stay nil or `[]` exactly as now, so no JSON `null`/`[]` changes.
- `oneRow(res sql.Result, err error) error`: returns err; then the `RowsAffected` error; `sql.ErrNoRows` unless exactly one row changed. Used **only** at the single-row-by-key sites repositories.go:236, 247, 378, 431, 446, 461, 490, 718; eval.go:141, 153 (the `isUniqueViolation` check stays before it); httptools.go:83, 181, 239; secrets.go:126. **Left as they are:** history.go:188, 226; agents.go:60, 151; eval_queue.go:180, 382, 537; erasure.go:69, 79; retention.go:101, 189, 196, 275 (counts, fallbacks or other errors). Sites that ignored the `RowsAffected` error now return it; both drivers implement it.
- `queryIDs(ctx, q rowQuerier, query, args...) ([]string, error)` reuses the existing `rowQuerier` interface (httptools.go:192) and closes rows on every path. It is not used at retention.go:60 (two columns) or eval_queue.go:313 (its wrapped scan error is kept).
- Loops that do more than scan and append (accumulate into a map, early exit) stay as they are.

## Non-goals

The other review findings (failover build-chain generic, BindMetrics hook fields, mcp/httptool destination checks, etc.) are not part of this change. No change to breaker configuration, to which errors open a circuit, or to SQL text.

## Plan

Slices, one commit each:

1. **Storage helpers.** `storage/rows.go` with the three helpers and `storage/rows_test.go` (collect: empty → nil, scan error, rows.Err; oneRow: Exec error, 0 rows, 1 row, 2 rows; queryIDs: on `*sql.DB` and in a `*sql.Tx`). Convert the loops in `repositories.go`, `history.go`, `eval.go`, `eval_queue.go`, `httptools.go`, `secrets.go`, `erasure.go`, `retention.go`. Existing storage tests run on SQLite always and on Postgres when configured.
2. **Metrics table.** `metrics.go`. Before the change, capture `WritePrometheus` output from a `Metrics` with distinct values in every field, `OTelStats`, `SmartTurnWorkers`, `RetentionEnabled` and `retentionLastOK` set, and diff after: only the timing lines (goroutines, heap, age) may differ. The capture is a scratch test and is not committed. The typed `v int64` and the existing name tests guard the table afterwards, and a golden file would break on every new metric.
3. **Breaker map.** `registry.go`, `nvidia.go`, `stt_failover.go`, `tts_failover.go`, `llm_failover.go`, `metrics.go`, `server.go`, `probe.go`, tests that touched the removed fields. New tests:
   - For every registered `provider.stage` except openai, the factory's breaker is `xBreaker(provider)`.
   - The google gauge has no endpoint label.
   - deepgram and openai breakers produce no gauge, no `/ready` key and no `circuit_state` timeline event.
   - A breaker created before `BindMetrics` exports after it.
   - `/ready` keys for google and nvidia are unchanged.
   - cartesia stt and tts are distinct breakers.
   - The rewritten nvidia tests still prove that no breaker is keyed by a secret-supplied endpoint.

Each slice: `go test -race ./server/...`. Then the full gate in `AGENTS.md`.

## Review record

The spec review (an independent agent) found nothing critical or high. Its findings and their resolutions:

- **M1: some loops return `[]`, not nil.** Resolved: `collect` takes the starting slice.
- **M2/M3: the deliberate deepgram, cartesia and elevenlabs exposure contradicts the docs and adds pages.** Resolved: dropped. Behaviour is kept exactly, and they stay unobserved.
- **M4: an openai observer would leak across tenants.** Resolved: openai gets no observer, which is stated and tested.
- **M5: factories and the lookup table could drift.** Resolved: one lookup, plus a table-driven test.
- **L1: ordering.** Resolved: observer before the store, an atomic metrics pointer, no `Range`.
- **L2: name the `oneRow` sites.** Resolved: listed.
- **L3: `queryIDs`.** Resolved: reuses `rowQuerier`, skips two sites, and the tx test runs a second statement.
- **L4: metrics.** Resolved: typed value, conditional blocks set in the capture, golden file rejected with the reason above.
- **L5: probe.** Moot now that behaviour is kept. The comment is fixed.
- **L6: acceptance tests.** Resolved: added above.

## Build notes and code review

Where the build differs from the plan:
- `sttBreaker` and `ttsBreaker` were deleted rather than kept as wrappers. Failover calls `r.breaker(stage, provider)`, the same function the factories call. `llmBreaker` stays, because openai needs the spec's base URL.
- The **factory-wiring test is narrower than planned.** A processor does not expose the breaker it was built with, so the test cannot compare a factory's breaker with failover's. `TestEveryProviderStageHasItsOwnBreaker` instead checks that every registered provider stage has its own stable breaker. The wiring is guaranteed by construction: factories and failover both call `r.breaker`. A factory that passed a literal breaker would not be caught. Accepted gap.

The independent code review found no correctness bug and confirmed the preserved behaviour site by site. Its findings and what was done:
1. **Medium: the planned factory-wiring test is missing.** Recorded as the gap above.
2. **Four rewritten assertions compared a lookup with itself.** Replaced with nil and not-shared checks; the `t.Fatal` messages are corrected.
3. **No `rows.Err` test.** Added `TestCollectReturnsRowsErr`, which uses an SQLite step error.
4. **`stages` was a space-separated string, `circuitOf` had an unused receiver, and nvidia had a dead endpoint.** `stages` is now `[]string`, `circuitOf` is a function, and nvidia's endpoint is left unset.
5. **docs/observability.md named only Nvidia's endpoint label and left silence out of the observed providers.** Both fixed. These predate the branch.
6. **Informational: a `Metrics` with nothing bound prints no google/xai lines.** Already recorded above.
