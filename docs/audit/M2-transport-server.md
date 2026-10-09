# M2 audit: transport/ws, transport/rtvi, server/, cmd/

Round 6 independent bug audit, branch `main` (HEAD 40dcf5f), read-only. No paid provider calls.

Verification run:

- `go vet ./...` (server module): clean.
- `go test -race -count=3 ./transport/...`: ok (rtvi, token, ws).
- `go test -race -count=3` on `server`, `server/auth`, `server/api`, `server/credential`, `server/runtime`, `server/agent`, `server/otelexport`: all ok, no races and no flakes.

Severity counts: 0 blocker, 0 high, 2 medium, 4 low.

## Findings

### M2-01 (medium): new IPs can be locked out of the per-IP handshake limiter, and IPv6 gets around every per-IP cap

- **Where:** `transport/ws/transport.go:178-220` (`allowHandshakeRate`), keyed by `SecurityConfig.ClientIP` (`transport/ws/tls.go:46`). The same full-address key is used for `pendingPerIP` (`:282`) and `activePerIP` (`:308`).
- **Evidence:** the rate map is capped at 10,000 entries. When it is full, a new key triggers an O(n) sweep under `rateMu` that removes only entries idle for more than 1 minute. If the map is still full, the request is refused (`return false`, which becomes a 429). The existing test `TestHandshakeRateMapRejectsNewKeysAtCapacity` asserts this fail-closed behaviour. The key is the full client address, so IPv6 is keyed per /128.
- **Failure scenario:** an unauthenticated attacker with one IPv6 /64 sends about 170 upgrade requests per second, each from a different address. The rate check runs first, before origin and token checks. The map stays full with fresh entries, so every legitimate caller whose IP is not already in the map gets `429 handshake rate limit exceeded` for as long as the flood continues. Each refused request also costs a 10k-entry sweep under the global mutex. The reference nginx listens on `[::]:443` and forwards the real client address in XFF, so this is reachable in the reference deploy. The same per-/128 keying makes `max_sessions_per_ip` and the per-IP pending cap meaningless for IPv6 clients. Those two still need a token, so they are lower risk.
- **Fix direction:** key IPv6 by /64 (or /56). When the map is full, fall back to a shared overflow bucket instead of refusing every new key.
- **Confidence:** confirmed (code plus the existing test that pins the behaviour).
- **Launch impact:** yes. Provider-independent, public `/ws` surface.

### M2-02 (medium): failed call setups are never logged, so provider and credential misconfiguration is invisible to operators

- **Where:** `server/server.go:450-471` (`onSessionReady`: the `resolveCall` error, the `fail()` closure, and the `BuildCallPipeline` error), `transport/ws/transport.go:683-687` (the `ErrSessionSetup` close).
- **Evidence:** on every setup failure the server only increments `SessionsFailedTotal` and records a redacted `EventError` in the in-memory timeline. That timeline sits on the admin listener, keyed by an internal numeric session id the operator never sees. The transport has no logger and sends the client the fixed string `session_setup_failed` with close code 1011. No `slog` call exists on these paths. The only setup-related log is in `resolveFallbacks` (fallbacks that were skipped).
- **Failure scenario:** a stored Nvidia credential has malformed JSON (`nvidia.stt: stored credential is not valid JSON`), or a NIM base URL is refused by the SSRF guard, or an xAI or Google credential cannot be decrypted (`ErrDecrypt`), or the agent config is incomplete. Every call fails immediately with `session_setup_failed`. Operators see a rising failure counter and nothing in the logs that names the cause, stage or provider.
- **Fix direction:** add one `s.logger.Warn("call setup failed", session, agent, stage, redacted err)` in `fail()` and on the `resolveCall` error path.
- **Confidence:** confirmed.
- **Launch impact:** yes. This is the main operator signal for a bad Google, xAI or Nvidia credential or endpoint at launch.

### M2-03 (low): the forced-cancel phase of Shutdown closes sessions one at a time while holding `s.mu`, with no ctx bound

