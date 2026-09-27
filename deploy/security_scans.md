# Production Deployment, Security Scans, and SBOM

Voice Framework V1 — September 2026

## 1. Target Linux Server Specification

Per Specification §39 and §42, the production target environment is modern 64-bit Linux server hardware:
- **Operating Systems Supported:**
  - Ubuntu 22.04 LTS (Jammy) or 24.04 LTS (Noble)
  - Debian 12 (Bookworm)
  - Red Hat Enterprise Linux (RHEL) / Rocky Linux / AlmaLinux 9
- **CPU Architectures:**
  - `x86_64` (AMD64)
  - `aarch64` (ARM64)
- **C Library (libc):**
  - GNU C Library (glibc) >= 2.34
- **Required Linux Capabilities:**
  - `CAP_NET_BIND_SERVICE` (if binding directly to ports < 1024 without reverse proxy)
  - All other Linux capabilities dropped (`cap_drop: ALL`).
- **File Descriptors and Process Limits:**
  - Recommended `LimitNOFILE`: 65,536 (supports up to 10,000 concurrent sockets/pipes).
  - Recommended `LimitNPROC`: 4,096 (supports VAD worker process supervision and Go runtime threads).

## 2. Immutable Release Image Digest

The release container image is published with an immutable SHA-256 digest:
```
govoice/server@sha256:d8c55866166162629b3f3b1464aa69a23992b8d4e7d44cf6b6a2243d4c680695
```

## 3. Dependency Vulnerability Severity Gate

Production releases enforce a strict automated vulnerability scanning gate:
- **Critical Severity:** 0 allowed.
- **High Severity:** 0 allowed.
- **Medium / Low Severity:** Evaluated and triaged per release cycle.
- **Automated Scanning Tools:**
  - Trivy container scan: `trivy image --exit-code 1 --severity CRITICAL,HIGH govoice/server:v1.0.0`
  - Govulncheck Go module scan: `govulncheck ./...`

## 4. Child Process Supervision & Reaping

In containerized deployments, `tini` runs as PID 1 to ensure that:
1. Zombie child processes spawned by the VAD worker process executor (`vadworker`) are automatically reaped.
2. `SIGTERM` signals from container orchestrators (Kubernetes, Docker) are correctly propagated to the Go server for drain-then-cancel graceful shutdown.
