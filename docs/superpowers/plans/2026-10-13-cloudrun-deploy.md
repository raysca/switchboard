# Cloud Run Deployment and Demo Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A merge to `main` builds, deploys and smoke-tests two Cloud Run services (the voice server on one port with the admin UI, and the dentistry MCP server), with Google, xAI and ElevenLabs keys from Secret Manager, no long-lived GCP credentials in GitHub, and guardrails against runaway cost.

**Architecture:** Declarative Cloud Run manifests (`deploy/gcp/*.service.yaml`, contract-tested) are rendered and applied by a GitHub Actions workflow authenticated through Workload Identity Federation. A one-time, dry-runnable bootstrap script creates the project plumbing and the secrets. A smoke script proves the deployed system without spending provider credit. A local rehearsal on the production image comes first; the first Cloud Run deploy is a measured spike.

**Tech Stack:** GitHub Actions, `gcloud`, Cloud Run (gen2), Artifact Registry, Secret Manager, Workload Identity Federation, Docker, bash, `gh`.

**Spec:** `docs/superpowers/specs/2026-10-13-cloudrun-dentistry-demo-design.md` (items 12-20). Needs plan 1 (demo features) and plan 2 (one-port server) merged first.

## Global Constraints

- No long-lived GCP credential anywhere: GitHub authenticates with Workload Identity Federation restricted to this repository and the `main` ref. GitHub stores variables only (project, region, provider, service account), never a provider key.
- Provider keys, the token secret, the management key and the MCP token live only in Secret Manager and reach the service as environment variables or a mounted file. They are never printed (scripts, logs, CI output), never in URLs, never in image layers.
- The voice service runs exactly one instance (`maxScale: 1`: SQLite and in-memory live calls), CPU always allocated, request timeout 3600 s, second-generation environment, concurrency 20, 2 vCPU / 2 GiB.
- The dentistry MCP service runs at most one instance (in-memory data), scales to zero, is protected by its bearer token and nothing else on it is secret.
- Every outward-facing or irreversible step (creating the GitHub repository, pushing, branch protection, creating GCP resources, the first deploy, live provider calls) is a STOP point: do it only after the user says go for that step in this session.
- `gofmt -w .`, `go test -race`, conventional commits ending with the session's attribution trailer, no model identifier in subjects.
- Shell scripts: `set -euo pipefail`, quoted variables, secret values only via stdin or the environment, never as arguments.

## Review Focus

- A bootstrap re-run must neither rotate an existing secret nor fail; and a dry run must print no secret value, ever. Pinned in Task 3.
- The WIF provider must refuse a token from another repository or another branch; the deployer service account must not be able to read provider keys. Pinned in Task 3 (conditions asserted in the dry-run output) and Task 6 (checked live).
- A manifest typo (`${REGION}` misspelt, a secret name that differs between the manifest and the bootstrap) must fail a test, not a deploy. Pinned in Task 2.
- A deploy whose smoke test fails must fail the workflow, leave the previous revision serving traffic, and say which check failed. Pinned in Task 4 (a 0%-traffic candidate is smoke-tested, then promoted).
- The smoke script's throttle check blocks the runner's address for a while; it must run last. Pinned in Task 4.
- Cold start: the first call after a scale-from-zero of the MCP service must still succeed within the MCP timeout. Measured in Task 7.
- Cost: a forgotten service must not bill unbounded provider spend; a budget alert, `max_sessions`, `max_call_duration` and a documented kill switch exist. Task 7.

---

### Task 1: the dentistry MCP image

**Files:**
- Create: `deploy/Dockerfile.dental-mcp`
- Modify: `.github/workflows/ci.yml` (job `image-smoke`: build and probe it), `server/deploy_contract_test.go`

**Interfaces:**
- Produces: image `dental-mcp` listening on `$PORT` (default 8081), non-root, `GET /health` open, `/mcp` bearer-protected (`DENTAL_MCP_TOKEN`).

- [ ] **Step 1: Failing contract test** (append to `server/deploy_contract_test.go`)

```go
func TestDentalMCPImageIsSmallAndNonRoot(t *testing.T) {
	body, err := os.ReadFile("../deploy/Dockerfile.dental-mcp")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, needle := range []string{"CGO_ENABLED=0", "./cmd/dental-mcp", "USER 65532", "ENTRYPOINT"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("Dockerfile.dental-mcp missing %q", needle)
		}
	}
	if strings.Contains(text, "DENTAL_MCP_TOKEN=") {
		t.Fatal("the token must never be baked into the image")
	}
}
```

Run: `cd server && go test -run TestDentalMCPImage ./` — Expected: FAIL (no such file).

- [ ] **Step 2: Write `deploy/Dockerfile.dental-mcp`**

```dockerfile
# The Smile Dental demo MCP server (cmd/dental-mcp): one static binary, fake data, no secrets baked in.
FROM golang:1.27.1-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS builder
WORKDIR /src
COPY go.work go.work.sum go.mod go.sum ./
COPY core/go.mod ./core/
COPY providers/cartesia/go.mod providers/cartesia/go.sum* ./providers/cartesia/
COPY providers/deepgram/go.mod providers/deepgram/go.sum* ./providers/deepgram/
COPY providers/elevenlabs/go.mod providers/elevenlabs/go.sum* ./providers/elevenlabs/
COPY providers/google/go.mod providers/google/go.sum* ./providers/google/
COPY providers/nvidia/go.mod providers/nvidia/go.sum* ./providers/nvidia/
COPY providers/openai/go.mod providers/openai/go.sum* ./providers/openai/
COPY providers/xai/go.mod providers/xai/go.sum* ./providers/xai/
COPY server/go.mod server/go.sum ./server/
RUN go work sync
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/dental-mcp ./cmd/dental-mcp

FROM gcr.io/distroless/static-debian12:nonroot@sha256:<digest at build time>
COPY --from=builder /out/dental-mcp /dental-mcp
USER 65532:65532
EXPOSE 8081
ENTRYPOINT ["/dental-mcp"]
```

Resolve and pin the distroless digest the way the main Dockerfile pins its bases (`docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot`). The builder stage copies the module graph the same way as `deploy/Dockerfile` because `cmd/dental-mcp` imports `govoice/server/...` through `go.work`.

- [ ] **Step 3: Build, run and probe it**

```bash
docker build -q -f deploy/Dockerfile.dental-mcp -t dental-mcp:test .
docker run --rm -d --name dmcp -p 18767:8081 -e DENTAL_MCP_TOKEN=probe-token-probe-token dental-mcp:test
curl -s http://127.0.0.1:18767/health
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:18767/mcp
docker stop dmcp
```

Expected: `{"status":"ok"}`; `401`; the container exits cleanly on stop. A container started without `DENTAL_MCP_TOKEN` exits non-zero with `set DENTAL_MCP_TOKEN` in its log.

- [ ] **Step 4: CI.** In `ci.yml` job `image-smoke`, after `deploy/smoke.sh`, add:

```yaml
      - name: dental-mcp image builds, refuses to run without a token, answers /health
        run: |
          docker build -q -f deploy/Dockerfile.dental-mcp -t dental-mcp:ci .
          ! docker run --rm dental-mcp:ci 2>/dev/null
          docker run -d --name dmcp -p 18767:8081 -e DENTAL_MCP_TOKEN=ci-token-ci-token-ci dental-mcp:ci
          for i in $(seq 1 20); do curl -fsS http://127.0.0.1:18767/health && break || sleep 1; done
          test "$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:18767/mcp)" = 401
```

- [ ] **Step 5: Gate and commit**

Run: `cd server && go test -race ./ -run 'TestDentalMCPImage|TestImageShips' && cd .. && gofmt -l server`
Expected: PASS.

```bash
git add deploy/Dockerfile.dental-mcp .github/workflows/ci.yml server/deploy_contract_test.go
git commit -m "build: image for the dentistry demo MCP server"
```

