# Production Hardening and Security Operations Guide

This document describes the production security controls, runtime operational contracts, backup/restore procedures, and packaging standards for Switchboard per specifications §12 and §39.

---

## 1. Security Architecture & Boundary Controls

### 1.1 Management API Authentication
- **Minimum Secret Length**: The management API secret configured via `GOVOICE_MANAGEMENT_API_KEY` (or custom configured env) must be at least **32 bytes** (256 bits). Initializing `auth.NewManagement` with any secret under 32 bytes fails immediately at startup.
- **Constant-Time Comparison**: The API secret is stored internally as a SHA-256 digest (`[32]byte`) and verified using `crypto/subtle.ConstantTimeCompare`, neutralizing timing side-channel attacks.
- **Strict Format Enforcement**: All management endpoints require an `Authorization: Bearer <token>` header. Requests with missing headers, malformed schemes, internal whitespace/tabs, or empty tokens are rejected with `401 Unauthorized`.
- **Key Separation**: Per contract §12, the management API secret must be strictly distinct from the voice WebSocket token signing key (`TOKEN_SECRET`). Reusing the same key for both boundaries is rejected at server startup.

### 1.2 Secret & Credential Redaction
- **Environment Only**: Provider keys (API keys, the service-account file path) and MCP bearer tokens live in the server's environment and are never written to the database. See [docs/security/credentials.md](../security/credentials.md).
- **Response Redaction**: No management API accepts or returns a secret value. `GET /api/v1/providers`, agent readiness (`missing_env`) and MCP server views (`auth_env`, `auth_env_set`) name variables, never values.
- **Zero Raw Audio Persistence**: No database table contains raw audio columns or audio payloads. Traces only retain structural latency, spans, events, and turn metadata.

### 1.3 Destination Controls for MCP Servers
- **Endpoint Gating**: Remote MCP servers (`sse` transport) must point to explicitly allowed network endpoints. Arbitrary local loopback manipulation is restricted to admin control.
- **Schema Immutability**: Discovered MCP tool definitions are sha256-hashed and frozen at the time of agent version publication.

---

## 2. Persistence Reliability & Backup/Restore

### 2.1 SQLite Production Operations
- **Single-Node WAL Mode**: SQLite runs in WAL journal mode with `_txlock=immediate`, `_busy_timeout=5000ms`, and mandatory foreign key checks enabled on all connections.
- **Online Backup (`VACUUM INTO`)**:
  - Live database backups must be performed using `VACUUM INTO '<destination_path>'` while the server is running.
  - Never copy only the `.db` file directly while WAL files (`-wal`, `-shm`) are active.
  - Backups can be verified with `PRAGMA quick_check` and `PRAGMA foreign_key_check`.
- **Migration Lock Recovery**:
  - Concurrent and consecutive startups synchronize via a `<database>.migration.lock` lockfile using OS-level `flock`.
  - The lock file remains on disk after clean server shutdown and is safely reacquired on subsequent boots. If an unexpected process termination occurs, the OS automatically releases the flock, allowing clean reboot without manual cleanup.

### 2.2 PostgreSQL High Availability
- **Advisory Session Lock**: Migrations use Goose's PostgreSQL advisory session lock (`lock.NewPostgresSessionLocker`) ensuring only one node executes DDL during rolling deployments.
- **Point-in-Time Recovery**: PostgreSQL backups should leverage continuous WAL archiving and `pg_dump` with transaction consistency.

---

## 3. Runtime Lifecycle & Graceful Drain

### 3.1 Graceful Shutdown Protocol
1. **Unready Transition**: On receiving `SIGTERM` or `SIGINT`, the server immediately sets its internal readiness state to false (`ready.Store(false)`). The `/ready` endpoint returns `503 Service Unavailable` so load balancers stop routing new traffic.
2. **Listener Shutdown**: The public HTTP listener (`/ws`) closes immediately to reject new incoming connections.
3. **Active Session Drain**:
   - The server enters a drain loop waiting up to `DrainTimeout` (default 30–45s) for existing voice sessions to conclude naturally.
   - If `ImmediateCancel: true` is configured, sessions are aborted immediately without a drain wait.
4. **Deadline Expiration**: If active calls remain after `DrainTimeout`, the server forces cancellation on remaining connections and returns an explicit drain timeout error.
5. **Resource Teardown**: VAD executors, background retention goroutines, OpenTelemetry exporters, and database connection pools are closed orderly.

---

## 4. Container Packaging & Sandbox Standards

### 4.1 Container Invariants (`deploy/Dockerfile`, `deploy/docker-compose.yaml`)
- **Non-Root Execution**: Runs under unprivileged user and group `govoice:govoice` (`uid=10001, gid=10001`).
- **Read-Only Root Filesystem**: Root filesystem is mounted `read_only: true`.
- **Capability Drops**: All Linux capabilities are dropped (`cap_drop: - ALL`).
- **Privilege Escalation Prevention**: `security_opt: - no-new-privileges:true` is enforced.
- **Bounded Writables**: Only `/tmp` (as memory tmpfs) and `/var/log/govoice` (as named volume) are writable.
- **Init Process**: Uses `/usr/bin/tini` as `ENTRYPOINT` for proper zombie reaping and signal forwarding.
