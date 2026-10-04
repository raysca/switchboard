# Container Voice Package Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the container the V1 distribution and prove one proxied call plus one 30-minute Google call through that image.

**Architecture:** The server process already budgets `drain_timeout + 5s` after SIGTERM. Compose must wait longer than that budget. Nginx is the public TLS edge. Live proof reuses the existing Google call harness against the image, then holds a second call across more than one STT rotation.

**Tech Stack:** Go 1.27.1, Docker Compose, Nginx, `tini`, production YAML, Google STT/LLM/TTS, existing `//go:build live` harness.

**Spec:** `docs/superpowers/specs/2026-10-04-container-voice-package-design.md`

## Global Constraints

- WebSocket is the only client transport. Do not add WebRTC, SIP, or telephony.
- The container is the release distribution. Do not block this plan on systemd.
- Playback clear stays the recorded waiver in `docs/compatibility/playback-clear-decision.json`. Do not claim the pinned client cleared its buffer.
- Do not invent concurrent-session counts or p95/p99 latencies. Those are a later plan.
- Production drain stays 45s. The shutdown budget is that drain plus 5s. Compose `stop_grace_period` is 60s.
- Do not print `TOKEN_SECRET`, JWTs, or Google credential contents.
- macOS cgo builds need `SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk` and `CGO_LDFLAGS=-Wl,-w`.

---

### Task 1: Shutdown budget and Compose grace

**Files:**
- Modify: `server/lifecycle.go` (create this file)
- Modify: `cmd/server/main.go` (the `shutdownTimeout` line)
- Modify: `deploy/docker-compose.yaml` (`stop_grace_period`)
- Modify: `deploy/govoice.service` (`TimeoutStopSec` only)
- Test: `server/lifecycle_budget_test.go`

**Interfaces:**
- Consumes: `cfg.Limits.DrainTimeout` from `server.Config`
- Produces: `func ShutdownBudget(drain time.Duration) time.Duration`

- [ ] **Step 1: Write the failing test**

```go
package server

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestShutdownBudgetAddsFiveSeconds(t *testing.T) {
	if got := ShutdownBudget(45 * time.Second); got != 50*time.Second {
		t.Fatalf("budget = %s, want 50s", got)
	}
}

func TestComposeGraceExceedsShutdownBudget(t *testing.T) {
	t.Setenv("TOKEN_SECRET", "contract-test-secret-not-a-real-token-value")
	cfg, err := LoadConfig("../config/production.yaml")
	if err != nil {
		t.Fatal(err)
	}
	budget := ShutdownBudget(cfg.Limits.DrainTimeout)
	compose, err := os.ReadFile("../deploy/docker-compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "stop_grace_period: 60s") {
		t.Fatalf("compose grace must be 60s, which is longer than budget %s", budget)
	}
	unit, err := os.ReadFile("../deploy/govoice.service")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), "TimeoutStopSec=60s") {
		t.Fatal("reference unit TimeoutStopSec must be 60s")
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run from the repo root:

```bash
export SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk
export CGO_LDFLAGS='-Wl,-w'
cd server && go test -count=1 -run 'TestShutdownBudgetAddsFiveSeconds|TestComposeGraceExceedsShutdownBudget' .
```

Expected: FAIL because `ShutdownBudget` is undefined and Compose still says `stop_grace_period: 45s`.

- [ ] **Step 3: Implement the budget and raise both supervisor timeouts**

Create `server/lifecycle.go`:

```go
package server

import "time"