---

### Task 2: Cloud Run service manifests with a contract test

**Files:**
- Create: `deploy/gcp/govoice.service.yaml`, `deploy/gcp/dental-mcp.service.yaml`, `server/gcp_manifest_test.go`

**Interfaces:**
- Produces: two Knative Service manifests with `${...}` placeholders: `REGION`, `PROJECT_ID`, `IMAGE`, `MIN_INSTANCES`, `DENTAL_MCP_URL`. Secret names used (the bootstrap in Task 3 creates exactly these): `TOKEN_SECRET`, `GOVOICE_MANAGEMENT_API_KEY`, `XAI_API_KEY`, `ELEVENLABS_API_KEY`, `GOOGLE_API_KEY`, `GOOGLE_SA_KEY`, `DENTAL_MCP_TOKEN`. Service accounts: `govoice-runtime@${PROJECT_ID}.iam.gserviceaccount.com`.

- [ ] **Step 1: Write the failing contract test** `server/gcp_manifest_test.go`

```go
package server

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every secret name the deployed services and the bootstrap script must agree on.
var gcpSecretNames = []string{"TOKEN_SECRET", "GOVOICE_MANAGEMENT_API_KEY", "XAI_API_KEY", "ELEVENLABS_API_KEY", "GOOGLE_API_KEY", "GOOGLE_SA_KEY", "DENTAL_MCP_TOKEN"}

var gcpSample = map[string]string{
	"REGION": "us-central1", "PROJECT_ID": "demo-proj", "IMAGE": "us-central1-docker.pkg.dev/demo-proj/govoice/govoice:abc123",
	"MIN_INSTANCES": "1", "DENTAL_MCP_URL": "https://dental-mcp-abc.a.run.app/mcp",
}

type manifest struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Metadata struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
			Spec struct {
				ServiceAccountName   string `yaml:"serviceAccountName"`
				ContainerConcurrency int    `yaml:"containerConcurrency"`
				TimeoutSeconds       int    `yaml:"timeoutSeconds"`
				Containers           []struct {
					Image   string   `yaml:"image"`
					Command []string `yaml:"command"`
					Args    []string `yaml:"args"`
					Ports   []struct {
						ContainerPort int `yaml:"containerPort"`
					} `yaml:"ports"`
					Env []struct {
						Name      string `yaml:"name"`
						Value     string `yaml:"value"`
						ValueFrom *struct {
							SecretKeyRef struct {
								Name string `yaml:"name"`
								Key  string `yaml:"key"`
							} `yaml:"secretKeyRef"`
						} `yaml:"valueFrom"`
					} `yaml:"env"`
					Resources struct {
						Limits map[string]string `yaml:"limits"`
					} `yaml:"resources"`
					StartupProbe struct {
						HTTPGet struct {
							Path string `yaml:"path"`
							Port int    `yaml:"port"`
						} `yaml:"httpGet"`
					} `yaml:"startupProbe"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []struct {
					Name   string `yaml:"name"`
					Secret struct {
						SecretName string `yaml:"secretName"`
					} `yaml:"secret"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func renderManifest(t *testing.T, path string) manifest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	// Any placeholder not in the sample is a typo.
	for _, m := range regexp.MustCompile(`\$\{([A-Z_]+)\}`).FindAllStringSubmatch(text, -1) {
		if _, ok := gcpSample[m[1]]; !ok {
			t.Fatalf("%s uses unknown placeholder ${%s}", path, m[1])
		}
	}
	text = os.Expand(text, func(k string) string { return gcpSample[k] })
	var m manifest
	if err := yaml.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func secretEnv(m manifest) map[string]string { // env var -> secret name
	out := map[string]string{}
	for _, e := range m.Spec.Template.Spec.Containers[0].Env {
		if e.ValueFrom != nil {
			if e.ValueFrom.SecretKeyRef.Key != "latest" {
				panic("secret " + e.ValueFrom.SecretKeyRef.Name + " must follow latest")
			}
			out[e.Name] = e.ValueFrom.SecretKeyRef.Name
		}
	}
	return out
}

func TestVoiceServiceManifest(t *testing.T) {
	m := renderManifest(t, "../deploy/gcp/govoice.service.yaml")
	a, spec := m.Spec.Template.Metadata.Annotations, m.Spec.Template.Spec
	c := spec.Containers[0]
	for k, want := range map[string]string{
		"autoscaling.knative.dev/minScale": "1", "autoscaling.knative.dev/maxScale": "1",
		"run.googleapis.com/execution-environment": "gen2", "run.googleapis.com/cpu-throttling": "false",
	} {
		if a[k] != want {
			t.Errorf("annotation %s = %q, want %q", k, a[k], want)
		}
	}
	if spec.TimeoutSeconds != 3600 || spec.ContainerConcurrency != 20 || c.Resources.Limits["cpu"] != "2" || c.Resources.Limits["memory"] != "2Gi" {
		t.Errorf("sizing: timeout=%d concurrency=%d limits=%v", spec.TimeoutSeconds, spec.ContainerConcurrency, c.Resources.Limits)
	}
	if spec.ServiceAccountName != "govoice-runtime@demo-proj.iam.gserviceaccount.com" {
		t.Errorf("service account = %q", spec.ServiceAccountName)
	}
	if strings.Join(c.Args, " ") != "-- /opt/govoice/bin/server -config /etc/govoice/cloudrun.yaml" || len(c.Command) != 1 || c.Command[0] != "/usr/bin/tini" {
		t.Errorf("command/args = %v %v", c.Command, c.Args)
	}
	if c.StartupProbe.HTTPGet.Path != "/health" || c.StartupProbe.HTTPGet.Port != 8080 || c.Ports[0].ContainerPort != 8080 {
		t.Errorf("probe/port: %+v %+v", c.StartupProbe, c.Ports)
	}
	want := map[string]string{
		"TOKEN_SECRET": "TOKEN_SECRET", "GOVOICE_MANAGEMENT_API_KEY": "GOVOICE_MANAGEMENT_API_KEY",
		"XAI_API_KEY": "XAI_API_KEY", "ELEVENLABS_API_KEY": "ELEVENLABS_API_KEY", "GOOGLE_API_KEY": "GOOGLE_API_KEY",
		"MCP_TOKEN_DENTAL": "DENTAL_MCP_TOKEN",
	}
	got := secretEnv(m)
	for env, secret := range want {
		if got[env] != secret {
			t.Errorf("env %s must come from secret %s, got %q", env, secret, got[env])
		}
	}
	// No secret-looking variable may carry a literal value.
	for _, e := range c.Env {
		if regexp.MustCompile(`KEY|SECRET|TOKEN|CREDENTIALS`).MatchString(e.Name) && e.ValueFrom == nil && e.Name != "GOOGLE_APPLICATION_CREDENTIALS" {
			t.Errorf("env %s must come from Secret Manager, not a literal", e.Name)
		}
		if e.Name == "GOOGLE_APPLICATION_CREDENTIALS" && e.Value != "/secrets/google/key.json" {
			t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q", e.Value)
		}
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].Secret.SecretName != "GOOGLE_SA_KEY" || c.VolumeMounts[0].MountPath != "/secrets/google" {
		t.Errorf("the Google key must be a mounted secret file: %+v %+v", spec.Volumes, c.VolumeMounts)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["GOVOICE_DATABASE_DSN"] != "/tmp/govoice.db" || env["DENTAL_MCP_URL"] != "https://dental-mcp-abc.a.run.app/mcp" {
		t.Errorf("env = %v", env)
	}
}

func TestDentalMCPServiceManifest(t *testing.T) {
	m := renderManifest(t, "../deploy/gcp/dental-mcp.service.yaml")
	a, spec := m.Spec.Template.Metadata.Annotations, m.Spec.Template.Spec
	c := spec.Containers[0]
	if a["autoscaling.knative.dev/minScale"] != "0" || a["autoscaling.knative.dev/maxScale"] != "1" {
		t.Errorf("scaling: %v (in-memory data: at most one instance, scale to zero)", a)
	}
	if got := secretEnv(m); got["DENTAL_MCP_TOKEN"] != "DENTAL_MCP_TOKEN" || len(got) != 1 {
		t.Errorf("secrets = %v", got)
	}
	if c.Ports[0].ContainerPort != 8081 || c.StartupProbe.HTTPGet.Path != "/health" || c.StartupProbe.HTTPGet.Port != 8081 {
		t.Errorf("port/probe: %+v %+v", c.Ports, c.StartupProbe)
	}
	if spec.ServiceAccountName != "govoice-runtime@demo-proj.iam.gserviceaccount.com" {
		t.Errorf("service account = %q", spec.ServiceAccountName)
	}
}

func TestManifestSecretsAreTheOnesTheBootstrapCreates(t *testing.T) {
	boot, err := os.ReadFile("../deploy/gcp/bootstrap.sh")
	if err != nil {
		t.Skip("bootstrap.sh arrives in the next task")
	}
	for _, name := range gcpSecretNames {
		if !strings.Contains(string(boot), name) {
			t.Errorf("bootstrap.sh does not create secret %s", name)
		}
	}
}
```

- [ ] **Step 2: Run to see failure** — `cd server && go test -run 'TestVoiceServiceManifest|TestDentalMCPServiceManifest' ./ 2>&1 | tail -4` — Expected: FAIL (file not found).

- [ ] **Step 3: Write `deploy/gcp/govoice.service.yaml`**

```yaml
# The voice server on Cloud Run: ONE instance (SQLite and live calls are in memory), CPU always
# allocated (a call is a long-lived request that needs CPU between requests), 60-minute request
# timeout (a call is a WebSocket request; the server caps a call at 20 minutes itself).
# Rendered with envsubst by .github/workflows/deploy.yml; ${...} placeholders are checked by
# server/gcp_manifest_test.go.
apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: govoice
  labels:
    cloud.googleapis.com/location: ${REGION}
spec:
  template:
    metadata:
      annotations:
        autoscaling.knative.dev/minScale: "${MIN_INSTANCES}"
        autoscaling.knative.dev/maxScale: "1"
        run.googleapis.com/execution-environment: gen2
        run.googleapis.com/cpu-throttling: "false"
        run.googleapis.com/startup-cpu-boost: "true"
        run.googleapis.com/sessionAffinity: "true"
    spec:
      serviceAccountName: govoice-runtime@${PROJECT_ID}.iam.gserviceaccount.com
      containerConcurrency: 20
      timeoutSeconds: 3600
      containers:
        - image: ${IMAGE}
          command: ["/usr/bin/tini"]
          args: ["--", "/opt/govoice/bin/server", "-config", "/etc/govoice/cloudrun.yaml"]
          ports:
            - containerPort: 8080
          resources:
            limits:
              cpu: "2"
              memory: 2Gi
          startupProbe:
            httpGet:
              path: /health
              port: 8080
            periodSeconds: 3
            timeoutSeconds: 2
            failureThreshold: 20
          env:
            - {name: GOVOICE_DATABASE_DSN, value: /tmp/govoice.db}
            - {name: DENTAL_MCP_URL, value: "${DENTAL_MCP_URL}"}
            - {name: GOOGLE_APPLICATION_CREDENTIALS, value: /secrets/google/key.json}
            - name: TOKEN_SECRET
              valueFrom: {secretKeyRef: {name: TOKEN_SECRET, key: latest}}
            - name: GOVOICE_MANAGEMENT_API_KEY
              valueFrom: {secretKeyRef: {name: GOVOICE_MANAGEMENT_API_KEY, key: latest}}
            - name: XAI_API_KEY
              valueFrom: {secretKeyRef: {name: XAI_API_KEY, key: latest}}
            - name: ELEVENLABS_API_KEY
              valueFrom: {secretKeyRef: {name: ELEVENLABS_API_KEY, key: latest}}
            - name: GOOGLE_API_KEY
              valueFrom: {secretKeyRef: {name: GOOGLE_API_KEY, key: latest}}
            - name: MCP_TOKEN_DENTAL
              valueFrom: {secretKeyRef: {name: DENTAL_MCP_TOKEN, key: latest}}
          volumeMounts:
            - {name: google-key, mountPath: /secrets/google}
      volumes:
        - name: google-key
          secret:
            secretName: GOOGLE_SA_KEY
            items:
              - {key: latest, path: key.json}
```

`deploy/gcp/dental-mcp.service.yaml`:

```yaml
# The demo's scheduling MCP server: in-memory fake data, so at most one instance; scales to zero.
apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: dental-mcp
  labels:
    cloud.googleapis.com/location: ${REGION}
spec:
  template:
    metadata:
      annotations:
        autoscaling.knative.dev/minScale: "0"
        autoscaling.knative.dev/maxScale: "1"
    spec:
      serviceAccountName: govoice-runtime@${PROJECT_ID}.iam.gserviceaccount.com
      containerConcurrency: 40
      timeoutSeconds: 300
      containers:
        - image: ${IMAGE}
          ports:
            - containerPort: 8081
          resources:
            limits:
              cpu: "1"
              memory: 256Mi
          startupProbe:
            httpGet:
              path: /health
              port: 8081
            periodSeconds: 2
            failureThreshold: 10
          env:
            - name: DENTAL_MCP_TOKEN
              valueFrom: {secretKeyRef: {name: DENTAL_MCP_TOKEN, key: latest}}
```

- [ ] **Step 4: Run to see it pass** — `cd server && go test -run 'Manifest' ./ -v 2>&1 | tail -10` — Expected: PASS (the bootstrap cross-check skips until Task 3).

- [ ] **Step 5: Commit**

```bash
git add deploy/gcp server/gcp_manifest_test.go
git commit -m "build(deploy): Cloud Run manifests for the voice server and the dentistry MCP"
```

---

### Task 3: the GCP bootstrap script (idempotent, dry-runnable)

**Files:**
- Create: `deploy/gcp/bootstrap.sh`, `server/gcp_bootstrap_test.go`

**Interfaces:**
- Consumes: the secret names and service-account names from Task 2.
- Produces: `deploy/gcp/bootstrap.sh`, run once by the user. Inputs (environment): `PROJECT_ID`, `GITHUB_REPO` (`owner/name`), optional `REGION` (default `us-central1`), `AR_REPO` (default `govoice`), `BILLING_ACCOUNT` + `BUDGET_USD` (optional budget alert), provider values `XAI_API_KEY`, `ELEVENLABS_API_KEY`, `GOOGLE_API_KEY`, `GOOGLE_SA_KEY_FILE` (path to the service-account JSON). `DRY_RUN=1` prints every command and executes none. It sets the four GitHub repository variables through `gh variable set` when `gh` is authenticated.

- [ ] **Step 1: Write the failing tests** `server/gcp_bootstrap_test.go`

```go
package server

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runBootstrap(t *testing.T, env ...string) (string, error) {
	t.Helper()
	sa := t.TempDir() + "/sa.json"
	if err := os.WriteFile(sa, []byte(`{"type":"service_account","private_key":"SENTINEL-PRIVATE-KEY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "../deploy/gcp/bootstrap.sh")
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "DRY_RUN=1", "PROJECT_ID=demo-proj", "GITHUB_REPO=acme/go-voice",
		"XAI_API_KEY=SENTINEL-XAI", "ELEVENLABS_API_KEY=SENTINEL-ELEVEN", "GOOGLE_API_KEY=SENTINEL-GOOGLE", "GOOGLE_SA_KEY_FILE=" + sa}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestBootstrapDryRunPrintsThePlanAndNoSecret(t *testing.T) {
	out, err := runBootstrap(t)
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	for _, s := range []string{"SENTINEL-XAI", "SENTINEL-ELEVEN", "SENTINEL-GOOGLE", "SENTINEL-PRIVATE-KEY"} {
		if strings.Contains(out, s) {
			t.Fatalf("the dry run printed a secret value (%s):\n%s", s, out)
		}
	}
	for _, want := range []string{
		"run.googleapis.com", "artifactregistry.googleapis.com", "secretmanager.googleapis.com", "iamcredentials.googleapis.com",
		"artifacts repositories create govoice", "govoice-runtime", "govoice-deployer",
		"workload-identity-pools create github", "attribute.repository", "assertion.repository=='acme/go-voice'", "assertion.ref=='refs/heads/main'",
		"roles/run.admin", "roles/iam.workloadIdentityUser",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q", want)
		}
	}
	for _, name := range gcpSecretNames {
		if !strings.Contains(out, "secret "+name) {
			t.Errorf("plan does not create secret %s", name)
		}
	}
	// The deployer may read exactly one secret (the management key, for the smoke test), never the provider keys.
	if strings.Contains(out, "secrets add-iam-policy-binding XAI_API_KEY --member=serviceAccount:govoice-deployer") {
		t.Error("the deployer must not read provider keys")
	}
}

