# Voice Framework V1 — Security & Admission Architecture

This document describes the security model, admission token lifecycle, query redaction discipline, network isolation, and anti-abuse safeguards in Voice Framework V1.

---

## 1. Admission Token Security Model

Public clients cannot establish a voice session without a cryptographically signed, short-lived, single-use JWT admission token.

### Token Claims
```json
{
  "sub": "user-principal-id",
  "aud": "govoice",
  "iss": "govoice-auth",
  "exp": 1774704300,
  "iat": 1774704000
}
```

### Validation Rules
1. **Signature**: Validated using HMAC-SHA256 with a configured shared secret. In production profiles, secret keys must be at least 32 characters long, and file permissions for secret files must be strictly `0600` or `0400`.
2. **Single-Use Replay Prevention**:
   - The token's signature or unique hash is recorded in an in-memory cache upon consumption.
   - Any second attempt to connect using the same token is atomically rejected before WebSocket upgrade, preventing replay races.
3. **Clock Skew**: Tolerates a maximum clock skew of `+-30 seconds`.
4. **Post-Restart `iat` Boundary**:
   - When the server starts up, it records `StartedAt = time.Now()`.
   - Any token issued prior to the server's restart timestamp (`iat < StartedAt - 30s`) is rejected, preventing pre-restart token replays across service restarts.

---

## 2. Sensitive Query Redaction

Clients pass the admission token as a query parameter during the initial HTTP upgrade:
`GET /ws?token=eyJhbGciOi...`

To prevent credential leakage into monitoring systems and logs:
1. **Application-Level Redaction**:
   - All internal logs, error messages, and timelines redact the token parameter using a regex sanitizer: `(?i)(token=)[^&]+ -> token=[REDACTED]`.
2. **Reverse Proxy Access Log Redaction**:
   - The production Nginx reverse proxy configuration (`deploy/nginx.conf`) defines a mapping rule that strips the token parameter before writing to `access.log`:
     ```nginx
     map $args $clean_args {
         default $args;
         "~(.*)(^|&)token=[^&]+(.*)" $1$2token=[REDACTED]$3;
     }
     ```

---

## 3. Admission Limits & Anti-Abuse Guards

The admission controller (`server/admission.go`) enforces strict limits before allocating session memory, VAD workers, or provider streams:

| Guard | Production Default | Behavior on Breach |
|---|---|---|
| **Pending Handshakes** | 64 | Returns HTTP 503 (`ErrPendingHandshakeLimit`) |
| **Max Concurrent Sessions** | 250 | Returns HTTP 503 (`ErrMaxSessionsReached`) |
| **Per-Principal Concurrency** | 2 | Returns HTTP 429 (`ErrPrincipalLimitReached`) |
| **STT Rotation Headroom** | 2.0x required | Returns HTTP 503 if remaining provider quota cannot guarantee overlap |
| **Max Call Duration** | 3600 seconds (1 hr) | Session closes cleanly upon reaching duration |
| **VAD Worker Capacity** | Checked at handshake | Returns HTTP 503 if all native VAD workers are offline |

---

## 4. Administrative Surface Isolation

The voice server strictly separates client-facing traffic from operator and administrative endpoints:

```
+-------------------------------------------------------------------------+
|                               HOST                                      |
|                                                                         |
|  [Public Network] ----> Port 8080 (Public Listener)                     |
|                           - GET /ws (Authenticated WebSocket only)      |
|                           - No metrics, health, or debug endpoints      |
|                                                                         |
|  [Internal Network] --> Port 9090 (Admin Listener)                      |
|                           - GET /health                                 |
|                           - GET /ready                                  |
|                           - GET /metrics                                |
|                           - GET /timeline                               |
+-------------------------------------------------------------------------+
```

Administrative endpoints cannot be reached from port 8080 under any circumstances. In production deployments, port 9090 is restricted to localhost or private VPC networks.

---

## 5. Reconnection Discipline

Voice Framework V1 enforces a **clean-session policy**:
- **No Session Resumption**: The runtime does not support re-attaching or resuming dropped WebSocket connections.
- **Fresh Call Policy**: When a client loses connectivity, it must mint a fresh admission token and connect to establish a new session.
- **Resource Reclaim**: Dropped connections release all allocated provider streams, VAD resources, and goroutines within `2000ms`.