// ShutdownBudget is how long the process may spend in Server.Shutdown after SIGTERM.
// Docker and the reference unit must wait longer than this or they kill the process mid-drain.
func ShutdownBudget(drain time.Duration) time.Duration {
	if drain < 0 {
		drain = 0
	}
	return drain + 5*time.Second
}
```

In `cmd/server/main.go`, replace `cfg.Limits.DrainTimeout + 5*time.Second` with `server.ShutdownBudget(cfg.Limits.DrainTimeout)`.

In `deploy/docker-compose.yaml`, set `stop_grace_period: 60s`.

In `deploy/govoice.service`, set `TimeoutStopSec=60s` and replace the comment that says the drain is 30s with: `SIGTERM drains for drain_timeout. TimeoutStopSec is longer than drain_timeout plus the 5s process margin.`

- [ ] **Step 4: Run the test and confirm it passes**

```bash
cd server && go test -count=1 -run 'TestShutdownBudgetAddsFiveSeconds|TestComposeGraceExceedsShutdownBudget' .
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/lifecycle.go server/lifecycle_budget_test.go cmd/server/main.go deploy/docker-compose.yaml deploy/govoice.service
git commit -m "fix: give the container longer than the shutdown budget to drain"
```

### Task 2: Nginx certificate paths match Compose mounts

**Files:**
- Modify: `deploy/nginx.conf` (certificate directives)
- Modify: `deploy/docker-compose.yaml` (proxy volumes)
- Test: `server/deploy_contract_test.go`

**Interfaces:**
- Consumes: Compose service `proxy` volumes and `deploy/nginx.conf` `ssl_certificate` lines
- Produces: one read-only mount `./certs` onto `/etc/ssl/certs` covering every certificate and key Nginx opens

- [ ] **Step 1: Write the failing test**

```go
package server

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestNginxCertificatesAreMounted(t *testing.T) {
	nginx, err := os.ReadFile("../deploy/nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile("../deploy/docker-compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "./certs:/etc/ssl/certs:ro") {
		t.Fatal("compose must mount ./certs onto /etc/ssl/certs read-only")
	}
	re := regexp.MustCompile(`ssl_certificate(?:_key)?\s+(\S+);`)
	for _, match := range re.FindAllStringSubmatch(string(nginx), -1) {
		path := match[1]
		if !strings.HasPrefix(path, "/etc/ssl/certs/") {
			t.Fatalf("certificate path %s is outside the Compose mount /etc/ssl/certs", path)
		}
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

```bash
cd server && go test -count=1 -run TestNginxCertificatesAreMounted .
```

Expected: FAIL because `ssl_certificate_key` points at `/etc/ssl/private/...`.

- [ ] **Step 3: Point every Nginx certificate at the mounted directory**

In `deploy/nginx.conf`, set:

```nginx
ssl_certificate /etc/ssl/certs/voice.example.com.crt;
ssl_certificate_key /etc/ssl/certs/voice.example.com.key;
```

and:

```nginx
ssl_certificate /etc/ssl/certs/admin.crt;
ssl_certificate_key /etc/ssl/certs/admin.key;
```

Leave the Compose mount as `./certs:/etc/ssl/certs:ro`. Do not add a second mount.

- [ ] **Step 4: Run the test and confirm it passes**

```bash
cd server && go test -count=1 -run 'TestNginxCertificatesAreMounted|TestComposeGraceExceedsShutdownBudget' .
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add deploy/nginx.conf server/deploy_contract_test.go
git commit -m "fix: serve proxy certificates from the path Compose mounts"
```

### Task 3: Say the container is the distribution

**Files:**
- Modify: `docs/operations_runbook.md` (section 1 and the systemd section)
- Modify: `docs/release_evidence.md` (rows 16 and 22)
- Test: `server/deploy_contract_test.go` (append)

**Interfaces:**
- Consumes: the package decision in the spec
- Produces: runbook language a later operator can follow without installing systemd

- [ ] **Step 1: Write the failing test**

Append to `server/deploy_contract_test.go`:

```go
func TestRunbookNamesContainerAsDistribution(t *testing.T) {
	body, err := os.ReadFile("../docs/operations_runbook.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "The container image is the V1 distribution.") {
		t.Fatal("runbook must name the container as the V1 distribution")
	}
	if !strings.Contains(text, "The systemd unit is a reference file and is not required to run V1.") {
		t.Fatal("runbook must say the systemd unit is not required")
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

```bash
cd server && go test -count=1 -run TestRunbookNamesContainerAsDistribution .
```

Expected: FAIL because those sentences are absent.

- [ ] **Step 3: Update the runbook and the evidence table**

In `docs/operations_runbook.md`, under supported environments, add:

```markdown
The container image is the V1 distribution. Host resource numbers below are planning guidance, not a measured capacity result.
```

Above the systemd section, add:

```markdown
The systemd unit is a reference file and is not required to run V1.
```

Remove the sentence that says the framework is certified on Ubuntu, Debian, and RHEL. Replace it with: `The production image is Debian bookworm. A host install on Ubuntu, Debian, or RHEL is not part of this release.`

In `docs/release_evidence.md` rows 16 and 22, state that systemd is not a release gate and that container SIGTERM remains the shutdown proof until Task 5 replaces that sentence with the new run.

- [ ] **Step 4: Run the test and confirm it passes**

```bash
cd server && go test -count=1 -run 'TestRunbookNamesContainerAsDistribution|TestNginxCertificatesAreMounted|TestComposeGraceExceedsShutdownBudget' .
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add docs/operations_runbook.md docs/release_evidence.md server/deploy_contract_test.go
git commit -m "docs: make the container the V1 distribution"
```

### Task 4: Image answers health behind the proxy

**Files:**
- Create: `deploy/smoke.sh`
- Test: `server/deploy_contract_test.go` (script existence and flags)

**Interfaces:**
- Consumes: image `govoice:release-security`, Compose file from Tasks 1 and 2
- Produces: `bash deploy/smoke.sh`, which exits 0 only after proxy `/health` returns and a no-call SIGTERM exits 0

- [ ] **Step 1: Write the failing test**

Append:

```go
func TestSmokeScriptChecksProxyHealth(t *testing.T) {
	body, err := os.ReadFile("../deploy/smoke.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, needle := range []string{
		"docker compose -f deploy/docker-compose.yaml",
		"https://127.0.0.1/health",
		"docker kill -s TERM",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("smoke script missing %q", needle)
		}
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

```bash
cd server && go test -count=1 -run TestSmokeScriptChecksProxyHealth .
```

Expected: FAIL because `deploy/smoke.sh` does not exist.

- [ ] **Step 3: Write the smoke script**

`deploy/smoke.sh`:

```bash
#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
test -n "${TOKEN_SECRET:-}"
test -n "${GOOGLE_APPLICATION_CREDENTIALS_FILE:-}"
mkdir -p deploy/certs
if [[ ! -f deploy/certs/voice.example.com.crt ]]; then
  openssl req -x509 -newkey rsa:2048 -nodes -keyout deploy/certs/voice.example.com.key \
    -out deploy/certs/voice.example.com.crt -days 2 -subj '/CN=localhost'
  cp deploy/certs/voice.example.com.crt deploy/certs/admin.crt
  cp deploy/certs/voice.example.com.key deploy/certs/admin.key
fi
docker build -f deploy/Dockerfile -t govoice:release-security .
docker compose -f deploy/docker-compose.yaml up -d
trap 'docker compose -f deploy/docker-compose.yaml down' EXIT
for _ in $(seq 1 30); do
  if curl -fsS --insecure https://127.0.0.1/health; then
    break
  fi
  sleep 1
done
curl -fsS --insecure https://127.0.0.1/health
docker kill -s TERM govoice-server
docker wait govoice-server
test "$(docker inspect -f '{{.State.ExitCode}}' govoice-server)" = "0"
```

`chmod +x deploy/smoke.sh`. The script refuses to start without `TOKEN_SECRET` and `GOOGLE_APPLICATION_CREDENTIALS_FILE`. It does not echo either value.

- [ ] **Step 4: Run the contract test, then the smoke script**

```bash
cd server && go test -count=1 -run TestSmokeScriptChecksProxyHealth .
bash deploy/smoke.sh
```

Expected: the Go test passes. The script prints the health JSON and exits 0. Without Google credentials the server process exits 1 and the script fails; that is the correct fail-closed result, and the script must not be recorded as a pass in that case.

- [ ] **Step 5: Commit**

```bash
git add deploy/smoke.sh server/deploy_contract_test.go
git commit -m "test: smoke the proxied container health and SIGTERM path"
```

### Task 5: One live call through the image

**Files:**
- Create: `server/e2e_image_test.go`
- Modify: `docs/release_evidence.md` (row for the image call)
- Modify: `docs/benchmarks/image_call_result.json` (written by the test)

**Interfaces:**
- Consumes: a running `govoice:release-security` behind Nginx from `deploy/smoke.sh`, plus `mintImageToken` (issuer `govoice-v1`, audience `govoice-v1-client`, `kid` `v1`)
- Produces: `TestLiveImageCall` (`//go:build live`) and `docs/benchmarks/image_call_result.json`

- [ ] **Step 1: Write the failing test**

Create `server/e2e_image_test.go` with `//go:build live`. The container is a different process, so mint the JWT in the test. Do not call `createScenarioToken`.

```go
func mintImageToken(t *testing.T, jti string) string {
	t.Helper()
	secret := os.Getenv("TOKEN_SECRET")
	if secret == "" {
		t.Fatal("TOKEN_SECRET is required and must match the container")
	}
	now := time.Now()
	claims := token.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Issuer:    "govoice-v1",
			Audience:  jwt.ClaimStrings{"govoice-v1-client"},
			Subject:   "image-call-user",
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(5 * time.Minute)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "v1"
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
```

Call `mintImageToken(t, "jti-image-call")`. Dial `wss://127.0.0.1/ws?token=` plus that token, with `InsecureSkipVerify: true`. Task 6 calls `mintImageToken(t, "jti-image-long")`. Reuse `synthesizeUtterance`, `writePCM`, and `waitForText` from `e2e_live_test.go`. Send "What is two plus two?" and then "What did I just ask you?". Fail if no assistant message contains `two plus two` or `2 + 2`. Write `docs/benchmarks/image_call_result.json` with `transport`, `transcripts`, `assistant_texts`, and `ran_at` only.

- [ ] **Step 2: Run the test and confirm it fails**

```bash
export SDKROOT=/Library/Developer/CommandLineTools/SDKs/MacOSX26.sdk
export CGO_LDFLAGS='-Wl,-w'
export GOVOICE_ROOT="$PWD"
go test -tags live -count=1 -timeout 180s -run TestLiveImageCall -v ./server/
```

Expected: FAIL until `bash deploy/smoke.sh` is up and the dial succeeds.

- [ ] **Step 3: Point the test at the running proxy and match production trusted proxies**

The container's production config trusts `172.16.0.0/12`. Confirm the Compose network gateway is inside that range before treating `X-Forwarded-For` as the client. The test client connects to Nginx, not to port 8080.

- [ ] **Step 4: Run the test and confirm it passes**

Expected: PASS, with two transcripts and a follow-up answer. Update `docs/release_evidence.md` with the command, the exit status, and the artifact path. Quote only fields present in `image_call_result.json`.

- [ ] **Step 5: Commit**

```bash
git add server/e2e_image_test.go docs/benchmarks/image_call_result.json docs/release_evidence.md
git commit -m "test: prove one Google call through the container proxy"
```

### Task 6: Thirty-minute call through the image

**Files:**
- Create: `server/e2e_image_long_test.go`
- Modify: `docs/benchmarks/image_long_call_result.json` (written by the test)
- Modify: `docs/release_evidence.md`
- Modify: `docs/benchmarks/capacity_slo_report.md` (the 30-minute bullet only)

**Interfaces:**
- Consumes: the same running image and `mintImageToken` from Task 5. Use a new `jti` of `jti-image-long`.
- Produces: `TestLiveImageThirtyMinutes` (`//go:build live`)

- [ ] **Step 1: Write the failing test**

`//go:build live`. Hold silence for 31 minutes so the 240-second rotation threshold is crossed more than once. Then send one utterance, "What is three plus three?". Fail unless `voice_stt_rotations_total` scraped from `http://127.0.0.1:9090/metrics` is at least 2, and the transcript contains `3 + 3` or `three plus three`. Write `docs/benchmarks/image_long_call_result.json` with `hold_seconds`, `rotation_threshold_seconds`, `rotations`, `final_transcript`, and `ran_at`.

- [ ] **Step 2: Run the test and confirm it fails before the hold**

```bash
go test -tags live -count=1 -timeout 40m -run TestLiveImageThirtyMinutes -v ./server/
```

Expected: FAIL fast if the image is down or metrics are unreachable. Do not treat a missing binary as a long-call failure.

- [ ] **Step 3: Run the hold against the image started in Task 4**

Leave the smoke stack up. Do not start a second server on the same ports.

- [ ] **Step 4: Record only the observed result**

On PASS, set the 30-minute bullet in `docs/benchmarks/capacity_slo_report.md` to the artifact path and the rotation count from the JSON. Leave the concurrent-capacity bullet as not measured.

- [ ] **Step 5: Commit**

```bash
git add server/e2e_image_long_test.go docs/benchmarks/image_long_call_result.json docs/benchmarks/capacity_slo_report.md docs/release_evidence.md
git commit -m "test: record a 30-minute Google call through the container"
```

## Out of scope

- A 250-call run or any latency percentile.
- Changing `@pipecat-ai/client-js@1.13.1` or the playback-clear waiver.
- Building xAI STT or TTS into the default image.
- Installing or starting systemd on a host.

## Self-review

- Spec shutdown budget, proxy paths, image health, one call, and the 30-minute call each have a task.
- Capacity and playback stay out of scope in the spec and in the plan.
- `ShutdownBudget` is defined in Task 1 and used by `main.go` there. Later tasks do not rename it.
- Task 5 and Task 6 both dial the proxy from Task 4. They do not start a second server.