func TestBootstrapRefusesMissingInput(t *testing.T) {
	for _, drop := range []string{"PROJECT_ID", "GITHUB_REPO", "XAI_API_KEY", "ELEVENLABS_API_KEY", "GOOGLE_API_KEY", "GOOGLE_SA_KEY_FILE"} {
		cmd := exec.Command("bash", "../deploy/gcp/bootstrap.sh")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "DRY_RUN=1", drop + "="} // everything else unset
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "required") {
			t.Errorf("without %s: err=%v out=%s", drop, err, out)
		}
	}
	if out, err := runBootstrap(t, "GITHUB_REPO=not-a-repo"); err == nil || !strings.Contains(out, "owner/name") {
		t.Errorf("a malformed GITHUB_REPO must be refused: %v %s", err, out)
	}
}
```

The second test passes a deliberately incomplete environment for each dropped name; make the script validate every required variable before doing anything and name the first missing one with the word `required`.

- [ ] **Step 2: Run to see failure** — `cd server && go test -run TestBootstrap ./ 2>&1 | tail -5` — Expected: FAIL (no such script).

- [ ] **Step 3: Write `deploy/gcp/bootstrap.sh`**

```bash
#!/usr/bin/env bash
# One-time Google Cloud setup for the Cloud Run deployment (docs/deploy/cloudrun.md).
# Idempotent: re-running adds what is missing and NEVER rotates an existing secret.
# DRY_RUN=1 prints every command and runs none (and never prints a secret value).
#
# Required environment:
#   PROJECT_ID  GITHUB_REPO (owner/name)
#   XAI_API_KEY  ELEVENLABS_API_KEY  GOOGLE_API_KEY   provider keys (Gemini uses GOOGLE_API_KEY)
#   GOOGLE_SA_KEY_FILE   path to the service-account JSON for Google speech (STT/TTS)
# Optional: REGION (us-central1)  AR_REPO (govoice)  BILLING_ACCOUNT + BUDGET_USD (50)  for a budget alert
set -euo pipefail

