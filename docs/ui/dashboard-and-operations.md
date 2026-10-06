# Dashboard and Read-Only Server Operations View (SV-D / SV-E)

This document describes the design, API contracts, telemetry bounds, and user interface for the operational dashboard and read-only server operations view.

## 1. Overview & Architecture

The operations view and dashboard provide operators with real-time insight into:
- System operational health (`healthy`, `degraded`, or `unhealthy`).
- Active session count and capacity utilization vs configured limits.
- Total lifetime sessions, turns processed, error counts, and latency percentiles (average and p95).
- Agent & provider readiness summary.
- Recent session activity feed with direct deep-links to session details and traces (`/sessions/:id`).
- Complete read-only server runtime configuration and diagnostics:
  - System profile, uptime, start timestamp.
  - Operational limits: max sessions, call duration cap, voice token TTL.
  - Database engine: dialect, migration version, connection pool stats.
  - VAD diagnostics & worker readiness.

All operational endpoints adhere to strict security constraints:
- Protected behind management authentication (`Bearer <GOVOICE_MANAGEMENT_API_KEY>`).
- Public listener never exposes operational routes (returns 404).
- Voice connect tokens cannot authenticate management routes (returns 401).
- Read-only protection: mutating HTTP methods (POST, PUT, DELETE, PATCH) return 405 Method Not Allowed.
- Strict secret sanitization: connection strings, passwords, API keys, DSNs, and credentials are never included in responses.
- Bounded-cardinality values: status codes and labels use fixed enums without high-cardinality session IDs or arbitrary user strings.

---

## 2. API Contracts

### `GET /api/v1/operations/status`

**Headers:**
- `Authorization: Bearer <management_key>`

**Response: `200 OK`**
```json
{
  "data": {
    "uptime_seconds": 3600.5,
    "started_at": "2026-10-05T12:00:00Z",
    "profile": "production",
    "active_sessions": 4,
    "max_sessions": 50,
    "capacity_utilization": 0.08,
    "sessions_total": 120,
    "turns_total": 480,
    "errors_total": 1,
    "p95_latency_ms": 195.4,
    "average_latency_ms": 130.2,
    "session_rate": 0.033,
    "max_call_duration_seconds": 300,
    "voice_token_max_lifetime_seconds": 60,
    "database": {
      "dialect": "sqlite",
      "status": "connected",
      "open_connections": 1,
      "in_use": 0,
      "migration_version": 5
    },
    "vad": {
      "worker_status": "ready",
      "sample_rate": 16000,
      "frame_size": 512
    },
    "health": "healthy"
  }
}
```

### Health Determination Rules

The overall `health` status is evaluated deterministically:
1. `unhealthy`:
   - Database status is `"error"` (ping failed or connection severed), OR
   - VAD worker status is `"unavailable"`.
2. `degraded`:
   - Capacity utilization is >= 100% (`active_sessions >= max_sessions`), OR
   - VAD worker status is `"degraded"`, OR
   - Severe error rate (`errors_total > 10` and `errors_total / sessions_total > 0.5`).
3. `healthy`:
   - All components operating normally within limits.

---

## 3. UI Views

### Dashboard View (`/dashboard` — `DashboardView.tsx`)

1. **System Health Banner**:
   - Prominently displays `System Healthy`, `System Degraded`, or `System Unhealthy`.
   - Visual tone indicator (green good / red bad) with elapsed system uptime.
   - Informative operational summary description.
2. **Capacity Warning Alert**:
   - Triggers when session capacity reaches or exceeds 90% (`active_sessions / max_sessions >= 0.9`).
3. **Metrics Grid**:
   - **Active Sessions**: current concurrent calls vs max limit and percentage utilization.
   - **Total Sessions**: cumulative session count and session rate per second.
   - **Error Count**: aggregate error count with total turns processed.
   - **p95 Latency**: 95th percentile turn latency (with average latency subtext).
4. **Readiness Summary Cards**:
   - **Agent Readiness**: count of total configured vs ready and configured agents with deep-link to `/agents`.
   - **Provider Readiness**: count of detected providers and validated API credentials with deep-link to `/providers`.
5. **Recent Session Activity Feed**:
   - Shows latest 5 sessions with masked ID, status badge, turn count, relative start time, and direct deep-link button (`/sessions/:id`).
   - If empty, displays descriptive empty state prompting user to connect a voice client.
   - Header includes link to `/sessions` for full historical table.

### Server Operations View (`/server` — `ServerView.tsx`)

A strictly read-only diagnostics console containing 4 structured sections:
1. **System Overview**:
   - Server profile (`development` / `production`).
   - System health badge.
   - Human-formatted uptime.
   - Server started timestamp (UTC).
2. **Operational Limits**:
   - Maximum concurrent sessions limit.
   - Capacity utilization and active session breakdown.
   - Maximum call duration cap (seconds).
   - Voice connect token maximum lifetime (seconds).
3. **Storage Diagnostics**:
   - Database dialect (`sqlite` / `postgres` / `none`).
   - Connection status badge (`connected`, `unconfigured`, `error`).
   - Database migration version (`Version 5`, `Current`).
   - Open pool connection count and active in-use connection count.
4. **VAD Diagnostics**:
   - Silero VAD worker status badge (`ready`, `active`, `degraded`, `unavailable`).
   - Audio sample rate (16000 Hz).
   - Frame size (512 samples).
   - Engine readiness indicator.

---

## 4. Verification & Testing

Both server backend and UI suites verify the implementation:

### Server Tests
- `server/api_operations_boundary_test.go`:
  - Integrated HTTP boundary tests for `/api/v1/operations/status`.
  - Auth separation: missing key (401), invalid key (401), voice secret rejection (401).
  - Public listener route isolation (404).
  - Method not allowed (405).
  - Sanitization verification and header checks (`Cache-Control: no-store`, `X-Request-ID`).
  - Unconfigured database reporting.

Run backend tests:
```bash
cd server && SDKROOT=/Applications/Xcode.app/Contents/Developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX26.sdk CGO_LDFLAGS=-Wl,-w go test -race -count=1 ./...
```

### UI Tests
- `ui/src/api/client.test.ts`: typed API client method `getOperationsStatus()` testing.
- `ui/src/routes/dashboard/DashboardView.test.tsx`:
  - Unconnected empty state.
  - Loading state.
  - Error state with retry.
  - Healthy dashboard layout with metrics grid, readiness cards, and session links.
  - Capacity warning alert on high utilization.
  - Empty sessions feed state.
- `ui/src/routes/server/ServerView.test.tsx`:
  - Unconnected empty state.
  - Loading state.
  - Error state with retry.
  - Read-only diagnostics across all four tables.
  - Degraded and unhealthy state badge rendering.
  - Read-only protection verification (no forms or mutable buttons).

Run UI tests and production build:
```bash
cd ui && npm test -- --run && npm run typecheck && npm run build
```
