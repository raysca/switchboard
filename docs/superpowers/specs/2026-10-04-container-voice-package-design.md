# Container voice package

Date: 2026-10-04

## Decision

WebSocket is the V1 client transport. The container image is the V1 distribution. WebRTC, telephony, and a host systemd install are out of this release. `deploy/govoice.service` may stay as a reference file. It is not a release gate.

The playback-clear waiver stays. The pinned client cannot clear audio it has already buffered. This package does not change that pin.

Concurrent capacity and p95/p99 latency are a later plan. This plan must not invent those numbers.

## Shutdown budget

`cmd/server/main.go` waits `Limits.DrainTimeout + 5s` for `Server.Shutdown`. Production `config/production.yaml` sets `drain_timeout: 45s`, so the process needs 50 seconds after SIGTERM.

Docker sends SIGKILL when `stop_grace_period` ends. That period must be longer than the shutdown budget. The Compose value for this package is 60 seconds. The reference unit's `TimeoutStopSec` uses the same 60 seconds so the two files do not disagree. Only the Compose value is release-gating.

## Proxy

The public client connects to Nginx. Nginx proxies `/ws` and `/health` to the server container. Certificate and key paths in `deploy/nginx.conf` must be files that `deploy/docker-compose.yaml` mounts. Production keeps `trusted_proxy_mode` so the server accepts the proxy's forwarded client address.

## Proof this package is real

1. A contract test fails when Compose grace is less than or equal to the shutdown budget, and when Nginx certificate paths are not mounted.
2. `govoice:release-security` starts from the production config, answers `/health` through the proxy, and exits 0 on SIGTERM with no active call.
3. One live WebSocket call runs through that image with Google credentials. The call is the existing two-turn shape: a question, then a follow-up that refers to the question.
4. One live call stays up for 30 minutes, crosses more than one 240-second STT rotation, and still answers a final utterance.

Evidence files record the commands that were run and the output they printed.