need() { [[ -n "${!1:-}" ]] || { echo "bootstrap: $1 is required" >&2; exit 2; }; }
for v in PROJECT_ID GITHUB_REPO XAI_API_KEY ELEVENLABS_API_KEY GOOGLE_API_KEY GOOGLE_SA_KEY_FILE; do need "$v"; done
[[ "$GITHUB_REPO" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || { echo "bootstrap: GITHUB_REPO must look like owner/name" >&2; exit 2; }
[[ -r "$GOOGLE_SA_KEY_FILE" ]] || { echo "bootstrap: GOOGLE_SA_KEY_FILE is not a readable file" >&2; exit 2; }

REGION="${REGION:-us-central1}"
AR_REPO="${AR_REPO:-govoice}"
RUNTIME_SA="govoice-runtime@${PROJECT_ID}.iam.gserviceaccount.com"
DEPLOYER_SA="govoice-deployer@${PROJECT_ID}.iam.gserviceaccount.com"
DRY="${DRY_RUN:-}"

# run prints the command; it executes it unless DRY_RUN is set. Secrets never appear in arguments.
run() { echo "+ $*"; [[ -n "$DRY" ]] || "$@"; }
exists() { [[ -z "$DRY" ]] && "$@" >/dev/null 2>&1; }   # a dry run assumes nothing exists

echo "== project ${PROJECT_ID}, region ${REGION}, repo ${GITHUB_REPO} =="
[[ -n "$DRY" ]] && PROJECT_NUMBER="000000000000" || PROJECT_NUMBER="$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')"

echo "== APIs =="
run gcloud services enable run.googleapis.com artifactregistry.googleapis.com secretmanager.googleapis.com \
  iamcredentials.googleapis.com iam.googleapis.com cloudresourcemanager.googleapis.com sts.googleapis.com --project "$PROJECT_ID"

echo "== Artifact Registry =="
exists gcloud artifacts repositories describe "$AR_REPO" --location "$REGION" --project "$PROJECT_ID" ||
  run gcloud artifacts repositories create "$AR_REPO" --repository-format=docker --location "$REGION" --project "$PROJECT_ID"

echo "== service accounts =="
for sa in govoice-runtime govoice-deployer; do
  exists gcloud iam service-accounts describe "${sa}@${PROJECT_ID}.iam.gserviceaccount.com" --project "$PROJECT_ID" ||
    run gcloud iam service-accounts create "$sa" --display-name "$sa" --project "$PROJECT_ID"
done

echo "== secrets (created once; never rotated here) =="
# put_secret NAME SOURCE: SOURCE is "env:VAR", "file:PATH" or "random". The value goes in on stdin.
put_secret() {
  local name="$1" src="$2"
  if exists gcloud secrets describe "$name" --project "$PROJECT_ID"; then echo "secret $name exists, left unchanged"; return; fi
  echo "+ create secret $name (from ${src%%:*})"
  [[ -n "$DRY" ]] && return
  gcloud secrets create "$name" --replication-policy=automatic --project "$PROJECT_ID" >/dev/null
  case "$src" in
    env:*)  printf %s "${!src#env:}" | gcloud secrets versions add "$name" --data-file=- --project "$PROJECT_ID" >/dev/null ;;
    file:*) gcloud secrets versions add "$name" --data-file="${src#file:}" --project "$PROJECT_ID" >/dev/null ;;
    random) openssl rand -hex 32 | tr -d '\n' | gcloud secrets versions add "$name" --data-file=- --project "$PROJECT_ID" >/dev/null ;;
  esac
}
put_secret XAI_API_KEY env:XAI_API_KEY
put_secret ELEVENLABS_API_KEY env:ELEVENLABS_API_KEY
put_secret GOOGLE_API_KEY env:GOOGLE_API_KEY
put_secret GOOGLE_SA_KEY "file:${GOOGLE_SA_KEY_FILE}"
put_secret TOKEN_SECRET random
put_secret GOVOICE_MANAGEMENT_API_KEY random
put_secret DENTAL_MCP_TOKEN random

echo "== who may read what =="
for s in XAI_API_KEY ELEVENLABS_API_KEY GOOGLE_API_KEY GOOGLE_SA_KEY TOKEN_SECRET GOVOICE_MANAGEMENT_API_KEY DENTAL_MCP_TOKEN; do
  run gcloud secrets add-iam-policy-binding "$s" --member="serviceAccount:${RUNTIME_SA}" --role=roles/secretmanager.secretAccessor --project "$PROJECT_ID" --condition=None >/dev/null
