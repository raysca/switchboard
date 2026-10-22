# Switchboard deployment guide

For operators who run Switchboard outside a laptop. It covers what the repository ships for deployment: the container image, a Docker Compose stack, a reference systemd unit, an nginx reverse proxy, and a Google Cloud Run demo deployment. Day-2 work (drain behaviour, incidents, tuning) is in [operations.md](operations.md); what the repository does not provide is listed at the end ([Not provided](#18-not-provided-by-this-repository)).

The container image is the V1 distribution. The systemd unit is a reference file and is not required to run V1. The runtime image is Debian bookworm. A host install on Ubuntu, Debian or RHEL is not part of this release, and the host numbers below are planning guidance, not a measured result.

<!-- src: former runbook; deploy/Dockerfile:52 -->

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

<!-- src: former runbook; deploy/govoice.service:31-34 -->

- Measured, framework only (echo provider on an Apple M4 Pro, so no provider latency and not your hardware): Silero costs about 76 microseconds of one core per 32 ms window per call unbatched and about 52 microseconds at batch 8, so size `vad.workers = max(2, ceil(peak_concurrent_calls / 200))`. Keep steady-state CPU below about 70% of the allotment and prefer CPU requests without a tight CPU limit: a VAD worker that has not answered for about 150 ms is killed and respawned, so a throttled container shows `voice_vad_worker_restarts_total` and `vad_unavailable` rejects with normal latency. Set `limits.max_sessions` from a load test with your real providers, at most about 70% of the count where the p95 voice-to-voice budget or a failure counter first breaks. A 2-CPU container held 24 echo sessions at 0.58 cores (41.4 sessions per core).

<!-- src: docs/benchmarks/capacity.md:61; docs/benchmarks/container-soak-churn-chaos.md:30-37; former runbook  -->

- Limits as shipped: compose caps the server at 4 CPU and 4096M (reservation 1 CPU and 1024M); the Cloud Run manifest sets 2 CPU and 2Gi, and `config/cloudrun.yaml` sets `max_sessions: 10`.

<!-- src: deploy/docker-compose.yaml:50-57; deploy/gcp/govoice.service.yaml:32-35; config/cloudrun.yaml:41 -->

- Google speech quota: about 100 concurrently speaking calls per project for Chirp 3 HD streaming (see [operations.md](operations.md#tts-streaming)).

<!-- src: docs/operations.md:124 -->

## 3. Build

- Image: `docker build -t govoice:local -f deploy/Dockerfile .`. Build arguments are `VERSION`, `COMMIT` (both exported on the `voice_build_info` metric) and `BUILD_TAGS` (default `xaispeech`). The deploy workflow builds with `--build-arg VERSION="${SHA::7}" --build-arg COMMIT="${SHA}"`.

<!-- src: former runbook; deploy/Dockerfile:39-44; server/metrics.go:171; .github/workflows/deploy.yml:55 -->

- What the image contains: `/opt/govoice/bin/{server,vadworker,turnworker}`, the Silero VAD model and the Smart Turn model (both sha256-checked at build), ONNX Runtime 1.29.0 (sha256-checked per architecture), `/etc/govoice/production.yaml` and `/etc/govoice/cloudrun.yaml`, and the admin UI embedded in the server binary (Node is a build stage only). It runs as user `govoice` (uid 10001) behind `tini`; the default command is `server -config /etc/govoice/production.yaml`. Exposed ports are 8080, 9090 and 9092.

<!-- src: deploy/Dockerfile:5-10,33,46-50,58-115 -->

- `-tags xaispeech` is on by default in the image. Without it xAI STT and TTS do not exist in the binary, and the production profile refuses to start when a stored flow needs a provider that the build lacks (the error names each `flow <id> <stage>=<provider>`).

<!-- src: deploy/Dockerfile:36-38; server/management.go:84-91,128-150 -->

- Host binaries: `make ort && make build` writes `bin/server`, `bin/vadworker` and `bin/turnworker`, built with `-tags xaispeech`. `make ui-embed` builds the admin UI into the server binary so that `public_admin` can serve it. The server finds `vadworker` and `turnworker` next to its own executable, so keep the three together.

<!-- src: Makefile:19,26-45; former runbook -->

- Release hardening: `make release-security` checks that every `FROM` line and every compose image is digest-pinned, runs `govulncheck` v1.8.0, writes a module SBOM to `deploy/sbom.json` (git-ignored), builds the image as `govoice:release-security` and checks that the binaries and model exist in it, then scans the image with Trivy (falling back to Grype, then to a Trivy container). The gate is zero critical and zero high findings (the scan fails on any fixed HIGH or CRITICAL one); medium and low findings are triaged per release.

<!-- src: Makefile:104-130; .gitignore:75; former security-scans guide §2 -->

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

Migrations run forward-only at startup and a failed migration aborts the start. Back up before each upgrade, and restore into an isolated instance with the matching build. Details are in [storage.md](storage.md). The Cloud Run demo keeps SQLite in `/tmp/govoice.db`, which is lost when the instance stops (the demo reinstalls itself on start). Retention settings (`retention.*`, shipped on in `production.yaml`) are described in [privacy.md](privacy.md#server-data-retention-and-privacy-controls).

<!-- src: docs/storage.md (moved up from the former operations directory); former hardening guide §2; server/storage/backup_test.go:19; deploy/gcp/govoice.service.yaml:1-3,44; .github/workflows/deploy.yml:13; config/production.yaml:90-96 -->

## 8. Path A: Docker Compose (reference production stack)

`deploy/docker-compose.yaml` runs the server, Postgres and an nginx proxy. Steps:

1. Put TLS files in a `certs` directory next to `nginx.conf` (git-ignored): `voice.example.com.crt` and `.key` for the public vhost, and `admin.crt` and `.key` for the internal 9091 vhost. The paths are fixed by `nginx.conf` and mounted by compose at `/etc/ssl/certs`; replace `voice.example.com` in the two `server_name` lines and in the file names with your host. For a throwaway test, `deploy/smoke-google.sh` creates a self-signed pair with `openssl req -x509 -newkey rsa:2048 -nodes -keyout deploy/certs/voice.example.com.key -out deploy/certs/voice.example.com.crt -days 2 -subj '/CN=localhost'` and copies it to `admin.crt` and `admin.key`.
2. Put the provider variables in `deploy/govoice.env` and export `TOKEN_SECRET`, `GOVOICE_MANAGEMENT_API_KEY` and `POSTGRES_PASSWORD` (compose refuses to start without them).
3. Start: `docker compose -f deploy/docker-compose.yaml up -d`.
4. Call the management API from inside the container, because compose does not publish port 9090 (on Kubernetes use `kubectl port-forward` or `exec`): `docker exec govoice-server curl -H "Authorization: Bearer $GOVOICE_MANAGEMENT_API_KEY" http://127.0.0.1:9090/api/v1/...`. Then follow [quickstart.md](quickstart.md#5-check-providers-create-a-flow-issue-a-token) to check providers, create a flow and issue a voice token. This nginx proxies only `/ws` and `/health`, so the admin UI is not reachable through it; the UI needs `public_admin` ([section 10](#10-path-c-google-cloud-run-the-demo-deployment)).

<!-- src: deploy/nginx.conf:58,66,69-70,119-120; deploy/docker-compose.yaml:41-42,85-87; deploy/smoke-google.sh:18-23; former runbook; server/deploy_contract_test.go (TestNginxCertificatesAreMounted) -->

Container hardening as shipped: user 10001, read-only root filesystem, `cap_drop: ALL`, `no-new-privileges`, and only two writable places, a 256M `noexec` tmpfs on `/tmp` and a named volume for `/var/log/govoice`; a `/health` healthcheck, `stop_grace_period: 60s`, and the server waits for the Postgres healthcheck. `tini` is PID 1: it reaps the worker child processes and forwards SIGTERM to the server for the drain.

<!-- src: deploy/docker-compose.yaml:10-57; deploy/Dockerfile:113; former hardening guide §4; former security-scans guide §3 -->

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
# ONNX Runtime 1.29.0 for Linux: download and unpack the release tarball (x86_64: onnxruntime-linux-x64-1.29.0.tgz,
# arm64: onnxruntime-linux-aarch64-1.29.0.tgz) from https://github.com/microsoft/onnxruntime/releases/download/v1.29.0/
# and verify its sha256 against the values in deploy/Dockerfile. Use the directory name of your architecture below.
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

Before starting, set `probe_listen_addr` in the copied `production.yaml` to `127.0.0.1:9092` (or the private interface Prometheus scrapes), or firewall port 9092: on a host, `0.0.0.0:9092` binds every interface. The unit keeps only `CAP_NET_BIND_SERVICE` (needed only to bind ports below 1024 without a proxy), and sets `Restart=always`, `TimeoutStopSec=60s` (longer than `drain_timeout` plus the 5 s process margin), `LimitNOFILE`/`LimitNPROC`, and sandboxing (`ProtectSystem=strict`, `NoNewPrivileges`, `ReadWritePaths=/tmp /var/log/govoice`). `production.yaml` ships `storage.dialect: "postgres"`, so provide a database as described next.

**Writable paths and SQLite.** `ProtectSystem=strict` makes the whole filesystem read-only except `ReadWritePaths=/tmp /var/log/govoice`, and `PrivateTmp=true` gives the service a fresh, empty `/tmp` on every start. So a SQLite file in `/tmp` is lost at each restart, and `/var/log/govoice` is the log directory. If you use SQLite (one process only, [section 7](#7-storage)), give it its own directory and add it to the sandbox with a drop-in, then set `storage.dialect: "sqlite"` in `/etc/govoice/production.yaml` and `GOVOICE_DATABASE_DSN=/var/lib/govoice/switchboard.db` in `/etc/govoice/env`:

```bash
sudo install -d -m 700 -o govoice -g govoice /var/lib/govoice
sudo systemctl edit govoice.service     # add the two lines below, then save
#   [Service]
#   ReadWritePaths=/var/lib/govoice
sudo systemctl restart govoice.service
```

The directory must be writable by `govoice` because SQLite also creates WAL files and `<database>.migration.lock` beside the database file ([storage.md](storage.md)). Back it up as described in section 7.

**Postgres for the systemd path.** The Compose stack creates the role and database from `POSTGRES_USER=govoice` and `POSTGRES_DB=govoice`; on a host you create them yourself. The role must own the database, because the server runs its migrations at startup. Run, as a Postgres superuser, `CREATE ROLE govoice LOGIN PASSWORD '<url-safe password>';` and `CREATE DATABASE govoice OWNER govoice;`, then set the DSN in `/etc/govoice/env`: `GOVOICE_DATABASE_DSN=postgres://govoice:<password>@<host>:5432/govoice?sslmode=require`. Compose uses `sslmode=disable` because Postgres is on its private network; for a remote database use `require`, or `verify-full` with the CA configured on the host (pgx understands the standard libpq `sslmode` parameter). Keep the password URL-safe (for example `openssl rand -hex 24`), because the DSN is a URL.

<!-- src: former runbook; deploy/govoice.service:17-45; config/production.yaml:12,87-88; deploy/docker-compose.yaml:36-37,60-64; deploy/Dockerfile:58-75; server/storage/storage.go:56-70,131-160; server/management.go:84-92; docs/storage.md -->

nginx on a host: `deploy/nginx.conf` is a complete `nginx.conf` (top-level `user`, `events` and `http`), not a site fragment, so install it as `/etc/nginx/nginx.conf` rather than into `sites-enabled`, then `sudo nginx -t && sudo systemctl reload nginx`. It resolves upstreams through Docker's DNS (`resolver 127.0.0.11`) and names them `govoice:8080` and `govoice:9092`: change the resolver and both upstream names for a host install, and check the `user nginx` line against your distribution.

<!-- src: deploy/nginx.conf:4-15,50-52,79,89,130; former runbook (the runbook copies it into sites-available, which does not load a file of this shape) -->

## 10. Path C: Google Cloud Run (the demo deployment)

This is the deployment the repository automates, and it is a **demo**, not a production reference: one voice-server instance, SQLite on the instance's memory disk, a public `dental-mcp` service and the Smile Dental flow reinstalled on every start. Production-style scaling uses Path A. The shape:

- Voice server (`deploy/gcp/govoice.service.yaml`): `maxScale: 1` (SQLite and live calls are in memory), `minScale` from `MIN_INSTANCES`, CPU always allocated (`cpu-throttling: "false"`), second-generation execution environment, session affinity on, `timeoutSeconds: 3600`, `containerConcurrency: 20`, 2 CPU and 2Gi, a startup probe on `/health:8080`, and `config/cloudrun.yaml` (which caps a call at 20 minutes). It runs `tini -- server -config /etc/govoice/cloudrun.yaml`.
- `dental-mcp` (`deploy/gcp/dental-mcp.service.yaml`): in-memory fake data, so `maxScale: 1`, scaling to zero, 1 CPU and 256Mi, token from the `DENTAL_MCP_TOKEN` secret.

<!-- src: deploy/gcp/govoice.service.yaml:1-66; deploy/gcp/dental-mcp.service.yaml:1-34; config/cloudrun.yaml:1-4,46 -->

**One-time setup.** Export the variables listed in [section 5](#5-environment-variables) for the bootstrap (`PROJECT_ID`, `GITHUB_REPO` as `owner/name`, the three provider keys and `GOOGLE_SA_KEY_FILE`; optional `REGION`, default `us-central1`, `AR_REPO`, `BILLING_ACCOUNT` and `BUDGET_USD`), preview with `DRY_RUN=1 deploy/gcp/bootstrap.sh`, then run `deploy/gcp/bootstrap.sh`. It is idempotent and never rotates an existing secret. It enables the APIs it needs, creates an Artifact Registry repository, three service accounts (`govoice-runtime`, `dental-mcp-runtime`, `govoice-deployer`), seven Secret Manager secrets (`XAI_API_KEY`, `ELEVENLABS_API_KEY`, `GOOGLE_API_KEY`, `GOOGLE_SA_KEY`, and random `TOKEN_SECRET`, `GOVOICE_MANAGEMENT_API_KEY`, `DENTAL_MCP_TOKEN`) with read access scoped per account, a Workload Identity Federation pool and provider restricted to `GITHUB_REPO` on `refs/heads/main`, the GitHub repository variables `GCP_PROJECT_ID`, `GCP_REGION`, `GCP_WIF_PROVIDER` and `GCP_DEPLOYER_SA` (when `gh` is available), and, if `BILLING_ACCOUNT` is set, a budget alert (default 150 USD; the script notes that a warm 2 vCPU and 2 GiB instance alone is about 100 USD a month).

<!-- src: deploy/gcp/bootstrap.sh:1-134 -->

**Continuous deploy** (`.github/workflows/deploy.yml`). It runs after the `ci` workflow succeeds for a push to `main` in this repository, or by `workflow_dispatch` from `main` with a `min_instances` input (default 1; 0 stops idle spend and loses nothing, because the demo reinstalls on start). It authenticates with Workload Identity Federation (no GCP key is stored in GitHub), builds both images tagged with the commit SHA, deploys `dental-mcp`, deploys the voice server as a `candidate` revision with 0% traffic, runs `deploy/smoke-cloudrun.sh` against the candidate, and only then moves traffic to it and removes the tag. A failed smoke test removes the tag and leaves the previous revision serving. The first deploy (no previous revision) has no candidate step. Both services are granted the `allUsers` invoker role, so they are public: the management key and voice tokens are the protection.

<!-- src: .github/workflows/deploy.yml:1-109 -->

**Signing in to the admin UI.** After the first deploy, `gcloud secrets versions access latest --secret=GOVOICE_MANAGEMENT_API_KEY --project <project> | scripts/admin-link.sh https://<service-url>` (also printed by the bootstrap), or `make admin-link URL=https://<service-url>` with the key in `GOVOICE_MANAGEMENT_API_KEY`. The link is `<url>/#key=<key>`: the key is in the URL fragment, which browsers never send to a server, and the admin UI reads it into memory and removes it from the address bar and history at once. The link is a credential: do not paste it into chat or tickets. The hosted UI never sends a keyless "are you signed in" request (that would count as a failed login below).

<!-- src: deploy/gcp/bootstrap.sh:132-134; Makefile:199; former runbook -->

**Local rehearsal.** `deploy/rehearsal/` runs the production image, the dentistry MCP server and an nginx that behaves like Cloud Run's front end on `http://localhost:8080`, with the development profile. Follow [its README](../deploy/rehearsal/README.md); it states what the rehearsal does not prove.

<!-- src: deploy/rehearsal/README.md:1-20 -->

### Public admin mode (single port)

By default the management API (`/api/v1`) lives on the loopback admin listener and the admin UI runs separately. With `public_admin: {enabled: true}` the public listener also serves the management API and the embedded admin UI (build it in with `make ui-embed`), so one port carries the UI, the API and the voice socket. It is off by default and `config/cloudrun.yaml` turns it on.

- Every management route still needs the management key. `/metrics`, `/ready` and `/debug/*` are not mounted on the public port (they answer non-200 there): metrics expose internals and stay on the admin listener. `/health` stays public.
- `origin_allow_same_host: true` admits a browser whose `Origin` host equals the request `Host`, so the page can open the voice socket without listing its own origin in `origin_allowlist`. A foreign browser origin is still rejected.
- Production requires `tls.trusted_proxy_mode` for both keys: the `Host` header and the client address must come from your proxy, not the caller. See `config/cloudrun.yaml`.
- Failed management-key attempts share ONE global allowance (burst 10, one more every 6 s). A correct key is checked first and is never throttled, so no flood of bad keys, from any address, can lock the admin out. Once the allowance is spent, a wrong or missing key gets 429 `too_many_attempts` ("too many failed attempts; try again shortly", with `Retry-After`) instead of 401, and the UI shows that message. A missing key counts as a failure. `voice_management_auth_failures_total` counts every failed attempt and `voice_management_auth_throttled_total` those answered 429.
- `GET /api/v1/server-config` (key required, never cached) reports `client_ip` and `secure`; check it after each deploy. If `client_ip` is a Google front-end or load-balancer address, or `secure` is false, add the proxy's CIDR to `tls.trusted_proxies`; otherwise every caller shares one bucket in the per-IP rate limits and the handshake limiter (the auth throttle is global and unaffected). `deploy/smoke-cloudrun.sh` checks this for you.
- End-to-end check: `make e2e-oneport` (echo provider, no secrets).

<!-- src: moved from the former runbook; server/api_routes.go:35; server/config_validate.go:114-118; deploy/smoke-cloudrun.sh:30-39; Makefile:180 -->

## 11. TLS and the reverse proxy

The public edge must terminate TLS, either at a proxy or in the server. `deploy/nginx.conf` passes the WebSocket `Upgrade` and `Connection` headers, redacts `token=` in its JSON access log (`token=[REDACTED]`), allows TLS 1.2 and 1.3 only, sets 3600 s read and send timeouts for `/ws` (matching `max_call_duration: 60m`), proxies only `/ws` and `/health`, and answers 404 for `/metrics` and `/debug/`. It has a single upstream and no `proxy_next_upstream`.

<!-- src: deploy/nginx.conf:19-23,44-47,71,87-112; config/production.yaml:47 -->

The server can also terminate TLS itself with `tls.cert_file` and `tls.key_file`; the production profile needs either that or `tls.trusted_proxy_mode` with `tls.trusted_proxies`. With a trusted proxy the client address is the right-most untrusted `X-Forwarded-For` entry (else `X-Real-IP`), and the proxy must be listed in `tls.trusted_proxies`, otherwise every caller shares the proxy's per-IP bucket. A multi-instance edge needs an upstream block with `proxy_next_upstream error http_503`; the repository does not provide one.

<!-- src: server/server.go:409,1022-1036; server/config_validate.go:108-113; transport/ws/tls.go:55-86; docs/operations.md:37,40 -->

## 12. Health, readiness and probes

`/health` is a cheap local liveness check (`{"status":"live"}`). `/ready` answers 200 `{"status":"ready","circuits":{...}}` only when initialisation has finished, the server is not draining, admission is enabled, a VAD worker is online and the flow store answers a ping; otherwise 503 `{"status":"unready","reason":...}`. Provider circuits are reported for diagnosis and never make the server unready. Point readiness at `/ready` on the probe port (9092) and liveness at `/health` (9092 or public 8080), and never use `/ready` for liveness: it is 503 for the whole drain and would kill the instance mid-drain. A Kubernetes sketch (the repository ships no manifests): `terminationGracePeriodSeconds: 60`, `lifecycle.preStop.exec.command: ["sleep","5"]`, `readinessProbe.httpGet: {path: /ready, port: 9092}`, `livenessProbe.httpGet: {path: /health, port: 9092}`.

<!-- src: docs/observability.md:28-43; docs/operations.md:12-16,35,51 -->

## 13. Drain, rolling upgrades and rollback

On SIGTERM the server marks itself unready (`/ready` returns 503, `voice_draining 1`), refuses new calls with HTTP 503 `server_draining` while keeping the public listener open, lets active calls run until `limits.drain_timeout` (45 s in `production.yaml`; `voice_drain_sessions_remaining` shows the rest), then sends each remaining call a fatal RTVI error `server_draining` and close 1001 and force-closes it. It exits 0 when all calls ended inside the deadline and 1 when calls had to be force-closed. The full sequence is in the [operations guide](operations.md#graceful-drain-then-cancel-shutdown).

<!-- src: server/server.go:1093-1175; cmd/server/main.go:129-133; config/production.yaml:55; docs/operations.md:19-32 -->

- The grace period (`docker stop -t`, Compose `stop_grace_period`, systemd `TimeoutStopSec`, Kubernetes `terminationGracePeriodSeconds`) must be at least `drain_timeout` + `preStop` sleep + 5 s; the server budgets `drain_timeout + 5s`. Production: 45 + 5 + 5, so 60 s or more. Compose and the unit use 60 s; `docker stop` without `-t 60` defaults to 10 s and kills the process mid-drain.
- `limits.immediate_cancel: true` skips the drain wait and cancels calls at once.
- Add `preStop: sleep 5` so endpoint removal reaches the load balancer before the drain starts, enable load-balancer connection draining, and roll one instance at a time.
- Voice tokens are bound to the process that issued them. With more than one replica, `POST /api/v1/voice-tokens` and the `/ws` upgrade for that token must reach the same replica (sticky routing, or issue the token through the replica you will connect to), otherwise most calls fail with `invalid token`. A client that gets the drain 503 requests a new token and reconnects.
- Upgrade order for new providers: roll forward first, and create flows that use a new provider only after every replica runs the new build; to roll back, disable, archive or edit those flows first, because an old binary refuses to start when the flow store holds a flow it cannot run (the error names each `flow <id> <stage>=<provider>`).
- Migrations are forward-only: back up first ([storage.md](storage.md)). Before rolling back past migration 007, or past the additive per-flow fields `idle`, `audio` and `greeting`, read the cautions in the operations runbook.
- After a crash, the next start ends the calls the old process left `active` in storage (see the operations runbook).
- Cloud Run: the deploy workflow promotes a smoke-tested candidate as described in [section 10](#10-path-c-google-cloud-run-the-demo-deployment). After promotion the repository has no rollback script.

<!-- src: server/server.go:1100-1106; server/lifecycle.go:5-11; deploy/docker-compose.yaml:49; deploy/govoice.service:25-27; server/lifecycle_budget_test.go; docs/operations.md:25,36-39,43-44,46-50; transport/token/token.go:27,173 -->

## 14. Scaling and limits

The `limits` block in `config/production.yaml` sets the ceilings: `max_sessions` (250), `max_sessions_per_principal` (5), `max_sessions_per_ip` (5), `max_pending_handshakes` (100), `upgrade_timeout`, `ready_timeout`, `max_call_duration` (60m), `ping_interval` (15s) and `ping_timeout` (10s), and `drain_timeout` (45s); `max_sessions_per_flow`, `start_rate_per_principal` and `handshake_rate_per_ip` are commented defaults. A refused call gets an RTVI error (`capacity_exceeded`, `principal_limit`, `flow_limit` or `rate_limited`) and close 1013, counted in `voice_admission_rejects_total{reason}` with reasons `session_limit`, `vad_unavailable`, `principal_limit`, `flow_limit` and `start_rate_limit`. WebSocket upgrades are limited per source IP (`limits.handshake_rate_per_ip`, default 20 per second) and answered with HTTP 429 before the upgrade. Details are in the [operations guide](operations.md#rolling-deploys-and-session-affinity).

<!-- src: config/production.yaml:36-56; server/config_defaults.go:36-38; docs/operations.md:40-41 -->

Several replicas need Postgres and sticky token issuance plus `/ws` (section 13), and the same `limits.max_call_duration` on every replica. Live-call control and draft test calls are held in the memory of one instance. SQLite and the Cloud Run demo are single-instance by design.

<!-- src: docs/storage.md; docs/operations.md:39,44,49,112; deploy/gcp/govoice.service.yaml:17 -->

## 15. Monitoring and alerts

Prometheus scrapes `:9092/metrics` (the probe listener). `deploy/alerts/govoice.rules.yml` holds 15 alert rules, tuned for `max_sessions: 250`: `GovoiceDown`, `GovoiceDraining`, `GovoiceSessionFailures`, `GovoiceNearCapacity`, `GovoiceAdmissionRejecting`, `GovoiceVADUnavailable`, `GovoiceVADWorkerRestarts`, `GovoiceLeakedWork`, `GovoiceGoroutineGrowth`, `GovoiceProviderCircuitOpen`, `GovoiceProviderErrors`, `GovoiceVoiceToVoiceSlow`, `GovoiceAuthFailureSpike`, `GovoiceSlowClients` and `GovoiceOTelLosingSpans`. A test fails when a rule or this document names a metric the server does not emit. Logs are JSON when `observability.json_format` is true (as in `production.yaml`), and the per-session timeline is on the admin listener (`/debug/timeline?session=<ID>`). OpenTelemetry export is optional and off by default. The metric catalogue and the OTel keys are in [observability.md](observability.md); the response to each alert is in the [operations guide](operations.md).

<!-- src: deploy/alerts/govoice.rules.yml:1-110; config/production.yaml:79-83; docs/observability.md:47-158; server/docs_contract_test.go:72 -->

## 16. Security checklist

- [ ] Management key at least 32 bytes, different from the token secret (it is stored as a SHA-256 digest and compared in constant time; requests need `Authorization: Bearer <key>`).
- [ ] Token secret at least 32 characters and not the public development secret; `token.secret` empty in the YAML.
- [ ] Admin listener on loopback only; probe port 9092 not on public ingress; nginx returns 404 for `/metrics` and `/debug/`.
- [ ] `origin_allowlist` and `tls.trusted_proxies` set for your proxy; `GET /api/v1/server-config` shows the real `client_ip` and `secure: true`.
- [ ] Container hardening kept (section 8); `deploy/govoice.env` and `/etc/govoice/env` mode 600; the TLS key files never committed (the `certs` directory is git-ignored).
- [ ] `make release-security` clean for the image you ship.
- [ ] MCP servers use public HTTPS endpoints only, and tokens come from `MCP_TOKEN_*` variables ([security/mcp.md](security/mcp.md)). `mcp.allow_local_http` stays off in production (the server refuses to start with it).
- [ ] Private OpenAI-compatible base URLs stay refused unless `providers.openai_llm.allow_private_base_url` is set on purpose.
- [ ] The admin magic link is handled as a credential.
- [ ] Cloud Run: both services are public (`allUsers` invoker) and deploys come only from `main` of the named repository through Workload Identity Federation.
- [ ] Privacy controls (retention, recording, redaction) set as required: [privacy.md](privacy.md).

<!-- src: server/auth/auth.go:25,44-45; server/server.go:198; server/config_validate.go:100-125,290-300; deploy/nginx.conf:105-111; docs/providers.md:14; docs/security/mcp.md:13; .github/workflows/deploy.yml:25-29,63-64,84-85; deploy/gcp/bootstrap.sh:93-109 -->

## 17. Smoke tests, troubleshooting and checklist

| Check | Needs | What it proves |
|---|---|---|
| `deploy/smoke.sh` (CI job `image-smoke`) | docker, curl, openssl; no provider key | builds the image, starts it read-only with all capabilities dropped next to a throwaway Postgres, probes `/ready`, `/metrics` and `/health` from outside, checks the probe port does not serve the management API or `/debug/timeline`, checks xAI speech is in the build, and checks that SIGTERM exits 0 |
| `deploy/smoke-google.sh` | `TOKEN_SECRET`, `GOOGLE_APPLICATION_CREDENTIALS` (host path), docker, openssl, jq | the Compose stack end to end with a Google service account: flow ready, voice token issued, `https://127.0.0.1/health` through nginx, clean SIGTERM |
| `deploy/smoke-cloudrun.sh <url>` | `GOVOICE_MANAGEMENT_API_KEY`, jq; `SMOKE_LOCAL=1` for the rehearsal | UI served, `/metrics`, `/ready` and `/debug/timeline` absent on the public port, management API behind the key, `client_ip` and `secure` correct, three providers detected, the demo flow and MCP server ready, failed-login throttling |
| `make e2e-oneport` | Node and Playwright | the one-port server on the echo provider, no secrets |
| `make release-security` | docker, Go | the hardening gate of [section 3](#3-build) |

<!-- src: deploy/smoke.sh:1-70; deploy/smoke-google.sh:1-70; deploy/smoke-cloudrun.sh:1-62; .github/workflows/ci.yml:123-130; Makefile:180 -->

| Symptom | Likely cause | Fix |
|---|---|---|
| Start fails with one of the refusals in [section 4](#4-configuration-files) | production profile checks | fix the named setting |
| `storage enabled but GOVOICE_DATABASE_DSN is empty` | the DSN variable is unset | set `GOVOICE_DATABASE_DSN` (or the name in `storage.dsn_env`) |
| `stored flow stage(s) use providers missing from this build` | flow uses xAI speech on a build without `-tags xaispeech`, or a provider from a newer release | build with the default tag, or disable, archive or edit the listed flows |
| `/ready` is 503 | starting, draining, no VAD worker online, or the flow store does not answer | read the `reason` field; check Postgres and the worker logs |
| `invalid token` on some calls with several replicas | the token was issued by a different process (`ErrTokenEpoch`) | route token issue and `/ws` to the same replica |
| HTTP 429 on WebSocket upgrades, or every caller in one bucket | `tls.trusted_proxies` does not list the proxy | add the proxy CIDR; check `server-config` `client_ip` |
| `voice_vad_worker_restarts_total` rising, `vad_unavailable` rejects | CPU starvation or a tight CPU limit; or a worker crash (`vad: worker failed` WARN) | give more CPU; read the WARN line's `stderr` field |
| Calls cut at deploy time | grace period shorter than `drain_timeout` + 5 s | `docker stop -t 60`, `stop_grace_period` 60 s |
| `voice_{stt,llm,tts}_circuit_state` is 1 | five consecutive provider failures | check the provider and key; the circuit half-opens after 30 s |
| nginx cannot resolve `govoice` on a host | `nginx.conf` uses Docker's resolver and service names | change the resolver and upstreams (section 9) |
| `smoke-cloudrun.sh` reports the server does not see https, or `client_ip` is a private address | the proxy chain is trusted wrongly | correct `tls.trusted_proxies` |
| xAI TTS fails behind a proxy that blocks WebSockets | streaming TTS cannot connect | set `providers.xai_tts.disable_streaming: true` |

<!-- src: server/management.go:36-47,84-91; server/config_validate.go:100-125; deploy/smoke-cloudrun.sh:33-39; docs/operations.md:25,39-40,58-61,72-77,122; deploy/nginx.conf:50-52 -->

Pre-deploy checklist:

- [ ] Image built with `xaispeech` and scanned (`make release-security`).
- [ ] Config edited (section 4) and secrets set with modes checked (section 6).
- [ ] Database backed up; grace periods at least 60 s; probes wired; alerts loaded.
- [ ] Smoke test passed; `GET /api/v1/server-config` checked after the deploy.

## 18. Not provided by this repository

So that nothing here is assumed: the repository has no published container images or registry, no multi-architecture publish pipeline, no Helm or Kubernetes manifests (section 12 is a sketch), no Terraform, no supported host install on Ubuntu or RHEL (the unit is a reference), no backup automation or backup scripts, no Cloud Run rollback script, no non-demo Cloud Run configuration, no Grafana dashboards or Alertmanager routing, no published 9091 admin vhost in Compose, no multi-upstream nginx configuration, and no self-hosted NVIDIA endpoints ([providers.md](providers.md)).

<!-- src: git ls-files (no helm, terraform, k8s, dashboards); deploy/docker-compose.yaml:82-84; docs/providers.md:26 -->
