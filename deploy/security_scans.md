# Production Deployment, Security Scans, and SBOM

## 1. Target Linux Server Specification

The production target environment is modern 64-bit Linux server hardware:
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

## 2. Dependency Vulnerability Severity Gate

Production releases enforce a strict automated vulnerability scanning gate:
- **Critical Severity:** 0 allowed.
- **High Severity:** 0 allowed.
- **Medium / Low Severity:** Evaluated and triaged per release cycle.
- **Automated Scanning Tools:**
  - Trivy container scan: `trivy image --exit-code 1 --ignore-unfixed --severity HIGH,CRITICAL govoice:release-security` (when `trivy` is not installed the Makefile falls back to `grype --only-fixed --fail-on high`, then to the dockerized `aquasec/trivy` with the same flags)
  - Govulncheck Go module scan: `govulncheck ./...`

`make release-security` runs these scans, builds the image as `govoice:release-security`, and writes the module SBOM to `deploy/sbom.json` (git-ignored).

## 3. Child Process Supervision & Reaping

In containerized deployments, `tini` runs as PID 1 to ensure that:
1. Zombie child processes spawned by the VAD worker process executor (`vadworker`) are automatically reaped.
2. `SIGTERM` signals from container orchestrators (Kubernetes, Docker) are correctly propagated to the Go server for drain-then-cancel graceful shutdown.