done
# The deployer reads only the management key (the smoke test logs in with it); never a provider key.
run gcloud secrets add-iam-policy-binding GOVOICE_MANAGEMENT_API_KEY --member="serviceAccount:${DEPLOYER_SA}" --role=roles/secretmanager.secretAccessor --project "$PROJECT_ID" --condition=None >/dev/null

echo "== deployer permissions =="
run gcloud projects add-iam-policy-binding "$PROJECT_ID" --member="serviceAccount:${DEPLOYER_SA}" --role=roles/run.admin --condition=None >/dev/null
run gcloud artifacts repositories add-iam-policy-binding "$AR_REPO" --location "$REGION" --member="serviceAccount:${DEPLOYER_SA}" --role=roles/artifactregistry.writer --project "$PROJECT_ID" >/dev/null
run gcloud iam service-accounts add-iam-policy-binding "$RUNTIME_SA" --member="serviceAccount:${DEPLOYER_SA}" --role=roles/iam.serviceAccountUser --project "$PROJECT_ID" >/dev/null

echo "== Workload Identity Federation: only ${GITHUB_REPO} on main =="
exists gcloud iam workload-identity-pools describe github --location=global --project "$PROJECT_ID" ||
  run gcloud iam workload-identity-pools create github --location=global --display-name="GitHub Actions" --project "$PROJECT_ID"
exists gcloud iam workload-identity-pools providers describe github-oidc --location=global --workload-identity-pool=github --project "$PROJECT_ID" ||
  run gcloud iam workload-identity-pools providers create-oidc github-oidc --location=global --workload-identity-pool=github \
    --issuer-uri="https://token.actions.githubusercontent.com" \
    --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref" \
    --attribute-condition="assertion.repository=='${GITHUB_REPO}' && assertion.ref=='refs/heads/main'" \
    --project "$PROJECT_ID"
POOL="projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/github"
run gcloud iam service-accounts add-iam-policy-binding "$DEPLOYER_SA" --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/${POOL}/attribute.repository/${GITHUB_REPO}" --project "$PROJECT_ID" >/dev/null
WIF_PROVIDER="${POOL}/providers/github-oidc"

if [[ -n "${BILLING_ACCOUNT:-}" ]]; then
  echo "== budget alert =="
  run gcloud billing budgets create --billing-account="$BILLING_ACCOUNT" --display-name="govoice-demo" \
    --budget-amount="${BUDGET_USD:-50}USD" --threshold-rule=percent=0.5 --threshold-rule=percent=0.9 --threshold-rule=percent=1.0 \
    --filter-projects="projects/${PROJECT_ID}"
else
  echo "== budget alert skipped (set BILLING_ACCOUNT to create one; see docs/deploy/cloudrun.md) =="
fi

echo "== GitHub repository variables (not secrets) =="
if command -v gh >/dev/null 2>&1 && [[ -z "$DRY" ]]; then
  gh variable set GCP_PROJECT_ID --body "$PROJECT_ID" --repo "$GITHUB_REPO"
  gh variable set GCP_REGION --body "$REGION" --repo "$GITHUB_REPO"
  gh variable set GCP_WIF_PROVIDER --body "$WIF_PROVIDER" --repo "$GITHUB_REPO"
  gh variable set GCP_DEPLOYER_SA --body "$DEPLOYER_SA" --repo "$GITHUB_REPO"
else
  echo "+ gh variable set GCP_PROJECT_ID / GCP_REGION / GCP_WIF_PROVIDER / GCP_DEPLOYER_SA --repo $GITHUB_REPO"
fi
echo "done. The admin key: gcloud secrets versions access latest --secret=GOVOICE_MANAGEMENT_API_KEY --project $PROJECT_ID"
```

`chmod +x deploy/gcp/bootstrap.sh`. Notes for the implementer: `--condition=None` avoids the interactive prompt on projects with conditional bindings; `[[ -n "$DRY" ]] && X || Y` lines must not trip `set -e` (the form above is safe because the left side is a test); if `bash -n` or the dry-run test reports an unbound variable, fix the script, not the test.

- [ ] **Step 4: Run to see them pass** 

Run: `bash -n deploy/gcp/bootstrap.sh && cd server && go test -run 'TestBootstrap|TestManifestSecrets' ./ -v 2>&1 | tail -10` — Expected: PASS, and the bootstrap cross-check of Task 2 now runs (no skip).

- [ ] **Step 5: Commit**

```bash
git add deploy/gcp/bootstrap.sh server/gcp_bootstrap_test.go
git commit -m "build(deploy): idempotent GCP bootstrap with Workload Identity Federation"
```

---

### Task 4: the smoke script and the deploy workflow

**Files:**
- Create: `deploy/smoke-cloudrun.sh`, `.github/workflows/deploy.yml`, `server/deploy_workflow_test.go`

**Interfaces:**
- Consumes: manifests (Task 2), repository variables (Task 3), `GET /api/v1/server-config` fields `client_ip` and `secure` (plan 2), `GET /api/v1/providers`, `/api/v1/mcp-servers`, `/api/v1/flows`, demo auto-install (plan 1).
- Produces: `deploy/smoke-cloudrun.sh <base-url>` (key from `GOVOICE_MANAGEMENT_API_KEY`); the `deploy` workflow.

- [ ] **Step 1: Failing tests** `server/deploy_workflow_test.go`

```go
package server

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeployWorkflowShape(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/deploy.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	var wf struct {
		On          map[string]any `yaml:"on"`
		Permissions map[string]string
		Concurrency struct {
			Group            string `yaml:"group"`
			CancelInProgress bool   `yaml:"cancel-in-progress"`
		}
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if wf.Permissions["id-token"] != "write" || wf.Permissions["contents"] != "read" || len(wf.Permissions) != 2 {
		t.Errorf("permissions = %v (WIF needs id-token: write; nothing else)", wf.Permissions)
	}
	for _, trigger := range []string{"workflow_run", "workflow_dispatch"} {
		if _, ok := wf.On[trigger]; !ok {
			t.Errorf("missing trigger %s", trigger)
		}
	}
	if _, ok := wf.On["push"]; ok {
		t.Error("deploy must follow CI (workflow_run), not race it on push")
	}
	if wf.Concurrency.Group != "deploy" || wf.Concurrency.CancelInProgress {
		t.Errorf("concurrency = %+v: a deploy must never be cancelled half way", wf.Concurrency)
	}
	for _, needle := range []string{
		"google-github-actions/auth@", "workload_identity_provider: ${{ vars.GCP_WIF_PROVIDER }}",
		"github.event.workflow_run.conclusion == 'success'", "github.event.workflow_run.head_sha",
		"envsubst", "gcloud run services replace", "candidate", "update-traffic", "smoke-cloudrun.sh",
	} {
		if !strings.Contains(text, needle) {
			t.Errorf("deploy.yml lacks %q", needle)
		}
	}
	for _, banned := range []string{"credentials_json", "GCP_SA_KEY", "secrets.XAI", "secrets.ELEVEN", "secrets.GOOGLE"} {
		if strings.Contains(text, banned) {
			t.Errorf("deploy.yml must not use %q: no stored GCP key, no provider key in GitHub", banned)
		}
	}
}

func TestSmokeScriptSyntaxAndOrder(t *testing.T) {
	if out, err := exec.Command("bash", "-n", "../deploy/smoke-cloudrun.sh").CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	raw, _ := os.ReadFile("../deploy/smoke-cloudrun.sh")
	text := string(raw)
	// The throttle check blocks the runner's address, so it must be the last one.
	if strings.LastIndex(text, "too_many_attempts") < strings.LastIndex(text, "Smile Dental front desk") {
		t.Error("the throttle check must come after every authenticated check")
	}
	for _, needle := range []string{"/health", "client_ip", `"secure"`, "/api/v1/providers", "elevenlabs", "xai", "google", "/api/v1/mcp-servers", "/api/v1/flows", "/metrics"} {
		if !strings.Contains(text, needle) {
			t.Errorf("smoke script lacks a check for %q", needle)
		}
	}
	if strings.Contains(text, "set -x") {
		t.Error("set -x would print the management key")
	}
}
```

Run: `cd server && go test -run 'TestDeployWorkflow|TestSmokeScript' ./ 2>&1 | tail -5` — Expected: FAIL (files missing).

- [ ] **Step 2: Write `deploy/smoke-cloudrun.sh`**

```bash
#!/usr/bin/env bash
# Smoke test of a deployed Cloud Run revision. Costs nothing: no call is placed, no provider is called.
# Usage: GOVOICE_MANAGEMENT_API_KEY=... deploy/smoke-cloudrun.sh https://govoice-xyz.a.run.app
# The throttle check runs LAST: it uses up this machine's failed-login allowance for a few seconds.
set -euo pipefail
base="${1:?usage: smoke-cloudrun.sh <base-url>}"
base="${base%/}"
: "${GOVOICE_MANAGEMENT_API_KEY:?GOVOICE_MANAGEMENT_API_KEY is required}"
command -v jq >/dev/null || { echo "smoke: jq is required" >&2; exit 2; }
fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }
api() { curl -fsS --max-time 20 -H "Authorization: Bearer ${GOVOICE_MANAGEMENT_API_KEY}" "${base}$1"; }

