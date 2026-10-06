# Merge-review fixes — codex/v2-implementation

Source: xhigh code review (15 findings). Goal: make the branch mergeable. Every fix lands with a
regression test that fails before the fix. Gate: `make test` (race) + `gofmt` clean.

## Blockers
| ID | Fix | Test |
|---|---|---|
| T1 | Session watchdog runs **outside** the run loop (own ticker goroutine, exits with session) so a hung processor fails the session with `ErrProcessorWatchdog`. | hung ctx-ignoring processor → session fails within ~2× MaxFrameProcessing |
| T2 | `Close()` bounded: if run loop never started, don't wait; wait for `runLoopDone` under the `TeardownTimeout` deadline; stuck loop ⇒ `StateLeaked`, `Done()` still closes. | Close after failed Start returns; Close with hung processor returns `StateLeaked` within timeout |
| T3 | Verify WS transport Start-failure path now releases (session Done closes → admission released); `conn.session` not set until Start succeeds. | transport/server test: Attach failure releases admission slot |
| T4 | Wire storage + management plane in `cmd/server`: open store from config, SetStore, provider/agent/MCP/session APIs, retention job start/stop with shutdown. | server wiring test: configured DB ⇒ `/api/v1/agents` 200 (not 503); retention job started |
| T5 | Retention selection matches content **keys** only (quoted `"transcript":` etc.), not substrings; sanitize query/update errors propagate (tx rolls back). | ≥limit purged sessions with `frame=transcript_candidate` spans no longer starve newer content; injected sanitize error fails the run |

## Should-fix
| ID | Fix |
|---|---|
| T6 | `Session.Go` registers under the same lock that Close uses to transition state (no Add-after-Wait). |
| T7 | Processor `Close()` called after the run loop exits (bounded by T2 deadline); provider I/O still cancelled first via `s.cancel()`. |
| T8 | Tool loop: post-round stale check must not leave a committed success in history (record stale status in history, or check before commit). |
| T9 | VAD: build per-chunk trace only when the session is tracing. |
| T10 | Google STT rotation close runs via `session.Go` (fallback to goroutine only without session). |
| T11 | Nil-safe `ErrorFrame.Err` in server timeline hook. |
| T12 | `ErrMaxToolRounds` not returned for `done=false` + no calls (treat as done). |
| T13 | `cmd/server`: collapse dead Fatalf branch. |
| T14 | Remove `.superpowers/` and root `progress.md` from the tree; gitignore them. |
| T15 | `make test-postgres` target (docker or DSN) and document it; not run by default. |

## Verify
T16: full `make test` + gofmt + rerun of review probes; final review pass on the diff.