- **Where:** `server/server.go:845-858`.
- **Evidence:** after the drain deadline, the loop runs `call.session.Close()` synchronously for each active call while holding `s.mu`. `Session.Close` can take up to `TeardownTimeout`, which is 2 s per session for a wedged run loop or processor `Close` (`core/budget.go:28`). The loop never checks `ctx`. The `GoAway` sends run in parallel; the closes do not.
- **Failure scenario:** a drain timeout with many calls still active (production `max_sessions: 250`). The total time is the sum of the per-session close times. The process budget is `drain_timeout + 5 s` (`server/lifecycle.go`), and compose `stop_grace_period` is 60 s against 45 s + 5 s. A few slow closes overrun the budget, and the orchestrator SIGKILLs the process before `releaseCall`, recording finalisation and the OTel flush run. The result is session rows left active, `.part` recordings and lost final spans. Every per-call cleanup goroutine also blocks on `s.mu` for the whole loop.
- **Fix direction:** snapshot the calls under `s.mu`, release it, and close the sessions concurrently, bounded by `ctx`.
- **Confidence:** likely. The serial, unbounded loop is confirmed; the overrun depends on per-provider `Close` latency, which was not measured.
- **Launch impact:** yes on deploys and restarts, for all providers.

### M2-04 (low): the server's max-call-duration guard never fires, so its metric is always 0 and the client gets no reason

- **Where:** `server/server.go:635-653` against `transport/ws/transport.go:395-398`.
- **Evidence:** the transport arms `MaxConnectionLifetime` (the same `limits.max_call_duration`) at WebSocket accept. The server's own timer starts later, in `onSessionReady`, after client-ready and agent resolution. The transport context therefore always expires first and closes the session. The server goroutine then sees `session.Done()` and returns without counting.
- **Failure scenario:** `voice_max_duration_terminations_total` stays at 0 forever. The timeline never records "terminated: max call duration reached". The cost-guard hang-up is classified as `disconnect_timeout`, and the client gets a bare 1000 "call concluded" with no RTVI error reason, unlike the idle hang-up. No test covers this.
- **Fix direction:** drop one of the two timers. For example, have the transport timer invoke a server callback that counts the hang-up and calls `Hangup("max_call_duration")`.
- **Confidence:** confirmed (ordering is structural).
- **Launch impact:** minor (observability only; the call is still cut).

### M2-05 (low): the drain and affinity docs contradict how token pinning works

- **Where:** `server/server.go:383` (comment: "the single-use token stays valid for a retry elsewhere"), `docs/operations_runbook.md:164` ("no sticky-session config is needed... New calls land on whichever instance is ready"), against `transport/token/token.go` (`ErrTokenEpoch`) and `docs/api/v1.md:55`.
- **Evidence:** every API-issued token carries the issuing process's random `issuer_epoch`, and any other process rejects it (`ErrTokenEpoch`, 401). `docs/api/v1.md` correctly says issuance and `/ws` must hit the same replica. The runbook and the drain comment say the opposite.
- **Failure scenario:** a multi-replica deploy that follows the runbook (an L4/L7 load balancer, no affinity between `POST /api/v1/voice-tokens` and `/ws`) fails about (N-1)/N of calls with `invalid token`. A client that gets the drain 503 and retries elsewhere with the same token is always refused.
- **Fix direction:** fix the runbook and the comment to state that issuance and admission must be pinned to one replica, or ship the shared issuance service.
- **Confidence:** confirmed.
- **Launch impact:** only for multi-replica launches.

### M2-06 (low): the reference admin proxy cannot work as shipped, and making it work exposes unauthenticated debug endpoints

- **Where:** `deploy/nginx.conf` (admin server on port 9091, `proxy_pass govoice:9090`, allows 10.0.0.0/8), `deploy/docker-compose.yaml` (`127.0.0.1:9090:9090`), `config/production.yaml` (`admin_listen_addr: 127.0.0.1:9090`), `server/server.go:391-405`.
- **Evidence:** the admin listener binds container loopback, so both the nginx admin upstream and the compose port mapping are unreachable. `deploy/smoke.sh` works around this with `docker exec`. `/api/v1/*` requires the management bearer, but `/metrics`, `/ready` and `/debug/timeline` on the same listener have no auth.
- **Failure scenario:** an operator sets `admin_listen_addr: 0.0.0.0:9090` to make the documented nginx admin surface or Prometheus scraping work. `/debug/timeline?session=N` then serves per-call principals (`principal=...`, often an email or user id), redacted error text and turn timing to anything in 10.0.0.0/8 or on the Docker network, with sequential, guessable ids.
- **Fix direction:** put `/debug/timeline` (and optionally `/metrics`) behind the management bearer, and fix the compose and nginx admin wiring.
- **Confidence:** confirmed (config inconsistency). The exposure needs an operator change.
- **Launch impact:** deploy hygiene, provider-independent.

## Known-open items from `docs/pipecat-parity-matrix.md` (status re-verified)