# 1. alive, and the UI is the embedded one
for _ in $(seq 1 30); do curl -fsS --max-time 5 "${base}/health" >/dev/null 2>&1 && break || sleep 2; done
curl -fsS "${base}/health" | jq -e '.status == "live"' >/dev/null || fail "/health is not live"
curl -fsS "${base}/" | grep -q '<div id="root"' || fail "the admin UI is not served at /"
curl -fsS "${base}/flows" | grep -q '<div id="root"' || fail "a client route does not fall back to the UI"

# 2. the operational surfaces are not on the public port
for p in /metrics /ready /debug/timeline; do
  code="$(curl -s -o /dev/null -w '%{http_code}' "${base}${p}")"
  [[ "$code" != 200 ]] || fail "${p} is served on the public port"
done

# 3. the management API is behind the key, and the proxy is trusted correctly
[[ "$(curl -s -o /dev/null -w '%{http_code}' "${base}/api/v1/server-config")" == 401 ]] || fail "the management API answers without a key"
cfg="$(api /api/v1/server-config)"
echo "$cfg" | jq -e '.data.secure == true' >/dev/null || fail "the server does not see the request as https: check tls.trusted_proxies (peer vs Cloud Run)"
ip="$(echo "$cfg" | jq -r '.data.client_ip')"
if [[ ! "$ip" =~ ^[0-9a-fA-F:.]+$ ]] || [[ "$ip" =~ ^(10\.|127\.|169\.254\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.) ]]; then
  fail "client_ip is ${ip}: the proxy chain is trusted wrongly (every caller would share one rate-limit bucket)"
fi
echo "client_ip ${ip}"

# 4. providers: the three the demo uses are detected
prov="$(api /api/v1/providers)"
for id in google xai elevenlabs; do
  echo "$prov" | jq -e --arg id "$id" '.data[] | select(.id == $id and .detected == true)' >/dev/null || fail "provider ${id} is not detected (missing secret?)"
done

# 5. the dentistry MCP server is connected and offers its tools
mcp="$(api /api/v1/mcp-servers)"
echo "$mcp" | jq -e '.data[] | select(.name == "Smile Dental scheduling")' >/dev/null || fail "the dental MCP server is not registered (demo.auto_install / DENTAL_MCP_URL)"

# 6. the demo flow was installed by the server and is ready (install runs in the background after start)
ready=""
for _ in $(seq 1 30); do
  ready="$(api /api/v1/flows | jq -r '.data[] | select(.name == "Smile Dental front desk") | .readiness.state' || true)"
  [[ "$ready" == "ready" ]] && break
  sleep 2
done
[[ "$ready" == "ready" ]] || fail "the flow 'Smile Dental front desk' is not ready (state: ${ready:-missing})"

# 7. LAST: wrong keys are refused, then throttled
for _ in $(seq 1 10); do curl -s -o /dev/null -H "Authorization: Bearer wrong" "${base}/api/v1/server-config"; done
curl -s -H "Authorization: Bearer wrong" "${base}/api/v1/server-config" | grep -q too_many_attempts || fail "failed logins are not throttled"
echo "SMOKE OK ${base}"
```

`chmod +x`. Adjust the JSON paths (`.data[]`, `.readiness.state`) to the real response envelopes by running the script against a local one-port server (`bin/server-fake` with `public_admin`, the dental MCP and `demo.auto_install`; plan 2 Task 6's setup shows how) before relying on it. The local run can only skip check 3's `secure == true`/public-IP assertions: add `SMOKE_LOCAL=1` that downgrades exactly those two to warnings, and print when it did.

- [ ] **Step 3: Write `.github/workflows/deploy.yml`**

```yaml
# Deploys to Cloud Run after CI passes on main (or by hand). Authenticates with Workload Identity
# Federation: no GCP key is stored in GitHub. The new revision takes no traffic until the smoke
# test passes; a failed smoke test leaves the previous revision serving.
name: deploy
on:
  workflow_run:
    workflows: [ci]
    types: [completed]
    branches: [main]
  workflow_dispatch:
    inputs:
      min_instances:
        description: "Voice server minimum instances (1 keeps it warm; 0 stops idle spend and loses nothing: the demo reinstalls on start)"
        default: "1"
permissions:
  contents: read
  id-token: write
concurrency:
  group: deploy
  cancel-in-progress: false
