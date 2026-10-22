# Switchboard deployment guide

For operators who run Switchboard outside a laptop. It covers what the repository ships for deployment: the container image, a Docker Compose stack, a reference systemd unit, an nginx reverse proxy, and a Google Cloud Run demo deployment. Day-2 work (drain behaviour, incidents, tuning) is in [operations.md](operations_runbook.md); what the repository does not provide is listed at the end ([Not provided](#18-not-provided-by-this-repository)).

The container image is the V1 distribution. The systemd unit is a reference file and is not required to run V1. The runtime image is Debian bookworm. A host install on Ubuntu, Debian or RHEL is not part of this release, and the host numbers below are planning guidance, not a measured result.

<!-- src: docs/operations_runbook.md:9,77; deploy/Dockerfile:52 -->

Most paragraphs end with an HTML `src:` comment naming the files (and lines) that support it. The comments are invisible when rendered and keep this guide auditable; update them when you change the facts.


## 1. Topology and ports

```
 browser / Pipecat client ──443──▶ nginx ──8080──▶ server  (/ws, /health)
 Prometheus ──────────────9092───────────────────▶ server  (/health, /ready, /metrics)
 operator ── docker exec ──127.0.0.1:9090─────────▶ server  (/api/v1, /debug/timeline)
                                                    server ──5432──▶ Postgres (flow store)
                                                    server ──────────▶ provider APIs (egress)
                                                    tini ─ server ─┬─ vadworker (child processes)
                                                                   └─ turnworker
```

<!-- src: deploy/docker-compose.yaml:5-92; deploy/nginx.conf:55-138; deploy/Dockerfile:113-114; config/production.yaml:5-12 -->

| Port | Bind | Serves | Auth | Source |
|---|---|---|---|---|
| 8080 public | `0.0.0.0:8080` | `/ws` and `/health`. With `public_admin.enabled` it also serves `/api/v1` and the embedded admin UI. Never `/metrics`, `/ready` or `/debug` | voice token on `/ws`; management key on `/api/v1` | `config/production.yaml:5`; `config/cloudrun.yaml:10-11`; `deploy/smoke-cloudrun.sh` step 2 |
| 9090 admin | `127.0.0.1:9090` (container loopback) | `/api/v1/*`, `/debug/timeline`, `/health`, `/ready`, `/metrics` | management key on `/api/v1` | `config/production.yaml:6-8`; `docs/observability.md:26` |
| 9092 probe | `0.0.0.0:9092`; `""` disables it (as `cloudrun.yaml` does) | `/health`, `/ready`, `/metrics` only | none: keep it off public ingress | `config/production.yaml:9-12`; `config/cloudrun.yaml:8`; `server/config_validate.go:93-94` |
| 80, 443 nginx | compose publishes `80:80` and `443:443` | 301 to https; TLS for `/ws` and `/health`; 404 for `/metrics` and `/debug/` | none at nginx | `deploy/nginx.conf:55-112`; `deploy/docker-compose.yaml:82-84` |
| 9091 nginx | internal vhost, allows `10.0.0.0/8` and `127.0.0.1` | `/health`, `/ready`, `/metrics`, proxied to 9092 | none | `deploy/nginx.conf:114-138` |
| 5432 | compose network only | Postgres | `POSTGRES_PASSWORD` | `deploy/docker-compose.yaml:37,60-74` |

Compose publishes 8080 and 9092 on `127.0.0.1` only. It does **not** publish 9091: the nginx vhost on 9091 exists in `nginx.conf`, but only 80 and 443 are mapped on the proxy container, so add a mapping yourself if you want it.

<!-- src: deploy/docker-compose.yaml:38-42,82-84; deploy/nginx.conf:115 -->

## 2. Prerequisites and sizing

- Docker with a Compose version that accepts `env_file` entries with `path` and `required` (the compose file uses them). To build outside Docker: Go 1.27.1, Node 22 for the admin UI, and `make ort` once to fetch the ONNX Runtime. The image builds for `amd64` and `arm64` through `TARGETARCH`; any other value fails the build.

<!-- src: deploy/docker-compose.yaml:30-32; deploy/Dockerfile:5,12,58-69; Makefile:40-45; docs/quickstart.md:9 -->

- Host planning guidance, not measured: 8 vCPU and 8 GB RAM per 250 concurrent calls, `LimitNOFILE=65536`, `LimitNPROC=4096`, and a low-jitter path to Google (`us-central1` recommended). The systemd unit sets the two limits.

<!-- src: docs/operations_runbook.md:19-24; deploy/govoice.service:31-34 -->

- Measured, framework only (echo provider on an Apple M4 Pro, so no provider latency and not your hardware): Silero costs about 76 microseconds of one core per 32 ms window per call unbatched and about 52 microseconds at batch 8, so size `vad.workers = max(2, ceil(peak_concurrent_calls / 200))`. Keep steady-state CPU below about 70% of the allotment and prefer CPU requests without a tight CPU limit: a VAD worker that has not answered for about 150 ms is killed and respawned, so a throttled container shows `voice_vad_worker_restarts_total` and `vad_unavailable` rejects with normal latency. Set `limits.max_sessions` from a load test with your real providers, at most about 70% of the count where the p95 voice-to-voice budget or a failure counter first breaks. A 2-CPU container held 24 echo sessions at 0.58 cores (41.4 sessions per core).

<!-- src: docs/benchmarks/capacity.md:61; docs/benchmarks/container-soak-churn-chaos.md:30-37; docs/operations_runbook.md:26-32 (moved here) -->

- Limits as shipped: compose caps the server at 4 CPU and 4096M (reservation 1 CPU and 1024M); the Cloud Run manifest sets 2 CPU and 2Gi, and `config/cloudrun.yaml` sets `max_sessions: 10`.

<!-- src: deploy/docker-compose.yaml:50-57; deploy/gcp/govoice.service.yaml:32-35; config/cloudrun.yaml:41 -->

- Google speech quota: about 100 concurrently speaking calls per project for Chirp 3 HD streaming (see [operations.md](operations_runbook.md#tts-streaming)).

<!-- src: docs/operations_runbook.md:256 -->

## 3. Build

- Image: `docker build -t govoice:local -f deploy/Dockerfile .`. Build arguments are `VERSION`, `COMMIT` (both exported on the `voice_build_info` metric) and `BUILD_TAGS` (default `xaispeech`). The deploy workflow builds with `--build-arg VERSION="${SHA::7}" --build-arg COMMIT="${SHA}"`.

<!-- src: docs/operations_runbook.md:40; deploy/Dockerfile:39-44; server/metrics.go:171; .github/workflows/deploy.yml:55 -->

- What the image contains: `/opt/govoice/bin/{server,vadworker,turnworker}`, the Silero VAD model and the Smart Turn model (both sha256-checked at build), ONNX Runtime 1.29.0 (sha256-checked per architecture), `/etc/govoice/production.yaml` and `/etc/govoice/cloudrun.yaml`, and the admin UI embedded in the server binary (Node is a build stage only). It runs as user `govoice` (uid 10001) behind `tini`; the default command is `server -config /etc/govoice/production.yaml`. Exposed ports are 8080, 9090 and 9092.

<!-- src: deploy/Dockerfile:5-10,33,46-50,58-115 -->

- `-tags xaispeech` is on by default in the image. Without it xAI STT and TTS do not exist in the binary, and the production profile refuses to start when a stored flow needs a provider that the build lacks (the error names each `flow <id> <stage>=<provider>`).

<!-- src: deploy/Dockerfile:36-38; server/management.go:84-91,128-150 -->

- Host binaries: `make ort && make build` writes `bin/server`, `bin/vadworker` and `bin/turnworker`, built with `-tags xaispeech`. `make ui-embed` builds the admin UI into the server binary so that `public_admin` can serve it. The server finds `vadworker` and `turnworker` next to its own executable, so keep the three together.

<!-- src: Makefile:19,26-45; docs/operations_runbook.md:79 -->

- Release hardening: `make release-security` checks that every `FROM` line and every compose image is digest-pinned, runs `govulncheck` v1.8.0, writes a module SBOM to `deploy/sbom.json` (git-ignored), builds the image as `govoice:release-security` and checks that the binaries and model exist in it, then scans the image with Trivy (falling back to Grype, then to a Trivy container). The scan fails on any fixed HIGH or CRITICAL finding.

<!-- src: Makefile:104-130; .gitignore:75 -->

## 4. Configuration files

| File | Used by | What it sets |
|---|---|---|
| `config/production.yaml` | the image default, Compose, systemd | Postgres store; probe listener on 9092; trusted proxies `127.0.0.1/32`, `::1/128`, `172.16.0.0/12` (covers the compose subnet `172.28.0.0/16`); `max_sessions: 250`, `max_pending_handshakes: 100`; `drain_timeout: 45s`; `max_call_duration: 60m`; VAD `process` mode with 8 workers; retention on (7 days content, 30 days metadata); `idle.user_timeout: 30s` |
| `config/cloudrun.yaml` | the Cloud Run demo | `public_admin` on; SQLite; `max_sessions: 10`; `max_call_duration: 20m`; trusted proxy `169.254.0.0/16`; `origin_allow_same_host: true` with an empty `origin_allowlist`; probe listener off; 2 VAD workers; retention 1 day and 7 days; `demo.auto_install: dental` |
| `config/development.yaml` | local development only | in-process VAD, an origin allowlist that includes `*`, `mcp.allow_local_http` |
| `config/perf.yaml` | `deploy/docker-compose.perf.yaml` | the echo-provider performance stack |
| `deploy/rehearsal/server.yaml` | the local Cloud Run rehearsal | `cloudrun.yaml` with the development profile; a test enforces that it differs only in profile, `tls` and `mcp` |

<!-- src: config/production.yaml:5-110; config/cloudrun.yaml:4-99; config/development.yaml:1-50; deploy/docker-compose.perf.yaml:1-12; server/deploy_contract_test.go:243-276 -->

The server takes `-config` (default `config/development.yaml`), `-profile` (`development` or `production`) and `-vad-mode` (`process` or `inprocess`). Unknown YAML keys are rejected at startup. The YAML holds infrastructure only: models, voices, prompts and flows live in the flow store and provider secrets in the environment, and a `pipelines:` section is rejected.

<!-- src: cmd/server/main.go:28-31; server/config_load.go:42,70-71; docs/quickstart.md:41 -->

Edit before a production start: `origin_allowlist` (it ships with the placeholder `https://app.govoice.internal`), and `tls.trusted_proxies` so it matches your proxy (compose's `172.16.0.0/12` is right only for the bundled nginx). `providers.google_stt.project_id` may stay empty: the server takes the project from the service-account file.

<!-- src: config/production.yaml:18-24,70-72; server/registry.go:294-297 -->

The production profile refuses to start when any of these holds (messages are from the code):

- `mcp.allow_local_http` is set;
- there is no TLS certificate and key, and no `tls.trusted_proxy_mode` with `tls.trusted_proxies`, unless `insecure_overrides.allow_plain_ws_in_production` is set;
- `trusted_proxy_mode` is on without `trusted_proxies`;
- `origin_allow_same_host` or `public_admin` is on without `tls.trusted_proxy_mode`;
- the token secret is missing, is the repository-public development secret, or is shorter than 32 characters;
- the VAD mode is `inprocess`, or the Smart Turn analyzer is loaded in a mode other than `process`;
- `storage.dialect` is empty, or the DSN variable is empty (`storage enabled but GOVOICE_DATABASE_DSN is empty`);
- the management key is shorter than 32 bytes when set, or equals the token secret, or is missing while `public_admin` is on;
- a stored flow uses a provider the build cannot run.

<!-- src: server/config_validate.go:100-125,172-179,290-300; server/management.go:34-47,84-91; server/server.go:185-198; server/auth/auth.go:25 -->

## 5. Environment variables

The complete set the shipped code and scripts read. Provider secrets are environment-only: they are never stored or returned by any API ([credentials.md](security/credentials.md)).

| Variable | Needed | Read by | Notes |
|---|---|---|---|
| `TOKEN_SECRET` | production | server | The default name; `token.secret_env` changes it. At least 32 characters, not the public development secret. Signs voice tokens |
| `GOVOICE_MANAGEMENT_API_KEY` | production in practice | server | Name set by `management.api_key_env`. At least 32 bytes, different from the token secret. Unset disables the management API (it answers 503 `management_unavailable`); `public_admin` refuses to start without it |
| `GOVOICE_DATABASE_DSN` | production | server | Name set by `storage.dsn_env`. A SQLite file path or a `postgres://` URL, matching `storage.dialect` |
| `GOVOICE_DEV_DIR` | no | server, development profile only | Directory of the local development store (default `.govoice-dev`) |
| `GOVOICE_ROOT`, `GOVOICE_MODEL_PATH`, `GOVOICE_ORT_LIB`, `GOVOICE_TURN_MODEL_PATH`, `LD_LIBRARY_PATH` | image and unit set them | server and worker processes | Where the Silero model, the Smart Turn model and ONNX Runtime live. The image sets the first three plus `LD_LIBRARY_PATH`; the unit sets the first three |
| `ORT_DISABLE_TELEMETRY` | no | set by the server | Set to `1` before ONNX Runtime starts and passed to the workers; do not override it |
| `GOOGLE_APPLICATION_CREDENTIALS` | per provider | server | A path to a service-account JSON file (STT, TTS, Vertex LLM) |
| `GOOGLE_API_KEY` | per provider | server | Gemini API key (LLM) |
| `XAI_API_KEY` | per provider | server | xAI LLM, STT, TTS |
| `OPENAI_API_KEY`, `OPENAI_BASE_URL` | per provider | server | OpenAI or compatible LLM; a private base URL needs `providers.openai_llm.allow_private_base_url` |
| `NVIDIA_API_KEY`, `NVIDIA_STT_FUNCTION_ID`, `NVIDIA_TTS_FUNCTION_ID` | per provider | server | Hosted NVIDIA only |
| `DEEPGRAM_API_KEY`, `CARTESIA_API_KEY`, `ELEVENLABS_API_KEY` | per provider | server | STT and TTS providers |
| `MCP_TOKEN_<NAME>` | per MCP server | server | Bearer token named by an MCP server's `auth_env`; the name must match `MCP_TOKEN_[A-Z0-9_]{1,48}` |
| variables named in `otel.headers_env` | with OTel | server | Header values for the OTLP exporter; an empty one stops startup |
| `DENTAL_MCP_URL` | demo only | server | Needed by `demo.auto_install: dental` |
| `POSTGRES_PASSWORD` | Compose only | compose file | URL-safe (for example `openssl rand -hex 24`) |
| `DENTAL_MCP_TOKEN`, `PORT` | `dental-mcp` image | `cmd/dental-mcp` | Token is required; `PORT` defaults to 8081 |
| `PROJECT_ID`, `GITHUB_REPO`, `GOOGLE_SA_KEY_FILE`, `REGION`, `AR_REPO`, `BILLING_ACCOUNT`, `BUDGET_USD`, `DRY_RUN` (and `XAI_API_KEY`, `ELEVENLABS_API_KEY`, `GOOGLE_API_KEY`) | one-time Cloud Run bootstrap only | `deploy/gcp/bootstrap.sh` | See [section 10](#10-path-c-google-cloud-run-the-demo-deployment) |
| `SMOKE_IMAGE`, `SMOKE_LOCAL` | smoke scripts only | `deploy/smoke.sh`, `deploy/smoke-cloudrun.sh` | See [section 17](#17-smoke-tests-troubleshooting-and-checklist) |

<!-- src: server/config_validate.go:290-300; server/config.go:81; server/server.go:185-198; server/management.go:38-47,197; vad/silero.go:40-44,62-82,120,131-140; smartturn/model.go:25; deploy/Dockerfile:107-111; deploy/govoice.service:19-21; server/catalog/data/*.yaml; docs/providers.md:11-26; docs/security/mcp.md:9; server/otelexport/otelexport.go:88-100; server/demo_api.go:35,176; deploy/docker-compose.yaml:36-37,67; cmd/dental-mcp/main.go:2,19-23; deploy/gcp/bootstrap.sh:6-10,23; deploy/smoke.sh:15; deploy/smoke-cloudrun.sh:4 -->

## 6. Secrets

Provider keys, the token secret, the management key and the database password come from the environment, never from the YAML (`token.secret` is `""` in `production.yaml`). Generate secrets with `openssl rand -hex 32`. Rotation is a change plus a restart: a running call keeps the clients it built with the old key, and nothing in the repository rotates secrets for you.

<!-- src: config/production.yaml:26-28; docs/security/credentials.md:23-25; deploy/gcp/bootstrap.sh:3,66 -->

- Compose: export `TOKEN_SECRET`, `GOVOICE_MANAGEMENT_API_KEY` and `POSTGRES_PASSWORD` in the shell that runs `docker compose`; put the provider variables, one per line, in `deploy/govoice.env` (mode 600, git-ignored); mount a Google service-account file read-only and point `GOOGLE_APPLICATION_CREDENTIALS` at the path inside the container.
- systemd: `/etc/govoice/env` (read by `EnvironmentFile`), mode 600, root-owned, holding `TOKEN_SECRET`, `GOVOICE_MANAGEMENT_API_KEY`, `GOVOICE_DATABASE_DSN` and the provider variables.
- Cloud Run: Secret Manager secrets created by the bootstrap script and injected with `secretKeyRef` (see [section 10](#10-path-c-google-cloud-run-the-demo-deployment)).
- The VAD and Smart Turn worker processes get an allowlisted environment that excludes every provider variable.

<!-- src: deploy/docker-compose.yaml:25-37; .gitignore:64; deploy/govoice.service:12-18; deploy/gcp/govoice.service.yaml:43-66; vad/silero.go:129-140 -->

## 7. Storage

| | SQLite | PostgreSQL |
|---|---|---|
| Use | one process on a local filesystem | multi-instance |
| Rule | never two processes on one file; never a filesystem without reliable locks | migrations take an advisory lock, so replicas can start together |
| Backup | online `VACUUM INTO`; verify with `PRAGMA quick_check` and `PRAGMA foreign_key_check` | `pg_dump` or physical backup with point-in-time recovery |

Migrations run forward-only at startup and a failed migration aborts the start. Back up before each upgrade, and restore into an isolated instance with the matching build. Details are in [storage.md](operations/storage.md). The Cloud Run demo keeps SQLite in `/tmp/govoice.db`, which is lost when the instance stops (the demo reinstalls itself on start). Retention settings (`retention.*`, shipped on in `production.yaml`) are described in [privacy.md](privacy.md#server-data-retention-and-privacy-controls).

<!-- src: docs/storage.md (moved from docs/operations/storage.md:9,13,15); docs/operations/hardening.md:29-40; server/storage/backup_test.go:19; deploy/gcp/govoice.service.yaml:1-3,44; .github/workflows/deploy.yml:13; config/production.yaml:90-96 -->

## 8. Path A: Docker Compose (reference production stack)

`deploy/docker-compose.yaml` runs the server, Postgres and an nginx proxy. Steps:

1. Put TLS files in a `certs` directory next to `nginx.conf` (git-ignored): `voice.example.com.crt` and `.key` for the public vhost, and `admin.crt` and `.key` for the internal 9091 vhost. The paths are fixed by `nginx.conf` and mounted by compose at `/etc/ssl/certs`; replace `voice.example.com` in the two `server_name` lines and in the file names with your host. For a throwaway test, `deploy/smoke-google.sh` creates a self-signed pair with `openssl req -x509 -newkey rsa:2048 -nodes -keyout deploy/certs/voice.example.com.key -out deploy/certs/voice.example.com.crt -days 2 -subj '/CN=localhost'` and copies it to `admin.crt` and `admin.key`.
2. Put the provider variables in `deploy/govoice.env` and export `TOKEN_SECRET`, `GOVOICE_MANAGEMENT_API_KEY` and `POSTGRES_PASSWORD` (compose refuses to start without them).
3. Start: `docker compose -f deploy/docker-compose.yaml up -d`.
4. Call the management API from inside the container, because compose does not publish port 9090: `docker exec govoice-server curl -H "Authorization: Bearer $GOVOICE_MANAGEMENT_API_KEY" http://127.0.0.1:9090/api/v1/...`. Then follow [quickstart.md](quickstart.md#5-check-providers-create-a-flow-issue-a-token) to check providers, create a flow and issue a voice token. This nginx proxies only `/ws` and `/health`, so the admin UI is not reachable through it; the UI needs `public_admin` ([section 10](#10-path-c-google-cloud-run-the-demo-deployment)).

<!-- src: deploy/nginx.conf:58,66,69-70,119-120; deploy/docker-compose.yaml:41-42,85-87; deploy/smoke-google.sh:18-23; docs/operations_runbook.md:67,71; server/deploy_contract_test.go (TestNginxCertificatesAreMounted) -->

Container hardening as shipped: user 10001, read-only root filesystem, `cap_drop: ALL`, `no-new-privileges`, a 256M `noexec` tmpfs on `/tmp`, a named volume for `/var/log/govoice`, a `/health` healthcheck, `stop_grace_period: 60s`, and the server waits for the Postgres healthcheck. `tini` is PID 1: it reaps the worker child processes and forwards SIGTERM to the server for the drain.

<!-- src: deploy/docker-compose.yaml:10-57; deploy/Dockerfile:113; docs/operations/hardening.md:59-65; deploy/security_scans.md:34-38 -->

## 9. Path B: systemd (reference unit)

The unit mirrors the image layout. It is a reference file and not required for V1.

```bash
# Service user
sudo useradd -r -s /usr/sbin/nologin -d /opt/govoice govoice

# Binaries (server, vadworker, turnworker side by side), models, ONNX Runtime
sudo install -d -m 755 /opt/govoice/bin /opt/govoice/models /opt/govoice/lib /opt/govoice/third_party /var/log/govoice
sudo install -m 755 bin/server bin/vadworker bin/turnworker /opt/govoice/bin/
sudo install -m 644 third_party/silero_vad.onnx /opt/govoice/models/silero_vad.onnx
sudo install -m 644 third_party/smart_turn_v3.2_cpu.onnx /opt/govoice/third_party/smart_turn_v3.2_cpu.onnx
# ONNX Runtime 1.29.0 for Linux; the tarball URL and sha256 are in deploy/Dockerfile
sudo install -m 644 onnxruntime-linux-x64-1.29.0/lib/libonnxruntime.so /opt/govoice/lib/libonnxruntime.so
sudo chown -R root:root /opt/govoice && sudo chown govoice:govoice /var/log/govoice

# Configuration and unit
sudo mkdir -p /etc/govoice
sudo cp config/production.yaml /etc/govoice/production.yaml
sudo cp deploy/govoice.service /etc/systemd/system/govoice.service
# create /etc/govoice/env (mode 600) first, see section 6
sudo systemctl daemon-reload
sudo systemctl enable --now govoice.service

sudo systemctl status govoice.service
journalctl -u govoice.service -f
```

Before starting, set `probe_listen_addr` in the copied `production.yaml` to `127.0.0.1:9092` (or the private interface Prometheus scrapes), or firewall port 9092: on a host, `0.0.0.0:9092` binds every interface. The unit sets `Restart=always`, `TimeoutStopSec=60s` (longer than `drain_timeout` plus the 5 s process margin), `LimitNOFILE`/`LimitNPROC`, and sandboxing (`ProtectSystem=strict`, `NoNewPrivileges`, `ReadWritePaths=/tmp /var/log/govoice`). Because of `ProtectSystem=strict`, a SQLite database must live under one of those writable paths.

<!-- src: docs/operations_runbook.md:79-118; deploy/govoice.service:17-45; config/production.yaml:12 -->

nginx on a host: `deploy/nginx.conf` is a complete `nginx.conf` (top-level `user`, `events` and `http`), not a site fragment, so install it as `/etc/nginx/nginx.conf` rather than into `sites-enabled`, then `sudo nginx -t && sudo systemctl reload nginx`. It resolves upstreams through Docker's DNS (`resolver 127.0.0.11`) and names them `govoice:8080` and `govoice:9092`: change the resolver and both upstream names for a host install, and check the `user nginx` line against your distribution.

<!-- src: deploy/nginx.conf:4-15,50-52,79,89,130; docs/operations_runbook.md:127-131 (the runbook copies it into sites-available, which does not load a file of this shape) -->