| Item | Status now |
|---|---|
| `PingMaxFailures` has no YAML key | Still open: `Config.Limits` has no field, and `NewServer` defaults it to 2 (`transport/ws/transport.go:237`). |
| Abandoned tab holds its slot until `max_call_duration` unless `idle.user_timeout` is set | Still open: `config/production.yaml` still only has the commented `idle:` recommendation. |
| Raw error text at `server/call.go` ("turn not recorded") and `server/recording.go` ("call recording not saved") | Still open: `call.go:99` and `recording.go:61` log `err.Error()` without redaction. |
| Admission is reject-only | Still open (`server/admission.go`). |
| Recording unencrypted at rest, taps before filters, not stopped by a mid-call agent disable | Still open. `attachRecording` is bound once at setup. |

## Areas checked, found clean

- **WS frame decoding:** coder/websocket's default 32 KiB read limit applies, which is below the 64 KiB `MaxEncodedFrameSize`. Audio is bounded at 4 KiB, 10-40 ms, mono, 16/24/48 kHz, aligned. Nested message data is capped at 16 KiB, and protobuf errors close with 1003. Text frames are ignored. Audio before client-ready closes the call.
- **RTVI:** duplicate client-ready is rejected (the `Swap` guard prevents a session leak). Version gating works. send-text is size, UTF-8 and rate limited and returns fixed error text. DTMF is validated and rate limited per key. Non-RTVI labels are ignored. Error frames to the client carry a category only, never `ef.Err`.
- **Handshake order:** pending and active slots are reserved under one lock and released on every return path through `defer`. The pending slot is released after Accept. The token is validated and consumed atomically. Replay, expiry, iat-before-start, future iat, lifetime cap, alg allowlist (HS256 only), production `kid` and scope/epoch checks are all present. The replay cache is bounded and fails closed. Signature verification happens before any replay-cache write.
- **Slot accounting:** admission is released on the resolve failure, `fail()` and session-done paths. Session `Close` always closes `Done`, including when `Start` failed. The `connecting` WaitGroup and `shuttingDown` are ordered under `s.mu`, so drain cannot miss a registering call. The ws-level slot is held by `ServeHTTP` and released by `defer`.
- **Close paths:** abrupt disconnect closes quickly (EOF on Read, then a cancelled Read ctx closes the socket). The ping liveness path uses `CloseNow`. `closeBounded` bounds handshakes. The ready watchdog exits on ctx. `wg.Wait` covers the reader, writer and pinger. The pacer's flush, drain and close paths abort in-flight waits, and pacer callbacks recover panics.
- **Backpressure:** the output gate in `WaitRoom` is bounded by the write timeout. `enqueueOutput` gives up at the deadline and treats a hang-up as non-fatal. Inbound audio drops on a full data queue, and a full control queue closes the call. Per-session VAD outstanding is capped by `PerSessionLimit`. Unbounded inbound audio rate only hurts the caller's own STT stream.
- **Management API:** a bearer key of at least 32 bytes is compared as a constant-time SHA-256 digest. The key must differ from the voice signing key. JSON bodies are limited to 64 KiB, unknown fields and trailing values are rejected, pagination is bounded, and path parsing is strict. Credential DTOs never echo secrets, and errors are fixed strings. SQL is parameterised (no string-built queries). The token endpoint caps principal and agent id at 128 characters and checks the agent is callable. Effective token lifetime is at most 60 s.
- **SSRF:** the MCP server URL blocklist covers private, metadata and reserved ranges. NIM base URLs and Riva endpoints go through the `AllowPrivate` dial guard, and plaintext requires opt-in. The OTel endpoint rejects userinfo and query strings, and its headers come from env.
- **Secrets:** decrypted credentials are wiped after the pipeline build (`wipeSecrets`, and the deferred clear on error). Dev store keys are 0600 and created with `O_EXCL`. Production refuses an empty or dev token secret and secrets under 32 characters. Token query strings are redacted in the nginx access log.
- **Recording paths:** the id is validated by `record.ValidID`, `storage.IsID` and a length cap. Files use `O_EXCL` and mode 0600. Size and duration are capped.
- **Config:** the strict YAML decode uses `KnownFields`. Removed keys get actionable errors. Production requires TLS or a trusted proxy, VAD and analyzer in `process` mode, and a non-dev secret. The ping timeout must be less than the ping interval. The `-profile` override re-applies production defaults and validation.
- **Timeline:** each ring is bounded at `timeline_max_events`. The registry is pruned by idle TTL every minute, and error details are redacted.
- **Metrics:** no per-principal or per-agent labels, so there is no cardinality blow-up. The connected-clients gauge is balanced by `closeOnce`.
- **`cmd/server`:** the config is validated after the `-vad-mode` override. Shutdown runs within `ShutdownBudget`, and ORT is shut down after the executor closes.