jobs:
  deploy:
    if: github.event_name == 'workflow_dispatch' || github.event.workflow_run.conclusion == 'success'
    runs-on: ubuntu-24.04
    timeout-minutes: 45
    env:
      PROJECT_ID: ${{ vars.GCP_PROJECT_ID }}
      REGION: ${{ vars.GCP_REGION }}
      MIN_INSTANCES: ${{ inputs.min_instances || '1' }}
      SHA: ${{ github.event.workflow_run.head_sha || github.sha }}
      REGISTRY: ${{ vars.GCP_REGION }}-docker.pkg.dev/${{ vars.GCP_PROJECT_ID }}/govoice
    steps:
      - uses: actions/checkout@v4
        with:
          ref: ${{ github.event.workflow_run.head_sha || github.sha }}
      - uses: google-github-actions/auth@v2
        with:
          workload_identity_provider: ${{ vars.GCP_WIF_PROVIDER }}
          service_account: ${{ vars.GCP_DEPLOYER_SA }}
      - uses: google-github-actions/setup-gcloud@v2
      - run: gcloud auth configure-docker "${REGION}-docker.pkg.dev" --quiet

      - name: Build and push the images
        run: |
          docker build -q -f deploy/Dockerfile.dental-mcp -t "${REGISTRY}/dental-mcp:${SHA}" .
          docker build -q -f deploy/Dockerfile --build-arg VERSION="${SHA::7}" --build-arg COMMIT="${SHA}" -t "${REGISTRY}/govoice:${SHA}" .
          docker push "${REGISTRY}/dental-mcp:${SHA}"
          docker push "${REGISTRY}/govoice:${SHA}"

      - name: Deploy the dentistry MCP server
        run: |
          IMAGE="${REGISTRY}/dental-mcp:${SHA}" envsubst '${REGION} ${PROJECT_ID} ${IMAGE}' < deploy/gcp/dental-mcp.service.yaml > /tmp/dental-mcp.yaml
          gcloud run services replace /tmp/dental-mcp.yaml --region "${REGION}" --project "${PROJECT_ID}"
          gcloud run services add-iam-policy-binding dental-mcp --region "${REGION}" --project "${PROJECT_ID}" \
            --member=allUsers --role=roles/run.invoker >/dev/null
          echo "DENTAL_MCP_URL=$(gcloud run services describe dental-mcp --region "${REGION}" --project "${PROJECT_ID}" --format='value(status.url)')/mcp" >> "$GITHUB_ENV"

      - name: Deploy the voice server as a candidate (existing traffic stays on the serving revision)
        run: |
          export IMAGE="${REGISTRY}/govoice:${SHA}"
          envsubst '${REGION} ${PROJECT_ID} ${IMAGE} ${MIN_INSTANCES} ${DENTAL_MCP_URL}' < deploy/gcp/govoice.service.yaml > /tmp/govoice.yaml
          PREV="$(gcloud run services describe govoice --region "${REGION}" --project "${PROJECT_ID}" --format='value(status.traffic[0].revisionName)' 2>/dev/null || true)"
          if [ -n "${PREV}" ]; then
            # spec.traffic is a sibling of spec.template: keep 100% on the serving revision; the new one gets 0% and a tag.
            printf '  traffic:\n    - revisionName: %s\n      percent: 100\n    - latestRevision: true\n      percent: 0\n      tag: candidate\n' "${PREV}" >> /tmp/govoice.yaml
          fi
          gcloud run services replace /tmp/govoice.yaml --region "${REGION}" --project "${PROJECT_ID}"
          gcloud run services add-iam-policy-binding govoice --region "${REGION}" --project "${PROJECT_ID}" \
            --member=allUsers --role=roles/run.invoker >/dev/null
          URL="$(gcloud run services describe govoice --region "${REGION}" --project "${PROJECT_ID}" --format='value(status.url)')"
          echo "SERVICE_URL=${URL}" >> "$GITHUB_ENV"
          echo "PREV_REVISION=${PREV}" >> "$GITHUB_ENV"
          if [ -n "${PREV}" ]; then echo "SMOKE_URL=https://candidate---${URL#https://}" >> "$GITHUB_ENV"; else echo "SMOKE_URL=${URL}" >> "$GITHUB_ENV"; fi

      - name: Smoke test the candidate
        run: |
          set +x
          export GOVOICE_MANAGEMENT_API_KEY="$(gcloud secrets versions access latest --secret=GOVOICE_MANAGEMENT_API_KEY --project "${PROJECT_ID}")"
          echo "::add-mask::${GOVOICE_MANAGEMENT_API_KEY}"
          deploy/smoke-cloudrun.sh "${SMOKE_URL}"

      - name: Promote the candidate
        run: |
          if [ -n "${PREV_REVISION}" ]; then
            gcloud run services update-traffic govoice --to-latest --region "${REGION}" --project "${PROJECT_ID}"
            gcloud run services update-traffic govoice --remove-tags candidate --region "${REGION}" --project "${PROJECT_ID}"
          fi
          echo "### Deployed ${SHA::7}: ${SERVICE_URL}" >> "$GITHUB_STEP_SUMMARY"
```

A failed smoke test fails the job before the promote step: the previous revision keeps 100% of the traffic and the candidate stays at 0% for inspection (the first deploy has no previous revision, so the smoke test runs on the live URL). The smoke script's check 7 (throttling) blocks the runner's address only for seconds, and it is last.

- [ ] **Step 4: Run to see the tests pass** — `chmod +x deploy/smoke-cloudrun.sh && cd server && go test -run 'TestDeployWorkflow|TestSmokeScript' ./ -v 2>&1 | tail -10` — Expected: PASS.

- [ ] **Step 5: Lint what CI will run** — `actionlint .github/workflows/deploy.yml` if available (`brew install actionlint`), otherwise `python3 -c "import yaml,sys;yaml.safe_load(open('.github/workflows/deploy.yml'))"`.

- [ ] **Step 6: Commit**

```bash
git add deploy/smoke-cloudrun.sh .github/workflows/deploy.yml server/deploy_workflow_test.go
git commit -m "build(deploy): GitHub Actions deploy to Cloud Run with a smoke-tested candidate"
```

---

### Task 5: local rehearsal on the production image

**Files:**
- Create: `deploy/rehearsal/docker-compose.yaml`, `deploy/rehearsal/nginx.conf`, `deploy/rehearsal/server.yaml`, `deploy/rehearsal/.env.example`, `deploy/rehearsal/README.md`
- Modify: `.gitignore` (`deploy/rehearsal/.env`, `deploy/rehearsal/google.json`), `server/deploy_contract_test.go`

**Interfaces:**
- Produces: `docker compose -f deploy/rehearsal/docker-compose.yaml up --build`: the production image with the real providers behind a reverse proxy that behaves like Cloud Run (adds `X-Forwarded-Proto: https` and `X-Forwarded-For`), the dentistry MCP, the UI and WebSocket on `http://localhost:8080`. Used for the manual acceptance run in Task 7.

Design notes: the production profile refuses `mcp.allow_local_http`, and the MCP destination policy accepts only public HTTPS (or loopback in development). The rehearsal therefore runs the voice server with the **development** profile (everything else from `cloudrun.yaml`) so it can reach the dental MCP on loopback: the dental MCP container joins the server's network namespace (`network_mode: "service:govoice"`), so it is `127.0.0.1:8081` for the server. What this does not prove (the production profile's checks, the real Cloud Run proxy) is proven by the Task 7 spike.

- [ ] **Step 1: `deploy/rehearsal/server.yaml`**: start from `config/cloudrun.yaml`; change `profile: development`, add `mcp: {allow_local_http: true, timeout: 5s}`, drop `trusted_proxies` entries that do not apply (keep `172.16.0.0/12` for the compose network), keep `public_admin.enabled: true`, `origin_allow_same_host: true`, `demo.auto_install: "dental"`, `storage.dialect: sqlite`. Add a header comment listing exactly what differs from `config/cloudrun.yaml` and why. A test (next step) fails if the two files drift in anything else.

- [ ] **Step 2: `deploy/rehearsal/nginx.conf`**

```nginx
events {}
http {
  map $http_upgrade $connection_upgrade { default upgrade; '' close; }
  server {
    listen 8080;
    location / {
      proxy_pass http://govoice:8080;
      proxy_http_version 1.1;
      proxy_set_header Host $http_host;
      proxy_set_header X-Forwarded-Proto https;       # what Cloud Run's front end sets
      proxy_set_header X-Forwarded-For $remote_addr;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection $connection_upgrade;
      proxy_read_timeout 3600s;
    }
  }
}
```

(`govoice` service name resolves inside the compose network; the proxy joins the default network and reaches the server's published-to-network port 8080.)

- [ ] **Step 3: `deploy/rehearsal/docker-compose.yaml`**

```yaml
# Rehearse the Cloud Run deployment on your machine with REAL providers (spends a little credit).
# 1. cp deploy/rehearsal/.env.example deploy/rehearsal/.env and fill it in   2. cp your Google service-account JSON to deploy/rehearsal/google.json
# 3. docker compose -f deploy/rehearsal/docker-compose.yaml up --build        4. open http://localhost:8080
services:
  govoice:
    build: {context: ../.., dockerfile: deploy/Dockerfile}
    command: ["/opt/govoice/bin/server", "-config", "/etc/rehearsal/server.yaml"]
    env_file: .env
    environment:
      GOVOICE_DATABASE_DSN: /tmp/govoice.db
      GOOGLE_APPLICATION_CREDENTIALS: /secrets/google.json
      DENTAL_MCP_URL: http://127.0.0.1:8081/mcp
      MCP_TOKEN_DENTAL: ${DENTAL_MCP_TOKEN}
    volumes:
      - ./server.yaml:/etc/rehearsal/server.yaml:ro
      - ./google.json:/secrets/google.json:ro
    read_only: true
    tmpfs: [/tmp]
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
  dental-mcp:
    build: {context: ../.., dockerfile: deploy/Dockerfile.dental-mcp}
    network_mode: "service:govoice"          # 127.0.0.1:8081 for the server, as the loopback MCP policy requires
    environment:
      DENTAL_MCP_TOKEN: ${DENTAL_MCP_TOKEN}
    read_only: true
    cap_drop: [ALL]
  proxy:
    image: nginx:1.27-alpine@sha256:<digest at build time>
    ports: ["8080:8080"]
    volumes: ["./nginx.conf:/etc/nginx/nginx.conf:ro"]
    depends_on: [govoice]
```

`.env.example`: `XAI_API_KEY=`, `ELEVENLABS_API_KEY=`, `GOOGLE_API_KEY=`, `TOKEN_SECRET=` (32+ chars), `GOVOICE_MANAGEMENT_API_KEY=` (32+ chars, different from TOKEN_SECRET), `DENTAL_MCP_TOKEN=`. Comment line: "`openssl rand -hex 32` makes a good value for the last three". Add the two paths to `.gitignore`.

The binary path in `command` assumes the image's default `PATH`/layout from `deploy/Dockerfile`; the entrypoint (tini) stays. The server runs as uid 10001: `google.json` must be readable by it (`chmod 644`; it is a local, gitignored file).

- [ ] **Step 4: Drift test** (append to `server/deploy_contract_test.go`): load both YAML files into `map[string]any`, delete the keys the header comment declares different (`profile`, `mcp`, `tls`), and fail if the remaining maps are not deeply equal. Run it: FAIL first if you mistype a key, then PASS.

- [ ] **Step 5: Boot it and look** 

Run: `docker compose -f deploy/rehearsal/docker-compose.yaml up --build -d` then `deploy/smoke-cloudrun.sh http://localhost:8080` with `SMOKE_LOCAL=1 GOVOICE_MANAGEMENT_API_KEY=<the .env value>`.
Expected: `SMOKE OK`; open `http://localhost:8080`, enter the key, see the "Smile Dental front desk" flow (installed on start), and the Providers page with Google, xAI and ElevenLabs detected.

- [ ] **Step 6: Commit**

```bash
git add deploy/rehearsal .gitignore server/deploy_contract_test.go
git commit -m "build(deploy): rehearse the Cloud Run deployment locally with real providers"
```

---

### Task 6: GitHub repository and the first push (STOP: needs the user's go)

Nothing in this task runs without the user's explicit go-ahead in this session; each numbered step is its own go.

- [ ] **Step 1: Pre-flight (local, read-only).** Show the user: `git remote -v` (empty), `git branch -a`, `git log --oneline -1`, `git status --short`, and the size of what will be pushed (`git count-objects -vH`). Note for the user that `.github/workflows/live.yml` schedules a nightly paid run (03:17 UTC) that needs repository secrets that will not exist; ask whether to leave it (it will fail nightly) or disable the schedule in the same push.
- [ ] **Step 2: Create the private repository** (user go): `gh repo create <owner>/go-voice --private --source=. --remote=origin --description "Voice AI pipeline server (Go)"` (no push yet).
- [ ] **Step 3: Push `main` and the archive tag** (user go): `git push -u origin main` and `git push origin archive/container-voice-package`. Do not push other branches unless asked.
- [ ] **Step 4: Watch the first CI run**: `gh run watch --exit-status` (the macOS job and `make ort` download are slow; the `graph-e2e` job runs `make e2e-fake`, which now includes the one-port suite). Fix what the first real runner run reveals (missing `make ort` pins, submodule fetch, Playwright browsers) with ordinary commits and re-push.
- [ ] **Step 5: Protect `main`** (user go): require the checks `linux`, `macos`, `postgres`, `image-smoke`, `graph-e2e` (the list in `ci.yml`'s header), no force-push: `gh api -X PUT repos/<owner>/go-voice/branches/main/protection --input protection.json` with the JSON written to the scratchpad, not committed.
- [ ] **Step 6: Record** the repo URL and what was pushed in the session notes; no repository files change in this task.

---

### Task 7: the first Cloud Run deploy as a spike, guardrails, and the acceptance run (STOP: needs the user's go for each external step)

**Files:**
- Create: `docs/deploy/cloudrun.md`
- Modify: `config/cloudrun.yaml` (measured `trusted_proxies`, sizes), `deploy/gcp/govoice.service.yaml` if measurements demand it

- [ ] **Step 1: Bootstrap (user go; the user runs it, with their credentials).** The user exports the six inputs in their own shell and runs `PROJECT_ID=... GITHUB_REPO=... deploy/gcp/bootstrap.sh` after `DRY_RUN=1` once to read the plan. Never ask the user to paste a key into this conversation.
- [ ] **Step 2: Rehearse locally first (Task 5) and make the two demo calls** by hand against real providers, with the acceptance script from the spec (Sam Taylor / Maria Lopez). Fix prompts or provider choices if a call does not go to plan; commit prompt changes with a test that the installer's flow still decodes.
- [ ] **Step 3: First deploy** (user go): `gh workflow run deploy.yml` then `gh run watch`. The smoke test's `client_ip`/`secure` check is the proxy-trust measurement: if it fails with the peer shown in `gcloud run services logs read govoice`, put the peer's CIDR in `tls.trusted_proxies` of `config/cloudrun.yaml`, keep the existing entries, commit, and let the next deploy confirm.
- [ ] **Step 4: Measure and write down** in `docs/deploy/cloudrun.md`: cold start (stop all instances, time `/health`), memory high-water mark under one call (`gcloud monitoring` or the Cloud Run metrics page), first-audio latency in the browser (the admin UI's call detail shows it), VAD worker count that fits 2 vCPU, and the MCP service's cold start inside a call (the first tool call after scale-to-zero must finish within `mcp.timeout`; if not, set the MCP service `minScale: 1` and say so in the doc).
- [ ] **Step 5: Acceptance** (user go for the paid calls): the six steps of the spec's acceptance script on the deployed URL. The user makes the two calls in their browser; verify afterwards from the API (not by watching): the calls list shows both with `Front desk → New patient` / `→ Returning patient` paths, `call.end` spans with `ended_by_flow`, MCP tool calls recorded, and per-turn latencies present.
- [ ] **Step 6: Guardrails documented and verified.** In `docs/deploy/cloudrun.md`: the budget alert (bootstrap creates it when `BILLING_ACCOUNT` is set), `limits.max_sessions: 10` and `max_call_duration: 20m` in `config/cloudrun.yaml`, per-IP limits, and the kill switch: `gcloud run services update govoice --min-instances 0 --region <r>` (idle spend stops; the demo reinstalls on the next start) or `gcloud run services delete govoice` (total stop). Verify the kill switch once.
- [ ] **Step 7: Write `docs/deploy/cloudrun.md`**: one-time setup (bootstrap inputs and outputs), the secrets table (name, where it is used, how to rotate: `printf %s "$NEW" | gcloud secrets versions add NAME --data-file=-` then redeploy), how a deploy flows (CI -> deploy -> candidate -> smoke -> traffic), rollback (`gcloud run services update-traffic govoice --to-revisions <rev>=100`), cost notes (instance-based billing while an instance exists, scale to zero when not demoing), and the demo's limits (SQLite resets on restart; one instance; call history is lost on restart; the management API is on the internet behind the key).
- [ ] **Step 8: Final gate and review.** `gofmt -l .`, `make verify`, `make e2e-free`; then the independent reviewer (AGENTS.md) with the Review Focus list; fix Critical/Important findings test-first. Commit: `git add docs config deploy && git commit -m "docs(deploy): Cloud Run runbook with measurements and guardrails"`.
